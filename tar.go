package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Used to limit the number of background workers writing
// files at any one time.
// Channel of size $writeWorkers filled with bool tokens.
// In main thread hot path we block until a token is free
// to take, this token is then returned to the channel when
// the background writer thread is finished.
var openFileTokens chan bool

func ExtractTar(stream io.Reader) {
	openFileTokens = make(chan bool, *writeWorkers)
	tarReader := tar.NewReader(stream)
	for i := 0; i < *writeWorkers; i++ {
		openFileTokens <- true
	}
	// Hard linking is special, we need to make sure the file we're
	// linking to exists when creating the hard link.
	// For each hard link we essentially "stop the world." We stop
	// issuing any more file writes and wait on this WaitGroup for
	// all existing writes to finish (one of them might be the file
	// we need to link to).
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
			// Directories are synchronously created since a later file
			// might require it exist already.
			if err := os.MkdirAll(path, info.Mode()); err != nil {
				log.Fatalf("ExtractTarGz: Mkdir() failed: %s", err.Error())
			}
			os.Chmod(path, info.Mode())
			os.Chown(path, header.Uid, header.Gid)
		case tar.TypeReg:
			// Read file contents into a buffer to pass along to background
			// writer thread.
			buf, err := io.ReadAll(tarReader)
			if err != nil {
				log.Fatal("Failed to read from resp:", err.Error())
			}
			<-openFileTokens
			wg.Add(1)
			go writeFileAsync(path, buf, header, &wg)
		case tar.TypeLink:
			newPath := filepath.Join(*outputDir, linkName)
			hardLink(newPath, path, header, &wg)
		case tar.TypeSymlink:
			// Symlinks don't require the stop-the-world synchronization
			// of hard links since they don't require the source file
			// to exist.
			if *overwrite {
				if _, err := os.Lstat(path); err == nil {
					os.Remove(path)
				}
			}
			if err = os.Symlink(linkName, path); err != nil {
				log.Fatal("Failed to symlink: ", err.Error())
			}
			os.Lchown(path, header.Uid, header.Gid)
		default:
			if *ignoreNodeFiles {
				fmt.Fprintln(
					os.Stderr,
					"ExtractTarGz: uknown type:",
					string(header.Typeflag),
					" in ",
					header.Name)
			} else {
				log.Fatal(
					"ExtractTarGz: uknown type:",
					string(header.Typeflag),
					" in ",
					header.Name)
			}
		}
	}
	// Wait for all threads to finish, otherwise fastar
	// might exit before last few files done writing.
	wg.Wait()
}

func writeFileAsync(filename string, buf []byte, header *tar.Header, wg *sync.WaitGroup) {
	defer wg.Done()
	defer func() { openFileTokens <- true }()
	if *overwrite {
		if _, err := os.Stat(filename); err == nil {
			os.Remove(filename)
		}
	}
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, header.FileInfo().Mode())
	if err != nil {
		log.Fatal("Create file failed: ", err.Error())
	}
	defer os.Chmod(filename, header.FileInfo().Mode())
	defer os.Chown(filename, header.Uid, header.Gid)
	defer file.Close()
	_, err = io.Copy(file, bytes.NewReader(buf))
	if err != nil {
		log.Fatal("Copy file failed: ", err.Error())
	}
}

func hardLink(newPath string, path string, header *tar.Header, wg *sync.WaitGroup) {
	wg.Wait()

	if *overwrite {
		if _, err := os.Stat(path); err == nil {
			os.Remove(path)
		}
	}
	if err := os.Link(newPath, path); err != nil {
		log.Fatal("Failed to hardlink: ", err.Error())
	}
	os.Chown(path, header.Uid, header.Gid)
}
