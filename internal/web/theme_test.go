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

// Every page leads with the logo: landing, dumpster header and error page.
func TestLogoLeadsTheTitle(t *testing.T) {
	for _, tc := range []struct{ page, title string }{
		{"index.html", ">Dumpster</span>"},
		{"debug.html", ">Dumpster</span>"},
		{"error.html", ">Something broke</span>"},
	} {
		var buf bytes.Buffer
		if err := TemplatesFor("v1").ExecuteTemplate(&buf, tc.page, map[string]any{
			"ID":    "01a183a8-f05f-75a3-91f6-010285188788",
			"Title": "Something broke",
		}); err != nil {
			t.Fatalf("%s: %v", tc.page, err)
		}
		out := buf.String()
		logo := strings.Index(out, `<svg class="logo"`)
		word := strings.Index(out, tc.title)
		if logo < 0 || word < 0 {
			t.Fatalf("%s: logo at %d, title at %d", tc.page, logo, word)
		}
		if logo > word {
			t.Fatalf("%s draws the logo after the title", tc.page)
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

// A logo beside a wrapping headline costs a phone ~45px of its 353px measure,
// which pushed the error title from three lines to five. It stacks instead.
func TestWrappingHeadlineStacksTheLogoOnPhones(t *testing.T) {
	raw, err := fs.ReadFile(Static, "style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	css := strings.ReplaceAll(string(raw), "\n", "")
	if !strings.Contains(css, "@media (max-width: 480px){") {
		t.Fatal("no narrow-screen rule: the logo will squeeze the error headline")
	}
	media := css[strings.Index(css, "@media (max-width: 480px){"):]
	window := media[:min(400, len(media))]
	if !strings.Contains(window, ".brand-top{flex-direction:column") {
		t.Fatal("the narrow-screen rule does not stack .brand-top")
	}
}

// The event row is time | event name | body. Sizing those first columns in pixels
// let the timestamp paint straight over the event name the moment the root font
// size changed — a 113px timestamp inside a 70px column.
func TestEventColumnsSizeFromContent(t *testing.T) {
	raw, err := fs.ReadFile(Static, "style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	css := strings.ReplaceAll(string(raw), "\n", "")

	start := strings.Index(css, ".ev{")
	if start < 0 {
		t.Fatal("no .ev rule in the stylesheet")
	}
	end := strings.Index(css[start:], "}")
	rule := css[start : start+end]

	// Only the column list matters: the rule's padding and gap are legitimately
	// in pixels.
	at := strings.Index(rule, "grid-template-columns:")
	if at < 0 {
		t.Fatalf("no grid-template-columns in .ev: %s", rule)
	}
	cols := rule[at+len("grid-template-columns:"):]
	if semi := strings.Index(cols, ";"); semi >= 0 {
		cols = cols[:semi]
	}
	if !strings.HasPrefix(cols, "max-content max-content") {
		t.Fatalf("event columns are not content-sized, so a longer timestamp will overprint the name: %q", cols)
	}
	if strings.Contains(cols, "px") {
		t.Fatalf("event columns carry a pixel width again: %q", cols)
	}
}

// One long nowrap string in a row's meta — an IPv6 address, typically — used to
// widen the whole column past the viewport: 630px of content inside a 393px
// phone. Flex and grid items need permission to shrink, and the meta needs to
// ellipsize rather than push.
func TestRowsCannotWidenThePage(t *testing.T) {
	raw, err := fs.ReadFile(Static, "style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	css := strings.ReplaceAll(string(raw), "\n", "")

	if !strings.Contains(css, ".grid>*{min-width:0}") {
		t.Fatal("grid children may not shrink: a long row will widen the page")
	}

	start := strings.Index(css, ".row{")
	if start < 0 {
		t.Fatal("no .row rule")
	}
	row := css[start : start+strings.Index(css[start:], "}")]
	if !strings.Contains(row, "minmax(0,auto)") {
		t.Fatalf("the meta column cannot shrink below its content: %s", row)
	}

	start = strings.Index(css, ".when{")
	if start < 0 {
		t.Fatal("no .when rule")
	}
	when := css[start : start+strings.Index(css[start:], "}")]
	if !strings.Contains(when, "overflow:hidden") || !strings.Contains(when, "text-overflow:ellipsis") {
		t.Fatalf("the meta text has no way to truncate: %s", when)
	}
}

// On a phone the row's meta keeps the size and the age: a clipped IPv6 address is
// the least useful thing on the line. Desktop keeps all three.
func TestPhoneRowsKeepSizeAndAgeOnly(t *testing.T) {
	js, err := fs.ReadFile(Static, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	for _, want := range []string{"class: 'when'", "class: 'size'", "class: 'ip'", "class: 'age'"} {
		if !strings.Contains(string(js), want) {
			t.Fatalf("the row no longer renders %s, so the narrow-screen rule has nothing to hide", want)
		}
	}

	raw, err := fs.ReadFile(Static, "style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	css := strings.ReplaceAll(string(raw), "\n", "")

	media := strings.Index(css, "@media (max-width: 480px){")
	if media < 0 {
		t.Fatal("no narrow-screen media query")
	}
	// The rule has to live inside that block: outside it, the address would vanish
	// on every screen.
	if inner := braceSpan(css[media:]); !strings.Contains(inner, ".row .ip{display:none}") {
		t.Fatalf("the address is not hidden inside the narrow-screen block: %.200s", inner)
	}
}

// braceSpan returns the contents of the block starting at the first '{'.
func braceSpan(s string) string {
	open := strings.Index(s, "{")
	if open < 0 {
		return ""
	}
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[open+1 : i]
			}
		}
	}
	return s[open+1:]
}
