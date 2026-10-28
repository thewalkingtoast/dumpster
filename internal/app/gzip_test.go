package app

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A filled dumpster's snapshot is the largest thing this app emits: 500 records
// at 64 KiB an inline field is tens of megabytes on a page load. Compressing it
// is free until someone opens the page, unlike compressing at ingest.
func TestRecordsJSONIsGzippedWhenAccepted(t *testing.T) {
	a := newTestApp(t)
	sid := rent(t, a)
	if res, err := noRedirect.Post(a.BaseURL()+"/d/"+sid+"/orders", "application/json",
		strings.NewReader(`{"sku":"A1","qty":3}`)); err != nil {
		t.Fatalf("POST: %v", err)
	} else {
		res.Body.Close()
	}

	req, err := http.NewRequest(http.MethodGet, a.BaseURL()+"/d/"+sid+"/_records.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("GET snapshot: %v", err)
	}
	defer res.Body.Close()

	if got := res.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := res.Header.Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("Vary = %q, want Accept-Encoding (a cache would otherwise serve gzip to everyone)", got)
	}
	zr, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzipped body: %v", err)
	}
	var snap struct {
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("decompressed body is not JSON: %v", err)
	}
	if len(snap.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(snap.Records))
	}
}

// Clients that do not ask for gzip must not receive it.
func TestRecordsJSONIsPlainWhenGzipIsNotAccepted(t *testing.T) {
	a := newTestApp(t)
	sid := rent(t, a)
	if res, err := noRedirect.Post(a.BaseURL()+"/d/"+sid+"/orders", "application/json",
		strings.NewReader(`{"sku":"A1"}`)); err != nil {
		t.Fatalf("POST: %v", err)
	} else {
		res.Body.Close()
	}

	res, body := get(t, a.BaseURL()+"/d/"+sid+"/_records.json")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if enc := res.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("Content-Encoding = %q, want empty", enc)
	}
	var snap struct {
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
}
