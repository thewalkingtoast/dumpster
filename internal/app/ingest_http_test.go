package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
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

func TestHTTPIngestBinaryBodyIsBase64(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	payload := []byte{0x00, 0xff, 0xfe, 0x10}
	req, _ := http.NewRequest(http.MethodPost, a.BaseURL()+"/d/"+id+"/bin", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	res.Body.Close()

	rec := records(t, a, id)[0]
	if rec.Req.BodyText != "" {
		t.Fatalf("binary body leaked into text: %q", rec.Req.BodyText)
	}
	want := base64.StdEncoding.EncodeToString(payload)
	if rec.Req.BodyBase64 != want {
		t.Fatalf("base64 = %q, want %q", rec.Req.BodyBase64, want)
	}
	if rec.Req.Decoded != nil {
		t.Fatalf("binary body should not decode, got %+v", rec.Req.Decoded)
	}
}

func TestHTTPIngestFormBody(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	res, err := http.PostForm(a.BaseURL()+"/d/"+id+"/form", url.Values{"a": {"1"}, "b": {"2", "3"}})
	if err != nil {
		t.Fatalf("PostForm: %v", err)
	}
	res.Body.Close()

	rec := records(t, a, id)[0]
	if rec.Req.Decoded == nil || rec.Req.Decoded.Format != "form" {
		t.Fatalf("decoded = %+v", rec.Req.Decoded)
	}
	// Assert the rendered shape, not the Go type: the memory backend keeps
	// url.Values while the redis backend round-trips through JSON into
	// map[string]any. Marshalling is the one view both agree on.
	encoded, err := json.Marshal(rec.Req.Decoded.Value)
	if err != nil {
		t.Fatalf("marshal decoded: %v", err)
	}
	if !strings.Contains(string(encoded), `"b":["2","3"]`) {
		t.Fatalf("decoded value = %s, want b to carry two values", encoded)
	}
}

func TestHTTPIngestTruncatesHugeBody(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.MaxBody = 16 })
	id := rent(t, a)

	req, _ := http.NewRequest(http.MethodPost, a.BaseURL()+"/d/"+id+"/big",
		strings.NewReader(strings.Repeat("x", 500)))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()

	var ack struct {
		Received int  `json:"received_bytes"`
		Trunc    bool `json:"truncated"`
	}
	if err := json.NewDecoder(res.Body).Decode(&ack); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	if ack.Received != 16 || !ack.Trunc {
		t.Fatalf("ack = %+v, want 16 bytes truncated", ack)
	}
	rec := records(t, a, id)[0]
	if !rec.Req.Truncated || rec.Req.BodyBytes != 16 {
		t.Fatalf("record = %+v", rec.Req)
	}
}

func TestRecordsJSONNewestFirst(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	for _, path := range []string{"/one", "/two", "/three"} {
		res, err := http.Post(a.BaseURL()+"/d/"+id+path, "text/plain", strings.NewReader("hi"))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		res.Body.Close()
	}

	res, body := get(t, a.BaseURL()+"/d/"+id+"/_records.json")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var snap struct {
		Dumpster model.Dumpster `json:"dumpster"`
		Records  []model.Record `json:"records"`
	}
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode snapshot: %v\n%s", err, body)
	}
	if snap.Dumpster.ID != id {
		t.Fatalf("dumpster id = %q, want %q", snap.Dumpster.ID, id)
	}
	if len(snap.Records) != 3 {
		t.Fatalf("records = %d, want 3", len(snap.Records))
	}
	if snap.Records[0].Seq != 3 {
		t.Fatalf("first seq = %d, want 3 (newest first)", snap.Records[0].Seq)
	}
}

func TestDebugPageShell(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	res, body := get(t, a.BaseURL()+"/d/"+id)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	for _, want := range []string{
		`data-id="` + id + `"`,
		`data-grpc="`,
		`data-tcp="`,
		`data-udp="`,
		`/static/app.js`,
		`id="records"`,
		`Rent a dumpster`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("debug page is missing %q", want)
		}
	}
}

func TestDebugPageGoneDumpster(t *testing.T) {
	a := newTestApp(t)
	res, body := get(t, a.BaseURL()+"/d/00000000-0000-7000-8000-000000000000")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
	if !strings.Contains(body, "gone") && !strings.Contains(body, "emptied") {
		t.Fatalf("404 page should explain the dumpster is gone, got:\n%s", body)
	}
}
