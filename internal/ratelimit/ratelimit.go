// Package ratelimit is a per-key token bucket, refilled continuously.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter allows `burst` actions per `window` for each key. Keys are IPs.
type Limiter struct {
	burst  int
	window time.Duration
	now    func() time.Time // overridable in tests

	mu    sync.Mutex
	state map[string]*bucket
}

type bucket struct {
	remaining float64
	updated   time.Time
}

// New returns a limiter. Non-positive arguments fall back to 1 per hour.
func New(burst int, window time.Duration) *Limiter {
	if burst <= 0 {
		burst = 1
	}
	if window <= 0 {
		window = time.Hour
	}
	return &Limiter{burst: burst, window: window, now: time.Now, state: make(map[string]*bucket)}
}

// Allow consumes one token for key and reports whether it was available.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, seen := l.state[key]
	if !seen {
		l.state[key] = &bucket{remaining: float64(l.burst) - 1, updated: now}
		return true
	}
	refill := float64(now.Sub(b.updated)) / float64(l.window) * float64(l.burst)
	if refill > 0 {
		b.remaining = min(float64(l.burst), b.remaining+refill)
		b.updated = now
	}
	if b.remaining < 1 {
		return false
	}
	b.remaining--
	return true
}

// Prune drops buckets untouched for a whole window, so the map cannot grow
// without bound. Call it from the reaper.
func (l *Limiter) Prune() {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-l.window)
	for k, b := range l.state {
		if b.updated.Before(cutoff) {
			delete(l.state, k)
		}
	}
}
