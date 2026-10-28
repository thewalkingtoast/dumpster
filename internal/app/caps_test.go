package app

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestYardIsFull(t *testing.T) {
	a := newTestApp(t, func(c *Config) { c.MaxDumpsters = 1 })
	rent(t, a)

	res, err := noRedirect.Post(a.BaseURL()+"/rent", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("second rent: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the yard is full", res.StatusCode)
	}
	// Unlike a rate limit, a full yard is a state a new dumpster fixes — so this
	// page keeps the form that 429 drops.
	body, _ := io.ReadAll(res.Body)
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
		req, err := http.NewRequest(http.MethodPost, a.BaseURL()+"/rent", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Forwarded-For", ip)
		res, err := noRedirect.Do(req)
		if err != nil {
			t.Fatalf("POST /rent: %v", err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
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

	res, err := noRedirect.Post(a.BaseURL()+"/rent", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("POST /rent: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(body), `action="/rent"`) {
		t.Fatalf("the rate-limited page offers a rent form that can only bounce back:\n%s", body)
	}
	if !strings.Contains(string(body), "Slow down") {
		t.Fatalf("the page stopped explaining why:\n%s", body)
	}
}
