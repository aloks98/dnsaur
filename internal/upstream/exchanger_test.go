package upstream

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/miekg/dns"
)

// countingExchanger records what it was asked and whether it was closed.
type countingExchanger struct {
	closed atomic.Int32
}

func (c *countingExchanger) Exchange(context.Context, *dns.Msg) (*dns.Msg, error) {
	return nil, context.Canceled
}
func (c *countingExchanger) Close() error { c.closed.Add(1); return nil }

// Close reaches every upstream, defaults and conditional routes alike, and
// an upstream named under two suffixes is closed once rather than twice.
func TestForwarderCloseReleasesEveryUpstream(t *testing.T) {
	f, err := New(Config{Upstreams: []string{"127.0.0.1:5301"}, Strategy: "race"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := f.SetConditional(map[string][]string{
		"a.example": {"127.0.0.1:5302"},
		"b.example": {"127.0.0.1:5302"}, // same address: one *up, two suffixes
	}); err != nil {
		t.Fatalf("SetConditional: %v", err)
	}
	counters := map[*up]*countingExchanger{}
	for _, u := range f.def {
		c := &countingExchanger{}
		u.ex, counters[u] = c, c
	}
	for _, ups := range f.condTableLoad().routes {
		for _, u := range ups {
			if _, seen := counters[u]; seen {
				continue
			}
			c := &countingExchanger{}
			u.ex, counters[u] = c, c
		}
	}
	if len(counters) != 2 {
		t.Fatalf("expected 2 distinct upstreams (one default, one shared by both suffixes), got %d", len(counters))
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for u, c := range counters {
		if got := c.closed.Load(); got != 1 {
			t.Errorf("upstream %s closed %d times, want exactly 1", u.addr, got)
		}
	}
}

// A suffix losing its upstream closes it: nothing will route there again,
// and it may be holding pooled connections.
func TestSetConditionalClosesOrphans(t *testing.T) {
	f, err := New(Config{Upstreams: []string{"127.0.0.1:5301"}, Strategy: "race"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := f.SetConditional(map[string][]string{"a.example": {"127.0.0.1:5302"}}); err != nil {
		t.Fatalf("SetConditional: %v", err)
	}
	var orphan *up
	for _, ups := range f.condTableLoad().routes {
		orphan = ups[0]
	}
	c := &countingExchanger{}
	orphan.ex = c

	// The suffix now routes somewhere else, so the old upstream is orphaned.
	if err := f.SetConditional(map[string][]string{"a.example": {"127.0.0.1:5303"}}); err != nil {
		t.Fatalf("SetConditional: %v", err)
	}
	if got := c.closed.Load(); got != 1 {
		t.Errorf("orphaned upstream closed %d times, want 1", got)
	}

	// And a surviving upstream is NOT closed — losing its pool on every
	// unrelated zone edit is the failure this reuse exists to prevent.
	var kept *up
	for _, ups := range f.condTableLoad().routes {
		kept = ups[0]
	}
	k := &countingExchanger{}
	kept.ex = k
	if err := f.SetConditional(map[string][]string{
		"a.example": {"127.0.0.1:5303"},
		"c.example": {"127.0.0.1:5304"},
	}); err != nil {
		t.Fatalf("SetConditional: %v", err)
	}
	if got := k.closed.Load(); got != 0 {
		t.Errorf("surviving upstream was closed %d times, want 0", got)
	}
}
