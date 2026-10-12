package main

import (
	"errors"
	"net/http"
	"time"
)

// errServerClosed is compared against http.ErrServerClosed without importing
// the value in main's error checks twice.
var errServerClosed = http.ErrServerClosed

// httpServer wraps http.Handler so that the server timeouts live in one place.
type httpServer struct {
	http.Handler
}

func newHTTPServer(h *httpServer) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}

var _ = errors.Is
