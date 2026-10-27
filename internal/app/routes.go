package app

import (
	"net/http"

	"github.com/adam/dumpster/internal/web"
)

// routes builds the mux. The "/" pattern is the catch-all: the debug page, its
// snapshots and every ingest path land there, dispatched by handleCatchAll.
func (a *App) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("POST /rent", a.handleRent)
	mux.HandleFunc("GET /healthz", a.handleHealth)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(web.Static))))
	mux.HandleFunc("/", a.handleCatchAll)
	return mux
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleCatchAll grows the debug page and ingest dispatch in later tasks; until
// then it is an honest 404.
func (a *App) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
}
