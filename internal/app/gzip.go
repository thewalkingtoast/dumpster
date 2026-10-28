package app

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// gzipPool reuses writers. A snapshot of a full dumpster is tens of megabytes of
// JSON, so allocating a fresh compressor per request would be the dominant cost.
// BestSpeed is deliberate: these documents compress ~4x, and the extra 3x from
// DefaultCompression is not worth tripling the CPU on a page load.
var gzipPool = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	return w
}}

// gzipJSON wraps a handler that emits one buffered JSON document.
//
// It is applied only to whole-document endpoints. SSE and WebSocket responses must
// never pass through here: both stream on their own schedule, and buffering them
// behind a compressor would hold events back until the writer flushed.
func gzipJSON(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			h(w, r)
			return
		}
		gz := gzipPool.Get().(*gzip.Writer)
		defer gzipPool.Put(gz)
		gz.Reset(w)

		w.Header().Set("Content-Encoding", "gzip")
		// Without Vary a shared cache would hand a gzip body to a client that
		// never asked for one.
		w.Header().Add("Vary", "Accept-Encoding")

		h(gzipWriter{ResponseWriter: w, w: gz}, r)
		_ = gz.Close()
	}
}

// gzipWriter compresses the body but leaves headers and status alone.
type gzipWriter struct {
	http.ResponseWriter
	w *gzip.Writer
}

func (g gzipWriter) Write(b []byte) (int, error) { return g.w.Write(b) }

// acceptsGzip parses Accept-Encoding the blunt way: token match, q=0 means no.
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		token, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		token = strings.ToLower(strings.TrimSpace(token))
		if token != "gzip" && token != "*" {
			continue
		}
		if strings.Contains(strings.ReplaceAll(params, " ", ""), "q=0") {
			return false
		}
		return true
	}
	return false
}
