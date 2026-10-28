package app

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/thewalkingtoast/dumpster/internal/model"
)

// An emission with nobody listening must still leave a row: otherwise the page
// shows nothing after a button press and the operator has to guess whether the
// click registered, the request failed, or the dumpster was simply empty.
func TestSSEEmitWithNoListenersRecordsTheAttempt(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	res, raw := emitSSE(t, a, id, "?count=2")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body struct {
		Emitted   int `json:"emitted"`
		Listeners int `json:"listeners"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Emitted != 2 || body.Listeners != 0 {
		t.Fatalf("response = %+v, want 2 emitted to 0 listeners", body)
	}

	recs := records(t, a, id)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1 (the emission itself)", len(recs))
	}
	rec := recs[0]
	if rec.Kind != model.KindSSE {
		t.Fatalf("kind = %q, want sse", rec.Kind)
	}
	if rec.Meta["role"] != "emit" || rec.Meta["listeners"] != "0" {
		t.Fatalf("meta = %+v, want role=emit listeners=0", rec.Meta)
	}
	if len(rec.Events) != 2 {
		t.Fatalf("recorded events = %d, want 2", len(rec.Events))
	}
	if !rec.Closed {
		t.Fatal("an emission is not a session: it should close immediately")
	}
}

// The whole point of the split: a listener holds the stream open, an emission
// arrives later, and the listener sees it.
func TestSSEListenerReceivesEmittedEvents(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	req, err := http.NewRequest(http.MethodGet, a.BaseURL()+"/d/"+id+"/sse", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A plain client: the shared one may carry a timeout, which would cut a
	// connection that is supposed to stay open.
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("GET sse: %v", err)
	}
	defer res.Body.Close()

	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	deadline := time.Now().Add(2 * time.Second)
	for a.hub.Subscribers(id) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("listener never attached")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if postRes, _ := emitSSE(t, a, id, "?count=3&event=poke&data=hi"); postRes.StatusCode != http.StatusOK {
		t.Fatalf("emit status = %d, want 200", postRes.StatusCode)
	}

	// The five welcome events land first now; the publisher's frames are the ones
	// carrying a payload, so skip past the burst.
	sc := bufio.NewScanner(res.Body)
	var data []string
	for len(data) < 3 && sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"payload"`) {
			data = append(data, strings.TrimPrefix(line, "data: "))
		}
	}
	if len(data) != 3 {
		t.Fatalf("listener received %d events, want 3 (scanner err: %v)", len(data), sc.Err())
	}
	if !strings.Contains(data[0], `"event":1`) || !strings.Contains(data[0], `"payload":"hi"`) {
		t.Fatalf("first frame = %s, want event 1 carrying the payload", data[0])
	}

	res.Body.Close() // disconnect the listener

	var listener *model.Record
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for i := range records(t, a, id) {
			rec := records(t, a, id)[i]
			if rec.Meta["role"] == "listener" && rec.Closed && len(rec.Events) == sseWelcomeEvents+3 {
				listener = &rec
			}
		}
		if listener != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if listener == nil {
		t.Fatalf("listener record did not close with %d events: %+v", sseWelcomeEvents+3, records(t, a, id))
	}
	for i, ev := range listener.Events {
		if ev.Dir != "out" {
			t.Fatalf("event %d direction = %q, want out (the dumpster sent it)", i, ev.Dir)
		}
		want := sseDefaultEvent
		if i >= sseWelcomeEvents {
			want = "poke"
		}
		if ev.Type != want {
			t.Fatalf("event %d type = %q, want %q", i, ev.Type, want)
		}
	}
}

// Making a dumpster speak is a state change, so it takes the same gate as
// renting: any client may send to a dumpster, only this page may make one emit.
func TestSSEEmitWithoutTokenIsForbidden(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	res, err := http.Post(a.BaseURL()+"/d/"+id+"/sse/emit?count=2", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST emit: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 without a token", res.StatusCode)
	}
	if n := len(records(t, a, id)); n != 0 {
		t.Fatalf("a forged emission recorded %d row(s)", n)
	}
}

// A quiet stream must stay warm on its own: proxies cut idle connections, and a
// listener that dies while nothing is being emitted looks like a broken tool.
func TestSSEListenerKeepsAnIdleStreamWarm(t *testing.T) {
	a := newTestApp(t)
	a.sseKeepAlive = 20 * time.Millisecond
	id := rent(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL()+"/d/"+id+"/sse", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer res.Body.Close()

	sc := bufio.NewScanner(res.Body)
	keepAlives, events := 0, 0
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, ": keep-alive"):
			keepAlives++
		case strings.HasPrefix(line, "event:"):
			events++
		}
	}
	if keepAlives < 3 {
		t.Fatalf("keep-alives = %d, want at least 3 on an idle stream", keepAlives)
	}
	// Connecting is no longer silent: the welcome burst arrives, then nothing but
	// keep-alives. An idle stream must not invent extra events.
	if events != sseWelcomeEvents {
		t.Fatalf("stream delivered %d events, want only the %d welcome ones", events, sseWelcomeEvents)
	}
}

// Connecting is the whole demonstration now: five events arrive unprompted, and
// each is recorded on the listener's own row.
func TestSSEListenerSendsFiveEventsOnConnect(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL()+"/d/"+id+"/sse", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("GET sse: %v", err)
	}

	sc := bufio.NewScanner(res.Body)
	events := 0
	for events < sseWelcomeEvents && sc.Scan() {
		if strings.HasPrefix(sc.Text(), "event: ") {
			events++
		}
	}
	res.Body.Close()
	if events != sseWelcomeEvents {
		t.Fatalf("received %d events on connect, want %d", events, sseWelcomeEvents)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, rec := range records(t, a, id) {
			if rec.Kind != model.KindSSE {
				continue
			}
			if rec.Meta["role"] != "listener" {
				continue
			}
			if len(rec.Events) >= sseWelcomeEvents {
				for i, ev := range rec.Events[:sseWelcomeEvents] {
					if ev.Dir != "out" || ev.Type != sseDefaultEvent {
						t.Fatalf("event %d = %s/%s, want out/%s", i, ev.Dir, ev.Type, sseDefaultEvent)
					}
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the listener row never recorded %d events", sseWelcomeEvents)
}

// The event name lands in an `event:` line, so a newline in it would let the
// caller forge entire frames — a hijacked type, extra data lines, a fake id.
func TestSSEEventNameCannotForgeFrames(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL()+"/d/"+id+"/sse", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("GET sse: %v", err)
	}
	defer res.Body.Close()

	sc := bufio.NewScanner(res.Body)
	// frame reads one blank-line-terminated block, skipping pure comment frames.
	frame := func() []string {
		for {
			var out []string
			for sc.Scan() {
				line := sc.Text()
				if line == "" {
					if len(out) > 0 {
						break
					}
					continue
				}
				out = append(out, line)
			}
			if len(out) == 0 || strings.HasPrefix(out[0], ":") {
				if len(out) == 0 {
					return nil
				}
				continue
			}
			return out
		}
	}

	for i := 0; i < sseWelcomeEvents; i++ {
		if f := frame(); f == nil {
			t.Fatal("welcome burst ended early")
		}
	}

	forged := "legit\nevent: forged\n\ndata: injected"
	if res, _ := emitSSE(t, a, id, "?count=1&event="+url.QueryEscape(forged)+"&data=hi"); res.StatusCode != http.StatusOK {
		t.Fatalf("emit status = %d, want 200", res.StatusCode)
	}

	f := frame()
	if f == nil {
		t.Fatal("listener received nothing for the emission")
	}
	events, datas := 0, 0
	for _, line := range f {
		if strings.HasPrefix(line, "event: ") {
			events++
			// The words survive as inert text inside the single name; what must
			// not survive is a second line that a client would read as its own field.
			if line == "event: forged" {
				t.Fatalf("event name carried a forged line: %q", f)
			}
		}
		if strings.HasPrefix(line, "data: ") {
			datas++
		}
	}
	if events != 1 || datas != 1 {
		t.Fatalf("frame = %q, want exactly one event: and one data: line", f)
	}

	// The row must agree with what listeners saw.
	for _, rec := range records(t, a, id) {
		if rec.Meta["role"] != "emit" {
			continue
		}
		for _, ev := range rec.Events {
			if strings.ContainsAny(ev.Type, "\r\n") {
				t.Fatalf("recorded event type carries a newline: %q", ev.Type)
			}
		}
	}
}
