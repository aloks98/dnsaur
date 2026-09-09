package zones

// What converting a received zone into rows costs as the zone grows.
//
// It is internal (package zones, like batch_internal_test.go and unlike every
// other test here) because build is the subject: it is unexported, and
// reaching it through Transfer would measure a TCP round trip, a sqlite
// transaction and a snapshot rebuild alongside the thing in question — the
// three of which move for reasons of their own and would bury it.
//
// A benchmark rather than a deadline, because "how many seconds" is the
// machine's answer rather than this code's, and a test asserting one would
// fail on a loaded box for reasons that have nothing to do with the zone. What
// it is here to make visible is the *shape*: every arriving RR is validated
// against the records already accepted (BuildRecord's RRSet-TTL and CNAME
// sibling rules), so handing it the whole accumulating slice makes the
// validation quadratic in the zone's size — 5×10⁹ comparisons at the 100,000
// records a transfer will accept — with the per-zone transfer lock held and
// the refresh pass waiting behind it.

import (
	"fmt"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// benchZoneRRs is a well-formed AXFR stream for a zone of n A records under
// distinct names, SOA-delimited the way build expects to receive one.
func benchZoneRRs(b *testing.B, apex string, n int) []dns.RR {
	b.Helper()
	rr := func(line string) dns.RR {
		out, err := dns.NewRR(line)
		if err != nil {
			b.Fatalf("dns.NewRR(%q): %v", line, err)
		}
		return out
	}
	soa := rr(fmt.Sprintf("%s. 900 IN SOA ns1.%s. hostmaster.%s. 7 900 300 604800 900", apex, apex, apex))
	out := make([]dns.RR, 0, n+3)
	out = append(out, soa, rr(fmt.Sprintf("%s. 3600 IN NS ns1.%s.", apex, apex)))
	for i := range n {
		out = append(out, rr(fmt.Sprintf("host%05d.%s. 300 IN A 10.%d.%d.%d",
			i, apex, i>>16&0xff, i>>8&0xff, i&0xff)))
	}
	return append(out, soa)
}

func BenchmarkBuild(b *testing.B) {
	const apex = "bench.e412.in"
	z := store.Zone{ID: 1, Name: apex, Type: "secondary"}
	t := &Transferrer{maxRecords: DefaultMaxTransferRecords}
	for _, n := range []int{1000, 5000, 20000} {
		b.Run(fmt.Sprintf("%d records", n), func(b *testing.B) {
			rrs := benchZoneRRs(b, apex, n)
			b.ResetTimer()
			for b.Loop() {
				if _, _, err := t.build(z, rrs); err != nil {
					b.Fatalf("build: %v", err)
				}
			}
		})
	}
}
