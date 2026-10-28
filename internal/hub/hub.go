// Package hub fans records out to every debug page watching one dumpster.
package hub

import (
	"context"
	"encoding/json"
	"sync"
)

// Hub keeps one subscriber set per dumpster. Publishing never blocks: a browser
// that falls behind drops messages rather than stalling an ingester.
type Hub struct {
	buf int

	mu     sync.Mutex
	subs   map[string]map[chan []byte]struct{}
	broker Broker
}

// New returns a hub whose subscriber channels buffer `buffer` messages.
func New(buffer int) *Hub {
	if buffer <= 0 {
		buffer = 64
	}
	return &Hub{buf: buffer, subs: make(map[string]map[chan []byte]struct{})}
}

// Subscribe returns a channel of pre-marshalled JSON plus a cancel func that is
// safe to call more than once.
func (h *Hub) Subscribe(dumpsterID string) (<-chan []byte, func()) {
	ch := make(chan []byte, h.buf)
	h.mu.Lock()
	if h.subs[dumpsterID] == nil {
		h.subs[dumpsterID] = make(map[chan []byte]struct{})
	}
	h.subs[dumpsterID][ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			set, ok := h.subs[dumpsterID]
			if !ok {
				return
			}
			if _, live := set[ch]; live {
				delete(set, ch)
				close(ch)
			}
			if len(set) == 0 {
				delete(h.subs, dumpsterID)
			}
		})
	}
	return ch, cancel
}

// Broker carries published messages to other instances.
type Broker interface {
	// Publish sends an already-marshalled payload for one dumpster.
	Publish(ctx context.Context, dumpsterID string, payload []byte) error
	// Run delivers remote payloads to handle until ctx is cancelled.
	Run(ctx context.Context, handle func(dumpsterID string, payload []byte)) error
}

// UseBroker attaches a cross-instance broker. Publishing then goes to the local
// subscribers and, through the broker, to every other instance.
func (h *Hub) UseBroker(b Broker) {
	h.mu.Lock()
	h.broker = b
	h.mu.Unlock()
}

// RunBroker pumps remote messages into the local subscribers.
func (h *Hub) RunBroker(ctx context.Context) error {
	h.mu.Lock()
	b := h.broker
	h.mu.Unlock()
	if b == nil {
		return nil
	}
	return b.Run(ctx, func(dumpsterID string, payload []byte) {
		h.deliver(dumpsterID, payload)
	})
}

// Subscribers reports how many listeners currently hold this dumpster open.
func (h *Hub) Subscribers(dumpsterID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[dumpsterID])
}

// Publish marshals msg once and delivers it locally, then to other instances.
func (h *Hub) Publish(dumpsterID string, msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.deliver(dumpsterID, b)

	h.mu.Lock()
	broker := h.broker
	h.mu.Unlock()
	if broker != nil {
		_ = broker.Publish(context.Background(), dumpsterID, b)
	}
}

// deliver fans out to local subscribers only. Never blocks.
func (h *Hub) deliver(dumpsterID string, payload []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[dumpsterID] {
		select {
		case ch <- payload:
		default: // subscriber is behind; drop rather than block the ingest
		}
	}
}

// Close ends every subscription for dumpsterID (the dumpster was reaped).
// Channels are closed exactly once, under the same lock Publish holds, so a
// send can never land on a closed channel.
func (h *Hub) Close(dumpsterID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.subs[dumpsterID]
	if !ok {
		return
	}
	for ch := range set {
		close(ch)
	}
	delete(h.subs, dumpsterID)
}
