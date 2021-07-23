package main

import (
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/avast/retry-go"
)

type HttpDownloader struct {
	Url    string
	client *http.Client
}

func (httpDownloader HttpDownloader) GetFileInfo() (int64, bool) {
	var resp *http.Response
	err := retry.Do(
		func() error {
			curResp, err := http.Head(httpDownloader.Url)
			if err != nil {
				return err
			}
			if curResp.StatusCode == 404 {
				log.Fatal("404, file not found")
			}
			if curResp.StatusCode < 200 || curResp.StatusCode > 299 {
				return errors.New("non-200 response for HEAD request " + strconv.Itoa(curResp.StatusCode))
			}
			resp = curResp
			return nil
		},
		retry.DelayType(retry.RandomDelay),
		retry.MaxJitter(time.Second*time.Duration(*retryWait)),
		retry.Attempts(uint(*retryCount)),
	)
	if err != nil {
		log.Fatal("Failed HEAD request for file size:", err.Error())
	}
	var supportsRange bool
	if resp.ContentLength > 1 {
		bound := int64(math.Max(0, float64(resp.ContentLength-1)))
		_, size := httpDownloader.GetRanges([]int64{0, bound})
		supportsRange = size == bound
	} else {
		supportsRange = false
	}
	return resp.ContentLength, supportsRange
}

func (httpDownloader HttpDownloader) GetRanges(ranges []int64) (io.ReadCloser, int64) {
	req, err := http.NewRequest("GET", httpDownloader.Url, nil)
	if err != nil {
		log.Fatal("Failed creating request:", err.Error())
	}
	rangeString := "bytes="
	for i := 0; i+1 < len(ranges); i += 2 {
		rangeString += strconv.FormatInt(ranges[i], 10) + "-" + strconv.FormatInt(ranges[i+1]-1, 10)
		if i+3 < len(ranges) {
			rangeString += ","
		}
	}
	if len(ranges) != 0 {
		req.Header.Add("Range", rangeString)
	}
	var resp *http.Response
	err = retry.Do(
		func() error {
			curResp, err := httpDownloader.client.Do(req)
			if err != nil {
				return err
			}
			if curResp.StatusCode < 200 || curResp.StatusCode > 299 {
				return errors.New("non-200 response " + strconv.Itoa(curResp.StatusCode))
			}
			resp = curResp
			return nil
		},
		retry.DelayType(retry.RandomDelay),
		retry.MaxJitter(time.Second*time.Duration(*retryWait)),
		retry.Attempts(uint(*retryCount)),
	)
	if err != nil {
		log.Fatal("Failed get request:", err.Error())
	}
	return resp.Body, resp.ContentLength
}
