package main

import (
	"bytes"
	"io"
	"log"
	"math"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
)

const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

func RandomString(n int64) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	return string(b)
}

func TestGenerateRangeString(t *testing.T) {
	ranges := [][]int64{{0, 1}}
	rangeString := GenerateRangeString(ranges)
	expected := "bytes=0-0"
	if rangeString != expected {
		t.Fatalf("Got %s, wanted %s", rangeString, expected)
	}
	ranges = [][]int64{{0, 1}, {3, 6}}
	rangeString = GenerateRangeString(ranges)
	expected = "bytes=0-0,3-5"
	if rangeString != expected {
		t.Fatalf("Got %s, wanted %s", rangeString, expected)
	}
}

type TestDownloader struct {
	Data             string
	RangeSupport     bool
	MultipartSupport bool
}

func (testDownloader TestDownloader) GetFileInfo() (int64, bool, bool) {
	return int64(len(testDownloader.Data)), testDownloader.RangeSupport, testDownloader.MultipartSupport
}

func (testDownloader TestDownloader) Get() io.ReadCloser {
	return io.NopCloser(strings.NewReader(testDownloader.Data))
}

func (testDownloader TestDownloader) GetRange(start, end int64) io.ReadCloser {
	return io.NopCloser(strings.NewReader(testDownloader.Data[start:end]))
}

func (testDownloader TestDownloader) GetRanges(ranges [][]int64) (*multipart.Reader, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for _, r := range ranges {
		part, err := writer.CreatePart(textproto.MIMEHeader{})
		if err != nil {
			log.Fatal(err)
		}
		_, err = io.Copy(part, strings.NewReader(testDownloader.Data[r[0]:r[1]]))
		if err != nil {
			log.Fatal(err)
		}
	}
	err := writer.Close()
	if err != nil {
		log.Fatal(err)
	}
	return multipart.NewReader(body, writer.Boundary()), nil
	// return nil, nil
}

func TestSingleReader(t *testing.T) {
	opts.RetryCount = math.MaxInt64
	for fileSize := int64(0); fileSize < 64; fileSize++ {
		data := RandomString(fileSize)
		downloader := TestDownloader{data, false, false}
		for chunkSize := int64(0); chunkSize < 32; chunkSize++ {
			for numWorkers := 1; numWorkers < 32; numWorkers++ {
				if bytes, err := io.ReadAll(GetDownloadStream(downloader, chunkSize, numWorkers)); err == nil {
					actual := string(bytes)
					if actual != data {
						t.Fatalf("Failed with fileSize: %d, chunkSize: %d, numWorkers: %d", fileSize, chunkSize, numWorkers)
					}
				} else {
					t.Fatalf("Unexpected error %v", err)
				}
			}
		}
	}
}

func TestRangeReader(t *testing.T) {
	opts.RetryCount = math.MaxInt64
	for fileSize := int64(0); fileSize < 64; fileSize++ {
		data := RandomString(fileSize)
		downloader := TestDownloader{data, true, false}
		for chunkSize := int64(1); chunkSize < 32; chunkSize++ {
			for numWorkers := 1; numWorkers < 32; numWorkers++ {
				if bytes, err := io.ReadAll(GetDownloadStream(downloader, chunkSize, numWorkers)); err == nil {
					actual := string(bytes)
					if actual != data {
						t.Fatalf("Failed with fileSize: %d, chunkSize: %d, numWorkers: %d", fileSize, chunkSize, numWorkers)
					}
				} else {
					t.Fatalf("Unexpected error %v", err)
				}
			}
		}
	}
}

func TestMultipartRangeReader(t *testing.T) {
	opts.RetryCount = math.MaxInt64
	for fileSize := int64(0); fileSize < 64; fileSize++ {
		data := RandomString(fileSize)
		downloader := TestDownloader{data, true, true}
		for chunkSize := int64(1); chunkSize < 32; chunkSize++ {
			for numWorkers := 1; numWorkers < 32; numWorkers++ {
				if bytes, err := io.ReadAll(GetDownloadStream(downloader, chunkSize, numWorkers)); err == nil {
					actual := string(bytes)
					if actual != data {
						t.Fatalf("Failed with fileSize: %d, chunkSize: %d, numWorkers: %d", fileSize, chunkSize, numWorkers)
					}
				} else {
					t.Fatalf("Unexpected error %v", err)
				}
			}
		}
	}
}

func TestHttpDownloaderGetFileInfo_SmallFileHeadRequest(t *testing.T) {
	opts.SkipHead = false
	opts.ChunkSize = 2000 // 2000 bytes > 1000 bytes file
	opts.RetryCount = 1
	opts.RetryWait = 1
	opts.MaxWait = 5

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	downloader := HttpDownloader{
		Url:    server.URL,
		client: &http.Client{},
	}

	contentLength, acceptRanges, multipartSupported := downloader.GetFileInfo()

	if contentLength != 1000 {
		t.Errorf("Expected content length 1000, got %d", contentLength)
	}
	if acceptRanges {
		t.Error("Expected accept ranges to be false for small files")
	}
	if multipartSupported {
		t.Error("Expected multipartSupported to be false")
	}
}

func TestHttpDownloaderGetFileInfo_LargeFileHeadRequest(t *testing.T) {
	opts.SkipHead = false
	opts.ChunkSize = 1000000 // 1MB < 2MB file
	opts.RetryCount = 1
	opts.RetryWait = 1
	opts.MaxWait = 5

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2000000") // 2MB file
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	downloader := HttpDownloader{
		Url:    server.URL,
		client: &http.Client{},
	}

	contentLength, acceptRanges, multipartSupported := downloader.GetFileInfo()

	if contentLength != 2000000 {
		t.Errorf("Expected content length 2000000, got %d", contentLength)
	}
	if !acceptRanges {
		t.Error("Expected accept ranges to be true for large files")
	}
	if multipartSupported {
		t.Error("Expected multipartSupported to be false")
	}
}

func TestHttpDownloaderGetFileInfo_LargeFileSkipHead(t *testing.T) {
	opts.SkipHead = true
	opts.ContentLength = 2000000 // 2MB file
	opts.AcceptRanges = "bytes"
	opts.ChunkSize = 1000000 // 1MB chunk size < 2MB file

	downloader := HttpDownloader{
		Url:    "http://example.com", // URL doesn't matter for skip-head
		client: &http.Client{},
	}

	contentLength, acceptRanges, multipartSupported := downloader.GetFileInfo()

	if contentLength != 2000000 {
		t.Errorf("Expected content length 2000000, got %d", contentLength)
	}
	if !acceptRanges {
		t.Error("Expected accept ranges to be true")
	}
	if multipartSupported {
		t.Error("Expected multipartSupported to be false")
	}
}

func TestHttpDownloaderGetFileInfo_SmallFileSkipHead(t *testing.T) {
	opts.SkipHead = true
	opts.ContentLength = 500000 // 500KB file
	opts.AcceptRanges = "bytes"
	opts.ChunkSize = 1000000 // 1MB chunk size (larger than file)

	downloader := HttpDownloader{
		Url:    "http://example.com", // URL doesn't matter for skip-head
		client: &http.Client{},
	}

	contentLength, acceptRanges, multipartSupported := downloader.GetFileInfo()

	if contentLength != 500000 {
		t.Errorf("Expected content length 500000, got %d", contentLength)
	}
	if acceptRanges {
		t.Error("Expected accept ranges to be false for small files")
	}
	if multipartSupported {
		t.Error("Expected multipartSupported to be false")
	}
}

func TestHttpDownloaderGetFileInfo_SkipHeadEmptyAcceptRanges(t *testing.T) {
	opts.SkipHead = true
	opts.ContentLength = 2000000 // 2MB file
	opts.AcceptRanges = ""       // Empty string should result in false
	opts.ChunkSize = 1000000     // 1MB chunk size < 2MB file

	downloader := HttpDownloader{
		Url:    "http://example.com", // URL doesn't matter for skip-head
		client: &http.Client{},
	}

	contentLength, acceptRanges, multipartSupported := downloader.GetFileInfo()

	if contentLength != 2000000 {
		t.Errorf("Expected content length 2000000, got %d", contentLength)
	}
	if acceptRanges {
		t.Error("Expected accept ranges to be false when AcceptRanges is empty")
	}
	if multipartSupported {
		t.Error("Expected multipartSupported to be false")
	}
}
