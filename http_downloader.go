package main

import (
	"errors"
	"io"
	"io/ioutil"
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
	req := httpDownloader.generateRequest("HEAD")
	resp := httpDownloader.retryHttpRequest(req)

	if resp.ContentLength > opts.ChunkSize {
		// Temporarily disable checking support for range/multipart. These checks
		// initiate file downloads from azure blob storage even if we don't consume
		// the whole body and this may be overloading their servers.
		// TODO: see if there's some way to determine multipart range support without
		// necessarily returning the whole file in the body.
		return resp.ContentLength, resp.Header.Get("Accept-Ranges") != "", false
	} else {
		// If the file is tiny it doesn't matter if we support any kind
		// of range request
		return resp.ContentLength, false, false
	}
}

func (httpDownloader HttpDownloader) Get() io.ReadCloser {
	req := httpDownloader.generateRequest("GET")
	return httpDownloader.retryHttpRequest(req).Body
}

func (httpDownloader HttpDownloader) GetRange(start, end int64) io.ReadCloser {
	req := httpDownloader.generateRequest("GET")

	rangeString := GenerateRangeString([][]int64{{start, end}})
	req.Header.Add("Range", rangeString)

	resp := httpDownloader.retryHttpRequest(req)
	return resp.Body
}

func (httpDownloader HttpDownloader) GetRanges(ranges [][]int64) (*multipart.Reader, error) {
	req := httpDownloader.generateRequest("GET")

	rangeString := GenerateRangeString(ranges)
	if len(ranges) != 0 {
		req.Header.Add("Range", rangeString)
	}

	resp := httpDownloader.retryHttpRequest(req)

	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		resp.Body.Close()
		return nil, errors.New("error parsing content type, multipart likely not supported")
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		return multipart.NewReader(resp.Body, params["boundary"]), nil
	}
	resp.Body.Close()
	return nil, errors.New("multipart not supported in content type")
}

func (httpDownloader HttpDownloader) generateRequest(requestMethod string) *http.Request {
	req, err := http.NewRequest(requestMethod, httpDownloader.Url, nil)
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
			if curResp.StatusCode < 200 || curResp.StatusCode > 299 {
				log.Printf("failed response: %+v\n", *curResp)
				if curResp.ContentLength != 0 {
					if body, err := ioutil.ReadAll(curResp.Body); err == nil {
						log.Println("response body:", string(body))
					}
				}
				if curResp.StatusCode == 404 {
					log.Println("404, file not found")
					os.Exit(int(unix.ENOENT))
				}
				// Azure blob storage can return either 429 or 503 when throttling
				// https://learn.microsoft.com/en-us/azure/storage/blobs/scalability-targets
				if curResp.StatusCode == 429 || curResp.StatusCode == 503 {
					throttled = true
					return errors.New("throttled by download server " + strconv.Itoa(curResp.StatusCode))
				}
				return errors.New("unknown non-2xx response " + strconv.Itoa(curResp.StatusCode))
			}
			resp = curResp
			return nil
		},
		retry.DelayType(retry.BackOffDelay),
		retry.Delay(time.Second*time.Duration(opts.RetryWait)),
		retry.Attempts(uint(opts.RetryCount)),
		retry.MaxDelay(time.Second*time.Duration(opts.MaxWait)),
	)
	if err != nil {
		log.Println("Failed get request:", err.Error())
		if throttled {
			os.Exit(int(unix.EBUSY))
		} else {
			log.Fatal("unknown http response")
		}
	}
	return resp
}
