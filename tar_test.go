package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const mib = 1 << 20

type entry struct {
	name     string
	size     int64  // regular files
	link     string // symlink or hard link target
	typeflag byte
}

// content is deterministic per name so expected bytes can be regenerated without storing them.
func content(name string, size int64) io.Reader {
	seed := int64(0)
	for _, c := range name {
		seed = seed*31 + int64(c)
	}
	return io.LimitReader(rand.New(rand.NewSource(seed)), size)
}

func digest(r io.Reader) [32]byte {
	h := sha256.New()
	io.Copy(h, r)
	var d [32]byte
	copy(d[:], h.Sum(nil))
	return d
}

// writeTar writes entries to a temp file (not memory, so memory tests measure only extraction).
func writeTar(t testing.TB, entries []entry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typeflag, Linkname: e.link, Mode: 0640,
			Uid: os.Getuid(), Gid: os.Getgid()}
		switch e.typeflag {
		case tar.TypeReg:
			h.Size = e.size
		case tar.TypeDir:
			h.Mode = 0750
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, content(e.name, e.size)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return path
}

// extract runs ExtractTar with the given threshold and returns the output directory.
func extract(t testing.TB, tarPath string, maxBuffer int64, writeWorkers int) string {
	t.Helper()
	saved := opts
	t.Cleanup(func() { opts = saved })
	out := t.TempDir()
	opts.OutputDir, opts.MaxBufferSize, opts.WriteWorkers = out, maxBuffer, writeWorkers
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ExtractTar(f)
	return out
}

func TestExtractTarStreamsAndBuffers(t *testing.T) {
	threshold := int64(1 * mib)
	entries := []entry{
		{name: "dir", typeflag: tar.TypeDir},
		{name: "dir/empty", typeflag: tar.TypeReg, size: 0},
		{name: "dir/tiny", typeflag: tar.TypeReg, size: 1},
		{name: "dir/below", typeflag: tar.TypeReg, size: threshold - 1},
		{name: "dir/at", typeflag: tar.TypeReg, size: threshold},
		{name: "dir/above", typeflag: tar.TypeReg, size: threshold + 1},
		// Not a multiple of streamChunkSize: exercises full chunks plus a partial last one.
		{name: "dir/large", typeflag: tar.TypeReg, size: 2*streamChunkSize + 12345},
		{name: "dir/exact-chunks", typeflag: tar.TypeReg, size: 2 * streamChunkSize},
		{name: "nested/deep/file", typeflag: tar.TypeReg, size: threshold + 7},
		{name: "dir/large-hardlink", typeflag: tar.TypeLink, link: "dir/large"},
		{name: "dir/symlink", typeflag: tar.TypeSymlink, link: "large"},
	}
	tarPath := writeTar(t, entries)

	cases := []struct {
		name      string
		maxBuffer int64
	}{
		{"stream everything", 0},
		{"mixed at 1 MiB", threshold},
		{"buffer everything", 1 << 40},
	}
	for _, c := range cases {
		for _, workers := range []int{1, 8} {
			t.Run(fmt.Sprintf("%s, %d write workers", c.name, workers), func(t *testing.T) {
				out := extract(t, tarPath, c.maxBuffer, workers)
				for _, e := range entries {
					p := filepath.Join(out, e.name)
					switch e.typeflag {
					case tar.TypeReg:
						f, err := os.Open(p)
						if err != nil {
							t.Fatalf("%s: %v", e.name, err)
						}
						got := digest(f)
						info, _ := f.Stat()
						f.Close()
						if info.Size() != e.size || got != digest(content(e.name, e.size)) {
							t.Errorf("%s: size %d, want %d, or content differs", e.name, info.Size(), e.size)
						}
						if info.Mode().Perm() != 0640 {
							t.Errorf("%s: mode %v, want 0640", e.name, info.Mode().Perm())
						}
					case tar.TypeLink:
						a, _ := os.Stat(p)
						b, _ := os.Stat(filepath.Join(out, e.link))
						if a == nil || b == nil || !os.SameFile(a, b) {
							t.Errorf("%s: not a hard link to %s", e.name, e.link)
						}
					case tar.TypeSymlink:
						if target, err := os.Readlink(p); err != nil || target != e.link {
							t.Errorf("%s: symlink target %q (%v), want %q", e.name, target, err, e.link)
						}
					}
				}
			})
		}
	}
}

// Streaming exists to keep memory flat: allocations must not scale with the file size.
func TestStreamedFileMemoryIsBounded(t *testing.T) {
	const size = 256 * mib
	tarPath := writeTar(t, []entry{{name: "big", typeflag: tar.TypeReg, size: size}})

	allocated := func(maxBuffer int64) uint64 {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		extract(t, tarPath, maxBuffer, 8)
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	streamed, buffered := allocated(1*mib), allocated(1<<40)
	t.Logf("allocated: streamed %d MiB, buffered %d MiB", streamed/mib, buffered/mib)
	// Two 8 MiB chunk buffers plus tar/os overhead.
	if streamed > 4*streamChunkSize {
		t.Errorf("streamed extraction allocated %d MiB, want <= %d MiB", streamed/mib, 4*streamChunkSize/mib)
	}
	if buffered < size {
		t.Errorf("buffered extraction allocated %d MiB, expected >= file size; test is not exercising buffering",
			buffered/mib)
	}
}

// A truncated archive must fail loudly, not leave a short file behind. ExtractTar exits via
// log.Fatal, so the extraction runs in a child process.
func TestTruncatedStreamedFileFails(t *testing.T) {
	if path := os.Getenv("FASTAR_TEST_TRUNCATED"); path != "" {
		opts.OutputDir, opts.MaxBufferSize, opts.WriteWorkers = os.Getenv("FASTAR_TEST_OUT"), 0, 2
		f, _ := os.Open(path)
		ExtractTar(f)
		return
	}
	full := writeTar(t, []entry{{name: "big", typeflag: tar.TypeReg, size: 3 * streamChunkSize}})
	data, _ := os.ReadFile(full)
	cut := filepath.Join(t.TempDir(), "cut.tar")
	os.WriteFile(cut, data[:len(data)/2], 0600)

	cmd := exec.Command(os.Args[0], "-test.run=^TestTruncatedStreamedFileFails$")
	cmd.Env = append(os.Environ(), "FASTAR_TEST_TRUNCATED="+cut, "FASTAR_TEST_OUT="+t.TempDir())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatal("extracting a truncated archive succeeded, want failure")
	}
	if !strings.Contains(stderr.String(), "Truncated archive") {
		t.Errorf("stderr = %q, want a truncation error", stderr.String())
	}
}

func BenchmarkExtractLargeFile(b *testing.B) {
	const size = 512 * mib
	tarPath := writeTar(b, []entry{{name: "big", typeflag: tar.TypeReg, size: size}})
	for _, c := range []struct {
		name      string
		maxBuffer int64
	}{{"buffered", 1 << 40}, {"streamed", 64 * mib}} {
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(size)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				extract(b, tarPath, c.maxBuffer, 8)
			}
		})
	}
}
