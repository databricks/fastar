package main

import (
	"io"
	"log"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
)

type S3Downloader struct {
	Url string
	svc *s3.S3
}

func (s3Downloader S3Downloader) GetFileInfo() (int64, bool) {
	bucket, key := getBucketAndKey(s3Downloader.Url)
	params := &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}
	req, resp := s3Downloader.svc.GetObjectRequest(params)
	err := req.Send()
	if err != nil {
		log.Fatal("AWS request failed: ", err.Error())
	}
	return *resp.ContentLength, true
}

func (s3Downloader S3Downloader) GetRanges(ranges []int64) (io.ReadCloser, int64) {
	rangeString := "bytes="
	for i := 0; i+1 < len(ranges); i += 2 {
		rangeString += strconv.FormatInt(ranges[i], 10) + "-" + strconv.FormatInt(ranges[i+1]-1, 10)
		if i+3 < len(ranges) {
			rangeString += ","
		}
	}
	bucket, key := getBucketAndKey(s3Downloader.Url)
	var params *s3.GetObjectInput
	if len(ranges) != 0 {
		params = &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
			Range:  aws.String(rangeString),
		}
	} else {
		params = &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		}
	}
	req, resp := s3Downloader.svc.GetObjectRequest(params)
	err := req.Send()
	if err != nil {
		log.Fatal("AWS request failed: ", err.Error())
	}
	return resp.Body, *resp.ContentLength
}

func getBucketAndKey(url string) (string, string) {
	parts := strings.Split(strings.Replace(url, "s3://", "", 1), "/")
	bucket := parts[0]
	key := strings.Join(parts[1:], "/")
	return bucket, key
}
