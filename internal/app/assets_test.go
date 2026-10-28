package app

import (
	"net/http"
	"strings"
	"testing"
)

// The page and the binary must agree on which assets to fetch: a stamped URL
// plus a stamped file is what lets the static handler cache forever.
func TestPagesAskForStampedAssets(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.Version = "deadbeef" })

	for _, path := range []string{"/", "/healthz"} {
		if path == "/healthz" {
			continue // JSON, no assets
		}
		_, body := get(t, a.BaseURL()+path)
		for _, want := range []string{"/static/style.css?v=deadbeef", "/static/theme.js?v=deadbeef"} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s does not ask for %s", path, want)
			}
		}
	}
}

// Caching is only safe because the URL changes with the build. An unstamped
// request must revalidate, or browsers invent their own freshness window.
func TestStaticCachePolicyDependsOnTheStamp(t *testing.T) {
	a := newTestApp(t)

	plain, code := get(t, a.BaseURL()+"/static/style.css")
	if code == "" {
		t.Fatal("style.css served an empty body")
	}
	if got := plain.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("unstamped Cache-Control = %q, want no-cache", got)
	}

	stamped, _ := get(t, a.BaseURL()+"/static/style.css?v=deadbeef")
	if got := stamped.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Fatalf("stamped Cache-Control = %q, want immutable", got)
	}
	if stamped.StatusCode != http.StatusOK {
		t.Fatalf("stamped request = %d, want 200: a query string must not break the file server", stamped.StatusCode)
	}
}

// The theme toggle must work with the exact markup app.js looks for.
func TestThemeScriptsAreServed(t *testing.T) {
	a := newTestApp(t)
	for _, name := range []string{"theme.js", "app.js", "style.css"} {
		res, body := get(t, a.BaseURL()+"/static/"+name)
		if res.StatusCode != http.StatusOK || body == "" {
			t.Fatalf("/static/%s = %d (%d bytes)", name, res.StatusCode, len(body))
		}
	}
}
