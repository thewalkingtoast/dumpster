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

	"github.com/adam/dumpster/internal/decode"
	"github.com/adam/dumpster/internal/model"
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
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "*")
		h.Set("Access-Control-Allow-Headers", "*")
		h.Set("Access-Control-Expose-Headers", "*")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; "+
				"script-src 'self'; connect-src 'self' ws: wss:; base-uri 'none'; form-action 'self'")
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

// maxInlineText caps how much body text is inlined into a record; the JSON
// snapshot would otherwise carry megabytes.
const maxInlineText = 64 << 10

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
func remoteFrom(r *http.Request, trustProxy bool) model.Remote {
	rem := model.Remote{Addr: r.RemoteAddr, IP: r.RemoteAddr}
	if host, port, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		rem.IP = host
		if p, convErr := strconv.Atoi(port); convErr == nil {
			rem.Port = p
		}
	}
	if r.TLS != nil {
		rem.TLS = true
		rem.TLSSNI = r.TLS.ServerName
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
func requestEnvelope(r *http.Request, subpath string) *model.Request {
	headers := make(map[string][]string, len(r.Header))
	for k, v := range r.Header {
		headers[k] = append([]string(nil), v...)
	}
	return &model.Request{
		Protocol:    "http",
		Method:      r.Method,
		Path:        r.URL.Path,
		Subpath:     subpath,
		Query:       r.URL.RawQuery,
		Host:        r.Host,
		HTTPVersion: r.Proto,
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
