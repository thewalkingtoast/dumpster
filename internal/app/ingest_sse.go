package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/thewalkingtoast/dumpster/internal/model"
)

// SSE is the one protocol where the dumpster speaks first: a listener connects,
// and five events arrive one a second, published to the dumpster rather than
// written to that one connection — so with several listeners open they all watch
// the same five land. Connecting is the trigger; there is no second endpoint to
// call, which is also why the burst is not a request anyone can forge.

// sseDefaultEvent is the event name used when a publisher does not choose one.
const sseDefaultEvent = "dump"

// sseWelcomeEvents is how many events a listener receives the moment it connects.
// Connecting is the demonstration: `curl -N` shows something without a second
// command, which is what the docs promise.
const sseWelcomeEvents = 5

// sseEventName makes a client-supplied event name safe to write into an `event:`
// line. A newline there would let the caller forge whole frames — extra data
// lines, a hijacked id, a different type — so control characters are dropped and
// the result is capped. An empty result falls back to the default name.
func sseEventName(raw string) string {
	clean := strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == 0 || r == 0x0b || r == 0x0c {
			return -1
		}
		return r
	}, raw)
	clean = strings.TrimSpace(clean)
	if r := []rune(clean); len(r) > 64 {
		clean = string(r[:64])
	}
	if clean == "" {
		return sseDefaultEvent
	}
	return clean
}

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

	// Five events, one every a.welcomeEvery, published to the dumpster: with two
	// listeners open, both watch them land. The goroutine outlives this connection
	// on purpose — a listener that hangs up should not cut the demonstration short
	// for everyone else.
	go a.broadcastWelcome(d.ID)

	keepAlive := time.NewTicker(a.sseKeepAlive)
	defer keepAlive.Stop()

	for {
		select {
		case <-keepAlive.C:
			// Valid SSE, invisible to EventSource handlers, and it keeps the
			// connection warm through any proxy that counts idle time.
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				a.closeRecord(d.ID, stored.Seq, "client write failed")
				return
			}
			if canFlush {
				flusher.Flush()
			}
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
			frame := fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", msg.ID, sseEventName(msg.Event), msg.Data)
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

// broadcastWelcome publishes the five opening events to every listener on a
// dumpster. Names are server-generated, so nothing here can be forged into the
// frame; the sanitiser in the write path stays as a belt for future publishers.
func (a *App) broadcastWelcome(id string) {
	for i := 1; i <= sseWelcomeEvents; i++ {
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(a.welcomeEvery):
		}
		body, err := json.Marshal(map[string]any{
			"dumpster": id,
			"event":    i,
			"of":       sseWelcomeEvents,
			"at":       time.Now().UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			return
		}
		a.hub.Publish(id, sseMessage{Type: "sse", ID: i, Event: sseDefaultEvent, Data: string(body)})
	}
}

// closeRecord marks a live record finished and tells every open dashboard.
func (a *App) closeRecord(sid string, seq uint64, note string) {
	if updated, err := a.store.Close(sid, seq, note); err == nil {
		a.publishUpdate(sid, &updated)
	}
}
