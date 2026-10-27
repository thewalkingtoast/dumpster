package app

import (
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.HTTPAddr != ":8080" || c.GRPCAddr != ":8081" || c.TCPAddr != ":8082" || c.UDPAddr != ":8083" {
		t.Fatalf("addrs = %+v", c)
	}
	if c.TTL != 12*time.Hour {
		t.Fatalf("TTL = %v, want 12h", c.TTL)
	}
	if c.MaxBody != 1<<20 {
		t.Fatalf("MaxBody = %d, want 1 MiB", c.MaxBody)
	}
	if c.RentPerHour != 10 || c.MaxRecords != 500 {
		t.Fatalf("limits = %+v", c)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("DUMPSTER_TTL", "90m")
	t.Setenv("DUMPSTER_TRUST_PROXY", "1")
	t.Setenv("DUMPSTER_MAX_BODY", "2048")
	t.Setenv("DUMPSTER_BASE_URL", "https://dumpster.example")

	c := DefaultConfig().FromEnv()
	if c.TTL != 90*time.Minute {
		t.Fatalf("TTL = %v, want 90m", c.TTL)
	}
	if !c.TrustProxy {
		t.Fatal("TrustProxy should be on")
	}
	if c.MaxBody != 2048 {
		t.Fatalf("MaxBody = %d, want 2048", c.MaxBody)
	}
	if c.BaseURL != "https://dumpster.example" {
		t.Fatalf("BaseURL = %q", c.BaseURL)
	}
}

func TestFromEnvIgnoresGarbage(t *testing.T) {
	t.Setenv("DUMPSTER_TTL", "not-a-duration")
	t.Setenv("DUMPSTER_MAX_BODY", "bananas")
	c := DefaultConfig().FromEnv()
	if c.TTL != 12*time.Hour || c.MaxBody != 1<<20 {
		t.Fatalf("garbage env should be ignored, got %+v", c)
	}
}
