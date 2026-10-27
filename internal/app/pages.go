package app

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/adam/dumpster/internal/store"
	"github.com/adam/dumpster/internal/web"
)

// render executes a template into a buffer first, so a template error cannot
// emit half a page with a 200 already on the wire.
func (a *App) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := web.Templates.ExecuteTemplate(&buf, name, data); err != nil {
		a.log.Error("render template", "template", name, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// renderError is the one HTML error page.
func (a *App) renderError(w http.ResponseWriter, status int, title, message string) {
	a.render(w, status, "error.html", map[string]any{"Title": title, "Message": message})
}

// handleIndex renders the landing page: one heading, one CTA, nothing else.
func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	a.render(w, http.StatusOK, "index.html", map[string]any{
		"Active": a.store.Len(),
		"TTL":    humanDuration(a.cfg.TTL),
	})
}

// handleRent is the single call to action: mint, then redirect to the dumpster.
func (a *App) handleRent(w http.ResponseWriter, r *http.Request) {
	if !a.rl.Allow(a.clientIP(r)) {
		w.Header().Set("Retry-After", "600")
		a.renderError(w, http.StatusTooManyRequests, "Slow down",
			"You have rented too many dumpsters this hour. They expire on their own; try again shortly.")
		return
	}
	d, err := a.store.Create()
	switch {
	case errors.Is(err, store.ErrFull):
		a.renderError(w, http.StatusServiceUnavailable, "The yard is full",
			"Every dumpster is currently rented. They empty themselves after "+humanDuration(a.cfg.TTL)+".")
	case err != nil:
		a.log.Error("create dumpster", "err", err)
		a.renderError(w, http.StatusInternalServerError, "Could not rent", "Something broke in the yard.")
	default:
		http.Redirect(w, r, "/d/"+d.ID, http.StatusSeeOther)
	}
}

// humanDuration renders 12h0m0s as "12h", and 500ms as "500ms".
func humanDuration(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return d.String()
}

// clientIP is the address used for rate limiting.
func (a *App) clientIP(r *http.Request) string {
	return remoteFrom(r, a.cfg.TrustProxy).IP
}
