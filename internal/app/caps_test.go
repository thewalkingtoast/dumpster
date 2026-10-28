package app

import (
	"net/http"
	"strings"
	"testing"
)

func TestYardIsFull(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.MaxDumpsters = 1 })
	rent(t, a)

	res, err := noRedirect.Post(a.BaseURL()+"/rent", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("second rent: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the yard is full", res.StatusCode)
	}
}

func TestRecordCapTrimsOldest(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.MaxRecords = 2 })
	id := rent(t, a)

	for i := 0; i < 3; i++ {
		res, err := http.Post(a.BaseURL()+"/d/"+id+"/x", "text/plain", strings.NewReader("hi"))
		if err != nil {
			t.Fatalf("POST %d: %v", i, err)
		}
		res.Body.Close()
	}
	recs := records(t, a, id)
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2 (MaxRecords)", len(recs))
	}
	if recs[0].Seq != 3 {
		t.Fatalf("newest seq = %d, want 3", recs[0].Seq)
	}
}

func TestBodyCapRejectsNothingButTruncates(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.MaxBody = 8 })
	id := rent(t, a)

	res, err := http.Post(a.BaseURL()+"/d/"+id+"/x", "text/plain",
		strings.NewReader(strings.Repeat("z", 4096)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("an oversized body must still be accepted, got %d", res.StatusCode)
	}
	rec := records(t, a, id)[0]
	if !rec.Req.Truncated || rec.Req.BodyBytes != 8 {
		t.Fatalf("record = %+v", rec.Req)
	}
}
