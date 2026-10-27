package app

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/adam/dumpster/internal/model"
)

func TestSSEStreamsAndRecordsEvents(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	res, err := http.Get(a.BaseURL() + "/d/" + id + "/sse?count=2&interval_ms=1")
	if err != nil {
		t.Fatalf("GET sse: %v", err)
	}
	defer res.Body.Close()

	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	payload, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if got := strings.Count(string(payload), "event: dump"); got != 2 {
		t.Fatalf("events = %d, want 2\n%s", got, payload)
	}
	if got := strings.Count(string(payload), "data: {"); got != 2 {
		t.Fatalf("data lines = %d, want 2\n%s", got, payload)
	}

	recs := records(t, a, id)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Kind != model.KindSSE {
		t.Fatalf("kind = %q, want sse", rec.Kind)
	}
	if len(rec.Events) != 2 {
		t.Fatalf("recorded events = %d, want 2", len(rec.Events))
	}
	if !rec.Closed || rec.CloseNote != "stream finished" {
		t.Fatalf("closed = %v note = %q", rec.Closed, rec.CloseNote)
	}
}

func TestSSECustomEventNameAndClamping(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	res, err := http.Get(a.BaseURL() + "/d/" + id + "/sse?count=0&interval_ms=0&event=tick")
	if err != nil {
		t.Fatalf("GET sse: %v", err)
	}
	defer res.Body.Close()
	payload, _ := io.ReadAll(res.Body)
	if got := strings.Count(string(payload), "event: tick"); got != 1 {
		t.Fatalf("count=0 should clamp to 1 tick, got %d\n%s", got, payload)
	}
}
