package main

import (
	"os"
	"strings"
	"testing"
)

// The README carries the nginx config we actually run. Each directive in there
// was learned the hard way, so the example is checked for the ones whose absence
// caused a real failure — a doc that drifts back to $http_host or drops the
// forwarded protocol is worse than no doc at all.
func TestReadmeNginxExampleKeepsTheHardWonDirectives(t *testing.T) {
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	readme := string(raw)

	start := strings.Index(readme, "### A complete nginx config")
	if start < 0 {
		t.Fatal("the README no longer carries the nginx example")
	}
	open := strings.Index(readme[start:], "```nginx")
	if open < 0 {
		t.Fatal("the nginx section has no code block")
	}
	open += start
	closeAt := strings.Index(readme[open+len("```nginx"):], "```")
	if closeAt < 0 {
		t.Fatal("the nginx block is unterminated")
	}
	block := readme[open : open+len("```nginx")+closeAt]

	for _, want := range []struct{ needle, why string }{
		{`proxy_set_header Host                  $host;`, "Host must be $host: $http_host is empty over HTTP/3 and the app answers 400"},
		{`X-Forwarded-Protocol  $server_protocol`, "without it every record claims the proxy's HTTP/1.1"},
		{`X-Forwarded-Proto     $scheme`, "without it a wss:// client is recorded as plaintext"},
		{`X-SSL-Server-Name     $ssl_server_name`, "without it tls_sni is always empty behind the proxy"},
		{`proxy_set_header Upgrade    $http_upgrade`, "WebSocket cannot upgrade without it"},
		{`proxy_read_timeout    3600`, "the 60s default cuts SSE and idle sockets"},
		{`listen      [::]:443 quic reuseport`, "HTTP/3 needs its own UDP listener"},
		{`Alt-Svc 'h3=":443"; ma=86400'`, "nothing discovers h3 without it"},
		{`X-Accel-Buffering`, "the doc must say how streaming survives proxy_buffering on"},
	} {
		if !strings.Contains(block, want.needle) {
			t.Fatalf("the nginx example lost %q — %s", want.needle, want.why)
		}
	}
}
