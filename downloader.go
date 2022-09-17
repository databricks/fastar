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

// Returns a single io.Reader byte stream that transparently makes use of parallel
// workers to speed up download.
//
// Infers s3 vs http download source by the url (should I include a flag to force this?)
//
// Will fall back to a single download stream if the download source doesn't support
// RANGE requests or if the total file is smaller than a single download chunk.
func GetDownloadStream(url string, chunkSize int64, numWorkers int) io.Reader {
	var downloader Downloader
	var netTransport = &http.Transport{
		Dial: (&net.Dialer{
			Timeout: time.Duration(*connTimeout) * time.Second,
		}).Dial,
		TLSHandshakeTimeout: time.Duration(*connTimeout) * time.Second,
	}
	var httpClient = http.Client{
		Transport: netTransport,
	}
	if strings.HasPrefix(url, "s3") {
		downloader = S3Downloader{url, s3.New(session.Must(session.NewSession(&aws.Config{HTTPClient: &httpClient})))}
	} else {
		downloader = HttpDownloader{url, &httpClient}
	}
	var size, supportsRange, supportsMultipart = downloader.GetFileInfo()
	fmt.Fprintln(os.Stderr, "File Size (MiB): "+strconv.FormatInt(size/1e6, 10))
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
	var workerNum = start / chunkSize

	// Don't use multipart if the current worker is too small to need it.
	// Otherwise the response won't contain multipart boundary tokens
	// and the reader object will break.
	// This variable can be changed later on if we need to reset a connection
	// due to a slow stream and our remaining download is now too small for multipart.
	//
	// TODO: find some way to make this cleaner... branching logic everywhere for single
	// range vs multipart is ugly af
	var useMultipart = supportsMultipart && (start+chunkSize*int64(numWorkers) < size)

	// Readers used for either multipart or single range reading
	var multipartChunk *multipart.Part
	var chunk io.ReadCloser

	// Keep track of how many times we've tried to connect to download server for current chunk.
	// Used for limiting retries on slow/stalled network connections.
	var attemptNumber = 1
	var maxTimeForChunkMilli = float64(chunkSize) / minSpeedBytesPerMillisecond

	multiReader = getMultipartReader(&downloader, start, chunkSize, size, numWorkers, useMultipart)

	// Which chunk of the overall file are we reading right now
	var curChunkStart = start
	// Used for logging periodic download speed stats
	var timeDownloadingMilli = float64(0)
	var totalDownloaded = float64(0)
	var lastLogTime = time.Now()
	// In memory buffer for caching our chunk until it's our turn to write to pipe
	var buf = make([]byte, chunkSize)
	var r = bytes.NewReader(buf)
	for curChunkStart < size {

		multipartChunk, chunk = getNextChunk(&downloader, multiReader, curChunkStart, chunkSize, useMultipart)
		if !useMultipart {
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
			if useMultipart {
				read, err = multipartChunk.Read(buf[totalRead:])
			} else {
				read, err = chunk.Read(buf[totalRead:])
			}
			// Make sure to handle bytes read before error handling, Read() can return bytes and EOF in the same call.
			totalRead += read
			totalDownloaded += float64(read)
			if err == io.EOF {
				if useMultipart {
					multipartChunk.Close()
				} else {
					chunk.Close()
				}
				break
			}
			if err != nil {
				log.Fatal("Failed to read from resp:", err.Error())
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
			if chunkTimedOut || chunkTooSlowSoFar {
				if attemptNumber > *retryCount {
					fmt.Fprintln(os.Stderr, "Too many slow/stalled connections for this chunk, giving up.")
					os.Exit(int(unix.EIO))
				}
				fmt.Fprintf(os.Stderr, "Worker %d timed out on current chunk, resetting connection\n", workerNum)
				// Reset useMultipart in case we're now far enough in the file that we can't use it anymore.
				useMultipart = supportsMultipart && (curChunkStart+chunkSize*int64(numWorkers) < size)
				multiReader = getMultipartReader(&downloader, curChunkStart, chunkSize, size, numWorkers, useMultipart)
				multipartChunk, chunk = getNextChunk(&downloader, multiReader, curChunkStart, chunkSize, useMultipart)
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
		if curChunkStart+chunkSize < size {
			nextChan <- true
		} else {
			writer.Close()
		}
		curChunkStart += (chunkSize * int64(numWorkers))
	}
}

func getMultipartReader(
	downloader *Downloader,
	start int64,
	chunkSize int64,
	size int64,
	numWorkers int,
	useMultipart bool) *multipart.Reader {
	if !useMultipart {
		return nil
	}
	var ranges = [][]int64{}
	var curChunkStart = start
	for curChunkStart < size {
		ranges = append(ranges, []int64{curChunkStart, curChunkStart + chunkSize})
		curChunkStart += (chunkSize * int64(numWorkers))
	}
	var reader, err = (*downloader).GetRanges(ranges)
	if err != nil {
		log.Fatal("Failed to get ranges from file:", err.Error())
	}
	return reader
}

// Callers must remember to call reader.Close()
func getNextChunk(
	downloader *Downloader,
	multiReader *multipart.Reader,
	start int64,
	chunkSize int64,
	useMultipart bool) (*multipart.Part, io.ReadCloser) {
	if useMultipart {
		var multipartChunk, err = multiReader.NextPart()
		if err != nil {
			log.Fatal("Error getting next multipart chunk:", err.Error())
		}
		return multipartChunk, nil
	} else {
		return nil, (*downloader).GetRange(start, start+chunkSize)
	}
}
