package app

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/adam/dumpster/internal/model"
)

func TestWSIngestEchoesAndRecordsFrames(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL(a, "/d/"+id+"/ws"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if typ != websocket.MessageText || string(data) != "hello" {
		t.Fatalf("echo = %v %q", typ, data)
	}
	if err := c.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		recs := records(t, a, id)
		if len(recs) == 1 && recs[0].Closed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	recs := records(t, a, id)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Kind != model.KindWS {
		t.Fatalf("kind = %q, want ws", rec.Kind)
	}
	if len(rec.Events) != 1 || rec.Events[0].Text != "hello" {
		t.Fatalf("events = %+v, want one 'hello'", rec.Events)
	}
	if !rec.Closed {
		t.Fatal("record should be closed once the socket closes")
	}
}

func TestWSIngestBinaryFrame(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(a, "/d/"+id+"/ws"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, []byte{0x00, 0xff}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	c.CloseNow()

	deadline := time.Now().Add(2 * time.Second)
	var rec model.Record
	for time.Now().Before(deadline) {
		recs := records(t, a, id)
		if len(recs) == 1 && len(recs[0].Events) == 1 {
			rec = recs[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(rec.Events) != 1 || rec.Events[0].Base64 == "" {
		t.Fatalf("binary frame should be base64, got %+v", rec.Events)
	}
	if rec.Events[0].Type != "binary" {
		t.Fatalf("event type = %q, want binary", rec.Events[0].Type)
	}
}
