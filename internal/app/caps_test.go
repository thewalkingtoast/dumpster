package app

import (
	"net/http"
	"strings"
	"testing"

	"github.com/thewalkingtoast/dumpster/internal/model"
)

func TestYardIsFull(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.MaxDumpsters = 1 })
	rent(t, a)

	res, body, _ := postRent(t, a, "")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the yard is full", res.StatusCode)
	}
	// Unlike a rate limit, a full yard is a state a new dumpster fixes — so this
	// page keeps the form that 429 drops.
	if !strings.Contains(string(body), `action="/rent"`) {
		t.Fatalf("the full-yard page should offer the rent form:\n%s", body)
	}
}

func TestRecordCapTrimsOldest(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.MaxRecords = 2 })
	id := rent(t, a)

	for i := 0; i < 3; i++ {
		res, err := http.Post(a.BaseURL()+"/d/"+id+"/x", "text/plain", strings.NewReader("hi"))
		if err != nil {
			t.Fatalf("POST %d: %v", i, err)
		}
		res.Body.Close()
	}
	recs := records(t, a, id)
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2 (MaxRecords)", len(recs))
	}
	if recs[0].Seq != 3 {
		t.Fatalf("newest seq = %d, want 3", recs[0].Seq)
	}
}

func TestBodyCapRejectsNothingButTruncates(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.MaxBody = 8 })
	id := rent(t, a)

	res, err := http.Post(a.BaseURL()+"/d/"+id+"/x", "text/plain",
		strings.NewReader(strings.Repeat("z", 4096)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("an oversized body must still be accepted, got %d", res.StatusCode)
	}
	rec := records(t, a, id)[0]
	if !rec.Req.Truncated || rec.Req.BodyBytes != 8 {
		t.Fatalf("record = %+v", rec.Req)
	}
}

// One address hitting its allowance gets a 429 naming the limit, not a 503 that
// blames the whole yard — and a different address is still welcome.
func TestRentStopsAtThePerAddressAllowance(t *testing.T) {
	a := newTestApp(t, func(c *Config) {
		c.MaxPerIP = 1
		c.RentPerHour = 100
		c.TrustProxy = true
	})

	post := func(ip string) (int, string) {
		t.Helper()
		res, body, _ := postRent(t, a, ip)
		return res.StatusCode, body
	}

	if code, _ := post("203.0.113.7"); code != http.StatusSeeOther {
		t.Fatalf("first rent = %d, want 303", code)
	}
	code, body := post("203.0.113.7")
	if code != http.StatusTooManyRequests {
		t.Fatalf("second rent from the same address = %d, want 429", code)
	}
	if !strings.Contains(body, "already holds 1 dumpster.") {
		t.Fatalf("429 page pluralised the limit wrongly")
	}
	if !strings.Contains(body, "Your yard is full") {
		t.Fatalf("429 page did not explain the per-address limit")
	}
	if code, _ := post("198.51.100.9"); code != http.StatusSeeOther {
		t.Fatalf("a different address = %d, want 303", code)
	}
}

// A rate-limited caller cannot rent, so the page must not offer the form: the
// button would take them straight back to the same page, which reads as a bug.
func TestRateLimitedPageDoesNotOfferToRent(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.RentPerHour = 1 })
	rent(t, a) // spends the single allowed rental for this address

	res, body, _ := postRent(t, a, "")
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", res.StatusCode)
	}
	if strings.Contains(body, `action="/rent"`) {
		t.Fatalf("the rate-limited page offers a rent form that can only bounce back:\n%s", body)
	}
	if !strings.Contains(body, "Slow down") {
		t.Fatalf("the page stopped explaining why:\n%s", body)
	}
}

// Behind nginx, X-Real-IP is the address nginx *writes* from the socket, so it
// cannot be spoofed through the proxy the way X-Forwarded-For can (nginx appends
// to that, leaving a client-supplied hop first). It stays off by default because
// a directly reachable app would let anyone claim any address.
func TestRealIPIsTrustedOnlyWhenConfigured(t *testing.T) {
	trusting := newTestApp(t, func(c *Config) { c.TrustRealIP = true })
	id := rent(t, trusting)
	res, err := http.Post(trusting.BaseURL()+"/d/"+id+"/x", "text/plain",
		strings.NewReader("hi"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	req, _ := http.NewRequest(http.MethodPost, trusting.BaseURL()+"/d/"+id+"/y", strings.NewReader("hi"))
	req.Header.Set("X-Real-IP", "203.0.113.9")
	res2, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()

	recs := records(t, trusting, id)
	var got *model.Record
	for i := range recs {
		if recs[i].Req.Subpath == "/y" {
			got = &recs[i]
		}
	}
	if got == nil {
		t.Fatal("the second request was not recorded")
	}
	if got.Remote.IP != "203.0.113.9" {
		t.Fatalf("remote.ip = %q, want the trusted X-Real-IP", got.Remote.IP)
	}
	if got.Remote.RealIP != "203.0.113.9" {
		t.Fatalf("x_real_ip = %q, want it preserved as sent", got.Remote.RealIP)
	}
}

func TestRealIPIsRecordedButNotTrustedByDefault(t *testing.T) {
	a := newTestApp(t)
	id := rent(t, a)

	req, _ := http.NewRequest(http.MethodPost, a.BaseURL()+"/d/"+id+"/y", strings.NewReader("hi"))
	req.Header.Set("X-Real-IP", "203.0.113.9")
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	rec := records(t, a, id)[0]
	if rec.Remote.IP == "203.0.113.9" {
		t.Fatal("an untrusted X-Real-IP was believed: any caller could pick their address")
	}
	if rec.Remote.RealIP != "203.0.113.9" {
		t.Fatalf("x_real_ip = %q, want the header kept for debugging", rec.Remote.RealIP)
	}
}
