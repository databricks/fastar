package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/s3"
	"golang.org/x/sys/unix"
)

type S3Downloader struct {
	Url string
	svc *s3.S3
}

func (s3Downloader S3Downloader) GetFileInfo() (int64, bool, bool) {
	req, resp := s3Downloader.generateRequestResponse(nil)
	sendAwsRequest(req)
	return *resp.ContentLength, true, false
}

func (s3Downloader S3Downloader) Get() io.ReadCloser {
	req, resp := s3Downloader.generateRequestResponse(nil)
	sendAwsRequest(req)
	return resp.Body
}

func (s3Downloader S3Downloader) GetRange(start, end int64) io.ReadCloser {
	rangeString := GenerateRangeString([][]int64{{start, end}})
	req, resp := s3Downloader.generateRequestResponse(&rangeString)
	sendAwsRequest(req)
	return resp.Body
}

// S3 doesn't support multipart range requests right now, so this will never be used
// for actual file download. Still here for when they eventually do support it though.
func (s3Downloader S3Downloader) GetRanges(ranges [][]int64) (*multipart.Reader, error) {
	return nil, errors.New("multipart range requests not supported by S3")
}

func (s3Downloader S3Downloader) generateRequestResponse(rangeString *string) (*request.Request, *s3.GetObjectOutput) {
	bucket, key := getBucketAndKey(s3Downloader.Url)
	params := &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}
	if rangeString != nil {
		params.Range = aws.String(*rangeString)
	}
	return s3Downloader.svc.GetObjectRequest(params)
}

func getBucketAndKey(url string) (string, string) {
	parts := strings.Split(strings.Replace(url, "s3://", "", 1), "/")
	bucket := parts[0]
	key := strings.Join(parts[1:], "/")
	return bucket, key
}

func sendAwsRequest(req *request.Request) {
	err := req.Send()
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case s3.ErrCodeNoSuchBucket:
				fmt.Fprintln(os.Stderr, "404, s3 bucket not found")
				os.Exit(int(unix.ENOENT))
			case s3.ErrCodeNoSuchKey:
				fmt.Fprintln(os.Stderr, "404, s3 key not found")
				os.Exit(int(unix.ENOENT))
			}
		}
		log.Fatal("AWS request failed, requestID: ", req.RequestID, ", error: ", err.Error())
	}
}
