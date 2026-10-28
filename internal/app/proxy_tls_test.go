package app

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
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

// The SNI is only observable if the proxy forwards it. nginx does that with
// $ssl_server_name; without it the field stays empty rather than being guessed.
func TestProxyForwardsTheSNI(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.TrustProxy = true })
	id := rent(t, a)

	req, err := http.NewRequest(http.MethodPost, a.BaseURL()+"/d/"+id+"/api/orders", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-SSL-Server-Name", "Dumpster.Toastsnacks.dev")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	res.Body.Close()

	rec := onlyRecord(t, a, id)
	if rec.Remote.TLSSNI != "dumpster.toastsnacks.dev" {
		t.Fatalf("tls_sni = %q, want the forwarded name, lowercased", rec.Remote.TLSSNI)
	}
	if rec.Remote.TLSVia != "proxy" {
		t.Fatalf("tls_via = %q, want proxy", rec.Remote.TLSVia)
	}
}

// Without a trusted proxy the header is a client's own claim, and claims are not
// observations.
func TestForwardedSNIIsIgnoredWithoutTrust(t *testing.T) {
	a := newTestApp(t) // TrustProxy off
	id := rent(t, a)

	req, _ := http.NewRequest(http.MethodPost, a.BaseURL()+"/d/"+id+"/api/orders", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-SSL-Server-Name", "evil.test")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	if got := onlyRecord(t, a, id).Remote.TLSSNI; got != "" {
		t.Fatalf("tls_sni = %q without a trusted proxy, want empty", got)
	}
}

// The value is only accepted if it could be an SNI. Go's client will not even
// send a header containing a newline, so the hostile shapes are checked here.
func TestForwardedSNIMustLookLikeAHostname(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"dumpster.toastsnacks.dev", "dumpster.toastsnacks.dev"},
		{"DUMPSTER.Toastsnacks.dev", "dumpster.toastsnacks.dev"},
		{"  spaced.test  ", "spaced.test"},
		{"", ""},
		{"not a hostname", ""},
		{"evil.test\r\nX-Injected: 1", ""},
		{"<script>alert(1)</script>", ""},
		{strings.Repeat("a", 254), ""},
	} {
		if got := sniFromProxy(tc.in); got != tc.want {
			t.Fatalf("sniFromProxy(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A direct handshake needs no proxy to say what the client asked for.
func TestDirectTLSCarriesItsSNI(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://dumpster.test/d/x", nil)
	req.TLS = &tls.ConnectionState{ServerName: "dumpster.test"}
	var a App
	a.cfg.TrustProxy = false
	rem := remoteFrom(req, a.cfg.TrustProxy, a.cfg.TrustRealIP)
	if !rem.TLS || rem.TLSVia != "direct" || rem.TLSSNI != "dumpster.test" {
		t.Fatalf("remote = %+v, want direct TLS with its SNI", rem)
	}
}
