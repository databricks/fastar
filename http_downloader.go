package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/avast/retry-go"
	"golang.org/x/sys/unix"
)

type HttpDownloader struct {
	Url    string
	client *http.Client
}

func (httpDownloader HttpDownloader) GetFileInfo() (int64, bool, bool) {
	req := httpDownloader.generateRequest()
	resp := httpDownloader.retryHttpRequest(req)

	if resp.ContentLength > opts.ChunkSize {
		_, err := httpDownloader.GetRanges([][]int64{{0, 1}, {1, 2}})
		if err == nil {
			// Assume that if multipart range works, single range also works
			return resp.ContentLength, true, true
		} else {
			// Dumb hack to see if the download source supports range requests.
			// Some servers don't publish `Accept-Ranges: bytes` for a HEAD response
			// even if they support RANGE. To determine, we intentionally make a
			// request for less than the full size and see if it's respected.
			body := httpDownloader.GetRange(0, 1)
			buf, err := io.ReadAll(body)
			return resp.ContentLength, (err == nil && len(buf) == 1), false
		}
	} else {
		// If the file is tiny it doesn't matter if we support any kind
		// of range request
		return resp.ContentLength, false, false
	}
}

func (httpDownloader HttpDownloader) Get() io.ReadCloser {
	req := httpDownloader.generateRequest()
	return httpDownloader.retryHttpRequest(req).Body
}

func (httpDownloader HttpDownloader) GetRange(start, end int64) io.ReadCloser {
	req := httpDownloader.generateRequest()

	rangeString := GenerateRangeString([][]int64{{start, end}})
	req.Header.Add("Range", rangeString)

	resp := httpDownloader.retryHttpRequest(req)
	return resp.Body
}

func (httpDownloader HttpDownloader) GetRanges(ranges [][]int64) (*multipart.Reader, error) {
	req := httpDownloader.generateRequest()

	rangeString := GenerateRangeString(ranges)
	if len(ranges) != 0 {
		req.Header.Add("Range", rangeString)
	}

	resp := httpDownloader.retryHttpRequest(req)

	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, errors.New("error parsing content type, multipart likely not supported")
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		return multipart.NewReader(resp.Body, params["boundary"]), nil
	}
	return nil, errors.New("multipart not supported in content type")
}

func (httpDownloader HttpDownloader) generateRequest() *http.Request {
	req, err := http.NewRequest("GET", httpDownloader.Url, nil)
	if err != nil {
		log.Fatal("Failed creating GET request:", err.Error())
	}

	for key, value := range opts.Headers {
		req.Header.Add(key, value)
	}
	return req
}

func (httpDownloader HttpDownloader) retryHttpRequest(req *http.Request) *http.Response {
	var resp *http.Response
	var throttled = false
	err := retry.Do(
		func() error {
			curResp, err := httpDownloader.client.Do(req)
			if err != nil {
				return err
			}
			if curResp.StatusCode == 404 {
				fmt.Fprintln(os.Stderr, "404, file not found")
				os.Exit(int(unix.ENOENT))
			}
			// Azure blob storage can return either 429 or 503 when throttling
			// https://learn.microsoft.com/en-us/azure/storage/blobs/scalability-targets
			if curResp.StatusCode == 429 || curResp.StatusCode == 503 {
				throttled = true
				return errors.New("throttled by download server " + strconv.Itoa(curResp.StatusCode))
			}
			if curResp.StatusCode < 200 || curResp.StatusCode > 299 {
				return errors.New("non-200 response " + strconv.Itoa(curResp.StatusCode))
			}
			resp = curResp
			return nil
		},
		retry.DelayType(retry.RandomDelay),
		retry.MaxJitter(time.Second*time.Duration(opts.RetryWait)),
		retry.Attempts(uint(opts.RetryCount)),
	)
	if err != nil {
		fmt.Fprint(os.Stderr, "Failed get request:", err.Error())
		if throttled {
			os.Exit(int(unix.EBUSY))
		} else {
			log.Fatal("unknown http response")
		}
	}
	return resp
}
