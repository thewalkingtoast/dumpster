package app

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/thewalkingtoast/dumpster/internal/web"
)

// routes builds the mux. The "/" pattern is the catch-all: the debug page, its
// snapshots and every ingest path land there, dispatched by handleCatchAll.
func (a *App) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("POST /rent", a.handleRent)
	mux.HandleFunc("GET /healthz", a.handleHealth)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(web.Static))))
	// Browsers ask for /favicon.ico whether or not the page links it, and the tab
	// icon is the only branding most people ever see.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		serveEmbedded(w, "favicon.ico", "image/x-icon")
	})
	// A manifest served as octet-stream is ignored by some browsers.
	mux.HandleFunc("GET /site.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		serveEmbedded(w, "site.webmanifest", "application/manifest+json")
	})
	mux.HandleFunc("GET /d/{id}", a.handleDumpsterPage)
	mux.HandleFunc("/", a.handleCatchAll)
	return mux
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if a.store != nil {
		if err := a.store.Health(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"ok":    false,
				"error": err.Error(),
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleCatchAll dispatches everything the mux does not claim. Reserved
// suffixes under a dumpster are added by later phases.
func (a *App) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	sid, sub, ok := splitDumpsterPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if sub == "/sse" {
		a.handleSSEListen(w, r, sid)
		return
	}
	if sub == "/sse/emit" {
		a.handleSSEEmit(w, r, sid)
		return
	}
	if sub == "/ws" {
		a.handleWSIngest(w, r, sid)
		return
	}
	if sub == "/_live" {
		a.handleLive(w, r, sid)
		return
	}
	if sub == "/_records.json" {
		r.SetPathValue("id", sid)
		gzipJSON(a.handleRecordsJSON)(w, r)
		return
	}
	a.handleHTTPIngest(w, r, sid, sub)
}

// splitDumpsterPath turns "/d/<id>/a/b" into ("<id>", "/a/b", true).
func splitDumpsterPath(p string) (sid, sub string, ok bool) {
	if len(p) < 4 || p[:3] != "/d/" {
		return "", "", false
	}
	tail := p[3:]
	if i := strings.IndexByte(tail, '/'); i >= 0 {
		if i == 0 {
			return "", "", false
		}
		return tail[:i], tail[i:], true
	}
	return tail, "", true
}

// serveEmbedded writes one file from the embedded asset set with an explicit
// content type. Icons and the manifest are immutable enough to cache for a day;
// a redesign means renaming the file.
func serveEmbedded(w http.ResponseWriter, name, contentType string) {
	data, err := fs.ReadFile(web.Static, name)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
