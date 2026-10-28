package app

import (
	"net"
	"testing"
	"time"

	"github.com/adam/dumpster/internal/model"
)

func TestUDPDatagramBecomesOneRecord(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	conn, err := net.Dial("udp", a.Addrs().UDP)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(id + "|hello datagram")); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var rec model.Record
	for time.Now().Before(deadline) {
		recs := records(t, a, id)
		if len(recs) == 1 {
			rec = recs[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec.Kind != model.KindUDP {
		t.Fatalf("kind = %q, want udp", rec.Kind)
	}
	if len(rec.Events) != 1 || rec.Events[0].Text != "hello datagram" {
		t.Fatalf("events = %+v", rec.Events)
	}
}

func TestUDPTwoDatagramsTwoRecords(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	conn, err := net.Dial("udp", a.Addrs().UDP)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	for _, payload := range []string{"one", "two"} {
		if _, err := conn.Write([]byte(id + "|" + payload)); err != nil {
			t.Fatalf("write %s: %v", payload, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(records(t, a, id)) == 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("records = %d, want 2 (one per datagram)", len(records(t, a, id)))
}

func TestUDPIgnoresGarbage(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	conn, err := net.Dial("udp", a.Addrs().UDP)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("too short")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := len(records(t, a, id)); got != 0 {
		t.Fatalf("garbage produced %d records", got)
	}
}
