// Package id mints and normalises dumpster identifiers.
package id

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// New returns a fresh UUIDv7 string. v7 is time-ordered, so dumpsters sort by
// rental time and any future SQL store keeps index locality.
func New() (string, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate uuidv7: %w", err)
	}
	return u.String(), nil
}

// Canonical accepts a dashed UUID, a bare 32-char hex UUID, or the gRPC path
// form "d<hex>", and returns the canonical dashed lowercase string.
//
// The "d" prefix is only stripped when the remainder parses, so a legitimate
// id that happens to start with the hex digit d is not mangled.
func Canonical(s string) (string, error) {
	if u, err := uuid.Parse(s); err == nil {
		return u.String(), nil
	}
	if len(s) == 33 && (s[0] == 'd' || s[0] == 'D') {
		if u, err := uuid.Parse(s[1:]); err == nil {
			return u.String(), nil
		}
	}
	return "", fmt.Errorf("invalid dumpster id %q", s)
}

// Hex strips the dashes, for use in a gRPC method path.
func Hex(canonical string) string { return strings.ReplaceAll(canonical, "-", "") }
