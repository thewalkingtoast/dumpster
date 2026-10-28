package store

import (
	"time"

	"github.com/adam/dumpster/internal/model"
)

// Backend is every operation the app needs from a dumpster store. The memory
// implementation lives in this package; the Redis one in internal/redisstore.
//
// Get and List report "not found" as false rather than a separate error, because
// every caller treats a backend failure and an expired dumpster the same way: the
// dumpster is not there. Backend.Health is what distinguishes the two, and
// /healthz reports it.
type Backend interface {
	Create(owner string) (model.Dumpster, error)
	Get(sid string) (model.Dumpster, bool)
	Add(sid string, rec *model.Record) (model.Record, error)
	AppendEvent(sid string, seq uint64, ev model.Event) (model.Record, error)
	Close(sid string, seq uint64, note string) (model.Record, error)
	List(sid string) ([]model.Record, bool)
	DeleteExpired(now time.Time) []string
	Len() int

	// Health reports whether the backend can serve requests right now.
	Health() error
	// Shutdown releases whatever the backend holds.
	Shutdown() error
}

var _ Backend = (*Store)(nil)
