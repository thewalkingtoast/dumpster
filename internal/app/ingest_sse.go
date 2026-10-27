package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/adam/dumpster/internal/model"
)

// handleSSEIngest streams `count` server-sent events back to the caller and
// records the connection plus each event it emitted, so the dumpster shows the
// stream the way the client saw it.
func (a *App) handleSSEIngest(w http.ResponseWriter, r *http.Request, sid string) {
	start := time.Now()
	d, ok := a.mustDumpster(w, sid)
	if !ok {
		return
	}

	count := clampInt(intQuery(r, "count", 5), 1, 100)
	interval := time.Duration(clampInt(intQuery(r, "interval_ms", 1000), 0, 60000)) * time.Millisecond
	eventName := r.URL.Query().Get("event")
	if eventName == "" {
		eventName = "dump"
	}

	rec := &model.Record{
		Kind:       model.KindSSE,
		ReceivedAt: start,
		Remote:     remoteFrom(r, a.cfg.TrustProxy),
		Req:        requestEnvelope(r, "/sse"),
		Timing:     timingFrom(r, start),
	}
	stored, err := a.store.Add(d.ID, rec)
	if err != nil {
		http.Error(w, "could not record", http.StatusInternalServerError)
		return
	}
	a.publishNew(d.ID, &stored)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)
	if canFlush {
		flusher.Flush()
	}

	for i := 1; i <= count; i++ {
		payload, err := json.Marshal(map[string]any{
			"dumpster": d.ID,
			"seq":      stored.Seq,
			"event":    i,
			"of":       count,
			"at":       time.Now().UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			a.closeRecord(d.ID, stored.Seq, "marshal failed")
			return
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", i, eventName, payload); err != nil {
			a.closeRecord(d.ID, stored.Seq, "client write failed")
			return
		}
		if canFlush {
			flusher.Flush()
		}
		if updated, err := a.store.AppendEvent(d.ID, stored.Seq, model.Event{
			At: time.Now(), Dir: "out", Type: "event", Bytes: len(payload), Text: string(payload),
		}); err == nil {
			a.publishUpdate(d.ID, &updated)
		}

		if i == count {
			break
		}
		select {
		case <-r.Context().Done():
			a.closeRecord(d.ID, stored.Seq, "client disconnected")
			return
		case <-time.After(interval):
		}
	}
	a.closeRecord(d.ID, stored.Seq, "stream finished")
}

// intQuery reads an integer query parameter, falling back to def.
func intQuery(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// clampInt keeps a caller-supplied number inside sane bounds.
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// closeRecord marks a live record finished and tells every open dashboard.
func (a *App) closeRecord(sid string, seq uint64, note string) {
	if updated, err := a.store.Close(sid, seq, note); err == nil {
		a.publishUpdate(sid, &updated)
	}
}
