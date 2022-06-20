package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	urlpkg "net/url"
	"os"
	"path"
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
// Also returns the bare filename for the download, (empty string if reading stdin)
//
// Infers s3 vs http download source by the url (should I include a flag to force this?)
//
// Will fall back to a single download stream if the download source doesn't support
// RANGE requests or if the total file is smaller than a single download chunk.
func GetDownloadStream(url string, chunkSize int64, numWorkers int) (io.Reader, string) {
	if url == "-" {
		return os.Stdin, ""
	}

	parsedUrl, err := urlpkg.Parse(url)
	if err != nil {
		log.Fatal("Failed to parse url: ", err.Error())
	}
	filename := path.Base(parsedUrl.Path)

	var downloader Downloader
	netTransport := &http.Transport{
		Dial: (&net.Dialer{
			Timeout: time.Duration(*connTimeout) * time.Second,
		}).Dial,
		TLSHandshakeTimeout: time.Duration(*connTimeout) * time.Second,
	}
	httpClient := http.Client{
		Transport: netTransport,
	}
	if strings.HasPrefix(url, "s3") {
		downloader = S3Downloader{url, s3.New(session.Must(session.NewSession(&aws.Config{HTTPClient: &httpClient})))}
	} else {
		downloader = HttpDownloader{url, &httpClient}
	}
	size, supportsRange, supportsMultipart := downloader.GetFileInfo()
	fmt.Fprintln(os.Stderr, "File Size (MiB): "+strconv.FormatInt(size/1e6, 10))
	if !supportsRange || size < chunkSize {
		return downloader.Get(), filename
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
	return reader, filename
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
		if multiReader, err = downloader.GetRanges(ranges); err != nil {
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
			if multipartChunk, err = multiReader.NextPart(); err != nil {
				log.Fatal("Error getting next multipart chunk:", err.Error())
			}
			defer multipartChunk.Close()
		} else {
			chunk = downloader.GetRange(curChunkStart, curChunkStart+chunkSize)
			defer chunk.Close()
		}

		// Read data off the wire and into an in memory buffer to greedily
		// force the chunk to be read. Otherwise we'd still be
		// bottlenecked by resp.Body.Read() when copying to stdout.
		//
		// We explicitly use Read() rather than ReadAll() as this lets us use
		// a static buffer of our choosing. ReadAll() allocates a new buffer
		// for every single chunk, resulting in a ~30% slowdown.
		totalRead := 0
		emptyReadCount := 0
		slowReadCount := 0
		startTime := time.Now()
		for {
			var read int
			if useMultipart {
				read, err = multipartChunk.Read(buf[totalRead:])
			} else {
				read, err = chunk.Read(buf[totalRead:])
			}
			// Make sure to handle bytes read before error handling,
			// Read() can return bytes and EOF in the same call.
			totalRead += read
			if err == io.EOF {
				break
			}
			if err != nil {
				log.Fatal("Failed to read from resp:", err.Error())
			}
			// Fast fail for stalled downloads where we haven't read anything
			// off the wire after multiple Read() calls.
			if read == 0 && err == nil {
				emptyReadCount++
				if emptyReadCount == 100 {
					fmt.Fprintln(os.Stderr, "Too many empty reads from response, fast failing")
					os.Exit(int(unix.EIO))
				}
			} else {
				emptyReadCount = 0
			}
			// Fast fail if our average throughput is below the threshold after multiple Read() calls
			if totalRead > 0 && (float64(totalRead)/float64(time.Since(startTime).Nanoseconds()) < minSpeedBytesPerNanosecond) {
				slowReadCount++
				if slowReadCount == 100 {
					fmt.Fprintf(os.Stderr, "Average throughput slower than %sB/s, fast failing\n", *minSpeed)
					os.Exit(int(unix.EIO))
				}
			} else {
				slowReadCount = 0
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
