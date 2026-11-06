package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pierrec/lz4"
)

type modelFile struct {
	name string
	data []byte
}

// modelFiles is shaped like a model artefact: one file big enough to be streamed to disk,
// a few that are buffered, and many small ones. Sizes scale with unit.
func modelFiles(unit int) []modelFile {
	rng := rand.New(rand.NewSource(1))
	file := func(name string, size int) modelFile {
		data := make([]byte, size)
		rng.Read(data)
		// Bytes below 128 hold 7 bits each: gzip -9 shrinks them to 0.88 and lz4 not at all,
		// close to our models (0.90 and 0.98), so decompression costs about the same.
		for i := range data {
			data[i] &= 0x7f
		}
		return modelFile{name, data}
	}
	files := []modelFile{file("model/weights.bin", 24*unit)}
	for i := 0; i < 4; i++ {
		files = append(files, file(fmt.Sprintf("model/shard-%d.bin", i), 2*unit))
	}
	for i := 0; i < 64; i++ {
		files = append(files, file(fmt.Sprintf("model/assets/%02d.json", i), unit/16))
	}
	return files
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// archive packs files as a tarball, compressed when format is "tar.gz" or "tar.lz4".
func archive(tb testing.TB, files []modelFile, format string) []byte {
	var buf bytes.Buffer
	var w io.WriteCloser = nopWriteCloser{&buf}
	switch format {
	case "tar.gz":
		// Lower levels store this data uncompressed, which would make decompression a copy.
		w, _ = gzip.NewWriterLevel(&buf, gzip.BestCompression)
	case "tar.lz4":
		w = lz4.NewWriter(&buf)
	}
	tw := tar.NewWriter(w)
	for _, f := range files {
		hdr := &tar.Header{Name: f.name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(f.data)),
			Uid: os.Getuid(), Gid: os.Getgid()}
		if err := tw.WriteHeader(hdr); err != nil {
			tb.Fatal(err)
		}
		if _, err := tw.Write(f.data); err != nil {
			tb.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		tb.Fatal(err)
	}
	if err := w.Close(); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

// runFastar does what the CLI does: download url, decompress it and extract it into dir.
func runFastar(url, dir string, chunkSize int64, numWorkers int) {
	downloader := GetDownloader(url, false, false)
	defer downloader.(HttpDownloader).client.CloseIdleConnections()
	opts.OutputDir = dir
	ExtractTar(decompress(GetDownloadStream(downloader, chunkSize, numWorkers), filepath.Base(url)))
}

// useCLIExtractOpts sets the extraction options to the CLI defaults.
func useCLIExtractOpts() {
	opts.WriteWorkers = 8
	opts.MaxBufferSize = 64e6
}

var formats = []string{"tar", "tar.gz", "tar.lz4"}

func TestFastarPipeline(t *testing.T) {
	files := modelFiles(64 << 10)
	for _, format := range formats {
		srv := (&fakeStore{data: archive(t, files, format)}).start()
		defer srv.Close()
		for _, workers := range []int{1, 4} {
			t.Run(fmt.Sprintf("%s/workers=%d", format, workers), func(t *testing.T) {
				useTestOpts(t, 100<<10)
				useCLIExtractOpts()
				opts.MaxBufferSize = 1 << 20 // below the biggest file, so it is streamed
				dir := t.TempDir()
				runFastar(srv.URL+"/model."+format, dir, 100<<10, workers)
				for _, f := range files {
					got, err := os.ReadFile(filepath.Join(dir, f.name))
					if err != nil || !bytes.Equal(got, f.data) {
						t.Fatalf("%s: differs from the archive (%d of %d bytes, error %v)", f.name, len(got), len(f.data), err)
					}
				}
			})
		}
	}
}

var benchArchives struct {
	once  sync.Once
	files []modelFile
	data  map[string][]byte // by format
}

// BenchmarkFastar measures the whole CLI pipeline, as the staging benchmark does, reporting
// MB/s of extracted data so formats compare directly. Files land in the page cache, so it
// measures fastar rather than the disk.
func BenchmarkFastar(b *testing.B) {
	const chunkSize = 4 << 20
	// Built once per process, since -count reruns this function.
	benchArchives.once.Do(func() {
		benchArchives.files = modelFiles(4 << 20) // 144 MiB extracted
		benchArchives.data = map[string][]byte{}
		for _, format := range formats {
			benchArchives.data[format] = archive(b, benchArchives.files, format)
		}
	})
	var extracted int64
	for _, f := range benchArchives.files {
		extracted += int64(len(f.data))
	}

	for _, store := range []struct {
		name string
		s    fakeStore
	}{
		{"local", fakeStore{}},
		// Same as BenchmarkDownload's s3like.
		{"s3like", fakeStore{ttfb: 20 * time.Millisecond, connSetup: 20 * time.Millisecond, rate: 90e6}},
	} {
		for _, format := range formats {
			s := store.s
			s.data = benchArchives.data[format]
			srv := s.start()
			defer srv.Close()
			b.Run(fmt.Sprintf("%s/%s", store.name, format), func(b *testing.B) {
				useTestOpts(b, chunkSize)
				useCLIExtractOpts()
				b.SetBytes(extracted)
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					dir, err := os.MkdirTemp("", "fastar-bench")
					if err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
					runFastar(srv.URL+"/model."+format, dir, chunkSize, 4)
					b.StopTimer()
					for _, f := range benchArchives.files {
						if info, err := os.Stat(filepath.Join(dir, f.name)); err != nil || info.Size() != int64(len(f.data)) {
							b.Fatalf("%s: wrong size or missing (%v)", f.name, err)
						}
					}
					os.RemoveAll(dir)
					b.StartTimer()
				}
			})
		}
	}
}
