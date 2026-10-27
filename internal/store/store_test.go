package store

import (
	"testing"
	"time"

	"github.com/adam/dumpster/internal/model"
)

func TestCreateAndGet(t *testing.T) {
	s := New(Limits{MaxRecords: 3, MaxEvents: 2, MaxDumpsters: 2}, time.Hour)
	d, err := s.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(d.ID) != 36 {
		t.Fatalf("id = %q, want a dashed 36-char uuid", d.ID)
	}
	if got := d.ExpiresAt.Sub(d.CreatedAt); got != time.Hour {
		t.Fatalf("ttl = %v, want 1h", got)
	}
	got, ok := s.Get(d.ID)
	if !ok {
		t.Fatal("Get: fresh dumpster not found")
	}
	if got.ID != d.ID {
		t.Fatalf("Get returned %q, want %q", got.ID, d.ID)
	}
}

func TestMaxDumpsters(t *testing.T) {
	s := New(Limits{MaxDumpsters: 2}, time.Hour)
	if _, err := s.Create(); err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	if _, err := s.Create(); err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	if _, err := s.Create(); err != ErrFull {
		t.Fatalf("Create 3 err = %v, want ErrFull", err)
	}
}

func TestAddAssignsSeqTrimsAndListsNewestFirst(t *testing.T) {
	s := New(Limits{MaxRecords: 3}, time.Hour)
	d, err := s.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := s.Add(d.ID, &model.Record{Kind: model.KindHTTP}); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	recs, ok := s.List(d.ID)
	if !ok {
		t.Fatal("List: not found")
	}
	if len(recs) != 3 {
		t.Fatalf("len = %d, want 3 (MaxRecords)", len(recs))
	}
	if recs[0].Seq != 5 || recs[2].Seq != 3 {
		t.Fatalf("seqs = %d..%d, want 5..3", recs[0].Seq, recs[2].Seq)
	}
	if recs[0].Events == nil {
		t.Fatal("Events is nil; JSON would serialise null instead of []")
	}
	if _, err := s.Add("does-not-exist", &model.Record{}); err != ErrNotFound {
		t.Fatalf("Add on unknown dumpster = %v, want ErrNotFound", err)
	}
}
