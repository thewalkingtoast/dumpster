package app

import (
	"net/http"
	"strings"
	"testing"
)

// A page that carries a per-process token must never be stored: served from a
// cache it becomes a form that always fails, with no useful explanation.
func TestPagesAreNeverCached(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	for _, path := range []string{"/", "/d/" + id} {
		res, _ := get(t, a.BaseURL()+path)
		if got := res.Header.Get("Cache-Control"); !strings.Contains(got, "no-store") {
			t.Fatalf("%s: Cache-Control = %q, want no-store", path, got)
		}
	}
}

// Two instances with the same secret must accept each other's tokens, or the
// documented multi-instance mode silently breaks every form.
func TestSharedCSRFSecretWorksAcrossInstances(t *testing.T) {
	const shared = "a-shared-secret-long-enough"
	first := newTestApp(t, func(c *Config) { c.CSRFSecret = shared })
	second := newTestApp(t, func(c *Config) { c.CSRFSecret = shared })

	token := csrfFromPage(t, first)

	// Minted by the first instance, spent against the second.
	req, err := http.NewRequest(http.MethodPost, second.BaseURL()+"/rent",
		strings.NewReader("csrf="+token))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", second.BaseURL())
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("cross-instance rent: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: a shared secret should make tokens portable", res.StatusCode)
	}
}

// A secret short enough to guess is not a secret.
func TestCSRFSecretMustBeLongEnough(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.GRPCAddr = "127.0.0.1:0"
	cfg.TCPAddr = "127.0.0.1:0"
	cfg.UDPAddr = "127.0.0.1:0"
	cfg.CSRFSecret = "tooshort"

	err := New(cfg).Start()
	if err == nil {
		t.Fatal("Start accepted a 8-byte CSRF secret")
	}
	if !strings.Contains(err.Error(), "at least") {
		t.Fatalf("error = %v, want it to state the minimum length", err)
	}
}

// Firefox sends Origin: null for a same-origin form post. Rejecting it made the
// main button unusable in that browser while every other client worked.
func TestNullOriginIsAcceptedWithAValidToken(t *testing.T) {
	a := newTestApp(t)
	token := csrfFromPage(t, a)

	req, err := http.NewRequest(http.MethodPost, a.BaseURL()+"/rent",
		strings.NewReader("csrf="+token))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "null")
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("rent with a null origin: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: an opaque origin must not block a valid token", res.StatusCode)
	}
}

// ...but an opaque origin is not a free pass: the token is still required.
func TestNullOriginStillNeedsTheToken(t *testing.T) {
	a := newTestApp(t)
	req, _ := http.NewRequest(http.MethodPost, a.BaseURL()+"/rent", strings.NewReader("csrf="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "null")
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 without a token", res.StatusCode)
	}
}

// A named foreign origin is still a flat refusal, token or not.
func TestForeignOriginIsRejectedEvenWithAToken(t *testing.T) {
	a := newTestApp(t)
	token := csrfFromPage(t, a)
	req, _ := http.NewRequest(http.MethodPost, a.BaseURL()+"/rent",
		strings.NewReader("csrf="+token))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.test")
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a foreign origin", res.StatusCode)
	}
}

// The header itself is the root cause; it must not come back.
func TestReferrerPolicyKeepsTheOriginIntact(t *testing.T) {
	a := newTestApp(t)
	res, _ := get(t, a.BaseURL()+"/")
	if got := res.Header.Get("Referrer-Policy"); got == "no-referrer" {
		t.Fatal("Referrer-Policy is no-referrer again: Firefox will null the form Origin and renting breaks")
	}
	if got := res.Header.Get("Referrer-Policy"); got != "same-origin" {
		t.Fatalf("Referrer-Policy = %q, want same-origin", got)
	}
}
