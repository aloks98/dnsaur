package zones_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
)

// Task 5 (see docs/superpowers/specs/2026-08-08-zones-design.md §9.4) is a
// measurement: does Reload's whole-store rebuild stay cheap now that a
// fleet of secondaries triggers it on every successful transfer, or does it
// need a per-zone path? These benchmarks are what the decision recorded on
// Reload's doc comment is based on, and they are committed so the decision
// is revisitable rather than taken on trust.

// reloadFixture opens a fresh sqlite-backed store and installs nZones
// primary zones of nRecords A records apiece, returning the store ready for
// Reload to read.
//
// sqlite, not postgres: it is dnsaur's documented default (README's example
// config uses it), and Open pins it to a single physical connection
// (store.go, SetMaxOpenConns(1)). That single connection is exactly the
// resource a Reload's reads and a transfer's install compete for, so
// benchmarking against it — rather than against postgres's pool, or against
// an in-memory fake with no connection to contend for at all — is what lets
// these numbers speak to "does a rebuild degrade anything else" instead of
// begging the question.
//
// Records install through ReplaceRecords, one transaction per zone, the
// same call a real transfer's install makes (transfer.go) — not one
// autocommitted INSERT per row, which on a single connection would turn
// seeding the largest fixture here into minutes spent on setup that has
// nothing to do with what Reload costs.
func reloadFixture(b *testing.B, nZones, nRecords int) store.Store {
	b.Helper()
	st, err := store.Open(context.Background(), "sqlite", filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	for zi := range nZones {
		name := fmt.Sprintf("zone%d.bench.internal", zi)
		z := store.Zone{
			Name: name, Type: "primary", Enabled: true,
			SOANS: "ns1." + name, SOAMbox: "hostmaster." + name,
			SOASerial: 1, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
			SOAMinimum: 900, SOATTL: 900,
		}
		id, err := st.Zones().AddZone(ctx, z)
		if err != nil {
			b.Fatal(err)
		}
		z.ID = id
		recs := make([]store.ZoneRecord, nRecords)
		for ri := range recs {
			recs[ri] = store.ZoneRecord{
				ZoneID: id, Name: fmt.Sprintf("host%d", ri), Type: "A", TTL: 300,
				RData: fmt.Sprintf("192.0.2.%d", ri%256), Enabled: true,
			}
		}
		if err := st.Zones().ReplaceRecords(ctx, z, nil, nil, recs); err != nil {
			b.Fatal(err)
		}
	}
	return st
}

// reloadSizes is the fixture matrix, chosen to answer three separate
// questions rather than just "is 20x500 fast":
//
//   - zones=20,records=500 is the spec's homelab sizing (§9.4) and the
//     figure a reader will actually compare this benchmark's headline
//     number against.
//   - zones=1,records=10000 and zones=200,records=50 both hold the total
//     record count at that homelab figure's 10,000 and swing zone count
//     from one extreme to the other. The gap between them (or the lack of
//     one) says whether zone count is a cost driver independent of total
//     records, which "20 zones of 500" alone cannot say — it varies both
//     at once.
//   - zones=50,records=1000 is 50,000 records, five homelabs' worth of
//     zone data on one server: the worst case worth costing, not the
//     median one.
var reloadSizes = []struct {
	name    string
	zones   int
	records int
}{
	{"zones=20,records=500", 20, 500},
	{"zones=1,records=10000", 1, 10000},
	{"zones=200,records=50", 200, 50},
	{"zones=50,records=1000", 50, 1000},
}

// BenchmarkReload is Reload exactly as production runs it: a real
// sqlite-backed store, read the way Reload reads it (Zones then
// AllRecords), rebuilt into a fresh Index every call.
func BenchmarkReload(b *testing.B) {
	for _, sz := range reloadSizes {
		b.Run(sz.name, func(b *testing.B) {
			st := reloadFixture(b, sz.zones, sz.records)
			r := zones.NewResolver(st.Zones())
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := r.Reload(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkReloadStoreRead isolates the half of Reload that is a database
// round trip — Zones and AllRecords, nothing built from what they return —
// so it can be weighed against BenchmarkReloadIndexBuild to say which half
// of Reload's cost is the store and which is Go rebuilding the index.
func BenchmarkReloadStoreRead(b *testing.B) {
	for _, sz := range reloadSizes {
		b.Run(sz.name, func(b *testing.B) {
			st := reloadFixture(b, sz.zones, sz.records)
			zs := st.Zones()
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := zs.Zones(ctx); err != nil {
					b.Fatal(err)
				}
				if _, err := zs.AllRecords(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkReloadIndexBuild isolates the other half: NewZone and NewIndex
// over data already in memory, with the store read paid once, outside the
// timed loop, instead of on every iteration. See BenchmarkReloadStoreRead.
func BenchmarkReloadIndexBuild(b *testing.B) {
	for _, sz := range reloadSizes {
		b.Run(sz.name, func(b *testing.B) {
			st := reloadFixture(b, sz.zones, sz.records)
			ctx := context.Background()
			zs, err := st.Zones().Zones(ctx)
			if err != nil {
				b.Fatal(err)
			}
			all, err := st.Zones().AllRecords(ctx)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				built := make([]zones.Zone, len(zs))
				for j, z := range zs {
					built[j] = zones.NewZone(z, all[z.ID])
				}
				_ = zones.NewIndex(built)
			}
		})
	}
}

// BenchmarkReloadConcurrent is the worst realistic case named in the task
// brief: several secondaries finishing their transfers within the same
// scheduler tick — plausible once the startup spread (refresh.go,
// startupSpread) has expired and every zone is back on its own SOA-refresh
// clock — each calling Reload, the whole-store rebuild, at once.
//
// sqlite's single connection (reloadFixture) means these calls cannot
// actually run concurrently against the database; what this measures is
// whether queuing behind that one connection turns a fleet-wide refresh
// into a pile-up worse than the same calls made one after another, or
// whether it costs about the same.
func BenchmarkReloadConcurrent(b *testing.B) {
	st := reloadFixture(b, 20, 500)
	r := zones.NewResolver(st.Zones())
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := r.Reload(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
}
