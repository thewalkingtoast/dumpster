package redisstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/adam/dumpster/internal/model"
	"github.com/adam/dumpster/internal/store"
)

func newBackend(t *testing.T, tweak ...func(*Options)) (*Backend, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	opts := Options{
		URL:          "redis://" + mr.Addr(),
		TTL:          time.Hour,
		MaxRecords:   3,
		MaxEvents:    2,
		MaxDumpsters: 2,
	}
	for _, f := range tweak {
		f(&opts)
	}
	b, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = b.Shutdown() })
	return b, mr
}

func TestCreateAndGet(t *testing.T) {
	b, _ := newBackend(t)
	d, err := b.Create("10.0.0.1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(d.ID) != 36 {
		t.Fatalf("id = %q", d.ID)
	}
	got, ok := b.Get(d.ID)
	if !ok || got.ID != d.ID {
		t.Fatalf("Get = %+v %v", got, ok)
	}
	if ttl := got.ExpiresAt.Sub(got.CreatedAt); ttl != time.Hour {
		t.Fatalf("ttl = %v, want 1h", ttl)
	}
	if _, ok := b.Get("00000000-0000-7000-8000-000000000000"); ok {
		t.Fatal("unknown id should not resolve")
	}
}

func TestMaxDumpsters(t *testing.T) {
	b, _ := newBackend(t)
	if _, err := b.Create("10.0.0.1"); err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	if _, err := b.Create("10.0.0.1"); err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	if _, err := b.Create("10.0.0.1"); err != store.ErrFull {
		t.Fatalf("Create 3 = %v, want ErrFull", err)
	}
}

func TestAddAssignsSeqTrimsAndListsNewestFirst(t *testing.T) {
	b, _ := newBackend(t)
	d, _ := b.Create("10.0.0.1")
	for i := 0; i < 5; i++ {
		if _, err := b.Add(d.ID, &model.Record{Kind: model.KindHTTP}); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	recs, ok := b.List(d.ID)
	if !ok {
		t.Fatal("List: not found")
	}
	if len(recs) != 3 {
		t.Fatalf("len = %d, want 3 (MaxRecords)", len(recs))
	}
	if recs[0].Seq != 5 || recs[2].Seq != 3 {
		t.Fatalf("seqs = %d..%d, want 5..3", recs[0].Seq, recs[2].Seq)
	}
}

func TestAppendEventCapsAndCountsDrops(t *testing.T) {
	b, _ := newBackend(t)
	d, _ := b.Create("10.0.0.1")
	rec, err := b.Add(d.ID, &model.Record{Kind: model.KindTCP})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := b.AppendEvent(d.ID, rec.Seq, model.Event{Type: "chunk"}); err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}
	}
	recs, _ := b.List(d.ID)
	if len(recs[0].Events) != 2 {
		t.Fatalf("events = %d, want 2", len(recs[0].Events))
	}
	if recs[0].DroppedEvents != 2 {
		t.Fatalf("dropped = %d, want 2", recs[0].DroppedEvents)
	}
}

func TestCloseMarksRecord(t *testing.T) {
	b, _ := newBackend(t)
	d, _ := b.Create("10.0.0.1")
	rec, _ := b.Add(d.ID, &model.Record{Kind: model.KindWS})
	closed, err := b.Close(d.ID, rec.Seq, "closed normally")
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed.Closed || closed.CloseNote != "closed normally" {
		t.Fatalf("closed = %+v", closed)
	}
}

func TestExpiredKeysHidesAndSweeps(t *testing.T) {
	b, mr := newBackend(t, func(o *Options) { o.TTL = time.Minute })
	d, _ := b.Create("10.0.0.1")

	// miniredis only advances TTLs on request: fast-forward past the expiry.
	mr.FastForward(2 * time.Minute)

	if _, ok := b.Get(d.ID); ok {
		t.Fatal("expired dumpster is still visible")
	}
	gone := b.DeleteExpired(time.Now())
	if len(gone) != 1 || gone[0] != d.ID {
		t.Fatalf("DeleteExpired = %v, want [%s]", gone, d.ID)
	}
	if b.Len() != 0 {
		t.Fatalf("Len = %d, want 0 after the sweep", b.Len())
	}
}

func TestHealth(t *testing.T) {
	b, mr := newBackend(t)
	if err := b.Health(); err != nil {
		t.Fatalf("Health: %v", err)
	}
	mr.Close()
	if err := b.Health(); err == nil {
		t.Fatal("Health should fail once the server is gone")
	}
}

// The per-caller cap must behave exactly like the memory store's, including
// refilling as dumpsters expire — the allowance is a sorted set scored by expiry
// so it prunes itself rather than needing a sweeper.
func TestCreateCapsEachCallerAndRefillsAfterExpiry(t *testing.T) {
	b, _ := newBackend(t, func(o *Options) {
		o.MaxPerIP = 1
		o.MaxDumpsters = 0 // isolate the per-caller cap from the global one
		o.TTL = time.Hour
	})
	// anchor to the real clock: miniredis expires keys against wall time, so a
	// clock in the past would make the allowance set vanish before it is read
	clock := time.Now()
	b.now = func() time.Time { return clock }

	if _, err := b.Create("9.9.9.9"); err != nil {
		t.Fatalf("first rent: %v", err)
	}
	if _, err := b.Create("9.9.9.9"); !errors.Is(err, store.ErrFullPerIP) {
		t.Fatalf("second rent from one address = %v, want ErrFullPerIP", err)
	}
	if _, err := b.Create("8.8.8.8"); err != nil {
		t.Fatalf("a different address was blocked: %v", err)
	}

	clock = clock.Add(2 * time.Hour) // the first dumpster has expired
	if _, err := b.Create("9.9.9.9"); err != nil {
		t.Fatalf("expired dumpster still counted against the caller: %v", err)
	}
}

// The Redis backend marshals its own copy, so it needs the same guarantee.
func TestRedisStoredTimesAreUTC(t *testing.T) {
	b, _ := newBackend(t, func(o *Options) { o.MaxDumpsters = 0 })
	// Anchor to now in a non-UTC zone: miniredis expires keys against wall time,
	// so a clock in the past would delete the dumpster before Add could run.
	loc := time.FixedZone("MDT", -6*3600)
	clock := time.Now().In(loc)
	b.now = func() time.Time { return clock }

	d, err := b.Create("1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if d.CreatedAt.Location() != time.UTC {
		t.Fatalf("created_at = %v, want UTC", d.CreatedAt)
	}
	rec, err := b.Add(d.ID, &model.Record{ReceivedAt: clock})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.ReceivedAt.Format(time.RFC3339); !strings.HasSuffix(got, "Z") {
		t.Fatalf("received_at = %q, want Z form", got)
	}
	upd, err := b.AppendEvent(d.ID, rec.Seq, model.Event{At: clock.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if len(upd.Events) != 1 || upd.Events[0].At.Location() != time.UTC {
		t.Fatalf("event time = %v, want UTC", upd.Events)
	}
}
