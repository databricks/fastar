package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

// Generate inclusive-inclusive range header string from the array
// of indices passed to GetRanges().
//
// Returns garbage for empty ranges array.
func GenerateRangeString(ranges []int64) string {
	rangeString := "bytes="
	for i := 0; i+1 < len(ranges); i += 2 {
		rangeString += strconv.FormatInt(ranges[i], 10) + "-" + strconv.FormatInt(ranges[i+1]-1, 10)
		if i+3 < len(ranges) {
			rangeString += ","
		}
	}
	return rangeString
}

type Downloader interface {
	// Return the file size, RANGE request support, and multipart RANGE request support.
	GetFileInfo() (int64, bool, bool)

	// Reader for the entire file.
	Get() io.ReadCloser

	// Return an io.Reader with the data in single specified range.
	//
	// start is inclusive and end is exclusive.
	GetRange(start, end int64) io.ReadCloser

	// Return a multipart.Reader with the data in the specified ranges and an error if failed.
	//
	// ranges is an array where pairs of ints represent ranges of data to include.
	// These pairs follow the convention of [x, y] means data[x] inclusive to data[y] exclusive.
	GetRanges(ranges []int64) (*multipart.Reader, error)
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
	size, supportsRange, supportsMultipart := downloader.GetFileInfo()
	fmt.Fprintln(os.Stderr, "File Size (MiB): "+strconv.FormatInt(size>>20, 10))
	if !supportsRange || size < chunkSize {
		return downloader.Get()
	}

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
			supportsMultipart,
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
	supportsMultipart bool,
	size int64, // total file size
	start int64, // the starting offset for the first chunk for this worker
	chunkSize int64,
	numWorkers int,
	writer *io.PipeWriter,
	curChan chan bool,
	nextChan chan bool) {

	var err error
	var multiReader *multipart.Reader

	// Don't use multipart if the current worker doesn't need it.
	// Otherwise the response won't contain multipart boundary tokens
	// and the reader object will break.
	useMultipart := supportsMultipart && (start+chunkSize*int64(numWorkers) < size)

	if useMultipart {
		ranges := []int64{}
		curChunkStart := start
		for curChunkStart < size {
			ranges = append(ranges, curChunkStart, curChunkStart+chunkSize)
			curChunkStart += (chunkSize * int64(numWorkers))
		}
		multiReader, err = downloader.GetRanges(ranges)
		if err != nil {
			log.Fatal("Failed to get ranges from file:", err.Error())
		}
	}

	curChunkStart := start
	buf := make([]byte, chunkSize)
	r := bytes.NewReader(buf)
	var multipartChunk *multipart.Part
	var chunk io.ReadCloser
	for curChunkStart < size {
		if useMultipart {
			multipartChunk, err = multiReader.NextPart()
			if err != nil {
				log.Fatal("Error getting next multipart chunk:", err.Error())
			}
			defer multipartChunk.Close()
		} else {
			chunk = downloader.GetRange(curChunkStart, curChunkStart+chunkSize)
			defer chunk.Close()
		}

		// Read data off the wire and into an in memory buffer.
		// If this is the first chunk of the file, no point in first
		// reading it to a buffer before writing to stdout, we already need
		// it *now*.
		//
		// For all other chunks, read them into an in memory buffer to greedily
		// force the chunk to be read off the wire. Otherwise we'd still be
		// bottlenecked by resp.Body.Read() when copying to stdout.
		//
		// We explicitly use Read() rather than ReadAll() as this lets us use
		// a static buffer of our choosing. ReadAll() allocates a new buffer
		// for every single chunk, resulting in a ~30% slowdown.
		if curChunkStart > 0 {
			totalRead := 0
			for {
				var read int
				if useMultipart {
					read, err = multipartChunk.Read(buf[totalRead:])
				} else {
					read, err = chunk.Read(buf[totalRead:])
				}
				totalRead += read
				if err == io.EOF {
					break
				}
				if err != nil {
					log.Fatal("Failed to read from resp:", err.Error())
				}
			}
			// Only slice the buffer for the case of the leftover data.
			// I saw a marginal slowdown when always slicing (even if
			// the slice was of the whole buffer)
			if int64(totalRead) == chunkSize {
				r.Reset(buf)
			} else {
				r.Reset(buf[:totalRead])
			}
		}

		// Wait until previous worker finished before we start writing to stdout
		<-curChan
		if curChunkStart > 0 {
			_, err = io.Copy(writer, r)
		} else {
			if useMultipart {
				_, err = io.Copy(writer, multipartChunk)
			} else {
				_, err = io.Copy(writer, chunk)
			}
		}
		if err != nil {
			log.Fatal("io copy failed:", err.Error())
		}
		// Trigger next worker to start writing to stdout.
		// Only send token if next worker has more work to do,
		// otherwise they already exited and won't be waiting
		// for a token.
		if curChunkStart+chunkSize < size {
			nextChan <- true
		} else {
			writer.Close()
		}
		curChunkStart += (chunkSize * int64(numWorkers))
	}
}
