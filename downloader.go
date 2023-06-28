package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"golang.org/x/sys/unix"
)

// Generate inclusive-inclusive range header string from the array
// of indices passed to GetRanges().
//
// Returns garbage for empty ranges array.
func GenerateRangeString(ranges [][]int64) string {
	var rangeString = "bytes="
	for i := 0; i < len(ranges); i++ {
		rangeString += strconv.FormatInt(ranges[i][0], 10) + "-" + strconv.FormatInt(ranges[i][1]-1, 10)
		if i+1 < len(ranges) {
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
	// ranges is an n x 2 array where pairs of ints represent ranges of data to include.
	// These pairs follow the convention of [x, y] means data[x] inclusive to data[y] exclusive.
	GetRanges(ranges [][]int64) (*multipart.Reader, error)
}

func GetDownloader(url string) Downloader {
	var netTransport = &http.Transport{
		Dial: (&net.Dialer{
			Timeout: time.Duration(opts.ConnTimeout) * time.Second,
		}).Dial,
		TLSHandshakeTimeout: time.Duration(opts.ConnTimeout) * time.Second,
	}
	var httpClient = http.Client{
		Transport: netTransport,
	}
	if strings.HasPrefix(url, "s3") {
		return S3Downloader{url, s3.New(session.Must(session.NewSession(&aws.Config{HTTPClient: &httpClient})))}
	} else {
		return HttpDownloader{url, &httpClient}
	}
}

// Returns a single io.Reader byte stream that transparently makes use of parallel
// workers to speed up download.
//
// Will fall back to a single download stream if the download source doesn't support
// RANGE requests or if the total file is smaller than a single download chunk.
func GetDownloadStream(downloader Downloader, chunkSize int64, numWorkers int) io.Reader {
	var size, supportsRange, supportsMultipart = downloader.GetFileInfo()
	fmt.Fprintln(os.Stderr, "File Size (MiB): "+strconv.FormatInt(size/1e6, 10))
	if !supportsRange || size < chunkSize {
		return downloader.Get()
	}

	// Bool channels used to synchronize when workers write to the output stream.
	// Each worker sleeps until a token is pushed to their channel by the previous
	// worker. When they finish writing their current chunk they push a token to
	// the next worker. This avoids multiple workers writing data at the same time
	// and getting garbled.
	var chans []chan bool
	for i := 0; i < numWorkers; i++ {
		chans = append(chans, make(chan bool, 1))
	}

	// All workers share a single writer pipe, the reader side is used by the
	// eventual consumer.
	var reader, writer = io.Pipe()

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
	// Send initial token to worker 0 signifying they can start writing data to
	// the pipe.
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
	var workerNum = start / chunkSize

	var reader = NewReader(size, start, chunkSize, numWorkers, supportsMultipart, downloader)

	// Keep track of how many times we've tried to connect to download server for current chunk.
	// Used for limiting retries on slow/stalled network connections.
	var attemptNumber = 1
	var maxTimeForChunkMilli = float64(chunkSize) / minSpeedBytesPerMillisecond

	// Used for logging periodic download speed stats
	var timeDownloadingMilli = float64(0)
	var totalDownloaded = float64(0)
	var lastLogTime = time.Now()
	// In memory buffer for caching our chunk until it's our turn to write to pipe
	var buf = make([]byte, chunkSize)
	var r = bytes.NewReader(buf)
	for reader.CurPos < size {

		reader.RequestNextChunk()
		if !reader.UseMultipart() {
			// When not using multipart, every new chunk is a new network connection so reset attemptNumber
			attemptNumber = 1
		}

		// Read data off the wire and into an in memory buffer to greedily
		// force the chunk to be read. Otherwise we'd still be
		// bottlenecked by resp.Body.Read() when copying to stdout.
		//
		// We explicitly use Read() rather than ReadAll() as this lets us use
		// a static buffer of our choosing. ReadAll() allocates a new buffer
		// for every single chunk, resulting in a ~30% slowdown.
		var totalRead = 0
		var chunkStartTime = time.Now()
		for {
			var read int
			read, err = reader.Read(buf[totalRead:])
			// Make sure to handle bytes read before error handling, Read() can return bytes and EOF in the same call.
			totalRead += read
			totalDownloaded += float64(read)
			if err == io.EOF {
				reader.Close()
				break
			}
			if time.Since(lastLogTime).Seconds() >= 30 {
				var timeSoFarSec = (timeDownloadingMilli + float64(time.Since(chunkStartTime).Milliseconds())) / 1000
				fmt.Fprintf(os.Stderr, "Worker %d downloading average %.3fMBps\n", workerNum, totalDownloaded/1e6/timeSoFarSec)
				lastLogTime = time.Now()
			}
			var elapsedMilli = float64(time.Since(chunkStartTime).Milliseconds())
			// For accurately enforcing very high min speeds, check if the entire chunk is done in time
			var chunkTimedOut = elapsedMilli > maxTimeForChunkMilli
			// For enforcing very low min speeds in a reasonable amount of time, verify we're meeting the min speed
			// after a second.
			var chunkTooSlowSoFar = elapsedMilli > 1000 && float64(totalRead)/elapsedMilli < minSpeedBytesPerMillisecond
			if chunkTimedOut || chunkTooSlowSoFar || err != nil {
				if attemptNumber > opts.RetryCount {
					fmt.Fprintln(os.Stderr, "Too many slow/stalled/failed connections for this chunk, giving up.")
					os.Exit(int(unix.EIO))
				}
				if err != nil {
					fmt.Fprintf(os.Stderr, "Worker %d failed to read current chunk, resetting connection: %s\n", workerNum, err.Error())
				} else {
					fmt.Fprintf(os.Stderr, "Worker %d timed out on current chunk, resetting connection\n", workerNum)
				}
				reader.Reset()
				reader.RequestNextChunk()
				totalRead = 0
				chunkStartTime = time.Now()
				attemptNumber++
			}
		}
		timeDownloadingMilli += float64(time.Since(chunkStartTime).Milliseconds())
		// Only slice the buffer if not completely full.
		// I saw a marginal slowdown when always slicing (even if
		// the slice was of the whole buffer)
		if int64(totalRead) == chunkSize {
			r.Reset(buf)
		} else {
			r.Reset(buf[:totalRead])
		}

		// Wait until previous worker finished before we start writing to stdout.
		// They'll send a token through this channel when they're done.
		<-curChan
		if _, err = io.Copy(writer, r); err != nil {
			log.Fatal("io copy failed:", err.Error())
		}
		// Trigger next worker to start writing to stdout.
		// Only send token if next worker has more work to do,
		// otherwise they already exited and won't be waiting
		// for a token.
		if reader.CurPos+chunkSize < size {
			nextChan <- true
		} else {
			writer.Close()
		}
		reader.AdvanceNextChunk()
	}
	fmt.Fprintf(os.Stderr, "Worker %d total download speed %.3fMBps\n", workerNum, totalDownloaded/1e3/timeDownloadingMilli)
}
