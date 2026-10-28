package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/adam/dumpster/internal/model"
)

// SSE is the one protocol where the dumpster is the publisher rather than the
// receiver, which is why it needs two endpoints where the others need one:
//
//	GET  /d/<id>/sse       a client listens; its row stays open while it does
//	POST /d/<id>/sse/emit  the dumpster sends events to whoever is listening
//
// The earlier design had a single endpoint that streamed N events back on
// connect, which conflated the two roles: the client that triggered the stream
// was itself the trigger, so the page button and the curl command were the same
// request and there was no way to listen.

// sseMessage is what an emission publishes through the hub. Listeners share that
// channel with dashboard bookkeeping, so they filter on Type.
type sseMessage struct {
	Type  string `json:"type"` // always "sse"
	ID    int    `json:"id"`
	Event string `json:"event"`
	Data  string `json:"data"`
}

// handleSSEListen holds the connection open and forwards every event the
// dumpster emits, recording each one against the listening session.
func (a *App) handleSSEListen(w http.ResponseWriter, r *http.Request, sid string) {
	release, admitted := a.admitConn(a.clientIP(r))
	if !admitted {
		http.Error(w, "too many open connections from this address", http.StatusTooManyRequests)
		return
	}
	defer release()

	start := time.Now()
	d, ok := a.mustDumpster(w, sid)
	if !ok {
		return
	}

	rec := &model.Record{
		Kind:       model.KindSSE,
		ReceivedAt: start,
		Remote:     remoteFrom(r, a.cfg.TrustProxy, a.cfg.TrustRealIP),
		Req:        requestEnvelope(r, "/sse"),
		Timing:     timingFrom(r, start),
		Meta:       map[string]string{"role": "listener"},
	}
	stored, err := a.store.Add(d.ID, rec)
	if err != nil {
		http.Error(w, "could not record", http.StatusInternalServerError)
		return
	}
	a.publishNew(d.ID, &stored)

	// Subscribe before the first byte goes out: a listener that connects and then
	// misses the opening emission is a bug nobody would think to look for.
	events, cancel := a.hub.Subscribe(d.ID)
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)
	if canFlush {
		flusher.Flush()
	}
	// A comment line attaches the client immediately, so `curl -N` shows something
	// and a human knows the listener is live before anyone presses anything.
	_, _ = io.WriteString(w, ": listening\n\n")
	if canFlush {
		flusher.Flush()
	}

	for {
		select {
		case <-r.Context().Done():
			a.closeRecord(d.ID, stored.Seq, "listener disconnected")
			return
		case payload, open := <-events:
			if !open {
				a.closeRecord(d.ID, stored.Seq, "dumpster emptied")
				return
			}
			var msg sseMessage
			if err := json.Unmarshal(payload, &msg); err != nil || msg.Type != "sse" {
				continue // bookkeeping for the dashboard shares this channel
			}
			frame := fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", msg.ID, msg.Event, msg.Data)
			if _, err := io.WriteString(w, frame); err != nil {
				a.closeRecord(d.ID, stored.Seq, "client write failed")
				return
			}
			if canFlush {
				flusher.Flush()
			}
			if updated, err := a.store.AppendEvent(d.ID, stored.Seq, model.Event{
				At: time.Now(), Dir: "out", Type: msg.Event, Bytes: len(msg.Data), Text: msg.Data,
			}); err == nil {
				a.publishUpdate(d.ID, &updated)
			}
		}
	}
}

// handleSSEEmit pushes `count` events to every listener and records the emission
// as its own row. The row is written even with nobody listening: "fired five
// events into an empty room" is a fact a debugger should not have to guess.
func (a *App) handleSSEEmit(w http.ResponseWriter, r *http.Request, sid string) {
	const maxEvents = 100

	start := time.Now()
	d, ok := a.mustDumpster(w, sid)
	if !ok {
		return
	}

	// Emitting makes the dumpster act, so it is gated like renting: any client may
	// send to a dumpster, but only this page may make one speak.
	if !a.csrfOK(r) {
		a.renderError(w, http.StatusForbidden, "That request did not come from this page",
			"Reload the page and press the button again.")
		return
	}

	count := clampInt(intQuery(r, "count", 5), 1, maxEvents)
	eventName := r.URL.Query().Get("event")
	if eventName == "" {
		eventName = "dump"
	}
	note := r.URL.Query().Get("data")
	listeners := a.hub.Subscribers(d.ID)

	rec := &model.Record{
		Kind:       model.KindSSE,
		ReceivedAt: start,
		Remote:     remoteFrom(r, a.cfg.TrustProxy, a.cfg.TrustRealIP),
		Req:        requestEnvelope(r, "/sse/emit"),
		Timing:     timingFrom(r, start),
		Meta: map[string]string{
			"role":      "emit",
			"count":     strconv.Itoa(count),
			"listeners": strconv.Itoa(listeners),
		},
	}
	stored, err := a.store.Add(d.ID, rec)
	if err != nil {
		http.Error(w, "could not record", http.StatusInternalServerError)
		return
	}
	a.publishNew(d.ID, &stored)

	for i := 1; i <= count; i++ {
		body := map[string]any{
			"dumpster": d.ID,
			"event":    i,
			"of":       count,
			"at":       time.Now().UTC().Format(time.RFC3339Nano),
		}
		if note != "" {
			body["payload"] = note
		}
		data, err := json.Marshal(body)
		if err != nil {
			break
		}
		a.hub.Publish(d.ID, sseMessage{Type: "sse", ID: i, Event: eventName, Data: string(data)})
		if updated, err := a.store.AppendEvent(d.ID, stored.Seq, model.Event{
			At: time.Now(), Dir: "out", Type: eventName, Bytes: len(data), Text: string(data),
		}); err == nil {
			a.publishUpdate(d.ID, &updated)
		}
	}
	a.closeRecord(d.ID, stored.Seq, fmt.Sprintf("emitted %d event(s) to %d listener(s)", count, listeners))

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "emitted": count, "listeners": listeners})
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
