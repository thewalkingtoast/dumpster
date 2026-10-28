package app

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/thewalkingtoast/dumpster/internal/model"
)

// A proxy that terminates TLS leaves the app reading plaintext, so the record
// describes the proxy's leg unless the proxy is believed. nginx says what the
// client used in X-Forwarded-Proto; under -trust-proxy that is the truth.
func TestProxyTLSIsRecordedWhenTrusted(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.TrustProxy = true })
	id := rent(t, a)

	req, err := http.NewRequest(http.MethodPost, a.BaseURL()+"/d/"+id+"/api/orders", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-For", "2605:b40:1229:a800::1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	res.Body.Close()

	rec := onlyRecord(t, a, id)
	if rec.Req.Protocol != "https" {
		t.Fatalf("protocol = %q, want https", rec.Req.Protocol)
	}
	if !rec.Remote.TLS {
		t.Fatal("remote.tls = false; a client that arrived over TLS is recorded as plaintext")
	}
	if rec.Remote.TLSVia != "proxy" {
		t.Fatalf("tls_via = %q, want proxy (the app did not terminate it)", rec.Remote.TLSVia)
	}
}

// Without a trusted proxy the same header is a client's own claim, and a client
// can claim anything.
func TestForwardedProtoIsIgnoredWithoutTrust(t *testing.T) {
	a := newTestApp(t) // TrustProxy off
	id := rent(t, a)

	req, err := http.NewRequest(http.MethodPost, a.BaseURL()+"/d/"+id+"/api/orders", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-Proto", "https")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	res.Body.Close()

	rec := onlyRecord(t, a, id)
	if rec.Req.Protocol != "http" || rec.Remote.TLS {
		t.Fatalf("protocol = %q tls = %v; an untrusted X-Forwarded-Proto was believed",
			rec.Req.Protocol, rec.Remote.TLS)
	}
}

// The app's own TLS is unambiguous and needs no proxy to vouch for it.
func TestDirectTLSIsRecordedAsDirect(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://dumpster.test/d/x", nil)
	req.TLS = &tls.ConnectionState{ServerName: "dumpster.test"}

	tlsOn, via := clientTLS(req, false)
	if !tlsOn || via != "direct" {
		t.Fatalf("clientTLS = %v/%q, want true/direct", tlsOn, via)
	}
	if on, via := clientTLS(req, true); !on || via != "direct" {
		t.Fatalf("with a trusted proxy the direct case still wins, got %v/%q", on, via)
	}
}

// onlyRecord fetches the single recorded row, failing if there is not exactly one.
func onlyRecord(t *testing.T, a *App, id string) model.Record {
	t.Helper()
	recs := records(t, a, id)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	return recs[0]
}
