package store

import (
	"testing"
	"time"
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
