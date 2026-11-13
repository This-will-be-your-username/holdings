package main

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

var gzPool = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

type gzWriter struct {
	http.ResponseWriter
	zw *gzip.Writer
}

func (g *gzWriter) Write(b []byte) (int, error) { return g.zw.Write(b) }

// withGzip compresses text responses (HTML, CSS, JS, JSON, CSV) for clients that accept it,
// which matters on mobile connections: the Outlook page embeds chart data.
func withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		zw := gzPool.Get().(*gzip.Writer)
		zw.Reset(w)
		defer func() { zw.Close(); gzPool.Put(zw) }()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		w.Header().Del("Content-Length")
		next.ServeHTTP(&gzWriter{ResponseWriter: w, zw: zw}, r)
	})
}
