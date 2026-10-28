package app

import (
	"net/http"
	"testing"

	"github.com/alicebob/miniredis/v2"
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
