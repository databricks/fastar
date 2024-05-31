package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/sys/unix"
	"google.golang.org/api/option"
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

func ChunkFinished(curChunkStart, curProgress, fileSize, chunkSize int64) bool {
	return curProgress == chunkSize || curChunkStart+curProgress >= fileSize
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

func GetDownloader(url string, useFips bool) Downloader {
	// NOTE: Only S3 + HTTP downloaders use this transport. GCS uses the default transport configured by the SDK.
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
		cfg, err := config.LoadDefaultConfig(context.Background())
		if err != nil {
			log.Fatal("Failed to load s3 config: ", err)
		}
		client := s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.HTTPClient = &httpClient
			o.RetryMaxAttempts = opts.RetryCount
			if useFips {
				o.EndpointOptions.UseFIPSEndpoint = aws.FIPSEndpointStateEnabled
			}
		})
		return S3Downloader{url, client}
	} else if strings.HasPrefix(url, "gs") {
		ctx := context.Background()
		options := []option.ClientOption{}

		// Add custom credentials option if defined
		credsJSON := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS_JSON")
		if credsJSON != "" {
			options = append(options, option.WithCredentialsJSON([]byte(credsJSON)))
		}

		client, err := storage.NewClient(
			ctx,
			options...,
		)
		if err != nil {
			log.Fatal("Failed to create GCS client: ", err)
		}
		return GCSDownloader{url, client}
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
	log.Printf("File Size (MiB): %d", size/1e6)
	log.Println("Supports RANGE:", supportsRange)
	log.Println("Supports multipart RANGE:", supportsMultipart)
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

	// Used for logging periodic download speed stats
	var timeDownloadingMilli = float64(0)
	var totalDownloaded = float64(0)
	var lastLogTime = time.Now()

	// Read data off the wire and into an in memory buffer to greedily
	// force the chunk to be read. Otherwise we'd still be
	// bottlenecked by resp.Body.Read() when copying to stdout.
	var buf = make([]byte, chunkSize)

	for reader.CurChunkStart < size {

		reader.RequestChunk()
		if !reader.UseMultipart() {
			// When not using multipart, every new chunk is a new network request so reset attemptNumber
			attemptNumber = 1
		}

		var totalRead = int64(0)
		var totalWritten = int64(0)

		// Used by reader thread to tell writer there's more data it can pipe out
		moreToWrite := make(chan bool, 1)

		// Async thread to read off the network into in memory buffer
		go func() {
			var chunkStartTime = time.Now()
			var attemptStartTime = time.Now()
			for {
				if totalRead > totalWritten && len(moreToWrite) == 0 {
					moreToWrite <- true
				}
				var read int
				// We explicitly use Read() rather than ReadAll() as this lets us use
				// a static buffer of our choosing. ReadAll() allocates a new buffer
				// for every single chunk, resulting in a ~30% slowdown.
				// We also wouldn't be able to enforce min speeds as there's no way to
				// MITM ReadAll().
				read, err = reader.Read(buf[totalRead:])
				totalRead += int64(read)
				totalDownloaded += float64(read)
				// Make sure to handle bytes read before error handling, Read() can return successful bytes and error in the same call.
				if ChunkFinished(reader.CurChunkStart, totalRead, size, chunkSize) {
					reader.Close()
					break
				}
				var chunkElapsedMilli = float64(time.Since(chunkStartTime).Milliseconds())
				var attemptElapsedMilli = float64(time.Since(attemptStartTime).Milliseconds())
				if time.Since(lastLogTime).Seconds() >= 30 {
					var timeSoFarSec = (timeDownloadingMilli + chunkElapsedMilli) / 1000
					log.Printf("Worker %d downloading average %.3fMBps\n", workerNum, totalDownloaded/1e6/timeSoFarSec)
					lastLogTime = time.Now()
				}
				// For enforcing very low min speeds in a reasonable amount of time, verify our avg speed is fast enough
				// after MinSpeedWait seconds.
				var chunkTooSlowSoFar = attemptElapsedMilli > float64(opts.MinSpeedWait)*1000 && float64(totalRead)/chunkElapsedMilli < minSpeedBytesPerMillisecond
				if chunkTooSlowSoFar || err != nil {
					if attemptNumber > opts.RetryCount {
						log.Printf("Too many slow/stalled/failed connections for worker %d's chunk, giving up.", workerNum)
						os.Exit(int(unix.EIO))
					}
					if err != nil {
						log.Printf("Worker %d failed to read current chunk, resetting connection: %s\n", workerNum, err.Error())
					} else {
						log.Printf("Worker %d too slow so far for current chunk, resetting connection\n", workerNum)
					}
					reader.Reset(reader.CurChunkStart + totalRead)
					reader.RequestChunk()
					attemptNumber++
					// Reset timing related info relative to what we have left to download for this chunk
					attemptStartTime = time.Now()
				}
			}
			timeDownloadingMilli += float64(time.Since(chunkStartTime).Milliseconds())
			moreToWrite <- true
		}()

		// wait for our turn to write to shared pipe
		<-curChan

		// Logic to write our current chunk
		for !ChunkFinished(reader.CurChunkStart, totalWritten, size, chunkSize) {
			if ChunkFinished(reader.CurChunkStart, totalRead, size, chunkSize) {
				// This worker has read its entire chunk off the wire, pipe the rest to writer in a single call
				if written, err := io.Copy(writer, bytes.NewReader(buf[totalWritten:totalRead])); err != nil {
					log.Fatal("io copy failed:", err.Error())
				} else {
					totalWritten += int64(written)
				}
			} else {
				// Worker hasn't finished reading entire chunk but it's our turn to write. Eagerly
				// write what we have so far.
				// Avoid spinning until there's something to write, wait for reader thread to
				// tell us it has something.
				<-moreToWrite
				if written, err := writer.Write(buf[totalWritten:totalRead]); err != nil {
					log.Fatal("partial write failed:", err.Error())
				} else {
					totalWritten += int64(written)
				}
			}
		}

		// Trigger next worker to start writing to stdout.
		// Only send token if next worker has more work to do,
		// otherwise they already exited and won't be waiting
		// for a token.
		if reader.CurChunkStart+chunkSize < size {
			nextChan <- true
		} else {
			writer.Close()
		}
		reader.AdvanceNextChunk()
	}
	log.Printf("Worker %d total download speed %.3fMBps\n", workerNum, totalDownloaded/1e3/timeDownloadingMilli)
}
