package main

import (
	"compress/gzip"
	"fastar/http"
	"fastar/s3"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/pierrec/lz4"
	"gopkg.in/alecthomas/kingpin.v2"
)

var (
	rawUrl          = kingpin.Arg("url", "URL to download from").Required().String()
	numWorkers      = kingpin.Flag("download-workers", "How many parallel workers to download the file").Default("16").Int()
	chunkSize       = kingpin.Flag("chunk-size", "Size of file chunks (in MB) to pull in parallel").Default("50").Uint64()
	outputDir       = kingpin.Flag("directory", "Directory to extract tarball to. Dumps file to stdout if not specified.").Short('C').ExistingDir()
	writeWorkers    = kingpin.Flag("write-workers", "How many parallel workers to use to write file to disk").Default("8").Int()
	stripComponents = kingpin.Flag("strip-components", "Strip STRIP-COMPONENTS leading components from file names on extraction").Int()
)

func main() {
	kingpin.Parse()
	*chunkSize = *chunkSize << 20
	var size uint64
	var fileStream io.Reader
	if strings.HasPrefix(*rawUrl, "s3") {
		size, fileStream = s3.GetDownloadStream(*rawUrl, *chunkSize, *numWorkers)
	} else {
		size, fileStream = http.GetDownloadStream(*rawUrl, *chunkSize, *numWorkers)
	}

	url, err := url.Parse(*rawUrl)
	if err != nil {
		log.Fatal("Failed to parse url: ", err.Error())
	}
	filename := path.Base(url.Path)

	fmt.Fprintln(os.Stderr, "File name: "+filename)
	fmt.Fprintln(os.Stderr, "File Size (MiB): "+strconv.FormatUint(size>>20, 10))
	fmt.Fprintln(os.Stderr, "Num Download Workers: "+strconv.Itoa(*numWorkers))
	fmt.Fprintln(os.Stderr, "Chunk Size (Mib): "+strconv.FormatUint(*chunkSize>>20, 10))
	fmt.Fprintln(os.Stderr, "Num Disk Workers: "+strconv.Itoa(*writeWorkers))

	var finalStream io.Reader
	if strings.HasSuffix(filename, "lz4") {
		finalStream = lz4.NewReader(fileStream)
	} else if strings.HasSuffix(filename, "gz") {
		finalStream, err = gzip.NewReader(fileStream)
		if err != nil {
			log.Fatal("Error creating gzip stream: ", err.Error())
		}
	} else if strings.HasSuffix(filename, "tar") {
		finalStream = fileStream
	} else {
		fmt.Fprintln(os.Stderr, "Unknown file type for ", filename, " assuming tar")
		finalStream = fileStream
	}

	if *outputDir == "" {
		io.Copy(os.Stdout, finalStream)
	} else {
		ExtractTar(finalStream)
	}
}
