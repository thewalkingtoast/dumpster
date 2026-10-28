// Package store holds dumpsters and their records in memory.
package store

import (
	"errors"
	"sort"
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

	// ErrFullPerIP means this one caller already holds their whole allowance.
	// It is distinct from ErrFull so the page can tell a busy yard from a busy
	// wallet, and answer 429 instead of 503.
	ErrFullPerIP = errors.New("too many dumpsters from this address")
)

// Limits are the caps that keep an unauthenticated service alive.
type Limits struct {
	MaxRecords   int
	MaxEvents    int
	MaxDumpsters int // 0 = unlimited
	MaxPerIP     int // 0 = unlimited
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
	owner   string // renter's ip, for the per-caller cap
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
func (s *Store) Create(owner string) (model.Dumpster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.limits.MaxDumpsters > 0 && len(s.dumps) >= s.limits.MaxDumpsters {
		return model.Dumpster{}, ErrFull
	}
	if s.limits.MaxPerIP > 0 && s.countByOwnerLocked(owner, now) >= s.limits.MaxPerIP {
		return model.Dumpster{}, ErrFullPerIP
	}
	for attempt := 0; attempt < 5; attempt++ {
		sid, err := id.New()
		if err != nil {
			return model.Dumpster{}, err
		}
		if _, clash := s.dumps[sid]; clash {
			continue
		}
		d := model.Dumpster{ID: sid, CreatedAt: now, ExpiresAt: now.Add(s.ttl)}
		s.dumps[sid] = &entry{dump: d, owner: owner}
		return d, nil
	}
	return model.Dumpster{}, errors.New("could not mint a unique dumpster id")
}

// countByOwnerLocked counts the caller's live dumpsters. Expired entries are
// skipped rather than swept: the reaper runs on its own schedule and counting a
// corpse would lock a caller out of a dumpster they could legitimately rent.
func (s *Store) countByOwnerLocked(owner string, now time.Time) int {
	n := 0
	for _, e := range s.dumps {
		if e.owner == owner && e.dump.ExpiresAt.After(now) {
			n++
		}
	}
	return n
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

// Add assigns Seq, stores a copy of rec, trims to MaxRecords, and returns the
// stored record. Events is never nil, so callers can marshal it directly.
func (s *Store) Add(sid string, rec *model.Record) (model.Record, error) {
	e, ok := s.entry(sid)
	if !ok {
		return model.Record{}, ErrNotFound
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.dump.ExpiresAt.After(s.now()) {
		return model.Record{}, ErrNotFound
	}
	e.nextSeq++
	stored := *rec
	stored.Seq = e.nextSeq
	if stored.ReceivedAt.IsZero() {
		stored.ReceivedAt = s.now()
	}
	if stored.Events == nil {
		stored.Events = []model.Event{}
	}
	e.records = append(e.records, &stored)
	if over := len(e.records) - s.limits.MaxRecords; over > 0 {
		e.records = append([]*model.Record(nil), e.records[over:]...)
	}
	return stored, nil
}

// List returns copies of every record, newest first.
func (s *Store) List(sid string) ([]model.Record, bool) {
	e, ok := s.entry(sid)
	if !ok {
		return nil, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]model.Record, 0, len(e.records))
	for i := len(e.records) - 1; i >= 0; i-- {
		out = append(out, *e.records[i])
	}
	return out, true
}

// AppendEvent appends one event to the record with this Seq and returns the
// updated record. Once MaxEvents is reached the event is dropped and counted,
// so the UI can show the loss instead of pretending it never happened.
func (s *Store) AppendEvent(sid string, seq uint64, ev model.Event) (model.Record, error) {
	e, ok := s.entry(sid)
	if !ok {
		return model.Record{}, ErrNotFound
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.find(seq)
	if r == nil {
		return model.Record{}, ErrNotFound
	}
	if len(r.Events) >= s.limits.MaxEvents {
		r.DroppedEvents++
		return *r, nil
	}
	r.Events = append(r.Events, ev)
	return *r, nil
}

// Close marks the record finished and records why.
func (s *Store) Close(sid string, seq uint64, note string) (model.Record, error) {
	e, ok := s.entry(sid)
	if !ok {
		return model.Record{}, ErrNotFound
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.find(seq)
	if r == nil {
		return model.Record{}, ErrNotFound
	}
	r.Closed = true
	r.CloseNote = note
	return *r, nil
}

// find scans newest-first, which is where live protocols look.
func (e *entry) find(seq uint64) *model.Record {
	for i := len(e.records) - 1; i >= 0; i-- {
		if e.records[i].Seq == seq {
			return e.records[i]
		}
	}
	return nil
}

// DeleteExpired removes every dumpster whose TTL has passed and returns the
// removed ids, sorted, so the caller can close each one's subscribers.
func (s *Store) DeleteExpired(now time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var gone []string
	for sid, e := range s.dumps {
		e.mu.Lock()
		expired := !e.dump.ExpiresAt.After(now)
		e.mu.Unlock()
		if expired {
			delete(s.dumps, sid)
			gone = append(gone, sid)
		}
	}
	sort.Strings(gone)
	return gone
}

// Health always succeeds: the memory store cannot be unreachable.
func (s *Store) Health() error { return nil }

// Shutdown drops every dumpster. Nothing else holds a reference to them.
func (s *Store) Shutdown() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dumps = make(map[string]*entry)
	return nil
}
