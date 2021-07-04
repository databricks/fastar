package main

import (
	"log"
	"net/http"

	"github.com/didip/tollbooth"
)

func main() {
	fs := http.FileServer(http.Dir("/tmp"))
	limiter := tollbooth.LimitHandler(tollbooth.NewLimiter(5, nil), fs)
	log.Fatal(http.ListenAndServe(":8000", limiter))
}
