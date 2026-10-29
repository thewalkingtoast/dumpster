package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thewalkingtoast/dumpster/internal/decode"
	"github.com/thewalkingtoast/dumpster/internal/model"
)

type connAcceptedKey struct{}

// withConnAcceptedAt stashes the accept time so a handler can report how long a
// keep-alive connection sat before this request arrived.
func withConnAcceptedAt(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, connAcceptedKey{}, time.Now())
}

func connAcceptedAt(r *http.Request) (time.Time, bool) {
	t, ok := r.Context().Value(connAcceptedKey{}).(time.Time)
	return t, ok
}

// withHeaders stamps every response with the headers an unauthenticated debug
// endpoint needs: no sniffing, no indexing, and CORS open so a browser on any
// origin can dump into a dumpster.
func (a *App) withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		// same-origin, not no-referrer: Firefox nulls a form POST's Origin header
		// when the policy is no-referrer, which made every rent in Firefox look
		// cross-origin. This still keeps the referrer away from other origins.
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		// Older browsers ignore frame-ancestors; this is the same rule in a form
		// they understand.
		h.Set("X-Frame-Options", "DENY")

		// Wildcard CORS is deliberate and scoped: any client, including a page on
		// another origin, must be able to talk to a dumpster. It stops at the
		// dumpster boundary — the landing page embeds a CSRF token, and a remote
		// page that could read it could rent against someone else's address.
		if hasDumpsterSubpath(r.URL.Path) {
			h.Set("Access-Control-Allow-Origin", "*")
			h.Set("Access-Control-Allow-Methods", "*")
			h.Set("Access-Control-Allow-Headers", "*")
			h.Set("Access-Control-Expose-Headers", "*")

			// A browser preflight is not traffic anyone wants dumped: no payload,
			// and one per request would fill the list with junk rows.
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; "+
				"script-src 'self'; connect-src 'self' ws: wss:; base-uri 'none'; form-action 'self'; "+
				"object-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// writeJSON is the only JSON writer; it keeps errors unescaped so dumps of HTML
// stay readable.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// hasDumpsterSubpath reports whether p addresses something inside a dumpster —
// /d/<id>/anything — as opposed to /d/<id> itself, the HTML debug page. The page
// is the app's own UI: no machine client reads it, and it embeds a CSRF token.
// corsReadable reports whether another origin will be allowed to read this
// response. The CORS middleware sets the header before handlers run, so a handler
// can ask this question and refuse to emit anything it would not want read — a
// CSRF token, above all.
func corsReadable(w http.ResponseWriter) bool {
	return w.Header().Get("Access-Control-Allow-Origin") != ""
}

func hasDumpsterSubpath(p string) bool {
	if !strings.HasPrefix(p, "/d/") {
		return false
	}
	rest := p[len("/d/"):]
	cut := strings.IndexByte(rest, '/')
	return cut > 0 && cut < len(rest)-1
}

// maxInlineText caps how much body text is inlined into a record; the JSON
// snapshot would otherwise carry megabytes.
const maxInlineText = 64 << 10

// payloadPreview renders the first message of a streaming protocol for the
// request panel, capped exactly like an HTTP body so a stream cannot smuggle a
// megabyte past the inline budget.
func payloadPreview(b []byte) (text, b64 string) {
	if len(b) > maxInlineText {
		b = b[:maxInlineText]
	}
	if utf8.Valid(b) {
		return string(b), ""
	}
	return "", base64.StdEncoding.EncodeToString(b)
}

// readBody reads at most max bytes and reports whether it truncated.
func readBody(body io.Reader, max int) (data []byte, truncated bool) {
	data, err := io.ReadAll(io.LimitReader(body, int64(max)+1))
	if err != nil {
		return data, false
	}
	if len(data) > max {
		return data[:max], true
	}
	return data, false
}

// remoteFrom describes the caller. With trustProxy set, the first
// X-Forwarded-For hop becomes the reported IP, but the raw socket address and
// the header itself are always preserved.
// forwardedProtocol accepts a proxy's claim about the client's HTTP version, but
// only in the one shape an HTTP version can take. Anything else is a header of
// unknown provenance heading for a record a human will read, so it is dropped.
func forwardedProtocol(raw string) string {
	v := strings.TrimSpace(raw)
	switch v {
	case "HTTP/1.0", "HTTP/1.1", "HTTP/2.0", "HTTP/2", "HTTP/3.0", "HTTP/3":
		return v
	}
	return ""
}

// clientTLS reports whether the *client's* leg used TLS, and how we know. Behind a
// proxy that terminates TLS the app itself sees plaintext, so r.TLS is nil even
// for a wss:// or https:// client; nginx then says what the client used in
// X-Forwarded-Proto. That header is believed only under -trust-proxy, for the
// same reason X-Forwarded-For is: without a proxy in front it is a client's own
// claim, and a client can claim anything.
func clientTLS(r *http.Request, trustProxy bool) (bool, string) {
	if r.TLS != nil {
		return true, "direct"
	}
	if trustProxy {
		proto := strings.TrimSpace(strings.SplitN(r.Header.Get("X-Forwarded-Proto"), ",", 2)[0])
		if strings.EqualFold(proto, "https") {
			return true, "proxy"
		}
	}
	return false, ""
}

// sniFromProxy accepts a forwarded SNI only if it looks like the one thing SNI can
// be: a hostname. Anything else is a header of unknown provenance being carried
// into a record a human will read, so it is dropped rather than shown.
func sniFromProxy(raw string) string {
	name := strings.TrimSpace(raw)
	if name == "" || len(name) > 253 {
		return ""
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return ""
		}
	}
	return strings.ToLower(name)
}

func remoteFrom(r *http.Request, trustProxy, trustRealIP bool) model.Remote {
	rem := model.Remote{Addr: r.RemoteAddr, IP: r.RemoteAddr}
	if host, port, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		rem.IP = host
		if p, convErr := strconv.Atoi(port); convErr == nil {
			rem.Port = p
		}
	}
	if tls, via := clientTLS(r, trustProxy); tls {
		rem.TLS, rem.TLSVia = true, via
		if r.TLS != nil {
			// The app did the handshake, so the SNI is whatever the client sent.
			rem.TLSSNI = r.TLS.ServerName
		} else {
			// The proxy did it. nginx can forward $ssl_server_name; nothing else
			// about the handshake survives the hop, so an absent header leaves this
			// empty rather than guessing from Host — SNI and Host are not the same
			// thing, and a debugger that confuses them is worse than one that admits
			// it does not know.
			rem.TLSSNI = sniFromProxy(r.Header.Get("X-SSL-Server-Name"))
		}
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		rem.Forwarded = xff
		if trustProxy {
			first := strings.TrimSpace(strings.SplitN(xff, ",", 2)[0])
			if net.ParseIP(first) != nil {
				rem.IP = first
			}
		}
	}
	// X-Real-IP is preferred over X-Forwarded-For when both are offered: nginx
	// writes it from the socket, so a client cannot prepend a hop of its own the
	// way it can with an appended X-Forwarded-For.
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		rem.RealIP = real
		if trustRealIP && net.ParseIP(real) != nil {
			rem.IP = real
		}
	}
	return rem
}

// remoteFromAddr is the net.Addr flavour, for TCP and UDP.
func remoteFromAddr(addr net.Addr) model.Remote {
	rem := model.Remote{Addr: addr.String(), IP: addr.String()}
	if host, port, err := net.SplitHostPort(addr.String()); err == nil {
		rem.IP = host
		if p, convErr := strconv.Atoi(port); convErr == nil {
			rem.Port = p
		}
	}
	return rem
}

// timingFrom measures a request against handler entry and, when the connection
// carried an accept time, against connection accept.
func timingFrom(r *http.Request, start time.Time) model.Timing {
	t := model.Timing{TotalUS: time.Since(start).Microseconds()}
	if accepted, ok := connAcceptedAt(r); ok {
		t.ConnWaitUS = start.Sub(accepted).Microseconds()
	}
	return t
}

// fillBody records the raw body (text when UTF-8, base64 when not) and attaches
// the decoded view.
func fillBody(req *model.Request, contentType string, body []byte) {
	req.BodyBytes = len(body)
	if len(body) == 0 {
		return
	}
	if utf8.Valid(body) {
		text := string(body)
		if len(text) > maxInlineText {
			text = text[:maxInlineText]
		}
		req.BodyText = text
	} else {
		req.BodyBase64 = base64.StdEncoding.EncodeToString(body)
	}
	req.Decoded = decode.Cap(decode.Body(contentType, body), len(body), maxInlineText)
}

// requestEnvelope builds the shared Request from an HTTP request, cloning the
// header map because the record outlives the request.
func (a *App) requestEnvelope(r *http.Request, subpath string) *model.Request {
	headers := make(map[string][]string, len(r.Header))
	for k, v := range r.Header {
		headers[k] = append([]string(nil), v...)
	}
	scheme := "http"
	if tls, _ := clientTLS(r, a.cfg.TrustProxy); tls {
		scheme = "https"
	}
	// The app only ever sees the proxy's hop, which is HTTP/1.1. Under -trust-proxy
	// the client's own version replaces it, and the hop is kept beside it rather
	// than thrown away: "h3 in, 1.1 in here" is the interesting pair.
	version, hop := r.Proto, ""
	if a.cfg.TrustProxy {
		if v := forwardedProtocol(r.Header.Get("X-Forwarded-Protocol")); v != "" && v != r.Proto {
			version, hop = v, r.Proto
		}
	}
	return &model.Request{
		Protocol:    scheme,
		Method:      r.Method,
		Path:        r.URL.Path,
		Subpath:     subpath,
		Query:       r.URL.RawQuery,
		Host:        r.Host,
		HTTPVersion:  version,
		ProxyVersion: hop,
		Headers:     headers,
		ContentLen:  r.ContentLength,
	}
}

// subOrRoot renders an empty subpath as "/".
func subOrRoot(sub string) string {
	if sub == "" {
		return "/"
	}
	return sub
}
