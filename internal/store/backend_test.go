package store

import (
	"testing"
	"time"

	"github.com/thewalkingtoast/dumpster/internal/model"
)

func TestMemoryImplementsBackend(t *testing.T) {
	var b Backend = New(Limits{MaxRecords: 2}, time.Minute)

	d, err := b.Create("10.0.0.1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got, ok := b.Get(d.ID); !ok || got.ID != d.ID {
		t.Fatalf("Get = %+v, %v", got, ok)
	}
	rec, err := b.Add(d.ID, &model.Record{Kind: model.KindHTTP})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := b.AppendEvent(d.ID, rec.Seq, model.Event{Type: "frame"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if _, err := b.Close(d.ID, rec.Seq, "done"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	recs, ok := b.List(d.ID)
	if !ok || len(recs) != 1 {
		t.Fatalf("List = %d records, %v", len(recs), ok)
	}
	if b.Len() != 1 {
		t.Fatalf("Len = %d, want 1", b.Len())
	}
	if err := b.Health(); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if err := b.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
