package app

import "github.com/adam/dumpster/internal/model"

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
