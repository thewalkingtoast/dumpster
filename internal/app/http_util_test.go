package app

import (
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/adam/dumpster/internal/model"
)

func TestReadBodyTruncates(t *testing.T) {
	data, trunc := readBody(strings.NewReader("0123456789"), 4)
	if string(data) != "0123" || !trunc {
		t.Fatalf("data = %q trunc = %v", data, trunc)
	}
	data, trunc = readBody(strings.NewReader("abc"), 4)
	if string(data) != "abc" || trunc {
		t.Fatalf("data = %q trunc = %v", data, trunc)
	}
}

func TestRemoteFromProxy(t *testing.T) {
	r := httptest.NewRequest("POST", "/d/x", nil)
	r.RemoteAddr = "10.0.0.9:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")

	direct := remoteFrom(r, false)
	if direct.IP != "10.0.0.9" || direct.Port != 5555 {
		t.Fatalf("direct = %+v", direct)
	}
	if direct.Forwarded == "" {
		t.Fatal("the raw X-Forwarded-For must always be kept")
	}

	proxied := remoteFrom(r, true)
	if proxied.IP != "203.0.113.7" {
		t.Fatalf("proxied ip = %q, want 203.0.113.7", proxied.IP)
	}
	if proxied.Addr != "10.0.0.9:5555" {
		t.Fatalf("proxied addr = %q, want the real socket address", proxied.Addr)
	}
}

func TestFillBodyTextJSONAndBinary(t *testing.T) {
	req := &model.Request{}
	fillBody(req, "application/json", []byte(`{"a":1}`))
	if req.BodyText == "" || req.Decoded == nil || req.Decoded.Format != "json" {
		t.Fatalf("json req = %+v", req)
	}
	if req.BodyBytes != 7 {
		t.Fatalf("BodyBytes = %d", req.BodyBytes)
	}

	bin := &model.Request{}
	fillBody(bin, "application/octet-stream", []byte{0xff, 0x00, 0xfe})
	if bin.BodyText != "" {
		t.Fatal("binary body must not be rendered as text")
	}
	want := base64.StdEncoding.EncodeToString([]byte{0xff, 0x00, 0xfe})
	if bin.BodyBase64 != want {
		t.Fatalf("BodyBase64 = %q, want %q", bin.BodyBase64, want)
	}
}

func TestTimingFromWithoutConn(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	start := time.Now().Add(-3 * time.Millisecond)
	got := timingFrom(r, start)
	if got.TotalUS < 2000 {
		t.Fatalf("TotalUS = %d, want >= 2000", got.TotalUS)
	}
	if got.ConnWaitUS != 0 {
		t.Fatalf("ConnWaitUS = %d, want 0 when the context has no accept time", got.ConnWaitUS)
	}
}
