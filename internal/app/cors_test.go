package app

import (
	"net/http"
	"strings"
	"testing"
)

// Wildcard CORS exists so any client — including a page on another origin — can
// talk to a dumpster. It must stop at the dumpster boundary: the landing page
// carries a CSRF token, and a remote page able to read it could rent on someone
// else's address.
func TestCorsIsWildcardForDumpstersOnly(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	for _, path := range []string{"/d/" + id + "/_records.json", "/d/" + id} {
		res, _ := get(t, a.BaseURL()+path)
		if got := res.Header.Get("Access-Control-Allow-Origin"); got != "*" {
			t.Fatalf("%s: Allow-Origin = %q, want *", path, got)
		}
	}

	for _, path := range []string{"/", "/healthz"} {
		res, _ := get(t, a.BaseURL()+path)
		if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("%s: Allow-Origin = %q, want none (its HTML carries a CSRF token)", path, got)
		}
	}
}

// A browser preflight carries no payload, so it must not become a dump row —
// otherwise every cross-origin POST leaves one junk record behind.
func TestPreflightIsAnsweredAndNotRecorded(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	before := len(records(t, a, id))

	req, err := http.NewRequest(http.MethodOptions, a.BaseURL()+"/d/"+id+"/ingest", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", res.StatusCode)
	}
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("preflight Allow-Origin = %q, want *", got)
	}
	if got := len(records(t, a, id)); got != before {
		t.Fatalf("the preflight was dumped: %d records before, %d after", before, got)
	}

	// A plain OPTIONS with no preflight headers is real traffic and still dumps.
	plain, err := http.NewRequest(http.MethodOptions, a.BaseURL()+"/d/"+id+"/ingest", nil)
	if err != nil {
		t.Fatal(err)
	}
	res2, err := noRedirect.Do(plain)
	if err != nil {
		t.Fatalf("OPTIONS: %v", err)
	}
	res2.Body.Close()
	if got := len(records(t, a, id)); got != before+1 {
		t.Fatalf("a real OPTIONS was not dumped: %d records, want %d", got, before+1)
	}
}

// Creating a dumpster is the one action that belongs to this site, so the page
// that offers it must not be framable.
func TestRentPageCannotBeFramed(t *testing.T) {
	a := newTestApp(t)
	res, _ := get(t, a.BaseURL()+"/")
	if got := res.Header.Get("X-Frame-Options"); !strings.EqualFold(got, "DENY") {
		t.Fatalf("X-Frame-Options = %q, want DENY", got)
	}
	csp := res.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP lost frame-ancestors: %s", csp)
	}
}
