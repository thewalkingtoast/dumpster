package app

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/adam/dumpster/internal/model"
)

func TestTCPRecordsIDLineAndChunks(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	conn, err := net.Dial("tcp", a.Addrs().TCP)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := fmt.Fprintf(conn, "%s\nhello there\n", id); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var rec model.Record
	for time.Now().Before(deadline) {
		recs := records(t, a, id)
		if len(recs) == 1 && len(recs[0].Events) > 0 {
			rec = recs[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec.Kind != model.KindTCP {
		t.Fatalf("kind = %q, want tcp (records=%d)", rec.Kind, len(records(t, a, id)))
	}
	if rec.Meta["id_line"] != id {
		t.Fatalf("id_line = %q, want %q", rec.Meta["id_line"], id)
	}
	joined := ""
	for _, ev := range rec.Events {
		joined += ev.Text
	}
	if !strings.Contains(joined, "hello there") {
		t.Fatalf("events did not capture the payload: %+v", rec.Events)
	}
}

func TestTCPRejectsUnknownDumpster(t *testing.T) {
	a := newTestApp(t)
	conn, err := net.Dial("tcp", a.Addrs().TCP)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%s\nhi\n", "00000000-0000-7000-8000-000000000000"); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(line, "unknown") {
		t.Fatalf("reply = %q, want a message about an unknown dumpster", line)
	}
}
