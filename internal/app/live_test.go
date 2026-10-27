package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func wsURL(a *App, path string) string {
	return "ws" + strings.TrimPrefix(a.BaseURL(), "http") + path
}

func TestLiveSendsSnapshotThenNewRecords(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL(a, "/d/"+id+"/_live"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	var first wsMsg
	if err := wsjson.Read(ctx, c, &first); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if first.Type != "snapshot" {
		t.Fatalf("first message type = %q, want snapshot", first.Type)
	}
	if first.Dumpster == nil || first.Dumpster.ID != id {
		t.Fatalf("snapshot dumpster = %+v", first.Dumpster)
	}
	if len(first.Records) != 0 {
		t.Fatalf("snapshot has %d records, want 0", len(first.Records))
	}

	res, err := http.Post(a.BaseURL()+"/d/"+id+"/hello", "text/plain", strings.NewReader("yo"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	res.Body.Close()

	var next wsMsg
	if err := wsjson.Read(ctx, c, &next); err != nil {
		t.Fatalf("read new: %v", err)
	}
	if next.Type != "new" || next.Record == nil {
		t.Fatalf("got %+v", next)
	}
	if next.Record.Req.BodyText != "yo" {
		t.Fatalf("body = %q, want yo", next.Record.Req.BodyText)
	}
}

func TestLiveSendsExpiredWhenReaped(t *testing.T) {
	a := newTestApp(t, func(c *Config) {
		c.TTL = 400 * time.Millisecond
		c.ReapEvery = 20 * time.Millisecond
	})
	id := rent(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL(a, "/d/"+id+"/_live"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	var snap wsMsg
	if err := wsjson.Read(ctx, c, &snap); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	var got wsMsg
	if err := wsjson.Read(ctx, c, &got); err != nil {
		t.Fatalf("expected an expired message before the socket closed, got %v", err)
	}
	if got.Type != "expired" {
		t.Fatalf("type = %q, want expired", got.Type)
	}
}
