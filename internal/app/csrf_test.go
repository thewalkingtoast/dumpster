package app

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A forged POST must not mint a dumpster. Without the token, a page on another
// origin could rent on a visitor's address and spend their allowance.
func TestRentWithoutTokenIsForbidden(t *testing.T) {
	a := newTestApp(t)

	res, err := noRedirect.Post(a.BaseURL()+"/rent", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("POST /rent: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.StatusCode)
	}
	if n := a.store.Len(); n != 0 {
		t.Fatalf("a forged request created %d dumpster(s)", n)
	}
}

// The signature is what makes a token unforgeable, so a single flipped character
// has to be enough to reject it.
func TestTamperedTokenIsForbidden(t *testing.T) {
	a := newTestApp(t)
	token := csrfFromPage(t, a)

	last := token[len(token)-1]
	swap := byte('A')
	if last == 'A' {
		swap = 'B'
	}
	tampered := token[:len(token)-1] + string(swap)

	form := url.Values{csrfField: {tampered}}
	res, err := noRedirect.Post(a.BaseURL()+"/rent", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("POST /rent: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a tampered token", res.StatusCode)
	}
}

// Browsers attach Origin to form posts, so a request claiming another origin is
// rejected even when it carries a token it somehow obtained.
func TestCrossOriginRentIsForbidden(t *testing.T) {
	a := newTestApp(t)
	token := csrfFromPage(t, a)
	form := url.Values{csrfField: {token}}

	post := func(origin string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, a.BaseURL()+"/rent", strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := noRedirect.Do(req)
		if err != nil {
			t.Fatalf("POST /rent: %v", err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	if got := post("https://evil.example"); got != http.StatusForbidden {
		t.Fatalf("cross-origin rent = %d, want 403", got)
	}
	// "null" is what Firefox sends for a same-origin form post when the policy is
	// no-referrer, so it means "unknown", not "somewhere else". The token below is
	// valid, and the token is what actually defends the endpoint. A null origin
	// with no token is refused (TestNullOriginStillNeedsTheToken).
	if got := post("null"); got != http.StatusSeeOther {
		t.Fatalf("opaque origin rent = %d, want 303 with a valid token", got)
	}
	if got := post(a.BaseURL()); got != http.StatusSeeOther {
		t.Fatalf("same-origin rent = %d, want 303", got)
	}
}

// Both pages that can rent must carry a token, or the button they render is dead.
func TestEveryRentFormEmbedsAToken(t *testing.T) {
	a := newTestApp(t)

	_, index := get(t, a.BaseURL()+"/")
	if !strings.Contains(index, `name="csrf" value="`) {
		t.Fatalf("landing page has no csrf field:\n%s", index)
	}

	res, err := noRedirect.Post(a.BaseURL()+"/rent", "application/x-www-form-urlencoded", nil) // trigger 403 page
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	res2, gone := get(t, a.BaseURL()+"/d/00000000-0000-7000-8000-000000000000")
	if res2.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res2.StatusCode)
	}
	if !strings.Contains(gone, `name="csrf" value="`) {
		t.Fatalf("the gone page's rent form has no csrf field:\n%s", gone)
	}
}

// XSS defence in depth: the page never injects HTML, and the headers stop a
// stray script from running even if that discipline slips later.
func TestPageShipsNoScriptAndTightHeaders(t *testing.T) {
	a := newTestApp(t)
	res, body := get(t, a.BaseURL()+"/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if strings.Contains(body, "<script>") {
		t.Fatal("inline script on the page: the CSP would block it, and it should not be there")
	}

	csp := res.Header.Get("Content-Security-Policy")
	for _, want := range []string{"script-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP missing %q: %s", want, csp)
		}
	}
	if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
}
