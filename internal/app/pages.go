package app

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/thewalkingtoast/dumpster/internal/id"
	"github.com/thewalkingtoast/dumpster/internal/store"
	"github.com/thewalkingtoast/dumpster/internal/web"
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
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	// Every page here embeds a CSRF token minted by this process. Cached, it is a
	// form that fails with "that request did not come from this page" and no
	// explanation — so the page is never stored.
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// renderError is the one HTML error page.
func (a *App) renderError(w http.ResponseWriter, status int, title, message string) {
	// A rate-limited caller cannot rent, so the page must not offer the form:
	// pressing it would land them on this exact page again. Every other error
	// states a reason renting would fix, so the CTA belongs there.
	a.render(w, status, "error.html", map[string]any{
		"Title":   title,
		"Message": message,
		"Rent":    status != http.StatusTooManyRequests,
		"CSRF":    a.csrf.Token(),
	})
}

// handleIndex renders the landing page: one heading, one CTA, nothing else.
func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	a.render(w, http.StatusOK, "index.html", map[string]any{
		"Active": a.store.Len(),
		"TTL":    humanDuration(a.cfg.TTL),
		"CSRF":   a.csrf.Token(),
	})
}

// handleRent is the single call to action: mint, then redirect to the dumpster.
func (a *App) handleRent(w http.ResponseWriter, r *http.Request) {
	// Checked before the rate limiter: a forged request must not spend the
	// caller's allowance any more than it should mint a dumpster.
	if !a.csrfOK(r) {
		a.renderError(w, http.StatusForbidden, "That request did not come from this page",
			"Reload the page and press the button again.")
		return
	}
	if !a.rl.Allow(a.clientIP(r)) {
		w.Header().Set("Retry-After", "600")
		a.renderError(w, http.StatusTooManyRequests, "Slow down",
			"You have rented too many dumpsters this hour. They expire on their own; try again shortly.")
		return
	}
	d, err := a.store.Create(a.clientIP(r))
	switch {
	case errors.Is(err, store.ErrFullPerIP):
		w.Header().Set("Retry-After", "600")
		a.renderError(w, http.StatusTooManyRequests, "Your yard is full",
			"This address already holds "+plural(a.cfg.MaxPerIP, "dumpster")+". They expire after "+humanDuration(a.cfg.TTL)+".")
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

// plural renders 1 dumpster / 2 dumpsters.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// humanDuration renders 3h0m0s as "3h", and 500ms as "500ms".
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
	return remoteFrom(r, a.cfg.TrustProxy, a.cfg.TrustRealIP).IP
}

// handleDumpsterPage renders the shell; the JS fills the list from
// /_records.json and then from the live socket.
func (a *App) handleDumpsterPage(w http.ResponseWriter, r *http.Request) {
	canonical, err := id.Canonical(r.PathValue("id"))
	if err != nil {
		a.renderError(w, http.StatusNotFound, "Not a dumpster", "That link is not a dumpster at all.")
		return
	}
	d, ok := a.store.Get(canonical)
	if !ok {
		a.renderError(w, http.StatusNotFound, "This dumpster is gone",
			"It was emptied after "+humanDuration(a.cfg.TTL)+". Rent a new one and keep dumping.")
		return
	}
	a.render(w, http.StatusOK, "debug.html", map[string]any{
		"ID":         d.ID,
		"Origin":     a.BaseURL(),
		"ExpiresAt":  d.ExpiresAt.UTC().Format(time.RFC3339),
		"PublicGRPC": a.publicAddr(a.cfg.PublicGRPC, a.Addrs().GRPC),
		"TLS":        a.cfg.TLS,
		"PublicTCP":  a.publicAddr(a.cfg.PublicTCP, a.Addrs().TCP),
		"PublicUDP":  a.publicAddr(a.cfg.PublicUDP, a.Addrs().UDP),
		"CSRF":       a.csrf.Token(),
	})
}

// handleRecordsJSON returns the snapshot the page loads before the socket opens,
// and the thing to curl when you want to read a dumpster from a script.
func (a *App) handleRecordsJSON(w http.ResponseWriter, r *http.Request) {
	canonical, err := id.Canonical(r.PathValue("id"))
	if err != nil {
		http.Error(w, "not a dumpster url", http.StatusNotFound)
		return
	}
	d, ok := a.store.Get(canonical)
	if !ok {
		http.Error(w, "unknown or expired dumpster", http.StatusNotFound)
		return
	}
	recs, _ := a.store.List(d.ID)
	writeJSON(w, http.StatusOK, wsMsg{Type: "snapshot", Dumpster: &d, Records: recs})
}

// publicAddr renders the address a human types into nc or grpcurl: the explicit
// override if configured, otherwise the BaseURL host with the bound port.
func (a *App) publicAddr(explicit, bound string) string {
	if explicit != "" {
		return explicit
	}
	u, err := url.Parse(a.BaseURL())
	if err != nil || u.Hostname() == "" {
		return bound
	}
	_, port, err := net.SplitHostPort(bound)
	if err != nil || port == "" {
		return bound
	}
	return net.JoinHostPort(u.Hostname(), port)
}
