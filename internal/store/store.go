// Package store holds dumpsters and their records in memory.
package store

import (
	"errors"
	"sync"
	"time"

	"github.com/adam/dumpster/internal/id"
	"github.com/adam/dumpster/internal/model"
)

var (
	// ErrNotFound means the dumpster never existed, or has expired.
	ErrNotFound = errors.New("dumpster not found")
	// ErrFull means the global dumpster cap is reached.
	ErrFull = errors.New("too many dumpsters")
)

// Limits are the caps that keep an unauthenticated service alive.
type Limits struct {
	MaxRecords   int
	MaxEvents    int
	MaxDumpsters int // 0 = unlimited
}

// Store is a map of dumpsters, each behind its own mutex, so ingestion into one
// dumpster never blocks ingestion into another.
type Store struct {
	limits Limits
	ttl    time.Duration
	now    func() time.Time // overridable in tests

	mu    sync.RWMutex
	dumps map[string]*entry
}

type entry struct {
	mu      sync.Mutex
	dump    model.Dumpster
	records []*model.Record // oldest first; List reverses
	nextSeq uint64
}

// New returns an empty store. Non-positive record/event caps fall back to 500.
func New(limits Limits, ttl time.Duration) *Store {
	if limits.MaxRecords <= 0 {
		limits.MaxRecords = 500
	}
	if limits.MaxEvents <= 0 {
		limits.MaxEvents = 500
	}
	return &Store{
		limits: limits,
		ttl:    ttl,
		now:    time.Now,
		dumps:  make(map[string]*entry),
	}
}

// Create mints a new dumpster and returns it.
func (s *Store) Create() (model.Dumpster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limits.MaxDumpsters > 0 && len(s.dumps) >= s.limits.MaxDumpsters {
		return model.Dumpster{}, ErrFull
	}
	now := s.now()
	for attempt := 0; attempt < 5; attempt++ {
		sid, err := id.New()
		if err != nil {
			return model.Dumpster{}, err
		}
		if _, clash := s.dumps[sid]; clash {
			continue
		}
		d := model.Dumpster{ID: sid, CreatedAt: now, ExpiresAt: now.Add(s.ttl)}
		s.dumps[sid] = &entry{dump: d}
		return d, nil
	}
	return model.Dumpster{}, errors.New("could not mint a unique dumpster id")
}

// Get returns the dumpster if it exists and has not expired.
func (s *Store) Get(sid string) (model.Dumpster, bool) {
	e, ok := s.entry(sid)
	if !ok {
		return model.Dumpster{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.dump.ExpiresAt.After(s.now()) {
		return model.Dumpster{}, false
	}
	return e.dump, true
}

// Len is the number of dumpsters held, including those the reaper has not
// reached yet.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.dumps)
}

func (s *Store) entry(sid string) (*entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.dumps[sid]
	return e, ok
}
