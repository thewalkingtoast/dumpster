package app

import (
	"net/http"
	"strings"
	"testing"

	"github.com/adam/dumpster/internal/model"
)

func records(t *testing.T, a *App, id string) []model.Record {
	t.Helper()
	recs, ok := a.store.List(id)
	if !ok {
		t.Fatalf("store has no dumpster %q", id)
	}
	return recs
}

func TestHTTPIngestDumpsRequest(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	req, err := http.NewRequest(http.MethodPost,
		a.BaseURL()+"/d/"+id+"/v1/orders?page=2", strings.NewReader(`{"sku":"A1"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Trace", "abc123")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	recs := records(t, a, id)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Kind != model.KindHTTP {
		t.Fatalf("kind = %q", rec.Kind)
	}
	if rec.Seq != 1 {
		t.Fatalf("seq = %d, want 1", rec.Seq)
	}
	if rec.Req.Method != http.MethodPost || rec.Req.Subpath != "/v1/orders" {
		t.Fatalf("req = %+v", rec.Req)
	}
	if rec.Req.Query != "page=2" {
		t.Fatalf("query = %q", rec.Req.Query)
	}
	if got := rec.Req.Headers["X-Trace"]; len(got) != 1 || got[0] != "abc123" {
		t.Fatalf("X-Trace header = %v", got)
	}
	if rec.Req.BodyText != `{"sku":"A1"}` {
		t.Fatalf("body text = %q", rec.Req.BodyText)
	}
	if rec.Req.Decoded == nil || rec.Req.Decoded.Format != "json" {
		t.Fatalf("decoded = %+v", rec.Req.Decoded)
	}
	if rec.Remote.IP != "127.0.0.1" {
		t.Fatalf("remote ip = %q, want 127.0.0.1", rec.Remote.IP)
	}
	if rec.Timing.TotalUS <= 0 {
		t.Fatalf("timing = %+v", rec.Timing)
	}
}

func TestHTTPIngestUnknownDumpster(t *testing.T) {
	a := newTestApp(t)
	res, _ := get(t, a.BaseURL()+"/d/00000000-0000-7000-8000-000000000000/anything")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
}

func TestHTTPIngestGarbageID(t *testing.T) {
	a := newTestApp(t)
	res, _ := get(t, a.BaseURL()+"/d/not-a-uuid/anything")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
}

func TestHTTPIngestAcceptsEveryMethod(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, _ := http.NewRequest(method, a.BaseURL()+"/d/"+id+"/x", nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", method, res.StatusCode)
		}
	}
	if got := len(records(t, a, id)); got != 4 {
		t.Fatalf("records = %d, want 4", got)
	}
}
