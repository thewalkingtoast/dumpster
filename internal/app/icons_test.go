package app

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Browsers request /favicon.ico whether or not the page links it; a 404 there is
// the little broken-page glyph in every tab.
func TestFaviconIsServed(t *testing.T) {
	a := newTestApp(t)
	res, body := get(t, a.BaseURL()+"/favicon.ico")

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /favicon.ico = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "image/x-icon" {
		t.Fatalf("Content-Type = %q, want image/x-icon", ct)
	}
	if len(body) < 6 || body[0] != 0 || body[1] != 0 || body[2] != 1 || body[3] != 0 {
		t.Fatalf("body is not an ICO container (first bytes %q)", body[:min(len(body), 6)])
	}
}

// The manifest has to be valid JSON with a manifest content type, and every icon
// it names must actually exist — a manifest pointing at missing files is worse
// than no manifest, because the install prompt looks broken.
func TestWebManifestIsValidAndComplete(t *testing.T) {
	a := newTestApp(t)
	res, body := get(t, a.BaseURL()+"/site.webmanifest")

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /site.webmanifest = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/manifest+json" {
		t.Fatalf("Content-Type = %q, want application/manifest+json", ct)
	}

	var m struct {
		Name       string `json:"name"`
		ShortName  string `json:"short_name"`
		StartURL   string `json:"start_url"`
		ThemeColor string `json:"theme_color"`
		Icons      []struct {
			Src    string `json:"src"`
			Sizes  string `json:"sizes"`
			Type   string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if m.Name == "" || m.ShortName == "" || m.StartURL == "" {
		t.Fatalf("manifest is missing identity fields: %+v", m)
	}
	if len(m.Icons) < 3 {
		t.Fatalf("manifest lists %d icons, want at least a 192, a 512 and a maskable 512", len(m.Icons))
	}

	hasMaskable := false
	for _, icon := range m.Icons {
		if icon.Purpose == "maskable" {
			hasMaskable = true
		}
		if !strings.HasPrefix(icon.Src, "/static/") {
			t.Fatalf("icon %q is not served from /static/", icon.Src)
		}
		r, body := get(t, a.BaseURL()+icon.Src)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("manifest icon %s = %d, want 200", icon.Src, r.StatusCode)
		}
		if len(body) == 0 {
			t.Fatalf("manifest icon %s is empty", icon.Src)
		}
	}
	if !hasMaskable {
		t.Fatal("no maskable icon: Android will letterbox the app icon")
	}
}

// Every page someone might land on should carry the icon, the touch icon and the
// manifest — three templates, one habit.
func TestEveryPageLinksTheIcons(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	pages := map[string]string{
		"index": "/",
		"debug": "/d/" + id,
	}
	for name, path := range pages {
		_, body := get(t, a.BaseURL()+path)
		for _, want := range []string{
			`rel="icon" href="/static/icon.svg"`,
			`rel="icon" href="/favicon.ico"`,
			`rel="apple-touch-icon"`,
			`rel="manifest" href="/site.webmanifest"`,
			`name="theme-color"`,
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s page is missing %s", name, want)
			}
		}
	}

	// The error page is a page too.
	res, err := noRedirect.Get(a.BaseURL() + "/d/" + id + "/sse/emit")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(body), "<html") && !strings.Contains(string(body), `rel="icon"`) {
		t.Fatal("the HTML error page does not link the icon")
	}
}
