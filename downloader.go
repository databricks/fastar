package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

type Downloader interface {
	// Return the file size and if the server supports RANGE requests
	GetFileInfo() (int64, bool)

	// Return an io.Reader with the data in the specified ranges and the associated content length.
	//
	// ranges is an array where pairs of ints represent ranges of data to include.
	// These pairs follow the convention of [x, y] means data[x] inclusive to data[y] exclusive.
	//
	// If ranges is empty, this will return an io.Reader for the contents of the entire file.
	GetRanges(ranges []int64) (io.ReadCloser, int64)
}

// Returns a single io.Reader byte stream that transparently makes use of parallel
// workers to speed up download.
//
// Infers s3 vs http download source by the url (should I include a flag to force this?)
//
// Will fall back to a single download stream if the download source doesn't support
// RANGE requests or if the total file is smaller than a single download chunk.
func GetDownloadStream(url string, chunkSize int64, numWorkers int) io.Reader {
	var downloader Downloader
	if strings.HasPrefix(url, "s3") {
		downloader = S3Downloader{url, s3.New(session.Must(session.NewSession()))}
	} else {
		downloader = HttpDownloader{url, &http.Client{}}
	}
	size, supportsRange := downloader.GetFileInfo()
	if !supportsRange || size < chunkSize {
		stream, _ := downloader.GetRanges([]int64{})
		return stream
	}
	fmt.Fprintln(os.Stderr, "File Size (MiB): "+strconv.FormatInt(size>>20, 10))

	// Bool channels used to synchronize when workers write to the output stream.
	// Each worker sleeps until a token is pushed to their channel by the previous
	// worker. When they finish writing their current chunk they push a token to
	// the next worker.
	var chans []chan bool
	for i := 0; i < numWorkers; i++ {
		chans = append(chans, make(chan bool, 1))
	}

	// All workers share a single writer pipe, the reader side is used by the
	// eventual consumer.
	reader, writer := io.Pipe()

	for i := 0; i < numWorkers; i++ {
		go writePartial(
			downloader,
			size,
			int64(i)*chunkSize,
			chunkSize,
			numWorkers,
			writer,
			chans[i],
			chans[(i+1)%numWorkers])
	}
	chans[0] <- true
	return reader
}

// Individual worker thread entry function
func writePartial(
	downloader Downloader,
	size int64, // total file size
	start int64, // the starting offset for the first chunk for this worker
	chunkSize int64,
	numWorkers int,
	writer *io.PipeWriter,
	curChan chan bool,
	nextChan chan bool) {

	var err error
	buf := make([]byte, chunkSize)
	r := bytes.NewReader(buf)
	for {
		if start >= size {
			return
		}
		chunk, contentLength := downloader.GetRanges([]int64{start, start + chunkSize})
		defer chunk.Close()

		// Read data off the wire and into an in memory buffer.
		// If this is the first chunk of the file, no point in first
		// reading it to a buffer before writing to stdout, we already need
		// it *now*.
		// For all other chunks, read them into an in memory buffer to greedily
		// force the chunk to be read off the wire. Otherwise we'd still be
		// bottlenecked by resp.Body.Read() when copying to stdout.
		if start > 0 {
			totalRead := 0
			for totalRead < int(contentLength) {
				read, err := chunk.Read(buf[totalRead:])
				if err == io.EOF {
					break
				}
				if err != nil {
					log.Fatal("Failed to read from resp:", err.Error())
				}
				totalRead += read
			}
		}
		// Only slice the buffer for the case of the leftover data.
		// I saw a marginal slowdown when always slicing (even if
		// the slice was of the whole buffer)
		if contentLength == chunkSize {
			r.Reset(buf)
		} else {
			r.Reset(buf[:contentLength])
		}

		// Wait until previous worker finished before we start writing to stdout
		<-curChan
		if start > 0 {
			_, err = io.Copy(writer, r)
		} else {
			_, err = io.Copy(writer, chunk)
		}
		if err != nil {
			log.Fatal("io copy failed:", err.Error())
		}
		// Trigger next worker to start writing to stdout.
		// Only send token if next worker has more work to do,
		// otherwise they already exited and won't be waiting
		// for a token.
		if start+chunkSize < size {
			nextChan <- true
		} else {
			writer.Close()
		}
		start += (chunkSize * int64(numWorkers))
	}
}
