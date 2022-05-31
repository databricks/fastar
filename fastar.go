package main

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
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
	numWorkers      = kingpin.Flag("download-workers", "How many parallel workers to download the file").Default("8").Int()
	chunkSize       = kingpin.Flag("chunk-size", "Size of file chunks (in MB) to pull in parallel").Default("50").Int64()
	outputDir       = kingpin.Flag("directory", "Directory to extract tarball to. Defaults to current dir if not specified.").Short('C').ExistingDir()
	toStdout        = kingpin.Flag("to-stdout", "Dump downloaded file to stdout rather than extracting to disk").Default("false").Short('O').Bool()
	writeWorkers    = kingpin.Flag("write-workers", "How many parallel workers to use to write file to disk").Default("8").Int()
	stripComponents = kingpin.Flag("strip-components", "Strip STRIP-COMPONENTS leading components from file names on extraction").Int()
	compression     = kingpin.Flag("compression", "Force specific compression schema instead of inferring from magic bytes and filename extension (tar, gzip, or lz4)").Enum("tar", "gzip", "lz4")
	retryCount      = kingpin.Flag("retry-count", "Max number of retries for a single chunk").Default("2").Int()
	retryWait       = kingpin.Flag("retry-wait", "Max number of seconds to wait in between retries (with jitter)").Default("2").Int()
	minSpeed        = kingpin.Flag("min-speed", "Minimum speed per each chunk download. Fails if any are slower than this. 0 for no min speed, append K or M for KBps or MBps.").Default("1K").String()
	connTimeout     = kingpin.Flag("connection-timeout", "Abort download if TCP dial takes longer than this many seconds").Default("60").Int()
	ignoreNodeFiles = kingpin.Flag("ignore-node-files", "Don't throw errors on character or block device nodes").Default("false").Bool()
	overwrite       = kingpin.Flag("overwrite", "Overwrite any existing files").Default("false").Bool()
	headers         = kingpin.Flag("headers", "Headers to use with http request").Short('H').PlaceHolder("HEADER:VALUE").StringMap()

	minSpeedBytesPerNanosecond = 0.0
)

// Magic byte sequences prepended to the start of every gzip or lz4
// compressed bundle. When downloading a file we can check for either
// of these sequences to automatically infer if we need to perform
// decompression, as well as which compression schema was used.
const (
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
	processMinSpeedFlag()
	*chunkSize *= 1e6 // Convert chunk size from MB to B
	fileStream := GetDownloadStream(*rawUrl, *chunkSize, *numWorkers)

	url, err := url.Parse(*rawUrl)
	if err != nil {
		log.Fatal("Failed to parse url: ", err.Error())
	}
	filename := path.Base(url.Path)

	fmt.Fprintln(os.Stderr, "File name: "+filename)
	fmt.Fprintln(os.Stderr, "Num Download Workers: "+strconv.Itoa(*numWorkers))
	fmt.Fprintln(os.Stderr, "Chunk Size (Mib): "+strconv.FormatInt(*chunkSize/1e6, 10))
	fmt.Fprintln(os.Stderr, "Num Disk Workers: "+strconv.Itoa(*writeWorkers))

	magicNumber, splicedStream := getMagicNumber(fileStream)

	compressionType := getCompressionType(filename, magicNumber)

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

	if *toStdout {
		if _, err := io.Copy(os.Stdout, finalStream); err != nil {
			log.Fatal("Failed to write file to stdout: ", err.Error())
		}
	} else {
		if *outputDir == "" {
			if *outputDir, err = os.Getwd(); err != nil {
				log.Fatal("Failed to get current working directory: ", err.Error())
			}
		}
		ExtractTar(finalStream)
	}
}

// Reads first few bytes from file stream to get any possible
// magic numbers, returns a spliced-together reader since
// the original io.Reader has already been read from.
func getMagicNumber(reader io.Reader) (string, io.Reader) {
	buf := make([]byte, 4)
	totalRead := 0
	for totalRead < 4 {
		read, err := reader.Read(buf[totalRead:])
		if err != nil && err != io.EOF {
			log.Fatal("Failed to read magic number:", err.Error())
		}
		totalRead += read
		if err == io.EOF {
			return "", io.LimitReader(bytes.NewReader(buf[:totalRead]), int64(totalRead))
		}
	}
	magicNumber := hex.EncodeToString(buf)
	splicedStream := io.MultiReader(bytes.NewReader(buf), reader)
	return magicNumber, splicedStream
}

// Choose compression type by the following preference order:
// 1. User provided compression type flag
// 2. Inferred by magic number
// 3. Inferred by file extension prefix in URL
// 4. Default to raw tarball
func getCompressionType(filename string, magicNumber string) CompressionType {
	if *compression != "" {
		if *compression == "tar" {
			fmt.Fprintln(os.Stderr, "Forcing raw tar")
			return Tar
		} else if *compression == "gzip" {
			fmt.Fprintln(os.Stderr, "Forcing gzip")
			return Gzip
		} else {
			fmt.Fprintln(os.Stderr, "Forcing lz4")
			return Lz4
		}
	} else {
		if strings.HasPrefix(magicNumber, gzipMagicNumber) {
			fmt.Fprintln(os.Stderr, "Inferring gzip by magic number")
			return Gzip
		} else if strings.HasPrefix(magicNumber, lz4MagicNumber) {
			fmt.Fprintln(os.Stderr, "Inferring lz4 by magic number")
			return Lz4
		} else {
			fmt.Fprintln(os.Stderr, "Unrecognized magic number, falling back to file extension")
			if strings.HasSuffix(filename, "lz4") {
				fmt.Fprintln(os.Stderr, "Inferring lz4 by file extension")
				return Lz4
			} else if strings.HasSuffix(filename, "gz") {
				fmt.Fprintln(os.Stderr, "Inferring gzip by file extension")
				return Gzip
			} else if strings.HasSuffix(filename, "tar") {
				fmt.Fprintln(os.Stderr, "Inferring raw tar by file extension")
				return Tar
			} else {
				fmt.Fprintln(os.Stderr, "Unrecognized file extension, assuming raw tar")
				return Tar
			}
		}
	}
}

func processMinSpeedFlag() {
	var bytesPerSecond int
	var err error
	if strings.HasSuffix(*minSpeed, "K") {
		if bytesPerSecond, err = strconv.Atoi((*minSpeed)[:len(*minSpeed)-1]); err != nil {
			log.Fatal("Failed to parse min speed argument", minSpeed, err.Error())
		}
		bytesPerSecond *= 1e3
	} else if strings.HasSuffix(*minSpeed, "M") {
		if bytesPerSecond, err = strconv.Atoi((*minSpeed)[:len(*minSpeed)-1]); err != nil {
			log.Fatal("Failed to parse min speed argument", minSpeed, err.Error())
		}
		bytesPerSecond *= 1e6
	} else {
		if bytesPerSecond, err = strconv.Atoi(*minSpeed); err != nil {
			log.Fatal("Failed to parse min speed argument", minSpeed, err.Error())
		}
	}
	minSpeedBytesPerNanosecond = float64(bytesPerSecond) / 1e9
}
