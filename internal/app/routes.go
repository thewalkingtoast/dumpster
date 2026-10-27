package app

import (
	"net/http"
)

// routes builds the mux. The "/" pattern is a catch-all: everything the mux does
// not claim (the debug page, its snapshots, and all ingest paths) lands there.
func (a *App) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.handleHealth)
	mux.HandleFunc("/", a.handleCatchAll)
	return mux
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleCatchAll will grow the debug page and the ingest dispatch; until then it
// is an honest 404.
func (a *App) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
}
