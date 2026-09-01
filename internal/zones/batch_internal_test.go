package zones

// A direct, white-box test of batch, unlike every other test in this
// package (package zones_test — see transferserver_test.go's header
// comment). batch is unexported and has no seam a black-box test could
// drive it through independent of envelope size math, so this one file
// tests it in place instead. It stays internal on purpose: batch is a
// private helper, not a package seam, and exporting it to reach it from
// outside would widen the API for a test's convenience alone.

import (
	"reflect"
	"testing"

	"github.com/miekg/dns"
)

func TestBatch(t *testing.T) {
	rr := func(name string) dns.RR {
		r, err := dns.NewRR(name + ". 300 IN A 10.0.0.1")
		if err != nil {
			t.Fatalf("NewRR: %v", err)
		}
		return r
	}
	r1, r2, r3 := rr("one.example"), rr("two.example"), rr("three.example")
	n1, n2 := dns.Len(r1), dns.Len(r2)

	tests := []struct {
		name   string
		rrs    []dns.RR
		budget int
		want   [][]dns.RR
	}{
		{
			name:   "empty input produces no envelopes",
			rrs:    nil,
			budget: 1000,
			want:   nil,
		},
		{
			// The len(current) > 0 guard is what this pins: without it, an
			// oversized first record would close an empty envelope ahead of
			// itself rather than simply going out alone.
			name:   "a record larger than budget goes out alone rather than being dropped",
			rrs:    []dns.RR{r1},
			budget: n1 - 1,
			want:   [][]dns.RR{{r1}},
		},
		{
			name:   "records that exactly fill the budget share one envelope",
			rrs:    []dns.RR{r1, r2},
			budget: n1 + n2,
			want:   [][]dns.RR{{r1, r2}},
		},
		{
			name:   "one record past the exact fit starts a new envelope",
			rrs:    []dns.RR{r1, r2, r3},
			budget: n1 + n2,
			want:   [][]dns.RR{{r1, r2}, {r3}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := batch(tt.rrs, tt.budget)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("batch(%d records, budget %d) = %v, want %v", len(tt.rrs), tt.budget, got, tt.want)
			}
		})
	}
}
