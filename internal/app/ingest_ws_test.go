package app

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/thewalkingtoast/dumpster/internal/model"
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
	hello, pings := 0, 0
	for _, ev := range rec.Events {
		switch ev.Type {
		case "ping":
			pings++
		case "text":
			if ev.Text != "hello" {
				t.Fatalf("text event = %q, want hello", ev.Text)
			}
			hello++
		}
	}
	if hello != 1 {
		t.Fatalf("events = %+v, want exactly one 'hello' frame (pings: %d)", rec.Events, pings)
	}
	// A socket that carried a frame must not report an empty body either.
	if rec.Req.BodyBytes != 5 {
		t.Fatalf("body_bytes = %d, want 5", rec.Req.BodyBytes)
	}
	if rec.Req.BodyText != "hello" {
		t.Fatalf("body_text = %q, want hello", rec.Req.BodyText)
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
	var frame *model.Event
	for time.Now().Before(deadline) && frame == nil {
		recs := records(t, a, id)
		if len(recs) != 1 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		for i := range recs[0].Events {
			if recs[0].Events[i].Type == "binary" {
				frame = &recs[0].Events[i]
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if frame == nil {
		t.Fatal("no binary event recorded")
	}
	if frame.Base64 == "" {
		t.Fatalf("binary frame should be base64, got %+v", frame)
	}
}

// A socket demonstrates itself: five pings on connect, recorded like frames, so a
// client that sends nothing still sees the round trip work.
func TestWSConnectionSendsFivePings(t *testing.T) {
	a := newTestApp(t)
	a.welcomeEvery = 20 * time.Millisecond // the cadence itself is tested separately
	id := rent(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL(a, "/d/"+id+"/ws"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	// Control frames are handled on the read path: without a reader the client
	// never answers the pings, and nothing surfaces them as messages anyway.
	go func() {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		for _, rec := range records(t, a, id) {
			if rec.Kind != model.KindWS {
				continue
			}
			pings := 0
			for _, ev := range rec.Events {
				if ev.Dir == "out" && ev.Type == "ping" {
					pings++
				}
			}
			if pings >= welcomePings {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("the ws row never recorded %d pings", welcomePings)
}

// Same cadence on the socket side, read off the recorded timestamps: the pings
// are one a second, not five at once.
func TestWelcomePingsLandOnePerSecond(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(a, "/d/"+id+"/ws"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()
	go func() {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}()

	deadline := time.Now().Add(9 * time.Second)
	var pings []time.Time
	for time.Now().Before(deadline) {
		pings = pings[:0]
		for _, rec := range records(t, a, id) {
			if rec.Kind != model.KindWS {
				continue
			}
			for _, ev := range rec.Events {
				if ev.Dir == "out" && ev.Type == "ping" {
					pings = append(pings, ev.At)
				}
			}
		}
		if len(pings) >= 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(pings) < 3 {
		t.Fatalf("only %d pings recorded, want at least 3", len(pings))
	}
	if gap := pings[1].Sub(pings[0]); gap < 700*time.Millisecond || gap > 1600*time.Millisecond {
		t.Fatalf("gap between the first two pings = %v, want about a second", gap)
	}
}

// The frame's opcode decides the event type, but its bytes decide how it is shown:
// a binary frame that happens to be valid UTF-8 is text, and only bytes that are
// not valid UTF-8 need base64. The page's binary example depends on this, so it
// is pinned here rather than left implicit.
func TestBinaryFrameWithValidUTF8IsRecordedAsText(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(a, "/d/"+id+"/ws"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	if err := c.Write(ctx, websocket.MessageBinary, []byte{0x00, 0x01}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatalf("read echo: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, rec := range records(t, a, id) {
			if rec.Kind != model.KindWS {
				continue
			}
			for _, ev := range rec.Events {
				if ev.Type != "binary" {
					continue
				}
				if ev.Base64 != "" {
					t.Fatalf("valid UTF-8 was base64-encoded: %+v", ev)
				}
				if ev.Text == "" {
					t.Fatalf("the frame was recorded with no text and no base64: %+v", ev)
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no binary event recorded")
}
