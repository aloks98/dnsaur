package filter

import (
	"fmt"
	"runtime"
	"testing"
)

// A synthetic stand-in for a large aggregate blocklist: a million lines of the
// shapes those lists are made of — deep per-host names, a long tail of CDN
// hosts under a few thousand shared parents, and bare two-label domains.
// Duplicates are part of the shape, so the set ends up smaller than the input.
const benchLines = 1_000_000

func benchDomains() []string {
	out := make([]string, 0, benchLines)
	for i := range benchLines {
		switch i % 4 {
		case 0:
			out = append(out, fmt.Sprintf("ads%d.example%d.com", i, i%2000))
		case 1:
			out = append(out, fmt.Sprintf("t%d.track%d.net", i, i%3000))
		case 2:
			out = append(out, fmt.Sprintf("cdn%d.x%d.y%d.org", i%50, i%700, i%9000))
		default:
			out = append(out, fmt.Sprintf("host%d.io", i))
		}
	}
	return out
}

func heapInUse() uint64 {
	// Two collections: the first frees, the second settles what the first
	// resurrected through finalizers, so HeapInuse describes the live set.
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

func totalAlloc() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.TotalAlloc
}

// BenchmarkDomainSetBuild reports what a full-size list costs in memory rather
// than in time: what the compiled set holds, and what building it churned
// through — the second matters because a refresh keeps the outgoing set live
// while it compiles the new one. Run with -benchtime=1x.
func BenchmarkDomainSetBuild(b *testing.B) {
	doms := benchDomains()
	var heap, alloc, entries float64
	for b.Loop() {
		baseHeap, baseAlloc := heapInUse(), totalAlloc()
		s := NewDomainSet()
		for _, d := range doms {
			s.Add(d)
		}
		heap = float64(heapInUse() - baseHeap)
		alloc = float64(totalAlloc() - baseAlloc)
		entries = float64(s.Len())
		runtime.KeepAlive(s)
	}
	b.ReportMetric(entries, "entries")
	b.ReportMetric(heap/entries, "heap-B/entry")
	b.ReportMetric(alloc/entries, "alloc-B/entry")
}

var (
	sinkMatched string
	sinkOK      bool
)

// BenchmarkDomainSetMatch covers the three outcomes with different costs: an
// entry hit on the first probe, a subdomain hit that walks up to its entry,
// and a miss that walks the whole name. A query pays this once per set, over
// `2 + len(lists)` sets, so the allocation count is the number to watch.
func BenchmarkDomainSetMatch(b *testing.B) {
	s := NewDomainSet()
	for _, d := range benchDomains() {
		s.Add(d)
	}
	for _, c := range []struct{ name, qname string }{
		{"hit", "ads12.example12.com"},
		{"parent-hit", "deep.sub.ads12.example12.com"},
		{"miss", "www.nothing-here.example.co.uk"},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				sinkMatched, sinkOK = s.Match(c.qname)
			}
		})
	}
}
