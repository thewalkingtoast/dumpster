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

// SSE is the one protocol the dumpster publishes, so the page has two jobs:
// the command listens, the button sends. Making the button open an EventSource
// would turn the dashboard into its own listener and duplicate every row.
func TestSSESplitsListeningFromSending(t *testing.T) {
	html, err := fs.ReadFile(templateFS, "templates/debug.html")
	if err != nil {
		t.Fatalf("read debug.html: %v", err)
	}
	page := string(html)
	for _, want := range []struct{ needle, why string }{
		{"Send 5 SSE events", "the button does not say it sends events"},
		{"listener", "the page does not explain that the command is the listener"},
		{"Run the command first", "the page does not say which half to run first"},
		{"how many listeners it reached", "the page does not explain the no-listener case"},
	} {
		if !strings.Contains(page, want.needle) {
			t.Fatalf("%s (missing %q)", want.why, want.needle)
		}
	}

	js, err := fs.ReadFile(Static, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(js)
	if !strings.Contains(src, "/sse/emit?count=5") {
		t.Fatal("the button does not POST an emission")
	}
	if strings.Contains(src, "new EventSource") {
		t.Fatal("the page still opens its own SSE listener, which would duplicate its own rows")
	}
	if strings.Contains(src, "interval_ms") {
		t.Fatal("the stale eager-stream parameter is still in the command")
	}
}

// The row is a scanning surface, not a log line: it shows how long ago something
// landed, and the exact instant waits in the expanded detail rather than being
// dropped altogether.
func TestRowsShowOnlyTheAgeAndDetailKeepsTheInstant(t *testing.T) {
	js, err := fs.ReadFile(Static, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	s := string(js)

	if strings.Contains(s, "fmtTime(rec.received_at) + ' · '") {
		t.Fatal("rows still print an absolute timestamp next to the age")
	}
	if !strings.Contains(s, "block('received', fmtTime(rec.received_at))") {
		t.Fatal("the exact arrival time is shown nowhere; expanding a row must still reveal it")
	}
	if !strings.Contains(s, "'data-at': rec.received_at") {
		t.Fatal("the row lost the timestamp its age is derived from")
	}
}
