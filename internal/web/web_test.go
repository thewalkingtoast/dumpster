package web

import (
	"bytes"
	"io/fs"
	"strings"
	"testing"
)

func TestIndexRendersWithOneCTA(t *testing.T) {
	var buf bytes.Buffer
	err := Templates.ExecuteTemplate(&buf, "index.html", map[string]any{
		"Active": 3,
		"TTL":    "12h",
	})
	if err != nil {
		t.Fatalf("execute index.html: %v", err)
	}
	if got := strings.Count(buf.String(), "Rent a dumpster"); got != 1 {
		t.Fatalf("CTA appears %d times, want exactly 1", got)
	}
}

func TestStaticAssetsExist(t *testing.T) {
	for _, name := range []string{"style.css"} {
		if _, err := fs.Stat(Static, name); err != nil {
			t.Fatalf("static/%s: %v", name, err)
		}
	}
}

// Type size is a product decision: the page was hard to read at 16px. Rather than
// hand-scaling forty declarations, the root is scaled and body text follows it —
// so this test guards the two lines that make every rem in the sheet grow.
func TestStylesheetScalesFromTheRootFontSize(t *testing.T) {
	css, err := fs.ReadFile(Static, "style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	s := string(css)
	if !strings.Contains(s, "html{font-size:132%}") {
		t.Fatal("style.css lost its 132% root size: rem-based type no longer scales up")
	}
	if strings.Contains(s, "font:16px/") {
		t.Fatal("body sets a fixed px font size again, which overrides the scaled root")
	}
	if !strings.Contains(s, "font:1rem/") {
		t.Fatal("body no longer takes its size from the root font size")
	}
}

// The row timestamp is the page's ordering evidence: without milliseconds two
// requests in the same second look arbitrarily sorted, and without a date a
// record from before midnight is indistinguishable from one after it.
func TestTimestampFormatterKeepsMillisecondsAndDates(t *testing.T) {
	js, err := fs.ReadFile(Static, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	s := string(js)

	for _, want := range []struct{ needle, why string }{
		{"getMilliseconds()", "milliseconds dropped from the row time"},
		{"pad(d.getMilliseconds(), 3)", "milliseconds not zero-padded to three digits"},
		{"sameDay", "no same-day check, so older rows lose their date"},
		{"pad(d.getMonth() + 1)", "date prefix missing its month"},
	} {
		if !strings.Contains(s, want.needle) {
			t.Fatalf("%s (missing %q)", want.why, want.needle)
		}
	}
	if strings.Contains(s, "toLocaleTimeString") {
		t.Fatal("toLocaleTimeString is back: it drops milliseconds and can render midnight as 24:00")
	}
}

// Ages must tick without re-rendering rows: a full rebuild every second would
// fight the live socket and collapse whatever row the reader has open.
func TestAgeTickerRewritesOnlyAgeNodes(t *testing.T) {
	js, err := fs.ReadFile(Static, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	s := string(js)

	for _, want := range []struct{ needle, why string }{
		{"class: 'age', 'data-at': rec.received_at", "rows no longer carry an age element"},
		{"querySelectorAll('.age[data-at]')", "the tick does not target just the age nodes"},
		{"if (node.textContent !== next)", "the tick rewrites ages that did not change"},
		{"}, 1000)", "the ticker is not on a one-second interval"},
		{"document.hidden", "the ticker runs in hidden tabs"},
		{"startAgeTicker();", "the ticker is never started"},
	} {
		if !strings.Contains(s, want.needle) {
			t.Fatalf("%s (missing %q)", want.why, want.needle)
		}
	}
}

// Tabular figures keep the row from jittering as "9s ago" becomes "10s ago".
func TestAgeUsesTabularFigures(t *testing.T) {
	css, err := fs.ReadFile(Static, "style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	if !strings.Contains(string(css), "tabular-nums") {
		t.Fatal("the age is not tabular: rows will shift horizontally as it counts")
	}
}
