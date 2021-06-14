package main

import (
	"io"
	"log"
	"net/http"
	"strconv"
)

type HttpDownloader struct {
	Url    string
	client *http.Client
}

func (httpDownloader HttpDownloader) GetFileInfo() (int64, bool) {
	resp, err := http.Head(httpDownloader.Url)
	if err != nil {
		log.Fatal("Failed HEAD request for file size:", err.Error())
	}
	return resp.ContentLength, true
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
	resp, err := httpDownloader.client.Do(req)
	if err != nil {
		log.Fatal("Failed get request:", err.Error())
	}
	return resp.Body, resp.ContentLength
}
