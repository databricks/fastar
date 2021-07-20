package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var openFileTokens chan bool

var l sync.Mutex
var linkMap map[string]chan bool = make(map[string]chan bool)

func ExtractTar(stream io.Reader) {
	openFileTokens = make(chan bool, *writeWorkers)
	tarReader := tar.NewReader(stream)
	for i := 0; i < *writeWorkers; i++ {
		openFileTokens <- true
	}
	var wg sync.WaitGroup

	for {
		header, err := tarReader.Next()

		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatalf("ExtractTarGz: Next() failed: %s", err.Error())
		}

		name := header.Name
		linkName := header.Linkname
		if *stripComponents != 0 {
			name = filepath.Join(strings.Split(name, "/")[*stripComponents:]...)
			if linkName != "" {
				linkName = filepath.Join(strings.Split(linkName, "/")[*stripComponents:]...)
			}
		}
		if name == "" {
			continue
		}
		path := filepath.Join(*outputDir, name)
		info := header.FileInfo()
		pathDir, _ := filepath.Split(path)
		if _, err = os.Stat(pathDir); os.IsNotExist(err) {
			if err := os.MkdirAll(pathDir, 0755); err != nil {
				log.Fatalf("ExtractTarGz: Unspecified Mkdir() failed: %s", err.Error())
			}
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, info.Mode()); err != nil {
				log.Fatalf("ExtractTarGz: Mkdir() failed: %s", err.Error())
			}
			os.Chmod(path, info.Mode())
		case tar.TypeReg:
			buf := make([]byte, info.Size())
			totalRead := 0
			for totalRead < int(info.Size()) {
				read, err := tarReader.Read(buf[totalRead:])
				if err != nil && err != io.EOF {
					log.Fatal("Failed to read from resp:", err.Error())
				}
				totalRead += read
			}
			<-openFileTokens
			wg.Add(1)
			go writeFileAsync(path, buf, info, &wg)
		case tar.TypeLink:
			newPath := filepath.Join(*outputDir, linkName)
			wg.Add(1)
			go hardLinkAsync(newPath, path, &wg)
		case tar.TypeSymlink:
			if *overwrite {
				if _, err := os.Lstat(path); err == nil {
					os.Remove(path)
				}
			}
			if err = os.Symlink(linkName, path); err != nil {
				log.Fatal("Failed to symlink: ", err.Error())
			}
		default:
			if *ignoreNodeFiles {
				fmt.Fprintln(
					os.Stderr,
					"ExtractTarGz: uknown type:",
					header.Typeflag,
					" in ",
					header.Name)
			} else {
				log.Fatal(
					"ExtractTarGz: uknown type:",
					header.Typeflag,
					" in ",
					header.Name)
			}
		}
	}
	wg.Wait()
}

func writeFileAsync(filename string, buf []byte, info fs.FileInfo, wg *sync.WaitGroup) {
	defer wg.Done()
	defer func() { openFileTokens <- true }()
	if *overwrite {
		if _, err := os.Stat(filename); err == nil {
			os.Remove(filename)
		}
	}
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
	if err != nil {
		log.Fatal("Create file failed: ", err.Error())
	}
	defer os.Chmod(filename, info.Mode())
	defer func() {
		var c chan bool
		l.Lock()
		if _, exists := linkMap[filename]; !exists {
			linkMap[filename] = make(chan bool, 1)
		}
		c = linkMap[filename]
		l.Unlock()
		c <- true
	}()
	defer file.Close()
	_, err = io.Copy(file, bytes.NewReader(buf))
	if err != nil {
		log.Fatal("Copy file failed: ", err.Error())
	}
}

func hardLinkAsync(newPath string, path string, wg *sync.WaitGroup) {
	defer wg.Done()
	// If the file we're hard linking to doesn't exist yet,
	// make a channel in the map for the file and wait on it
	// until it's created.
	var c chan bool
	l.Lock()
	if _, exists := linkMap[newPath]; !exists {
		linkMap[newPath] = make(chan bool, 1)
	}
	c = linkMap[newPath]
	l.Unlock()
	<-c
	// Possible for multiple hard links to the same file, send a token
	// back to the channel for any future workers that also need to
	// link to it.
	c <- true

	if *overwrite {
		if _, err := os.Stat(path); err == nil {
			os.Remove(path)
		}
	}
	if err := os.Link(newPath, path); err != nil {
		log.Fatal("Failed to hardlink: ", err.Error())
	}
}
