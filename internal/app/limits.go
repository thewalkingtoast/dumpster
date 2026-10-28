package app

import (
	"net"
	"sync"
)

// connGate caps how many long-lived connections one caller may hold.
//
// Long-lived connections are the expensive kind: each costs a goroutine and a
// record that lives as long as the connection does. A rate limiter cannot bound
// them — a caller that opens slowly still accumulates — so they need a
// concurrent count as well as a rate.
type connGate struct {
	mu    sync.Mutex
	per   map[string]int
	limit int // 0 disables the cap
}

func newConnGate(limit int) *connGate {
	return &connGate{per: make(map[string]int), limit: limit}
}

// acquire reserves a slot for key, reporting whether one was free.
func (g *connGate) acquire(key string) bool {
	if g == nil || g.limit <= 0 {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.per[key] >= g.limit {
		return false
	}
	g.per[key]++
	return true
}

// release hands the slot back. It is safe to call for a key that never acquired
// one, so callers can defer it unconditionally.
func (g *connGate) release(key string) {
	if g == nil || g.limit <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if n := g.per[key]; n <= 1 {
		delete(g.per, key)
		return
	}
	g.per[key]--
}

// held reports the caller's current connections. Used by tests and diagnostics.
func (g *connGate) held(key string) int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.per[key]
}

// admitConn gates a long-lived connection for the caller, returning the release
// function to defer. A caller over the cap is refused before any record exists.
func (a *App) admitConn(ip string) (func(), bool) {
	if !a.conns.acquire(ip) {
		return func() {}, false
	}
	return func() { a.conns.release(ip) }, true
}

// admitIngest rate-limits one ingested item — a request, a frame, a datagram.
// UDP has no way to refuse, so its caller drops instead.
func (a *App) admitIngest(ip string) bool {
	return a.ingest.Allow(ip)
}

// ipFromAddr is the address of a raw connection, without the port.
func ipFromAddr(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr.String()); err == nil {
		return host
	}
	return addr.String()
}
