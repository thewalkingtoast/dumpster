package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/thewalkingtoast/dumpster/internal/model"
)

// handleWSIngest accepts any WebSocket client, records every frame it receives,
// and echoes each frame back so the caller can see the socket is alive.
func (a *App) handleWSIngest(w http.ResponseWriter, r *http.Request, sid string) {
	d, ok := a.mustDumpster(w, sid)
	if !ok {
		return
	}

	release, admitted := a.admitConn(a.clientIP(r))
	if !admitted {
		http.Error(w, "too many open connections from this address", http.StatusTooManyRequests)
		return
	}
	defer release()

	// InsecureSkipVerify disables the origin check: this endpoint exists to be
	// hit by arbitrary clients, including browsers on other origins.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(int64(a.cfg.MaxBody))

	start := time.Now()
	rec := &model.Record{
		Kind:       model.KindWS,
		ReceivedAt: start,
		Remote:     remoteFrom(r, a.cfg.TrustProxy, a.cfg.TrustRealIP),
		Req:        requestEnvelope(r, "/ws"),
		Timing:     timingFrom(r, start),
		Meta:       map[string]string{"subprotocol": c.Subprotocol()},
	}
	stored, err := a.store.Add(d.ID, rec)
	if err != nil {
		_ = c.Close(websocket.StatusInternalError, "could not record")
		return
	}
	a.publishNew(d.ID, &stored)

	// Same problem as gRPC, but longer-lived: a socket can stream for an hour, so
	// the running total is written back on the first frame and refreshed at close.
	var first []byte
	total := 0
	flushBody := func() {
		if total == 0 {
			return
		}
		if updated, err := a.store.PatchReq(d.ID, stored.Seq, func(req *model.Request) {
			req.BodyBytes = total
			req.BodyText, req.BodyBase64 = payloadPreview(first)
		}); err == nil {
			a.publishUpdate(d.ID, &updated)
		}
	}

	ctx := r.Context()
	// Five pings as soon as the socket opens, each recorded like a frame. Ping
	// waits for the pong, and the pong is read inside Read — which is why this
	// runs beside the read loop rather than before it.
	go a.welcomePings(ctx, c, d.ID, stored.Seq)

	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			flushBody()
			a.closeRecord(d.ID, stored.Seq, closeNote(err))
			return
		}
		total += len(data)
		if first == nil {
			first = data
			flushBody()
		}
		ev := model.Event{At: time.Now(), Dir: "in", Type: frameType(typ), Bytes: len(data)}
		if utf8.Valid(data) {
			ev.Text = truncateText(string(data), maxInlineText)
		} else {
			ev.Base64 = base64.StdEncoding.EncodeToString(data)
		}
		if updated, err := a.store.AppendEvent(d.ID, stored.Seq, ev); err == nil {
			a.publishUpdate(d.ID, &updated)
		}

		if err := c.Write(ctx, typ, data); err != nil {
			a.closeRecord(d.ID, stored.Seq, "echo failed")
			return
		}
	}
}

// closeNote turns a websocket read error into something a human can read.
func closeNote(err error) string {
	status := websocket.CloseStatus(err)
	switch {
	case status == websocket.StatusNormalClosure:
		return "closed normally"
	case status == websocket.StatusGoingAway:
		return "client went away"
	case status != -1:
		return "closed: " + status.String()
	default:
		return "read error: " + err.Error()
	}
}

// frameType names a WebSocket message type the way the UI shows it.
func frameType(t websocket.MessageType) string {
	if t == websocket.MessageText {
		return "text"
	}
	return "binary"
}

// truncateText caps a string at max bytes without splitting a rune.
func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for i := max; i > 0 && i > max-4; i-- {
		if utf8.RuneStart(s[i]) {
			return s[:i]
		}
	}
	return s[:max]
}

const (
	welcomePings   = 5
	welcomePingGap = 250 * time.Millisecond
)

// welcomePings proves the socket works in both directions without the client
// sending anything. It is safe beside the read loop: the library allows every
// method to be called concurrently except Read itself.
func (a *App) welcomePings(ctx context.Context, c *websocket.Conn, id string, seq uint64) {
	for i := 1; i <= welcomePings; i++ {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := c.Ping(pingCtx)
		cancel()
		if err != nil {
			return // the peer is gone or pong-less; the read loop will notice
		}
		note := fmt.Sprintf("ping %d of %d", i, welcomePings)
		if updated, err := a.store.AppendEvent(id, seq, model.Event{
			At: time.Now(), Dir: "out", Type: "ping", Bytes: 0, Text: note,
		}); err == nil {
			a.publishUpdate(id, &updated)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(welcomePingGap):
		}
	}
}
