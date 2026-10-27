package app

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"
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
