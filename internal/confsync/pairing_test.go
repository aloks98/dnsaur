package confsync

import (
	"strings"
	"testing"
)

// TestNewPairingCodeShape: §6's code is eight glyphs from an alphabet with no
// ambiguous pairs, rendered in two groups of four so it can be read off one
// screen and typed into another.
func TestNewPairingCodeShape(t *testing.T) {
	code, err := newPairingCode()
	if err != nil {
		t.Fatalf("newPairingCode: %v", err)
	}
	if len(code) != 9 || code[4] != '-' {
		t.Fatalf("%q is not XXXX-XXXX", code)
	}
	for i, r := range code {
		if i == 4 {
			continue
		}
		if !strings.ContainsRune(pairingAlphabet, r) {
			t.Fatalf("%q has %q, which is not in the alphabet", code, r)
		}
	}
	other, err := newPairingCode()
	if err != nil {
		t.Fatalf("newPairingCode: %v", err)
	}
	if other == code {
		t.Fatalf("two codes came out the same: %q", code)
	}
}

// TestNormalizePairingCode: the operator reads the code off one screen and
// types it into another, so the case and the grouping dash are theirs to get
// wrong.
func TestNormalizePairingCode(t *testing.T) {
	for _, in := range []string{"k7pq-4m2x", "K7PQ4M2X", "K7PQ-4M2X", "k7pq4m2x"} {
		if got := normalizePairingCode(in); got != "K7PQ4M2X" {
			t.Errorf("normalizePairingCode(%q) = %q", in, got)
		}
	}
}
