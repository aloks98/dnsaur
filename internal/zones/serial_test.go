package zones_test

import (
	"testing"

	"github.com/aloks98/dnsaur/internal/zones"
)

// RFC 1982 §3.2 defines the comparison over a circle, so `a > b` is right
// everywhere except the wrap and wrong exactly there — the worst possible
// failure shape, since it works for years and then strands a zone at the top
// of the space. Each row below names which half of the definition it pins.
func TestSerialNewer(t *testing.T) {
	const half = uint32(1) << 31

	tests := []struct {
		name string
		a, b uint32
		want bool
	}{
		// The ordinary case, which a naive `a > b` also gets right.
		{"one ahead", 2, 1, true},
		{"one behind", 1, 2, false},
		{"equal is not newer", 7, 7, false},
		{"far ahead, still inside the half", 1000, 1, true},

		// The wrap. A serial that has just passed 2^32-1 is newer than one
		// that has not, and this is the whole reason the helper exists.
		{"zero is the successor of the maximum", 0, 4294967295, true},
		{"the maximum is not newer than zero", 4294967295, 0, false},
		{"just past the wrap", 5, 4294967290, true},
		{"just before the wrap", 4294967290, 5, false},

		// The boundary of the defined region: a difference of 2^31 - 1 is
		// the largest that still has an answer.
		{"largest defined forward distance", half - 1, 0, true},
		{"largest defined backward distance", 0, half - 1, false},

		// §3.2 leaves a pair exactly 2^31 apart undefined. The policy is
		// "not newer", in both directions, because the two mistakes are not
		// symmetric: a transfer that does not happen is recovered by the
		// refresh timer, and one that should not have happened is a full
		// AXFR of someone else's zone — and on the outbound side it would
		// notify every target on every pass, forever.
		{"exactly half the space is undefined, forward", half, 0, false},
		{"exactly half the space is undefined, backward", 0, half, false},
		{"undefined, away from the origin", half + 100, 100, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := zones.SerialNewer(tc.a, tc.b); got != tc.want {
				t.Errorf("SerialNewer(%d, %d) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// A serial is never newer than itself, at any point in the space including
// the two that a subtraction is most likely to get wrong.
func TestSerialNewerIsIrreflexive(t *testing.T) {
	for _, s := range []uint32{0, 1, 1 << 31, 4294967295} {
		if zones.SerialNewer(s, s) {
			t.Errorf("SerialNewer(%d, %d) = true, want false", s, s)
		}
	}
}
