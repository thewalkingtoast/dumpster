package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/adam/dumpster/internal/model"
)

func TestCreateAndGet(t *testing.T) {
	s := New(Limits{MaxRecords: 3, MaxEvents: 2, MaxDumpsters: 2}, time.Hour)
	d, err := s.Create("10.0.0.1")
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
	if _, err := s.Create("10.0.0.1"); err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	if _, err := s.Create("10.0.0.1"); err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	if _, err := s.Create("10.0.0.1"); err != ErrFull {
		t.Fatalf("Create 3 err = %v, want ErrFull", err)
	}
}

func TestAddAssignsSeqTrimsAndListsNewestFirst(t *testing.T) {
	s := New(Limits{MaxRecords: 3}, time.Hour)
	d, err := s.Create("10.0.0.1")
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

func TestAppendEventCapsAndCountsDrops(t *testing.T) {
	s := New(Limits{MaxRecords: 5, MaxEvents: 2}, time.Hour)
	d, _ := s.Create("10.0.0.1")
	rec, err := s.Add(d.ID, &model.Record{Kind: model.KindTCP})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := s.AppendEvent(d.ID, rec.Seq, model.Event{Type: "chunk"}); err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}
	}
	recs, _ := s.List(d.ID)
	if len(recs[0].Events) != 2 {
		t.Fatalf("events = %d, want 2 (MaxEvents)", len(recs[0].Events))
	}
	if recs[0].DroppedEvents != 2 {
		t.Fatalf("dropped = %d, want 2", recs[0].DroppedEvents)
	}
}

func TestCloseMarksRecord(t *testing.T) {
	s := New(Limits{}, time.Hour)
	d, _ := s.Create("10.0.0.1")
	rec, _ := s.Add(d.ID, &model.Record{Kind: model.KindWS})
	closed, err := s.Close(d.ID, rec.Seq, "closed by peer")
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed.Closed || closed.CloseNote != "closed by peer" {
		t.Fatalf("closed = %+v", closed)
	}
}

func TestExpiryHidesAndDeletes(t *testing.T) {
	s := New(Limits{MaxRecords: 3}, 50*time.Millisecond)
	d, err := s.Create("10.0.0.1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, ok := s.Get(d.ID); !ok {
		t.Fatal("fresh dumpster should be visible")
	}
	time.Sleep(70 * time.Millisecond)
	if _, ok := s.Get(d.ID); ok {
		t.Fatal("expired dumpster is still visible")
	}
	if _, err := s.Add(d.ID, &model.Record{}); err != ErrNotFound {
		t.Fatalf("Add on expired = %v, want ErrNotFound", err)
	}
	gone := s.DeleteExpired(time.Now())
	if len(gone) != 1 || gone[0] != d.ID {
		t.Fatalf("DeleteExpired = %v, want [%s]", gone, d.ID)
	}
	if s.Len() != 0 {
		t.Fatalf("Len = %d, want 0", s.Len())
	}
	if len(s.DeleteExpired(time.Now())) != 0 {
		t.Fatal("second DeleteExpired should find nothing")
	}
}

// The cap is per caller: one noisy address must not lock out another, and the
// allowance must refill as old dumpsters expire rather than needing a reaper run.
func TestCreateCapsEachCallerAtTheirOwnAllowance(t *testing.T) {
	s := New(Limits{MaxPerIP: 2}, time.Hour)
	for i := 0; i < 2; i++ {
		if _, err := s.Create("1.1.1.1"); err != nil {
			t.Fatalf("rent %d: %v", i+1, err)
		}
	}
	if _, err := s.Create("1.1.1.1"); !errors.Is(err, ErrFullPerIP) {
		t.Fatalf("third rent from one address = %v, want ErrFullPerIP", err)
	}
	if _, err := s.Create("2.2.2.2"); err != nil {
		t.Fatalf("a different address was blocked: %v", err)
	}
}

func TestExpiredDumpstersDoNotCountAgainstTheirCaller(t *testing.T) {
	s := New(Limits{MaxPerIP: 1}, time.Hour)
	now := time.Unix(0, 0)
	s.now = func() time.Time { return now }

	if _, err := s.Create("1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("1.1.1.1"); !errors.Is(err, ErrFullPerIP) {
		t.Fatalf("second rent = %v, want ErrFullPerIP", err)
	}

	now = now.Add(2 * time.Hour) // the first dumpster is now a corpse
	if _, err := s.Create("1.1.1.1"); err != nil {
		t.Fatalf("expired dumpster still counted against the caller: %v", err)
	}
}

// A single dumpster must not be able to grow without bound: once it passes its
// byte allowance the oldest records go, exactly like the record-count trim.
func TestPerDumpsterByteBudgetEvictsOldest(t *testing.T) {
	s := New(Limits{MaxRecords: 100, MaxBytesPerDumpster: 8 << 10}, time.Hour)
	d, err := s.Create("10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		rec := &model.Record{
			Kind: model.KindHTTP,
			Req:  &model.Request{BodyText: strings.Repeat("x", 1024), BodyBytes: 1024},
		}
		if _, err := s.Add(d.ID, rec); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	recs, ok := s.List(d.ID)
	if !ok {
		t.Fatal("dumpster vanished")
	}
	if len(recs) >= 20 {
		t.Fatalf("byte budget never evicted: %d records retained", len(recs))
	}
	if recs[0].Seq != 20 {
		t.Fatalf("newest record has seq %d, want 20 — eviction removed the wrong end", recs[0].Seq)
	}
}

// Hitting the whole-board budget must degrade a record, not reject it: a debug
// tool that refuses traffic is useless exactly when something is going wrong.
func TestGlobalByteBudgetStripsBodiesInsteadOfRefusing(t *testing.T) {
	s := New(Limits{MaxRecords: 100, MaxBytes: 8 << 10}, time.Hour)
	fat := func() *model.Record {
		return &model.Record{
			Kind: model.KindHTTP,
			Req:  &model.Request{BodyText: strings.Repeat("y", 4096), BodyBytes: 4096},
		}
	}
	first, _ := s.Create("10.0.0.1")
	second, _ := s.Create("10.0.0.2")
	if _, err := s.Add(first.ID, fat()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(first.ID, fat()); err != nil {
		t.Fatal(err)
	}

	rec, err := s.Add(second.ID, fat())
	if err != nil {
		t.Fatalf("an add over the budget must still be recorded: %v", err)
	}
	if !rec.Req.BodySkipped {
		t.Fatal("body was stored after the board budget filled")
	}
	if rec.Req.BodyText != "" {
		t.Fatal("body text retained despite the budget")
	}
	if rec.Req.BodyBytes != 4096 {
		t.Fatalf("body_bytes = %d; the page must still report how much arrived", rec.Req.BodyBytes)
	}
}

// Accounting has to be symmetric: expired dumpsters must hand their bytes back,
// or the board budget would ratchet shut over time.
func TestByteAccountingIsReleasedOnExpiry(t *testing.T) {
	s := New(Limits{MaxRecords: 100, MaxBytes: 1 << 20}, time.Hour)
	now := time.Unix(0, 0)
	s.now = func() time.Time { return now }

	d, _ := s.Create("10.0.0.1")
	s.Add(d.ID, &model.Record{Kind: model.KindHTTP, Req: &model.Request{BodyText: strings.Repeat("z", 8192)}})
	if s.bytes == 0 {
		t.Fatal("no bytes were accounted")
	}
	now = now.Add(2 * time.Hour)
	s.DeleteExpired(now)
	if s.bytes != 0 {
		t.Fatalf("bytes after expiry = %d, want 0", s.bytes)
	}
}

// A structured decoded value (a JSON object) costs about as much as the body it
// came from. Counting it as a constant made the budget under-report by tens of
// kilobytes per record, so the global ceiling never tripped.
func TestRecordSizeCountsStructuredDecodedValues(t *testing.T) {
	const body = 40000
	r := &model.Record{Req: &model.Request{
		BodyBytes: body,
		Decoded:   &model.Decoded{Format: "json", Value: map[string]any{"sku": "A1"}},
	}}
	if got := recordSize(r); got < body {
		t.Fatalf("recordSize = %d, want at least %d", got, body)
	}
}

// Streaming protocols create the record before any payload exists, so the size
// has to be written back afterwards — and the byte accounting has to follow it.
func TestPatchReqWritesPayloadBackAndAdjustsAccounting(t *testing.T) {
	s := New(Limits{MaxRecords: 10, MaxBytes: 1 << 20}, time.Hour)
	d, _ := s.Create("10.0.0.1")
	rec, err := s.Add(d.ID, &model.Record{Kind: model.KindGRPC, Req: &model.Request{Protocol: "grpc"}})
	if err != nil {
		t.Fatal(err)
	}
	before := s.bytes

	updated, err := s.PatchReq(d.ID, rec.Seq, func(req *model.Request) {
		req.BodyBytes = 4096
		req.BodyText = strings.Repeat("p", 4096)
	})
	if err != nil {
		t.Fatalf("PatchReq: %v", err)
	}
	if updated.Req.BodyBytes != 4096 || len(updated.Req.BodyText) != 4096 {
		t.Fatalf("payload not written back: %+v", updated.Req)
	}
	if s.bytes <= before {
		t.Fatalf("accounting did not follow the new payload: %d -> %d", before, s.bytes)
	}

	recs, _ := s.List(d.ID)
	if recs[0].Req.BodyBytes != 4096 {
		t.Fatalf("stored record still reports %d bytes", recs[0].Req.BodyBytes)
	}
}

// Timestamps must be UTC at rest, not merely "correct on a host that happens to
// run UTC". A server with TZ set would otherwise write -06:00 offsets, mixing
// zones inside one snapshot; browsers cope, but the archive stops being sortable
// or greppable by eye.
func TestStoredTimesAreUTC(t *testing.T) {
	loc := time.FixedZone("MDT", -6*3600)
	s := New(Limits{MaxRecords: 10}, time.Hour)
	s.now = func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, loc) }

	d, err := s.Create("1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if d.CreatedAt.Location() != time.UTC || d.ExpiresAt.Location() != time.UTC {
		t.Fatalf("dumpster times = %v / %v, want UTC", d.CreatedAt, d.ExpiresAt)
	}

	rec, err := s.Add(d.ID, &model.Record{ReceivedAt: time.Date(2026, 6, 1, 12, 0, 1, 0, loc)})
	if err != nil {
		t.Fatal(err)
	}
	if rec.ReceivedAt.Location() != time.UTC {
		t.Fatalf("received_at = %v, want UTC", rec.ReceivedAt)
	}
	if got := rec.ReceivedAt.Format(time.RFC3339); got != "2026-06-01T18:00:01Z" {
		t.Fatalf("received_at = %q, want the same instant in Z form", got)
	}

	upd, err := s.AppendEvent(d.ID, rec.Seq, model.Event{At: time.Date(2026, 6, 1, 12, 0, 2, 0, loc)})
	if err != nil {
		t.Fatal(err)
	}
	if len(upd.Events) != 1 || upd.Events[0].At.Location() != time.UTC {
		t.Fatalf("event time = %v, want UTC", upd.Events)
	}
}
