package app

import (
	"net/http"
	"time"

	"github.com/adam/dumpster/internal/id"
	"github.com/adam/dumpster/internal/model"
)

// handleHTTPIngest records any request aimed at /d/{id}/… . The endpoint is a
// sink: it always answers 200 with the sequence number it assigned.
func (a *App) handleHTTPIngest(w http.ResponseWriter, r *http.Request, sid, sub string) {
	start := time.Now()
	d, ok := a.mustDumpster(w, sid)
	if !ok {
		return
	}

	// A CORS preflight is still traffic worth dumping, but it has no body and
	// the browser expects 204.
	if r.Method == http.MethodOptions {
		if _, err := a.recordHTTP(d, r, sub, start, nil, false, 0); err != nil {
			a.log.Error("record preflight", "err", err)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	bodyStart := time.Now()
	body, truncated := readBody(r.Body, a.cfg.MaxBody)
	bodyUS := time.Since(bodyStart).Microseconds()

	seq, err := a.recordHTTP(d, r, sub, start, body, truncated, bodyUS)
	if err != nil {
		a.log.Error("record http", "err", err)
		http.Error(w, "could not record", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"dumpster":       d.ID,
		"seq":            seq,
		"received_bytes": len(body),
		"truncated":      truncated,
	})
}

// recordHTTP builds, stores and broadcasts one HTTP record.
func (a *App) recordHTTP(d model.Dumpster, r *http.Request, sub string, start time.Time, body []byte, truncated bool, bodyUS int64) (uint64, error) {
	req := requestEnvelope(r, subOrRoot(sub))
	req.Truncated = truncated
	fillBody(req, r.Header.Get("Content-Type"), body)

	timing := timingFrom(r, start)
	rec := &model.Record{
		Kind:       model.KindHTTP,
		ReceivedAt: start,
		Remote:     remoteFrom(r, a.cfg.TrustProxy),
		Req:        req,
		Timing: model.Timing{
			ConnWaitUS: timing.ConnWaitUS,
			BodyUS:     bodyUS,
			TotalUS:    time.Since(start).Microseconds(),
		},
	}
	stored, err := a.store.Add(d.ID, rec)
	if err != nil {
		return 0, err
	}
	a.publishNew(d.ID, &stored)
	return stored.Seq, nil
}

// mustDumpster resolves an id or writes a plain-text 404. Ingest callers are
// scripts, not browsers, so an HTML error page would be noise.
func (a *App) mustDumpster(w http.ResponseWriter, sid string) (model.Dumpster, bool) {
	canonical, err := id.Canonical(sid)
	if err != nil {
		http.Error(w, "not a dumpster url", http.StatusNotFound)
		return model.Dumpster{}, false
	}
	d, ok := a.store.Get(canonical)
	if !ok {
		http.Error(w, "unknown or expired dumpster", http.StatusNotFound)
		return model.Dumpster{}, false
	}
	return d, true
}
