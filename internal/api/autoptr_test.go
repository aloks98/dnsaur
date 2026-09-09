package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
)

// snapshotReloader records what each zone reload would have served.
// ReloadZones rebuilds the served snapshot from the store, so whatever is in
// the store at that moment is what the server answers with until the next
// reload — capturing it is how a test can tell "the PTR is in the database"
// apart from "the PTR is being served".
type snapshotReloader struct {
	*fakeReloader
	store store.Store

	mu   sync.Mutex
	last map[int64][]store.ZoneRecord
}

func (sr *snapshotReloader) ReloadZones(ctx context.Context) error {
	recs, err := sr.store.Zones().AllRecords(ctx)
	if err != nil {
		return err
	}
	sr.mu.Lock()
	sr.last = recs
	sr.mu.Unlock()
	return sr.fakeReloader.ReloadZones(ctx)
}

// servedPTRs is the PTR names the most recent reload would serve for zoneID.
func (sr *snapshotReloader) servedPTRs(zoneID int64) []string {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	var out []string
	for _, r := range sr.last[zoneID] {
		if r.Type == ptrType {
			out = append(out, r.Name)
		}
	}
	return out
}

// captureReloads swaps in a reloader that snapshots the store on every zone
// reload. Server reads deps.Reloader at call time, so replacing it after
// construction is enough, and the embedded fakeReloader keeps ts.rl's counts
// working for any assertion that still wants them.
func (ts *zoneTestServer) captureReloads() *snapshotReloader {
	sr := &snapshotReloader{fakeReloader: ts.rl, store: ts.store}
	ts.srv.deps.Reloader = sr
	return sr
}

// The PTR has to be live in the request that caused it, not merely present
// in the database — spec §7's "inside the same request, no second call from
// any client". Each handler writes the PTR before the reload that publishes
// its forward write, so one snapshot rebuild serves both. Move syncPTR after
// reloadZones and the row is still written correctly, but nothing serves it
// until some unrelated later write rebuilds the snapshot; only the reload's
// own view catches that.
func TestAutoPTRIsServedByTheSameRequestsReload(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")
	snap := srv.captureReloads()
	path := fmt.Sprintf("/api/v1/zones/%d/records", fwd)

	rid := srv.createRecord(t, fwd, `{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)
	if got := snap.servedPTRs(rev); len(got) != 1 || got[0] != "10" {
		t.Fatalf("create: reload served PTRs %v, want [10] — the PTR was written after the reload", got)
	}

	srv.do(t, "PUT", fmt.Sprintf("%s/%d", path, rid),
		`{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.11"}`)
	if got := snap.servedPTRs(rev); len(got) != 1 || got[0] != "11" {
		t.Fatalf("update: reload served PTRs %v, want [11] — the move landed after the reload", got)
	}

	srv.do(t, "DELETE", fmt.Sprintf("%s/%d", path, rid), "")
	if got := snap.servedPTRs(rev); len(got) != 0 {
		t.Fatalf("delete: reload served PTRs %v, want none — the removal landed after the reload", got)
	}
}

func TestAutoPTRCreatesRecord(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")

	srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", fwd),
		`{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)

	ptr := srv.recordsByType(t, rev, "PTR")
	if len(ptr) != 1 || ptr[0].Name != "10" || ptr[0].RData != "bifrost.e412.in." {
		t.Fatalf("PTR = %+v; want one at \"10\" pointing to bifrost.e412.in.", ptr)
	}
}

// The second address family, so "A and AAAA" isn't one type with a comment.
// RFC 3596 §2.5: 32 nibbles, reversed — fd00::28 under a d.f.ip6.arpa zone
// leaves 30 of them as the record name.
//
// fd00::/8 is IPv6 ULA (RFC 4193) — ordinary, homelab-owned address space,
// deliberately NOT a built-in (store.BuiltinZones excludes d.f.ip6.arpa for
// exactly the reason 168.192.in-addr.arpa is excluded: it would make a PTR
// for the user's own LAN impossible; see BuiltinZones's doc comment). This
// fixture is a regression guard as much as a test: if d.f.ip6.arpa is ever
// re-added to the built-ins, creating this primary zone 409s as a
// duplicate and this test fails, pointing straight at why.
func TestAutoPTRCreatesRecordForAAAA(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "d.f.ip6.arpa")

	srv.createRecord(t, fwd, `{"name":"bifrost","type":"AAAA","ttl":300,"rdata":"fd00::28"}`)

	want := "8.2.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0"
	ptr := srv.recordsByType(t, rev, "PTR")
	if len(ptr) != 1 || ptr[0].Name != want || ptr[0].RData != "bifrost.e412.in." {
		t.Fatalf("PTR = %+v; want one at %q pointing to bifrost.e412.in.", ptr, want)
	}
}

func TestAutoPTRSkippedWithoutReverseZone(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rec := srv.do(t, "POST", fmt.Sprintf("/api/v1/zones/%d/records", fwd),
		`{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)
	// The forward write must still succeed — a missing reverse zone is not
	// an error, and auto-PTR must never create one.
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	for _, z := range srv.allZones(t) {
		// The built-ins (store.BuiltinZones) are seeded by migration, so
		// they were there before this request; only a zone auto-PTR
		// invented would be new. ("localhost" aside, they're all .arpa
		// zones, which is what the suffix check below is really asking.)
		if z.Type == "internal" {
			continue
		}
		if strings.HasSuffix(z.Name, ".arpa") {
			t.Fatalf("auto-PTR created zone %q", z.Name)
		}
	}
}

// The built-ins are type "internal", which every other write path refuses
// with a 409 — an automatic write must not be the one exception.
// 127.in-addr.arpa covers the whole 127.0.0.0/8 loopback range, so without
// the guard an A record pointing anywhere in it would drop a PTR into a
// read-only zone.
func TestAutoPTRSkipsInternalZone(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")

	// Deliberately not 127.0.0.1: it reverses to "1.0.0", the exact name
	// the RFC 6303 §4 built-in PTR (store.builtinRecords) already occupies
	// in this zone. Using it here would pass even with the internal-zone
	// guard (ptrZoneCandidates in autoptr.go) deleted, because addPTR's
	// first-wins rule ("address already has a PTR, leaving it") already
	// refuses the write at "1.0.0" for an unrelated reason before the
	// guard is ever consulted. 127.0.0.53 reverses to "53.0.0", a name
	// nothing has claimed, so only the guard itself can be what keeps
	// auto-PTR out.
	srv.createRecord(t, fwd, `{"name":"local","type":"A","ttl":300,"rdata":"127.0.0.53"}`)

	rev := srv.zoneIDByName(t, "127.in-addr.arpa")
	for _, p := range srv.recordsByType(t, rev, "PTR") {
		if p.Name == "53.0.0" {
			t.Fatalf("auto-PTR wrote into the internal zone: %+v", p)
		}
	}
}

// A disabled record is dropped from the served snapshot (zones.NewZone), so
// it answers nothing forward; a PTR for it would publish a reverse answer
// for a name that resolves to nothing.
func TestAutoPTRSkipsDisabledRecord(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")

	srv.createRecord(t, fwd, `{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10","enabled":false}`)

	if ptr := srv.recordsByType(t, rev, "PTR"); len(ptr) != 0 {
		t.Fatalf("PTR = %+v; want none for a disabled A record", ptr)
	}
}

// RFC 1034 §3.6.2: a CNAME must be the only record at its name. Submitting
// this PTR by hand is refused with 409 "CNAME cannot coexist with another
// record at the same name", so the automatic path must not produce it
// either — the resolver would serve the pair regardless of which path wrote
// it.
func TestAutoPTRLeavesCNAMEAlone(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")
	srv.createRecord(t, rev, `{"name":"10","type":"CNAME","ttl":300,"rdata":"10.sub.150.168.192.in-addr.arpa."}`)

	srv.createRecord(t, fwd, `{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)

	if ptr := srv.recordsByType(t, rev, "PTR"); len(ptr) != 0 {
		t.Fatalf("PTR = %+v; want none beside a CNAME", ptr)
	}
	cname := srv.recordsByType(t, rev, "CNAME")
	if len(cname) != 1 || cname[0].RData != "10.sub.150.168.192.in-addr.arpa." {
		t.Fatalf("CNAME = %+v; want the hand-written one untouched", cname)
	}
}

// The other side of that check: it refuses what the API refuses, and nothing
// more. A reverse zone whose apex is itself the address's reverse name gives
// rel "@", where the zone's own apex NS always sits — a blanket "any record
// already here" test would refuse this PTR forever, though a user may write
// it and RFC 1034 permits it.
func TestAutoPTRWritesAtApexBesideNS(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "10.150.168.192.in-addr.arpa")

	srv.createRecord(t, fwd, `{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)

	ptr := srv.recordsByType(t, rev, "PTR")
	if len(ptr) != 1 || ptr[0].Name != "@" || ptr[0].RData != "bifrost.e412.in." {
		t.Fatalf("PTR = %+v; want one at \"@\" pointing to bifrost.e412.in.", ptr)
	}
	if ns := srv.recordsByType(t, rev, "NS"); len(ns) != 1 {
		t.Fatalf("NS = %+v; want the apex NS still there beside it", ns)
	}
}

// The reverse zone's contents changed, so its serial has to move — a
// secondary or a cache comparing serials has no other signal that the PTR
// appeared or went away.
func TestAutoPTRBumpsReverseZoneSerial(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")

	before := srv.zone(t, rev).SOASerial
	rid := srv.createRecord(t, fwd, `{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)
	afterWrite := srv.zone(t, rev).SOASerial
	if afterWrite != before+1 {
		t.Fatalf("reverse serial %d -> %d after the PTR was written, want +1", before, afterWrite)
	}

	srv.do(t, "DELETE", fmt.Sprintf("/api/v1/zones/%d/records/%d", fwd, rid), "")
	if afterDelete := srv.zone(t, rev).SOASerial; afterDelete != afterWrite+1 {
		t.Fatalf("reverse serial %d -> %d after the PTR was removed, want +1", afterWrite, afterDelete)
	}

	// A forward write with no PTR to maintain must leave it alone.
	quiet := srv.zone(t, rev).SOASerial
	srv.createRecord(t, fwd, `{"name":"notes","type":"TXT","ttl":300,"rdata":"\"hello\""}`)
	if got := srv.zone(t, rev).SOASerial; got != quiet {
		t.Fatalf("reverse serial moved to %d on a write that touched no PTR, want %d", got, quiet)
	}
}

// First wins: a second name at the same address must not steal the reverse.
func TestAutoPTRDoesNotOverwriteExisting(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")
	path := fmt.Sprintf("/api/v1/zones/%d/records", fwd)

	srv.do(t, "POST", path, `{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)
	srv.do(t, "POST", path, `{"name":"nas","type":"A","ttl":300,"rdata":"192.168.150.10"}`)

	ptr := srv.recordsByType(t, rev, "PTR")
	if len(ptr) != 1 || ptr[0].RData != "bifrost.e412.in." {
		t.Fatalf("PTR = %+v; want the first writer to keep it", ptr)
	}
}

func TestAutoPTRMovesOnUpdate(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")
	path := fmt.Sprintf("/api/v1/zones/%d/records", fwd)

	rid := srv.createRecord(t, fwd, `{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)
	srv.do(t, "PUT", fmt.Sprintf("%s/%d", path, rid),
		`{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.11"}`)

	ptr := srv.recordsByType(t, rev, "PTR")
	if len(ptr) != 1 || ptr[0].Name != "11" {
		t.Fatalf("PTR = %+v; want it moved to \"11\"", ptr)
	}
}

func TestAutoPTRRemovedOnDelete(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")
	rid := srv.createRecord(t, fwd, `{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)

	srv.do(t, "DELETE", fmt.Sprintf("/api/v1/zones/%d/records/%d", fwd, rid), "")
	if ptr := srv.recordsByType(t, rev, "PTR"); len(ptr) != 0 {
		t.Fatalf("PTR = %+v; want it removed with the A record", ptr)
	}
}

// A PTR the user wrote by hand must survive the A record being deleted.
func TestAutoPTRLeavesForeignPTRAlone(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")
	srv.createRecord(t, rev, `{"name":"10","type":"PTR","ttl":300,"rdata":"handmade.e412.in."}`)
	rid := srv.createRecord(t, fwd, `{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)

	srv.do(t, "DELETE", fmt.Sprintf("/api/v1/zones/%d/records/%d", fwd, rid), "")
	ptr := srv.recordsByType(t, rev, "PTR")
	if len(ptr) != 1 || ptr[0].RData != "handmade.e412.in." {
		t.Fatalf("PTR = %+v; want the hand-written one untouched", ptr)
	}
}

// Deleting the whole forward zone must retire the PTRs its records owned,
// exactly as deleting one A record does. The zone's own records go with it
// through ON DELETE CASCADE, but the PTRs sit in the reverse zone, which no
// foreign key reaches — so without cleanup the reverse keeps answering with
// names that resolve to nothing. The reload assertion pins the ordering too:
// the retirement has to happen before the reload that publishes the delete,
// or the stale PTRs stay served until some unrelated later write.
func TestAutoPTRRetiredWhenForwardZoneDeleted(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")
	srv.createRecord(t, fwd, `{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)
	srv.createRecord(t, fwd, `{"name":"nas","type":"A","ttl":300,"rdata":"192.168.150.11"}`)
	snap := srv.captureReloads()

	if rec := srv.do(t, "DELETE", fmt.Sprintf("/api/v1/zones/%d", fwd), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete zone: status = %d body = %s", rec.Code, rec.Body)
	}

	if ptr := srv.recordsByType(t, rev, ptrType); len(ptr) != 0 {
		t.Fatalf("PTRs = %+v; want them retired with the zone whose records owned them", ptr)
	}
	if got := snap.servedPTRs(rev); len(got) != 0 {
		t.Fatalf("reload served PTRs %v; want none — retired after the reload that publishes the delete", got)
	}
}

// Renaming a forward zone moves every name its records answer to, so the
// PTRs pointing at the old names have to move with it. Before this, PATCH
// never touched auto-PTR at all: the reverse zone went on answering
// `nas.home.lan.` for a zone now called `home.arpa`, and a later delete of
// the A record looked for a PTR naming `nas.home.arpa`, never matched it,
// and left the stale one behind for good.
func TestAutoPTRFollowsAForwardZoneRename(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")
	srv.createRecord(t, fwd, `{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)
	snap := srv.captureReloads()

	if rec := srv.do(t, "PATCH", fmt.Sprintf("/api/v1/zones/%d", fwd), `{"name":"nexus.test"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("rename: status = %d body = %s", rec.Code, rec.Body)
	}

	ptr := srv.recordsByType(t, rev, ptrType)
	if len(ptr) != 1 || ptr[0].RData != "bifrost.nexus.test." {
		t.Fatalf("PTRs = %+v; want one pointing at bifrost.nexus.test.", ptr)
	}
	// The rename's own reload has to publish the moved PTR, for the reason
	// TestAutoPTRIsServedByTheSameRequestsReload gives.
	if got := snap.servedPTRs(rev); len(got) != 1 || got[0] != "10" {
		t.Fatalf("reload served PTRs %v; want [10]", got)
	}
}

// Disabling a forward zone takes its names out of service, so the reverse
// must stop answering with them; enabling it again puts them back. A PTR
// naming a zone that answers nothing is a reverse answer for a name that
// resolves nowhere — the same reason addPTR refuses to write one for a
// disabled record.
func TestAutoPTRFollowsAForwardZoneBeingDisabledAndEnabled(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")
	srv.createRecord(t, fwd, `{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)
	path := fmt.Sprintf("/api/v1/zones/%d", fwd)

	if rec := srv.do(t, "PATCH", path, `{"enabled":false}`); rec.Code != http.StatusNoContent {
		t.Fatalf("disable: status = %d body = %s", rec.Code, rec.Body)
	}
	if ptr := srv.recordsByType(t, rev, ptrType); len(ptr) != 0 {
		t.Fatalf("PTRs = %+v; want them retired with the zone that answers for them", ptr)
	}

	if rec := srv.do(t, "PATCH", path, `{"enabled":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("enable: status = %d body = %s", rec.Code, rec.Body)
	}
	ptr := srv.recordsByType(t, rev, ptrType)
	if len(ptr) != 1 || ptr[0].RData != "bifrost.e412.in." {
		t.Fatalf("PTRs = %+v; want the zone's PTR back", ptr)
	}
}

// The same guard removePTR applies everywhere else: a zone delete retires
// only the PTRs that still point at its own names. Here neither surviving
// PTR does — one was typed by hand, the other is owned by a name in a
// different forward zone — and both addresses also carry a record in the
// zone being deleted, which is precisely the shape a blanket "delete every
// PTR for these addresses" would destroy.
func TestAutoPTRZoneDeleteLeavesForeignPTRsAlone(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")
	other := srv.createZone(t, "nexus.test")
	rev := srv.createZone(t, "150.168.192.in-addr.arpa")
	srv.createRecord(t, rev, `{"name":"10","type":"PTR","ttl":300,"rdata":"handmade.e412.in."}`)
	srv.createRecord(t, other, `{"name":"gate","type":"A","ttl":300,"rdata":"192.168.150.11"}`)
	// Both decline to take a PTR under the first-wins rule, so neither owns
	// one when the zone holding them is deleted.
	srv.createRecord(t, fwd, `{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)
	srv.createRecord(t, fwd, `{"name":"mirror","type":"A","ttl":300,"rdata":"192.168.150.11"}`)

	srv.do(t, "DELETE", fmt.Sprintf("/api/v1/zones/%d", fwd), "")

	got := map[string]string{}
	for _, r := range srv.recordsByType(t, rev, ptrType) {
		got[r.Name] = r.RData
	}
	want := map[string]string{"10": "handmade.e412.in.", "11": "gate.nexus.test."}
	if len(got) != len(want) {
		t.Fatalf("PTRs = %v, want %v", got, want)
	}
	for name, rdata := range want {
		if got[name] != rdata {
			t.Errorf("PTR %q = %q, want %q", name, got[name], rdata)
		}
	}
}

// Auto-PTR is the fifth write path into a zone, and the only automatic one.
// The four explicit ones (POST/PUT/DELETE records, POST file) each answer 409
// for a secondary; this one has no caller to refuse, so the only thing keeping
// it out is ptrZoneCandidates' type test.
//
// It matters more since Milestone D2 made `secondary` a type the API can
// actually create. A PTR written here would be deleted by the next transfer —
// which is the harmless half — but the write also bumps the reverse zone's
// serial (bumpZoneSerial), and that serial belongs to the primary this zone
// is a copy of. This server would then advertise a version of someone else's
// zone that no one else has, and a downstream secondary that had already seen
// that number would never ask again.
//
// 192.168.150.10 reverses into the secondary below, which is enabled and
// currently serving (refreshed_at set, expires_at ahead) — so nothing but the
// type test can be what keeps auto-PTR out of it.
func TestAutoPTRSkipsSecondaryZone(t *testing.T) {
	srv := newTestServer(t)
	fwd := srv.createZone(t, "e412.in")

	rev := createSecondary(t, srv, "150.168.192.in-addr.arpa", "203.0.113.9")
	// A secondary that has transferred, so it is answering rather than sitting
	// in the "never transferred" state Zone.Serving refuses outright.
	z := srv.zone(t, rev)
	now := time.Now().UnixMilli()
	z.RefreshedAt, z.ExpiresAt = now-60_000, now+604_800_000
	if err := srv.store.Zones().UpdateZone(t.Context(), z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}
	before := srv.zone(t, rev).SOASerial

	srv.createRecord(t, fwd, `{"name":"bifrost","type":"A","ttl":300,"rdata":"192.168.150.10"}`)

	if ptr := srv.recordsByType(t, rev, "PTR"); len(ptr) != 0 {
		t.Fatalf("auto-PTR wrote into a secondary zone: %+v", ptr)
	}
	// The serial is the half that outlives the next transfer's cleanup.
	if after := srv.zone(t, rev).SOASerial; after != before {
		t.Errorf("auto-PTR bumped a secondary's serial %d -> %d; that number is its primary's to advance",
			before, after)
	}
}
