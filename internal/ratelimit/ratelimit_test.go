package ratelimit

import (
	"testing"
	"time"
)

func TestAllowConsumesBurst(t *testing.T) {
	l := New(3, time.Hour)
	for i := 0; i < 3; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("call %d should be allowed", i)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("fourth call should be denied")
	}
	if !l.Allow("5.6.7.8") {
		t.Fatal("a different key must be independent")
	}
}

func TestRefillsOverTime(t *testing.T) {
	l := New(2, time.Hour)
	now := time.Now()
	l.now = func() time.Time { return now }

	if !l.Allow("k") || !l.Allow("k") {
		t.Fatal("burst of 2 should be allowed")
	}
	if l.Allow("k") {
		t.Fatal("third call should be denied")
	}
	now = now.Add(30 * time.Minute) // half a window => one token back
	if !l.Allow("k") {
		t.Fatal("one token should have refilled")
	}
	if l.Allow("k") {
		t.Fatal("only one token should have refilled")
	}
}

func TestPruneDropsIdleBuckets(t *testing.T) {
	l := New(2, time.Minute)
	now := time.Now()
	l.now = func() time.Time { return now }
	l.Allow("k")
	now = now.Add(2 * time.Minute)
	l.Prune()

	l.mu.Lock()
	n := len(l.state)
	l.mu.Unlock()
	if n != 0 {
		t.Fatalf("state = %d buckets, want 0", n)
	}
}
