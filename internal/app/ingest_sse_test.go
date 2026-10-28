package app

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/thewalkingtoast/dumpster/internal/model"
)

// A quiet stream must stay warm on its own: proxies cut idle connections, and a
// listener that dies while nothing is being emitted looks like a broken tool.
func TestSSEListenerKeepsAnIdleStreamWarm(t *testing.T) {
	a := newTestApp(t)
	a.sseKeepAlive = 20 * time.Millisecond
	a.welcomeEvery = 20 * time.Millisecond // the cadence itself is tested separately
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
	a.welcomeEvery = 20 * time.Millisecond // the cadence itself is tested separately
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

// The burst is paced, not dumped: a debugger watching a terminal should see the
// events arrive. Measured rather than assumed, because a missing sleep is exactly
// the kind of thing that passes every content check.
func TestWelcomeBurstLandsOnePerSecond(t *testing.T) {
	a := newTestApp(t) // default cadence: one second
	id := rent(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL()+"/d/"+id+"/sse", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("GET sse: %v", err)
	}
	defer res.Body.Close()

	sc := bufio.NewScanner(res.Body)
	var at []time.Duration
	for len(at) < 2 && sc.Scan() {
		if strings.HasPrefix(sc.Text(), "event: ") {
			at = append(at, time.Since(start))
		}
	}
	if len(at) < 2 {
		t.Fatalf("only %d of the first two events arrived", len(at))
	}
	if at[0] < 700*time.Millisecond {
		t.Fatalf("first event arrived after %v; it should wait about a second", at[0])
	}
	if gap := at[1] - at[0]; gap < 700*time.Millisecond || gap > 1600*time.Millisecond {
		t.Fatalf("gap between the first two events = %v, want about a second", gap)
	}
}

// The event name lands in an `event:` line. Nothing sets it from a request any
// more, but the sanitiser stays in the write path as a belt for future
// publishers — so it is tested directly rather than through an endpoint.
func TestSSEEventNameIsSanitised(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"dump", "dump"},
		{"a\nb", "ab"},
		{"a\r\nb", "ab"},
		{"  spaced  ", "spaced"},
		{"", sseDefaultEvent},
		{"\n\n", sseDefaultEvent},
		{strings.Repeat("x", 200), strings.Repeat("x", 64)},
	} {
		if got := sseEventName(tc.in); got != tc.want {
			t.Fatalf("sseEventName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Connecting is the trigger and the burst belongs to the dumpster: a listener
// that was already open must see the five events a later connection sets off.
func TestBurstReachesEveryListener(t *testing.T) {
	a := newTestApp(t)
	a.welcomeEvery = 40 * time.Millisecond
	id := rent(t, a)

	open := func() (*http.Response, *bufio.Scanner) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL()+"/d/"+id+"/sse", nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatalf("GET sse: %v", err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res, bufio.NewScanner(res.Body)
	}
	readEvents := func(sc *bufio.Scanner, want int) int {
		n := 0
		for n < want && sc.Scan() {
			if strings.HasPrefix(sc.Text(), "event: ") {
				n++
			}
		}
		return n
	}

	// The first listener takes its own five...
	_, first := open()
	if n := readEvents(first, sseWelcomeEvents); n != sseWelcomeEvents {
		t.Fatalf("first listener got %d of its own %d events", n, sseWelcomeEvents)
	}

	// ...then a second connects, and the first must watch that burst too.
	_, second := open()
	if n := readEvents(second, sseWelcomeEvents); n != sseWelcomeEvents {
		t.Fatalf("second listener got %d of %d events", n, sseWelcomeEvents)
	}
	if n := readEvents(first, sseWelcomeEvents); n != sseWelcomeEvents {
		t.Fatalf("the already-open listener saw %d of the second connection's %d events", n, sseWelcomeEvents)
	}
}

// The endpoint is gone, but "gone" must not mean "silently behaves like ingest":
// a stale client would get a 200 and wonder why nothing arrived.
func TestRemovedEmitEndpointSaysWhatReplacedIt(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	for _, method := range []string{http.MethodPost, http.MethodGet} {
		req, err := http.NewRequest(method, a.BaseURL()+"/d/"+id+"/sse/emit?count=1", nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := noRedirect.Do(req)
		if err != nil {
			t.Fatalf("%s emit: %v", method, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()

		if res.StatusCode != http.StatusGone {
			t.Fatalf("%s /sse/emit status = %d, want 410", method, res.StatusCode)
		}
		if !strings.Contains(string(body), "/sse") {
			t.Fatalf("%s /sse/emit does not point at its replacement: %s", method, body)
		}
	}
	if n := len(records(t, a, id)); n != 0 {
		t.Fatalf("a call to the removed endpoint recorded %d row(s)", n)
	}
}
