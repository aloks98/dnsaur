package upstream

import (
	"strings"
	"testing"

	"github.com/miekg/dns"
)

func query(name string) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	return m
}

// Every padded query packs to a multiple of the block size, whatever its
// name length — which is the property that makes the length stop carrying
// information about the name.
func TestPadQueryRoundsUpToBlock(t *testing.T) {
	for _, name := range []string{
		"a.com",
		"example.com",
		"a-somewhat-longer-label.example.com",
		strings.Repeat("x", 60) + ".example.com",
		strings.Repeat("y", 60) + "." + strings.Repeat("z", 60) + ".example.com",
	} {
		t.Run(name, func(t *testing.T) {
			m := query(name)
			if err := padQuery(m, paddingBlock); err != nil {
				t.Fatalf("padQuery: %v", err)
			}
			wire, err := m.Pack()
			if err != nil {
				t.Fatalf("Pack: %v", err)
			}
			if len(wire)%paddingBlock != 0 {
				t.Errorf("packed length %d is not a multiple of %d", len(wire), paddingBlock)
			}
		})
	}
}

// Two queries of very different name lengths pad to the same size, which is
// the point restated as the thing an observer actually sees.
func TestPadQueryHidesNameLength(t *testing.T) {
	short, long := query("a.com"), query("a.com")
	long.Question[0].Name = dns.Fqdn(strings.Repeat("w", 40) + ".example.com")
	for _, m := range []*dns.Msg{short, long} {
		if err := padQuery(m, paddingBlock); err != nil {
			t.Fatalf("padQuery: %v", err)
		}
	}
	a, _ := short.Pack()
	b, _ := long.Pack()
	if len(a) != len(b) {
		t.Errorf("padded lengths differ: %d vs %d — the name length still shows", len(a), len(b))
	}
}

// Padding twice is padding once: the exchangers call it on a message they
// may then retry on a fresh connection.
func TestPadQueryIsIdempotent(t *testing.T) {
	m := query("example.com")
	if err := padQuery(m, paddingBlock); err != nil {
		t.Fatalf("first padQuery: %v", err)
	}
	first, _ := m.Pack()
	if err := padQuery(m, paddingBlock); err != nil {
		t.Fatalf("second padQuery: %v", err)
	}
	again, _ := m.Pack()
	if len(first) != len(again) {
		t.Errorf("padding twice changed the size: %d then %d", len(first), len(again))
	}
	var pads int
	for _, o := range m.IsEdns0().Option {
		if _, ok := o.(*dns.EDNS0_PADDING); ok {
			pads++
		}
	}
	if pads != 1 {
		t.Errorf("message carries %d padding options, want 1", pads)
	}
}

// A query with no OPT record gets one, because padding has nowhere else to
// live.
func TestPadQueryAddsOPTWhenAbsent(t *testing.T) {
	m := query("example.com")
	if m.IsEdns0() != nil {
		t.Fatal("fixture already has an OPT record; the test proves nothing")
	}
	if err := padQuery(m, paddingBlock); err != nil {
		t.Fatalf("padQuery: %v", err)
	}
	if m.IsEdns0() == nil {
		t.Error("no OPT record after padding")
	}
}

// The reply's padding is removed, and the OPT record survives with the rest
// of what it carries.
func TestStripPaddingKeepsTheOPT(t *testing.T) {
	m := query("example.com")
	m.SetEdns0(1232, true) // DO set: this is what must not be lost
	opt := m.IsEdns0()
	opt.Option = append(opt.Option,
		&dns.EDNS0_PADDING{Padding: make([]byte, 40)},
		&dns.EDNS0_NSID{Nsid: "abc"},
	)

	stripPadding(m)

	got := m.IsEdns0()
	if got == nil {
		t.Fatal("the OPT record was dropped; the DO bit and UDP size went with it")
	}
	if !got.Do() {
		t.Error("the DO bit was lost")
	}
	for _, o := range got.Option {
		if _, ok := o.(*dns.EDNS0_PADDING); ok {
			t.Error("padding survived the strip")
		}
	}
	if len(got.Option) != 1 {
		t.Errorf("the OPT holds %d options, want 1 (NSID kept, padding removed)", len(got.Option))
	}
}

// Padding-only OPT: still kept, still emptied.
func TestStripPaddingKeepsAnEmptiedOPT(t *testing.T) {
	m := query("example.com")
	m.SetEdns0(1232, false)
	opt := m.IsEdns0()
	opt.Option = append(opt.Option, &dns.EDNS0_PADDING{Padding: make([]byte, 40)})

	stripPadding(m)

	if m.IsEdns0() == nil {
		t.Fatal("the OPT record was dropped when padding was its only option")
	}
	if n := len(m.IsEdns0().Option); n != 0 {
		t.Errorf("the OPT holds %d options, want 0", n)
	}
}

// A message with no OPT at all is left alone rather than panicking.
func TestStripPaddingToleratesNoOPT(t *testing.T) {
	m := query("example.com")
	stripPadding(m)
	if m.IsEdns0() != nil {
		t.Error("stripPadding invented an OPT record")
	}
}
