package main

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
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
	compression     = kingpin.Flag("compression", "Force specific compression schema instead of inferring from magic bytes and filename extension").Enum("tar", "gzip", "lz4")

	gzipMagicNumber = "1f8b"
	lz4MagicNumber  = "04224d18"
)

type CompressionType int

const (
	Tar CompressionType = iota
	Gzip
	Lz4
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

	buf := make([]byte, 4)
	totalRead := 0
	for totalRead < 4 {
		read, err := fileStream.Read(buf[totalRead:])
		if err != nil && err != io.EOF {
			log.Fatal("Failed to read magic number:", err.Error())
		}
		totalRead += read
	}

	var compressionType CompressionType
	if *compression != "" {
		if *compression == "tar" {
			compressionType = Tar
			fmt.Fprintln(os.Stderr, "Forcing raw tar")
		} else if *compression == "gzip" {
			compressionType = Gzip
			fmt.Fprintln(os.Stderr, "Forcing gzip")
		} else if *compression == "lz4" {
			compressionType = Lz4
			fmt.Fprintln(os.Stderr, "Forcing lz4")
		} else {
			log.Fatal("Unrecognized compression type: ", *compression)
		}
	} else {
		magicNumber := hex.EncodeToString(buf)
		if strings.HasPrefix(magicNumber, gzipMagicNumber) {
			compressionType = Gzip
			fmt.Fprintln(os.Stderr, "Inferring gzip by magic number")
		} else if strings.HasPrefix(magicNumber, lz4MagicNumber) {
			compressionType = Lz4
			fmt.Fprintln(os.Stderr, "Inferring lz4 by magic number")
		} else {
			fmt.Fprintln(os.Stderr, "Unrecognized magic number, falling back to file extension")
			if strings.HasSuffix(filename, "lz4") {
				compressionType = Lz4
				fmt.Fprintln(os.Stderr, "Inferring lz4 by file extension")
			} else if strings.HasSuffix(filename, "gz") {
				compressionType = Gzip
				fmt.Fprintln(os.Stderr, "Inferring gzip by file extension")
			} else if strings.HasSuffix(filename, "tar") {
				compressionType = Tar
				fmt.Fprintln(os.Stderr, "Inferring raw tar by file extension")
			} else {
				fmt.Fprintln(os.Stderr, "Unrecognized file extension, assuming raw tar")
				compressionType = Tar
			}
		}
	}
	splicedStream := io.MultiReader(bytes.NewReader(buf), fileStream)

	var finalStream io.Reader
	if compressionType == Lz4 {
		finalStream = lz4.NewReader(splicedStream)
	} else if compressionType == Gzip {
		finalStream, err = gzip.NewReader(splicedStream)
		if err != nil {
			log.Fatal("Error creating gzip stream: ", err.Error())
		}
	} else if compressionType == Tar {
		finalStream = splicedStream
	} else {
		log.Fatal("CompressionType not set, should be impossible")
	}

	if *outputDir == "" {
		io.Copy(os.Stdout, finalStream)
	} else {
		ExtractTar(finalStream)
	}
}
