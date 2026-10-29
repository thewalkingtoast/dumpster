package web

import (
	"bytes"
	"io/fs"
	"regexp"
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
		{"}, 2000)", "the young pass is not on a two-second interval"},
		{"}, 60000)", "the slow pass is not on a one-minute interval"},
		{"maxAgeSeconds", "the tick has no age ceiling, so the fast pass rescans every row"},
		{"YOUNG_SECONDS", "no boundary for which rows the fast pass owns"},
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

// SSE and WebSocket now demonstrate themselves: connecting is enough, so the page
// must not offer a button that sends events, and must say what arrives on connect.
func TestConnectIsTheDemonstration(t *testing.T) {
	html, err := fs.ReadFile(templateFS, "templates/debug.html")
	if err != nil {
		t.Fatalf("read debug.html: %v", err)
	}
	page := string(html)

	for _, gone := range []string{"Send 5 SSE events", `id="btn-sse"`} {
		if strings.Contains(page, gone) {
			t.Fatalf("the page still offers %q, but connecting now sends the events", gone)
		}
	}
	for _, want := range []struct{ needle, why string }{
		{"every listener", "the SSE heading does not say the burst is shared"},
		{"Five events", "the SSE hint does not mention the burst"},
		{"every listener", "the SSE hint does not say the burst is shared"},
		{"pings", "the WebSocket hint does not mention the pings"},
	} {
		if !strings.Contains(page, want.needle) {
			t.Fatalf("%s (missing %q)", want.why, want.needle)
		}
	}

	js, err := fs.ReadFile(Static, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	if strings.Contains(string(js), "sse/emit") {
		t.Fatal("app.js still mentions sse/emit; that endpoint is gone")
	}
	if strings.Contains(page, "sse/emit") {
		t.Fatal("the page still documents sse/emit; that endpoint is gone")
	}
}

// Ages must tick without re-rendering rows: a full rebuild every second would
// fight the live socket and collapse whatever row the reader has open.
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

// The detail pane is where the payload actually lives, so its blocks copy on a
// tap: the same affordance the command blocks have, and the same fallback for
// plain-http origins where navigator.clipboard does not exist.
func TestDetailBlocksAreCopyable(t *testing.T) {
	js, err := fs.ReadFile(Static, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(js)
	for _, want := range []struct{ needle, why string }{
		{"wireCopyable(body, label)", "detail blocks are not wired for copying"},
		{"node.title = 'click to copy ' + label", "a block does not say what tapping it does"},
		{"copyText(node.textContent, node)", "the block does not copy its own text"},
		{"function legacyCopy", "no fallback: plain-http LAN is not a secure context"},
		{"wireCopyable(pre, '')", "the command blocks no longer share the copy path"},
	} {
		if !strings.Contains(src, want.needle) {
			t.Fatalf("%s (missing %q)", want.why, want.needle)
		}
	}

	css, err := fs.ReadFile(Static, "style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	sheet := string(css)
	if !strings.Contains(sheet, ".detail pre{") || !strings.Contains(sheet, "cursor:copy") {
		t.Fatal("detail blocks give no copy affordance")
	}
	if !strings.Contains(sheet, ".detail pre.copied") {
		t.Fatal("a copied block gives no visual confirmation")
	}
}

// The detail pane must stay protocol-agnostic: one renderer, one <pre> creation
// site, and that site wired for copying. A per-protocol section bolted on later
// would otherwise ship blocks nobody can copy — the exact regression this guards.
func TestDetailPaneIsProtocolAgnosticAndFullyCopyable(t *testing.T) {
	js, err := fs.ReadFile(Static, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(js)

	if got := strings.Count(src, "el('pre'"); got != 1 {
		t.Fatalf("%d <pre> creation sites; every detail block must come from the one wired block() helper", got)
	}
	if !strings.Contains(src, "wireCopyable(body, label)") {
		t.Fatal("the single block() renderer is not wired for copying")
	}

	start := strings.Index(src, "function detailNode(rec)")
	end := strings.Index(src, "function makeRow(rec)")
	if start < 0 || end <= start {
		t.Fatal("cannot locate detailNode; the guard needs updating")
	}
	if pane := src[start:end]; strings.Contains(pane, "rec.kind") {
		t.Fatal("detailNode branches on rec.kind: any protocol-specific section must still render through block()")
	}
	for _, block := range []string{"'received'", "'request'", "'headers'", "'body (raw)'", "'remote'", "'timing (µs)'", "'meta'"} {
		if !strings.Contains(src[start:end], block) {
			t.Fatalf("detail pane lost the %s block", block)
		}
	}
}

// A copy needs a confirmation the eye catches without moving it there. One toast
// for the page, reused: rapid taps replace the message rather than stacking.
func TestCopyShowsASingleFadingToast(t *testing.T) {
	js, err := fs.ReadFile(Static, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(js)
	for _, want := range []struct{ needle, why string }{
		{"showToast('Copied')", "copying does not raise the toast"},
		{"let toastEl = null", "the toast is not reused, so taps would stack up"},
		{"toastEl.classList.remove('show'), 1000", "the toast does not clear after one second"},
		{"'aria-live': 'polite'", "the toast is not announced to a screen reader"},
	} {
		if !strings.Contains(src, want.needle) {
			t.Fatalf("%s (missing %q)", want.why, want.needle)
		}
	}

	css, err := fs.ReadFile(Static, "style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	sheet := string(css)
	for _, want := range []struct{ needle, why string }{
		{".toast{", "no toast styling"},
		{"position:fixed", "the toast would scroll with the page instead of floating"},
		{"background:var(--sage)", "the toast does not use the copy green"},
		{"left:50%;transform:translateX(-50%)", "the toast is not centred"},
		{".toast.show{opacity:1}", "the toast has no visible state"},
	} {
		if !strings.Contains(sheet, want.needle) {
			t.Fatalf("%s (missing %q)", want.why, want.needle)
		}
	}
}

// The examples people copy must match the page they are standing on: over https
// the socket has to be wss, and a gRPC listener with a certificate must not be
// advertised as -plaintext.
func TestExamplesMatchTheServedScheme(t *testing.T) {
	js, err := fs.ReadFile(Static, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(js)

	if strings.Contains(src, `websocat "ws://`) {
		t.Fatal("the websocket example hardcodes ws:// and is blocked as mixed content over https")
	}
	if !strings.Contains(src, "${TLS ? 'wss' : 'ws'}") {
		t.Fatal("the websocket example does not follow the TLS switch")
	}
	if !strings.Contains(src, "TLS ? '-insecure' : '-plaintext'") {
		t.Fatal("the grpc example does not follow the TLS switch")
	}
	if !strings.Contains(src, "body.dataset.tls") {
		t.Fatal("the page never reads data-tls, so the switch can only ever read false")
	}
	// The live feed keeps using location.protocol: that socket really is in the
	// browser, so only the copy-paste example follows the server's switch.
}

// The switch has to reach the markup, or app.js reads an attribute that is not
// there and every deployment looks like plaintext.
func TestDebugPageCarriesTheTLSSwitch(t *testing.T) {
	for _, tc := range []struct {
		on   bool
		want string
	}{{true, `data-tls="true"`}, {false, `data-tls="false"`}} {
		var buf bytes.Buffer
		err := Templates.ExecuteTemplate(&buf, "debug.html", map[string]any{
			"ID":     "01a183a8-f05f-75a3-91f6-010285188788",
			"TLS":    tc.on,
			"Origin": "https://dumpster.example",
		})
		if err != nil {
			t.Fatalf("execute debug.html: %v", err)
		}
		if !strings.Contains(buf.String(), tc.want) {
			t.Fatalf("TLS=%v: page is missing %s", tc.on, tc.want)
		}
	}
}

// Every command box must have a command behind it. An empty <pre> reads as a
// broken page, and the only way to get one is a data-cmd with no entry in
// app.js — so the two files are checked against each other.
func TestEveryCommandBoxHasACommand(t *testing.T) {
	page, err := fs.ReadFile(templateFS, "templates/debug.html")
	if err != nil {
		t.Fatalf("read debug.html: %v", err)
	}
	js, err := fs.ReadFile(Static, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}

	have := map[string]bool{}
	for _, m := range regexp.MustCompile("(?m)^\\s+([a-z]+): `").FindAllStringSubmatch(string(js), -1) {
		have[m[1]] = true
	}
	boxes := regexp.MustCompile(`data-cmd="([a-z]+)"`).FindAllStringSubmatch(string(page), -1)
	if len(boxes) < 8 {
		t.Fatalf("found only %d command boxes; the template changed shape", len(boxes))
	}
	for _, m := range boxes {
		if !have[m[1]] {
			t.Fatalf("the page asks for the %q command but app.js has no such entry", m[1])
		}
	}
}

// "Echoes your frames" is a promise; the helper has to say how to make one.
func TestWebSocketHelperExplainsHowToSendFrames(t *testing.T) {
	page, err := fs.ReadFile(templateFS, "templates/debug.html")
	if err != nil {
		t.Fatalf("read debug.html: %v", err)
	}
	body := string(page)

	start := strings.Index(body, `data-cmd="ws"`)
	end := strings.Index(body, `data-cmd="tcp"`)
	if start < 0 || end < start {
		t.Fatal("cannot locate the WebSocket section")
	}
	ws := body[start:end]
	for _, want := range []struct{ needle, why string }{
		{"How to use?", "the WebSocket section has no helper"},
		{"press Enter", "the helper does not say how to send a frame interactively"},
		{"one text frame", "the helper does not say a line is a frame"},
		{"base64", "the helper does not explain how binary frames are recorded"},
		{"hung up without a close frame", "the helper does not explain why a one-shot row closes that way"},
		{`data-cmd="wsframe"`, "no one-shot example"},
		{`data-cmd="wsbinary"`, "no binary example"},
		{"new WebSocket", "no alternative for people without websocat"},
	} {
		if !strings.Contains(ws, want.needle) {
			t.Fatalf("%s (missing %q)", want.why, want.needle)
		}
	}
}

// The source link is a control like the theme switch: same box, same corner, and
// on the debug page it sits between the live status and the switch. "Same sized"
// is asserted as a comparison, so resizing one without the other fails.
func TestGitHubLinkSitsBesideTheThemeSwitch(t *testing.T) {
	var buf bytes.Buffer
	if err := Templates.ExecuteTemplate(&buf, "index.html", map[string]any{"Active": 3, "TTL": "12h"}); err != nil {
		t.Fatalf("execute index.html: %v", err)
	}
	page := buf.String()

	gh := strings.Index(page, `class="gh"`)
	theme := strings.Index(page, `class="theme"`)
	if gh < 0 {
		t.Fatal("the rendered page has no GitHub link")
	}
	if theme < 0 || gh > theme {
		t.Fatal("the GitHub link does not come before the theme switch")
	}
	for _, want := range []struct{ needle, why string }{
		{`href="https://github.com/thewalkingtoast/dumpster"`, "the link does not point at the repository"},
		{`rel="noopener noreferrer"`, "a target=_blank link without rel=noopener hands the new tab a handle on this one"},
		{`aria-label="Source on GitHub"`, "the link has no accessible name"},
		{`fill="currentColor"`, "the mark does not inherit the control's colour"},
	} {
		if !strings.Contains(page, want.needle) {
			t.Fatalf("%s (missing %q)", want.why, want.needle)
		}
	}

	// Every page carries it, in that order.
	for _, name := range []string{"index.html", "debug.html", "error.html"} {
		src, err := fs.ReadFile(templateFS, "templates/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		g := strings.Index(string(src), `{{template "githubLink"}}`)
		th := strings.Index(string(src), `{{template "themeToggle"}}`)
		if g < 0 || th < 0 || g > th {
			t.Fatalf("%s: the GitHub link is missing or after the theme switch", name)
		}
	}

	// Same size as the theme switch, read out of the rules rather than assumed.
	css := strings.ReplaceAll(readCSS(t), "\n", "")
	size := func(rule string) string {
		i := strings.Index(css, rule+"{")
		if i < 0 {
			return ""
		}
		block := css[i : i+strings.Index(css[i:], "}")]
		w := regexp.MustCompile(`width:(\d+px)`).FindStringSubmatch(block)
		h := regexp.MustCompile(`height:(\d+px)`).FindStringSubmatch(block)
		if w == nil || h == nil {
			return ""
		}
		return w[1] + "x" + h[1]
	}
	if got, want := size(".gh"), size(".theme"); got == "" || got != want {
		t.Fatalf("the GitHub link is %q, the theme switch is %q: they must be the same size", got, want)
	}
}

// readCSS is the stylesheet the binary serves, as a string.
func readCSS(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(Static, "style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	return string(b)
}
