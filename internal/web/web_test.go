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
	if !strings.Contains(s, "html{font-size:120%}") {
		t.Fatal("style.css lost its 120% root size: rem-based type no longer scales up by 20%")
	}
	if strings.Contains(s, "font:16px/") {
		t.Fatal("body sets a fixed px font size again, which overrides the scaled root")
	}
	if !strings.Contains(s, "font:1rem/") {
		t.Fatal("body no longer takes its size from the root font size")
	}
}
