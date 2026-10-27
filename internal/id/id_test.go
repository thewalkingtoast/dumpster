package id

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestNewIsOrderedV7(t *testing.T) {
	a, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a >= b {
		t.Fatalf("uuidv7 is not time-ordered: %q >= %q", a, b)
	}
	u, err := uuid.Parse(a)
	if err != nil {
		t.Fatalf("parse %q: %v", a, err)
	}
	if u.Version() != 7 {
		t.Fatalf("version = %d, want 7", u.Version())
	}
	if len(a) != 36 || strings.Count(a, "-") != 4 {
		t.Fatalf("id %q is not dashed 36-char", a)
	}
}

func TestCanonicalAcceptsEveryForm(t *testing.T) {
	want, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, in := range []string{want, Hex(want), "d" + Hex(want), strings.ToUpper(want)} {
		got, err := Canonical(in)
		if err != nil {
			t.Fatalf("Canonical(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("Canonical(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "nope", "d", strings.Repeat("z", 32)} {
		if _, err := Canonical(bad); err == nil {
			t.Fatalf("Canonical(%q) should have failed", bad)
		}
	}
}
