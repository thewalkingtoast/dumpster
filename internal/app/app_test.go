package app

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

// newTestApp starts an App on ephemeral ports and stops it when the test ends.
func newTestApp(t *testing.T, tweak ...func(*Config)) *App {
	t.Helper()
	cfg := DefaultConfig()
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.GRPCAddr = "127.0.0.1:0"
	cfg.TCPAddr = "127.0.0.1:0"
	cfg.UDPAddr = "127.0.0.1:0"
	cfg.ReapEvery = 20 * time.Millisecond
	for _, f := range tweak {
		f(&cfg)
	}
	a := New(cfg)
	if err := a.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := a.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return a
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return res, string(body)
}

func TestHealthz(t *testing.T) {
	a := newTestApp(t)
	res, body := get(t, a.BaseURL()+"/healthz")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if body != "{\"ok\":true}\n" {
		t.Fatalf("body = %q", body)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestStartBindsHTTP(t *testing.T) {
	a := newTestApp(t)
	if a.Addrs().HTTP == "" {
		t.Fatal("no HTTP address recorded")
	}
	if got := a.BaseURL(); got[:7] != "http://" {
		t.Fatalf("BaseURL = %q, want http:// prefix", got)
	}
}

func TestReaperRemovesExpired(t *testing.T) {
	a := newTestApp(t, func(c *Config) {
		c.TTL = 40 * time.Millisecond
		c.ReapEvery = 15 * time.Millisecond
	})
	d, err := a.store.Create()
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a.store.Len() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dumpster %s still in the store after its ttl", d.ID)
}

func TestSecurityHeaders(t *testing.T) {
	a := newTestApp(t)
	res, _ := get(t, a.BaseURL()+"/healthz")
	for header, want := range map[string]string{
		"X-Content-Type-Options":      "nosniff",
		"X-Robots-Tag":                "noindex, nofollow",
		"Access-Control-Allow-Origin": "*",
	} {
		if got := res.Header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
}
