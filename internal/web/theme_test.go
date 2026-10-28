package web

import (
	"bytes"
	"io/fs"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Every asset URL must carry the build stamp, or a new binary will hand out pages
// that ask for the previous stylesheet — which browsers then serve from cache.
func TestAssetURLsCarryTheBuildStamp(t *testing.T) {
	var buf bytes.Buffer
	err := TemplatesFor("abc1234").ExecuteTemplate(&buf, "debug.html", map[string]any{
		"ID": "01a183a8-f05f-75a3-91f6-010285188788",
	})
	if err != nil {
		t.Fatalf("execute debug.html: %v", err)
	}
	page := buf.String()

	for _, want := range []string{
		`/static/style.css?v=abc1234`,
		`/static/app.js?v=abc1234`,
		`/static/theme.js?v=abc1234`,
		`/static/icon.svg?v=abc1234`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("page is missing %s", want)
		}
	}
	// Nothing may slip through unstamped.
	if strings.Contains(page, `href="/static/style.css"`) || strings.Contains(page, `src="/static/app.js"`) {
		t.Fatal("an asset URL was rendered without the build stamp")
	}
}

// The theme must be decided before the first paint, which means a blocking
// script in <head>. Deferred or inline (blocked by CSP) would both flash.
func TestThemeScriptRunsBeforeAnyPaint(t *testing.T) {
	js, err := fs.ReadFile(Static, "theme.js")
	if err != nil {
		t.Fatalf("read theme.js: %v", err)
	}
	if !strings.Contains(string(js), `setAttribute('data-theme'`) {
		t.Fatal("theme.js never sets data-theme")
	}

	for _, page := range []string{"index.html", "debug.html", "error.html"} {
		var buf bytes.Buffer
		if err := TemplatesFor("v1").ExecuteTemplate(&buf, page, map[string]any{}); err != nil {
			t.Fatalf("%s: %v", page, err)
		}
		out := buf.String()
		script := strings.Index(out, `theme.js`)
		style := strings.Index(out, `style.css`)
		if script < 0 {
			t.Fatalf("%s does not load theme.js", page)
		}
		if style >= 0 && script > style {
			t.Fatalf("%s loads theme.js after the stylesheet", page)
		}
		if strings.Contains(out, `theme.js" defer`) || strings.Contains(out, `theme.js" async`) {
			t.Fatalf("%s defers theme.js, which allows a flash of the wrong palette", page)
		}
	}
}

// The toggle belongs to the right of the connection status in the header.
func TestTogglerSitsRightOfTheStatus(t *testing.T) {
	var buf bytes.Buffer
	if err := TemplatesFor("").ExecuteTemplate(&buf, "debug.html", map[string]any{
		"ID": "01a183a8-f05f-75a3-91f6-010285188788",
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()

	status := strings.Index(out, `id="status"`)
	toggle := strings.Index(out, `data-theme-toggle`)
	if status < 0 || toggle < 0 {
		t.Fatalf("header is missing the status (%d) or the toggle (%d)", status, toggle)
	}
	if toggle < status {
		t.Fatal("the theme toggle renders before the connection status")
	}
	// type="button" is written before the data attribute, so look at a window
	// around the button rather than strictly after it.
	window := out[max(0, toggle-140):min(len(out), toggle+320)]
	for _, want := range []string{`aria-label=`, `aria-pressed=`, `type="button"`} {
		if !strings.Contains(window, want) {
			t.Fatalf("the toggle is missing %s", want)
		}
	}
}

// Parsing colours is the only way to check a palette without a browser, and a
// light theme with unreadable text is worse than no light theme.
func TestThemesAreLegible(t *testing.T) {
	raw, err := fs.ReadFile(Static, "style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	css := string(raw)

	dark := mustVars(t, css, `:root\{([^}]*)\}`)
	light := mustVars(t, css, `\[data-theme="light"\]\{([^}]*)\}`)

	if len(dark) == 0 || len(light) == 0 {
		t.Fatalf("could not parse both palettes: dark=%d light=%d", len(dark), len(light))
	}

	// Every token the light palette overrides must exist in the dark one, or the
	// two themes drift into inconsistency.
	for name := range light {
		if _, ok := dark[name]; !ok {
			t.Fatalf("light theme defines %s, which the dark theme does not", name)
		}
	}

	for _, tc := range []struct {
		theme       string
		vars        map[string]string
		bodyMin     float64 // text against the page background
		uiMin       float64 // muted text against panels
	}{
		{"dark", dark, 4.5, 3.0},
		{"light", light, 4.5, 3.0},
	} {
		ink := tc.vars["--ink"]
		bg := tc.vars["--bg"]
		panel := tc.vars["--panel"]
		muted := tc.vars["--muted"]

		if got := contrast(ink, bg); got < tc.bodyMin {
			t.Fatalf("%s: --ink on --bg is %.2f:1, want at least %.1f:1", tc.theme, got, tc.bodyMin)
		}
		if got := contrast(muted, panel); got < tc.uiMin {
			t.Fatalf("%s: --muted on --panel is %.2f:1, want at least %.1f:1", tc.theme, got, tc.uiMin)
		}
	}
}

func mustVars(t *testing.T, css, pattern string) map[string]string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(css)
	if m == nil {
		return nil
	}
	out := map[string]string{}
	for _, decl := range strings.Split(m[1], ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(decl), ":")
		if !ok || !strings.HasPrefix(key, "--") {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(value), "#") {
			out[key] = strings.TrimSpace(value)
		}
	}
	return out
}

// contrast is the WCAG ratio between two #rrggbb colours.
func contrast(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func luminance(hex string) float64 {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return 0
	}
	var rgb [3]float64
	for i := 0; i < 3; i++ {
		v, err := strconv.ParseInt(hex[i*2:i*2+2], 16, 32)
		if err != nil {
			return 0
		}
		c := float64(v) / 255
		if c <= 0.03928 {
			rgb[i] = c / 12.92
		} else {
			rgb[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*rgb[0] + 0.7152*rgb[1] + 0.0722*rgb[2]
}

// Every page carries the switch: landing, error and dumpster.
func TestEveryPageHasAThemeSwitch(t *testing.T) {
	for _, page := range []string{"index.html", "debug.html", "error.html"} {
		var buf bytes.Buffer
		if err := TemplatesFor("v1").ExecuteTemplate(&buf, page, map[string]any{}); err != nil {
			t.Fatalf("%s: %v", page, err)
		}
		if !strings.Contains(buf.String(), "data-theme-toggle") {
			t.Fatalf("%s has no theme switch", page)
		}
	}
}

// The landing title and the dumpster header both lead with the logo.
func TestLogoLeadsTheTitle(t *testing.T) {
	for _, page := range []string{"index.html", "debug.html"} {
		var buf bytes.Buffer
		if err := TemplatesFor("v1").ExecuteTemplate(&buf, page, map[string]any{
			"ID": "01a183a8-f05f-75a3-91f6-010285188788",
		}); err != nil {
			t.Fatalf("%s: %v", page, err)
		}
		out := buf.String()
		logo := strings.Index(out, `<svg class="logo"`)
		word := strings.Index(out, ">Dumpster</span>")
		if logo < 0 || word < 0 {
			t.Fatalf("%s: logo at %d, title at %d", page, logo, word)
		}
		if logo > word {
			t.Fatalf("%s draws the logo after the title", page)
		}
	}
}

// One owner for the theme: app.js is only loaded on a dumpster page, so a switch
// there would leave the landing and error pages dead.
func TestOnlyThemeScriptOwnsTheSwitch(t *testing.T) {
	app, err := fs.ReadFile(Static, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	if strings.Contains(string(app), "data-theme-toggle") || strings.Contains(string(app), "dumpster-theme") {
		t.Fatal("app.js still handles the theme; landing and error pages do not load it")
	}

	theme, err := fs.ReadFile(Static, "theme.js")
	if err != nil {
		t.Fatalf("read theme.js: %v", err)
	}
	for _, want := range []string{"data-theme-toggle", "querySelectorAll", "localStorage"} {
		if !strings.Contains(string(theme), want) {
			t.Fatalf("theme.js does not handle %s", want)
		}
	}
}
