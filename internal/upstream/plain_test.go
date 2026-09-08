package upstream

import (
	"context"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
)

// The other half of spec §10's padding row. TestDoTPadsWhatItSends and
// TestDoHPadsWhatItSendsAndStripsWhatItGets deliver "an encrypted query is
// padded"; this delivers "a plain one is not", which nothing asserted.
//
// The refactor it exists to catch is a plausible one: hoisting dnssrv.Pad out
// of the two encrypted exchangers and into Forwarder.exchange, where the
// message is already being copied. Every other test in the package still
// passes afterwards, and every plaintext query starts carrying 128-byte
// alignment to port 53 — wasted bytes, and a changed fingerprint, hiding
// nothing that was not already in the clear.
//
// Asserted through the whole Forwarder rather than against plainExchanger
// alone, because Forwarder.exchange is where such a hoist would land.
func TestPlainQueryIsNotPadded(t *testing.T) {
	type seen struct {
		opt *dns.OPT
		raw int
	}
	got := make(chan seen, 4)
	addr := mockUpstream(t, func(w dns.ResponseWriter, m *dns.Msg) {
		wire, err := m.Pack()
		if err == nil {
			got <- seen{opt: m.IsEdns0(), raw: len(wire)}
		}
		r := new(dns.Msg)
		r.SetReply(m)
		rr, _ := dns.NewRR(m.Question[0].Name + " 300 IN A 5.6.7.8")
		r.Answer = []dns.RR{rr}
		_ = w.WriteMsg(r)
	})

	f, err := New(Config{Upstreams: []string{addr}, Strategy: "failover"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	if _, err := f.Handler().ServeDNS(context.Background(), req("example.com")); err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}

	select {
	case s := <-got:
		if s.opt != nil {
			for _, o := range s.opt.Option {
				if _, isPad := o.(*dns.EDNS0_PADDING); isPad {
					t.Fatal("a plaintext query carried an EDNS(0) Padding option")
				}
			}
		}
		// Belt and braces: padding is what would make the length a multiple
		// of the block, and this query is far shorter than one block.
		if s.raw >= dnssrv.PaddingBlockQuery {
			t.Errorf("the plaintext query was %d bytes, at or past the %d-byte padding block: "+
				"short as it is, that can only mean it was padded", s.raw, dnssrv.PaddingBlockQuery)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the upstream never recorded a query")
	}
}
