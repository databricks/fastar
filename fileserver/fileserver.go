package main

import (
	"log"
	"net/http"
	"os"

	"github.com/didip/tollbooth"
)

// Quick local http fileserver with rate limiting for e2e tests
func main() {
	fs := http.FileServer(http.Dir("/tmp"))
	limiter := tollbooth.LimitHandler(tollbooth.NewLimiter(1, nil), fs)
	for _, x := range os.Args[1:] {
		if x == "--throttle" {
			log.Fatal(http.ListenAndServe(":8000", limiter))
		}
	}
	log.Fatal(http.ListenAndServe(":8000", fs))
}
