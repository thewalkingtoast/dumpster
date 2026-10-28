package app

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A flood must not become dumpster content: the rate limit refuses before a
// record exists, not after.
func TestHTTPIngestIsRateLimited(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.IngestPerMinute = 2 })
	id := rent(t, a)

	var codes []int
	for i := 0; i < 3; i++ {
		res, err := http.Post(a.BaseURL()+"/d/"+id+"/x", "text/plain", strings.NewReader("hi"))
		if err != nil {
			t.Fatalf("POST %d: %v", i, err)
		}
		res.Body.Close()
		codes = append(codes, res.StatusCode)
	}

	want := []int{200, 200, http.StatusTooManyRequests}
	for i := range want {
		if codes[i] != want[i] {
			t.Fatalf("statuses = %v, want %v", codes, want)
		}
	}
	if n := len(records(t, a, id)); n != 2 {
		t.Fatalf("records = %d, want 2 (the refused request must not be dumped)", n)
	}
}

// UDP cannot be told to slow down, so its excess is dropped.
func TestUDPIngestIsRateLimited(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.IngestPerMinute = 2 })
	id := rent(t, a)

	conn, err := net.Dial("udp", a.Addrs().UDP)
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}
	defer conn.Close()

	for i := 0; i < 4; i++ {
		if _, err := conn.Write([]byte(id + "|hello")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		time.Sleep(20 * time.Millisecond) // keep datagram order observable
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(records(t, a, id)) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // room for a fourth to land if it were allowed
	if n := len(records(t, a, id)); n != 2 {
		t.Fatalf("records = %d, want 2 (surplus datagrams dropped)", n)
	}
}

// A caller may hold only so many long-lived sessions. The refused one is closed
// before it becomes a record.
func TestTCPConnectionsAreCappedPerAddress(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.MaxConnsPerIP = 1 })
	id := rent(t, a)

	first, err := net.Dial("tcp", a.Addrs().TCP)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	defer first.Close()
	if _, err := first.Write([]byte(id + "\nhello\n")); err != nil {
		t.Fatalf("write 1: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(records(t, a, id)) < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	if n := len(records(t, a, id)); n != 1 {
		t.Fatalf("first connection was not recorded (%d records)", n)
	}

	second, err := net.Dial("tcp", a.Addrs().TCP)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer second.Close()
	_, _ = second.Write([]byte(id + "\nhello\n"))
	_ = second.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	if _, err := second.Read(buf); err == nil {
		t.Fatal("the over-cap connection was accepted")
	}

	time.Sleep(100 * time.Millisecond)
	if n := len(records(t, a, id)); n != 1 {
		t.Fatalf("an over-cap connection was recorded: %d records", n)
	}
	if held := a.conns.held("127.0.0.1"); held != 1 {
		t.Fatalf("gate holds %d for the caller, want 1", held)
	}
}

// The gate has to hand slots back, or a caller would be locked out after their
// first burst of connections.
func TestConnGateReleasesOnClose(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.MaxConnsPerIP = 1 })
	id := rent(t, a)

	for i := 0; i < 3; i++ {
		conn, err := net.Dial("tcp", a.Addrs().TCP)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		_, _ = conn.Write([]byte(id + "\n"))
		time.Sleep(50 * time.Millisecond)
		conn.Close()
		time.Sleep(100 * time.Millisecond)
	}
	if held := a.conns.held("127.0.0.1"); held != 0 {
		t.Fatalf("gate still holds %d after every connection closed", held)
	}
}
