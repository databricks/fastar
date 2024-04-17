package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/googleapis/gax-go/v2"
	"golang.org/x/sys/unix"
	"google.golang.org/api/googleapi"
)

type GCSDownloader struct {
	Url string
	svc *storage.Client
}

// Returns a handle for the object referenced by this downloader with the Retryer configured
// as requested in the fastar options.
func (gcsDownloader GCSDownloader) objectWithRetry() *storage.ObjectHandle {
	bucket, object := getBucketAndObject(gcsDownloader.Url)

	remainingAttempts := opts.RetryCount
	return gcsDownloader.svc.Bucket(bucket).Object(object).Retryer(
		storage.WithErrorFunc(func(err error) bool {
			remainingAttempts--
			return storage.ShouldRetry(err) && remainingAttempts > 0
		}),
		storage.WithBackoff(gax.Backoff{
			Initial: time.Duration(opts.RetryWait) * time.Second,
			Max:     time.Duration(opts.MaxWait) * time.Second,
		}),
	)
}

func (gcsDownloader GCSDownloader) GetFileInfo() (int64, bool, bool) {
	attrs, err := gcsDownloader.objectWithRetry().Attrs(context.Background())

	handleGcsError(err, "GetFileInfo")

	return attrs.Size, true, false
}

func (gcsDownloader GCSDownloader) Get() io.ReadCloser {
	rc, err := gcsDownloader.objectWithRetry().NewReader(context.Background())

	handleGcsError(err, "Get")

	return rc
}

func (gcsDownloader GCSDownloader) GetRange(start, end int64) io.ReadCloser {
	rc, err := gcsDownloader.objectWithRetry().NewRangeReader(context.Background(), start, end-start)

	handleGcsError(err, "GetRange")

	return rc
}

// GCS doesn't support multipart range requests right now, so this will never be used
// for actual file download. Still here for when they eventually do support it though.
func (gcsDownloader GCSDownloader) GetRanges(ranges [][]int64) (*multipart.Reader, error) {
	return nil, errors.New("multipart range requests not supported by GCS")
}

// If err is nil, this is a noop. Otherwise the method will print an appropirate error message
// and exit with the appropriate error code.
func handleGcsError(err error, requestType string) {
	if err != nil {
		isErrObjNotFound := strings.Contains(err.Error(), "object doesn't exist")
		if e, ok := err.(*googleapi.Error); ok || isErrObjNotFound {
			if isErrObjNotFound {
				log.Printf("404, %s failed, GCS object doesn't exist\n", requestType)
				os.Exit(int(unix.ENOENT))
			}
			if e.Code == 404 {
				log.Printf("404, %s failed, GCS object or bucket doesn't exist\n", requestType)
				os.Exit(int(unix.ENOENT))
			}
		}
		log.Fatal(fmt.Sprintf("GCS request %s failed: ", requestType), err.Error())
	}
}

func getBucketAndObject(url string) (string, string) {
	parts := strings.Split(strings.Replace(url, "gs://", "", 1), "/")
	bucket := parts[0]
	object := strings.Join(parts[1:], "/")
	return bucket, object
}
