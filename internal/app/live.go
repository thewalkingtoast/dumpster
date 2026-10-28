package app

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/thewalkingtoast/dumpster/internal/id"
	"github.com/thewalkingtoast/dumpster/internal/model"
)

// wsMsg is the single shape pushed to dashboard sockets and snapshots.
type wsMsg struct {
	Type     string          `json:"type"` // snapshot | new | update | expired
	Record   *model.Record   `json:"record,omitempty"`
	Records  []model.Record  `json:"records,omitempty"`
	Dumpster *model.Dumpster `json:"dumpster,omitempty"`
}

// publishNew tells every open debug page that a row was added.
func (a *App) publishNew(sid string, rec *model.Record) {
	a.hub.Publish(sid, wsMsg{Type: "new", Record: rec})
}

// publishUpdate re-sends a whole record after an event was appended or it was
// closed, so the browser can replace one row without diffing.
func (a *App) publishUpdate(sid string, rec *model.Record) {
	a.hub.Publish(sid, wsMsg{Type: "update", Record: rec})
}

// handleLive streams the dumpster to one browser socket. The client is
// write-only: frames it sends are discarded by CloseRead, which also gives us a
// context cancelled the moment the tab closes.
func (a *App) handleLive(w http.ResponseWriter, r *http.Request, sid string) {
	canonical, err := id.Canonical(sid)
	if err != nil {
		http.Error(w, "not a dumpster url", http.StatusNotFound)
		return
	}
	d, ok := a.store.Get(canonical)
	if !ok {
		http.Error(w, "unknown or expired dumpster", http.StatusNotFound)
		return
	}

	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()

	ctx := c.CloseRead(context.Background())

	// Subscribe before the snapshot: a record that lands in between is sent as
	// "new" and deduplicated by seq in the browser, which is harmless. The
	// reverse order would silently lose it.
	sub, cancel := a.hub.Subscribe(d.ID)
	defer cancel()

	recs, _ := a.store.List(d.ID)
	if err := wsjson.Write(ctx, c, wsMsg{Type: "snapshot", Dumpster: &d, Records: recs}); err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case raw, open := <-sub:
			if !open {
				_ = wsjson.Write(ctx, c, wsMsg{Type: "expired"})
				_ = c.Close(websocket.StatusGoingAway, "dumpster expired")
				return
			}
			if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
				return
			}
		}
	}
}
