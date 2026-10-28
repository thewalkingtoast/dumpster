package redisstore

import (
	"context"
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
	d, err := b.Create()
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
	if _, err := b.Create(); err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	if _, err := b.Create(); err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	if _, err := b.Create(); err != store.ErrFull {
		t.Fatalf("Create 3 = %v, want ErrFull", err)
	}
}

func TestAddAssignsSeqTrimsAndListsNewestFirst(t *testing.T) {
	b, _ := newBackend(t)
	d, _ := b.Create()
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
	d, _ := b.Create()
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
	d, _ := b.Create()
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
	d, _ := b.Create()

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
