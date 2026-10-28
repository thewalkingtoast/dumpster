package app

import (
	"encoding/base64"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/adam/dumpster/internal/model"
)

// handleWSIngest accepts any WebSocket client, records every frame it receives,
// and echoes each frame back so the caller can see the socket is alive.
func (a *App) handleWSIngest(w http.ResponseWriter, r *http.Request, sid string) {
	d, ok := a.mustDumpster(w, sid)
	if !ok {
		return
	}

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
		Remote:     remoteFrom(r, a.cfg.TrustProxy),
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
