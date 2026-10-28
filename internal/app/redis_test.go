package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestRedisBackendServesDumpsters(t *testing.T) {
	mr := miniredis.RunT(t)
	a := newTestApp(t, func(c *Config) { c.RedisURL = "redis://" + mr.Addr() })

	id := rent(t, a)
	res, err := http.Post(a.BaseURL()+"/d/"+id+"/x", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	res.Body.Close()

	if got := len(records(t, a, id)); got != 1 {
		t.Fatalf("records = %d, want 1", got)
	}
	_, body := get(t, a.BaseURL()+"/healthz")
	if body != "{\"ok\":true}\n" {
		t.Fatalf("healthz = %q", body)
	}
}

func TestHealthzReportsADeadBackend(t *testing.T) {
	mr := miniredis.RunT(t)
	a := newTestApp(t, func(c *Config) { c.RedisURL = "redis://" + mr.Addr() })

	res, _ := get(t, a.BaseURL()+"/healthz")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("healthy = %d, want 200", res.StatusCode)
	}

	mr.Close()
	res, body := get(t, a.BaseURL()+"/healthz")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 once redis is gone", res.StatusCode)
	}
	if body == "" {
		t.Fatal("unhealthy body should explain why")
	}
}

func TestBadRedisURLFailsStart(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.RedisURL = "://not-a-url"
	if err := New(cfg).Start(); err == nil {
		t.Fatal("Start should fail on an unparseable redis url")
	}
}

func TestSecondInstanceSeesTrafficFromTheFirst(t *testing.T) {
	mr := miniredis.RunT(t)
	url := "redis://" + mr.Addr()

	a := newTestApp(t, func(c *Config) { c.RedisURL = url; c.InstanceID = "a" })
	b := newTestApp(t, func(c *Config) { c.RedisURL = url; c.InstanceID = "b" })

	id := rent(t, a)

	// The dashboard is open on instance B, which never received the ingest.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(b, "/d/"+id+"/_live"), nil)
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	defer c.CloseNow()

	var snap wsMsg
	if err := wsjson.Read(ctx, c, &snap); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Type != "snapshot" {
		t.Fatalf("first message = %q", snap.Type)
	}

	// Redis Pub/Sub drops messages published before the subscription is
	// established, and B's relay goroutine started inside Start. Let it attach.
	time.Sleep(200 * time.Millisecond)

	// Traffic lands on A.
	res, err := http.Post(a.BaseURL()+"/d/"+id+"/from-a", "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("POST A: %v", err)
	}
	res.Body.Close()

	var next wsMsg
	if err := wsjson.Read(ctx, c, &next); err != nil {
		t.Fatalf("B's socket never saw the record: %v", err)
	}
	if next.Type != "new" || next.Record == nil {
		t.Fatalf("got %+v", next)
	}
	if next.Record.Req.BodyText != "hello" {
		t.Fatalf("body = %q", next.Record.Req.BodyText)
	}

	// And B can read the record back from the shared store.
	recs := records(t, b, id)
	if len(recs) != 1 {
		t.Fatalf("B sees %d records, want 1", len(recs))
	}
}
