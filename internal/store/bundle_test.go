package store

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// forEachDriverPair runs fn once per (main, replica) driver combination, each
// side its own store: the deployment this exists for is a postgres main and a
// sqlite replica, and a bundle has to cross that boundary with the main's ids
// intact.
//
// postgres→postgres is deliberately absent. openPostgres hands every caller
// the same database, so that pair would be one box importing its own export —
// every "the replica did not have this before" assertion would be vacuous,
// and re-applying a bundle is already covered by the second ImportBundle in
// the round trip below.
func forEachDriverPair(t *testing.T, fn func(t *testing.T, main, replica Store)) {
	drivers := []struct {
		name string
		open func(*testing.T) Store
	}{{"sqlite", openSQLite}, {"postgres", openPostgres}}
	for _, m := range drivers {
		for _, r := range drivers {
			if m.name == "postgres" && r.name == "postgres" {
				continue
			}
			t.Run(m.name+"-to-"+r.name, func(t *testing.T) { fn(t, m.open(t), r.open(t)) })
		}
	}
}

// fixtureSeq numbers this file's fixture names apart. The postgres halves
// share one database that outlives the process (openPostgres), so a group
// name, list URL, key name, zone name or client matcher reused by a later
// subtest — or by a second `-count=2` pass — is a uniqueness violation rather
// than a fresh row. It starts from the clock for the second of those.
var fixtureSeq = func() *atomic.Int64 {
	var n atomic.Int64
	n.Store(time.Now().UnixMilli() & 0xffffff)
	return &n
}()

type fixture struct{ group, matcher, listURL, key, zone string }

func newFixture() fixture {
	n := fixtureSeq.Add(1)
	return fixture{
		group:   fmt.Sprintf("kids-%d", n),
		matcher: fmt.Sprintf("10.%d.%d.%d", n>>16&0xff, n>>8&0xff, n&0xff),
		listURL: fmt.Sprintf("https://example.com/ads-%d.txt", n),
		key:     fmt.Sprintf("xfer-%d.example.", n),
		zone:    fmt.Sprintf("e%d.example", n),
	}
}

// TestBundleRoundTripKeepsIDs is §4.1: the replica writes the main's rows
// under the main's ids, so every foreign key in the bundle — and every
// rule_id and list_id in the query log afterwards — means the same row on
// both boxes.
func TestBundleRoundTripKeepsIDs(t *testing.T) {
	forEachDriverPair(t, func(t *testing.T, main, replica Store) {
		ctx := t.Context()
		f := newFixture()

		gid, err := main.Clients().AddGroup(ctx, f.group)
		if err != nil {
			t.Fatal(err)
		}
		cid, err := main.Clients().AddClient(ctx, Client{Name: "tablet", Matcher: f.matcher, GroupID: gid})
		if err != nil {
			t.Fatal(err)
		}
		lid, err := main.Filters().AddList(ctx, List{Name: "ads", URL: f.listURL, Kind: "block", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := main.Filters().AssignList(ctx, gid, lid); err != nil {
			t.Fatal(err)
		}
		rid, err := main.Filters().AddRule(ctx, Rule{GroupID: gid, Action: "allow", Pattern: "cdn.example"})
		if err != nil {
			t.Fatal(err)
		}
		kid, err := main.TSIGKeys().Create(ctx, TSIGKey{Name: f.key, Algorithm: "hmac-sha256.", Secret: "c2VjcmV0"})
		if err != nil {
			t.Fatal(err)
		}
		zid, err := main.Zones().AddZone(ctx, Zone{
			Name: f.zone, Type: "primary", Enabled: true,
			SOANS: "ns1." + f.zone, SOAMbox: "hostmaster." + f.zone, SOASerial: 7,
			TSIGKeyID: kid, AllowTransfer: "key:" + f.key,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := main.Zones().AddRecord(ctx, ZoneRecord{ZoneID: zid, Name: "www", Type: "A", TTL: 300, RData: "10.0.0.1", Enabled: true}); err != nil {
			t.Fatal(err)
		}
		if err := main.Settings().Set(ctx, "blocking.mode", "nxdomain"); err != nil {
			t.Fatal(err)
		}
		if err := main.Settings().Set(ctx, "serve.dot.listen", ":8853"); err != nil {
			t.Fatal(err)
		}

		b, err := main.ExportBundle(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if b.Format != BundleFormat {
			t.Errorf("bundle format = %d, want %d", b.Format, BundleFormat)
		}
		if want, err := main.Settings().ConfigVersion(ctx); err != nil || b.ConfigVersion != want {
			t.Errorf("bundle config_version = %d (err %v), want the main's %d", b.ConfigVersion, err, want)
		}
		if _, local := b.Settings["serve.dot.listen"]; local {
			t.Error("serve.* is instance-local and must not be exported")
		}
		if b.Settings["blocking.mode"] != "nxdomain" {
			t.Errorf("blocking.mode in the bundle = %q, want nxdomain", b.Settings["blocking.mode"])
		}
		var exported *Zone
		for i, z := range b.Zones {
			if z.Type == "internal" {
				t.Fatalf("built-in zone %q rode in the bundle; both boxes seed their own (§4.2)", z.Name)
			}
			if z.ID == zid {
				exported = &b.Zones[i]
			}
		}
		if exported == nil {
			t.Fatalf("zone %d is not in the bundle: %+v", zid, b.Zones)
		}
		if exported.Name != f.zone || exported.Type != "primary" || exported.TSIGKeyID != kid || exported.AllowTransfer != "key:"+f.key {
			t.Errorf("exported zone = %+v, want the definition the main holds", *exported)
		}

		if err := replica.ImportBundle(ctx, b); err != nil {
			t.Fatal(err)
		}

		cs, err := replica.Clients().Clients(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var client Client
		for _, c := range cs {
			if c.ID == cid {
				client = c
			}
		}
		if client.GroupID != gid || client.Name != "tablet" || client.Matcher != f.matcher {
			t.Errorf("client %d on the replica = %+v, want the main's row in group %d", cid, client, gid)
		}
		ls, err := replica.Filters().ListsForGroup(ctx, gid)
		if err != nil || len(ls) != 1 || ls[0].ID != lid || ls[0].URL != f.listURL {
			t.Errorf("lists for group %d on the replica = %+v (err %v), want list %d", gid, ls, err, lid)
		}
		rs, err := replica.Filters().Rules(ctx, gid)
		if err != nil || len(rs) != 1 || rs[0].ID != rid || rs[0].Pattern != "cdn.example" {
			t.Errorf("rules for group %d on the replica = %+v (err %v), want rule %d", gid, rs, err, rid)
		}
		if k, ok, err := replica.TSIGKeys().Get(ctx, kid); err != nil || !ok || k.Secret != "c2VjcmV0" || k.Name != f.key {
			t.Errorf("key %d on the replica = %+v (ok %v, err %v), want the main's secret", kid, k, ok, err)
		}
		if v, ok, err := replica.Settings().Get(ctx, "blocking.mode"); err != nil || !ok || v != "nxdomain" {
			t.Errorf("blocking.mode on the replica = %q (ok %v, err %v)", v, ok, err)
		}
		if _, ok, err := replica.Settings().Get(ctx, "serve.dot.listen"); err != nil || ok {
			t.Errorf("serve.dot.listen reached the replica (ok %v, err %v); §4.3 keeps it local", ok, err)
		}

		// Records never travel: the replica's secondary transfers them.
		recs, err := replica.Zones().Records(ctx, zid)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			if r.Name == "www" && r.RData == "10.0.0.1" {
				t.Fatalf("the bundle carried a zone record: %+v", r)
			}
		}

		// §4.2: the built-ins are seeded by each box's own migrations and
		// are in no bundle, so an import must leave every one of them alone.
		zs, err := replica.Zones().Zones(ctx)
		if err != nil {
			t.Fatal(err)
		}
		internal := map[string]string{}
		for _, z := range zs {
			internal[z.Name] = z.Type
		}
		for _, name := range BuiltinZones {
			if internal[name] != "internal" {
				t.Errorf("built-in zone %q on the replica is %q after an import that carried none, want it untouched", name, internal[name])
			}
		}

		// Re-applying the same bundle is what the replica does every time
		// the main's version moves without its config changing.
		if err := replica.ImportBundle(ctx, b); err != nil {
			t.Fatalf("second import of the same bundle: %v", err)
		}

		// §4.1: the sequences are advanced past the imported ids, so a
		// promotion does not hand a new row an id the main already used.
		nid, err := replica.Clients().AddGroup(ctx, f.group+"-after")
		if err != nil || nid <= gid {
			t.Fatalf("group added on the replica after an import = %d (err %v), want an id above the imported %d", nid, err, gid)
		}
	})
}

// TestImportBundleDeletesWhatTheBundleLacks is the delete half of §5: a
// bundle is the whole config, so a row the main dropped has to go, children
// first — these foreign keys have no ON DELETE CASCADE.
func TestImportBundleDeletesWhatTheBundleLacks(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := t.Context()
		f := newFixture()
		gid, err := s.Clients().AddGroup(ctx, f.group)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Clients().AddClient(ctx, Client{Name: "c", Matcher: f.matcher, GroupID: gid}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Filters().AddRule(ctx, Rule{GroupID: gid, Action: "block", Pattern: "ads.example"}); err != nil {
			t.Fatal(err)
		}
		b, err := s.ExportBundle(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// Drop the group from the bundle; its client and its rule must go
		// with it.
		b.Groups = b.Groups[:len(b.Groups)-1]
		b.Clients = nil
		b.Rules = nil
		if err := s.ImportBundle(ctx, b); err != nil {
			t.Fatal(err)
		}
		gs, err := s.Clients().Groups(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range gs {
			if g.ID == gid {
				t.Fatal("the group survived an import whose bundle lacked it")
			}
		}
	})
}

// TestImportBundleIsAllOrNothing is §5's "partial application is not a state
// the replica can be in", and the version bump is part of it: a replica that
// recorded a version it did not manage to apply would stop pulling the config
// it is missing.
func TestImportBundleIsAllOrNothing(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := t.Context()
		f := newFixture()
		before, err := s.Settings().ConfigVersion(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.ExportBundle(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b.Clients = append(b.Clients, Client{ID: 99, Name: "orphan", Matcher: f.matcher, GroupID: 4242}) // dangling group_id
		if err := s.ImportBundle(ctx, b); err == nil {
			t.Fatal("an import with a dangling group_id succeeded")
		}
		after, err := s.Settings().ConfigVersion(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Errorf("config_version moved %d -> %d on a failed import", before, after)
		}
		cs, err := s.Clients().Clients(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			if c.ID == 99 {
				t.Fatal("a partial import left a row behind")
			}
		}
	})
}

// TestImportBundleRefusesUnknownFormat is §10's "upgrade the replica first":
// a format this build does not know is refused whole, not applied in the part
// it happens to understand.
func TestImportBundleRefusesUnknownFormat(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := t.Context()
		b, err := s.ExportBundle(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b.Format = BundleFormat + 1
		if err := s.ImportBundle(ctx, b); err == nil || !strings.Contains(err.Error(), "format") {
			t.Fatalf("import of an unknown format = %v, want a refusal naming the format", err)
		}
	})
}

// TestImportBundleRefusesToOverwriteABuiltinZone is the other half of §4.2's
// "both boxes seed their own". An id that lands on an RFC 6303 zone would
// rewrite it into a synced one, and the prune exempts internal zones, so that
// row would then be stuck on this box for good. Refused rather than silently
// skipped: two boxes disagreeing with nothing to say so is worse than a pull
// that fails and says why.
func TestImportBundleRefusesToOverwriteABuiltinZone(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := t.Context()
		f := newFixture()
		zid, err := s.Zones().AddZone(ctx, Zone{Name: f.zone, Type: "primary", Enabled: true, SOANS: "ns1." + f.zone})
		if err != nil {
			t.Fatal(err)
		}
		zs, err := s.Zones().Zones(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var builtin int64
		for _, z := range zs {
			if z.Name == BuiltinZones[0] {
				builtin = z.ID
			}
		}
		if builtin == 0 {
			t.Fatalf("no built-in zone %q to collide with", BuiltinZones[0])
		}

		b, err := s.ExportBundle(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i := range b.Zones {
			if b.Zones[i].ID == zid {
				b.Zones[i].ID = builtin
			}
		}
		if err := s.ImportBundle(ctx, b); err == nil || !strings.Contains(err.Error(), BuiltinZones[0]) {
			t.Fatalf("import colliding with a built-in zone = %v, want a refusal naming it", err)
		}
		if z, err := s.Zones().Zone(ctx, builtin); err != nil || z.Name != BuiltinZones[0] || z.Type != "internal" {
			t.Errorf("built-in zone %d after the refused import = %+v (err %v)", builtin, z, err)
		}
		// The refusal is a rollback like any other: the prune that ran
		// before it must not have taken the zone the bundle no longer named.
		if _, err := s.Zones().Zone(ctx, zid); err != nil {
			t.Errorf("zone %d after the refused import: %v", zid, err)
		}
	})
}

// TestImportBundleLeavesLocalSettingsAlone is §4.3 enforced on the receiving
// side. A bundle arrives over HTTP from another box, so filtering on export
// is only half of it: a main that names this replica's listen address must
// not be able to move it, and a key this box set for itself must not be
// pruned as "absent from the bundle".
func TestImportBundleLeavesLocalSettingsAlone(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := t.Context()
		if err := s.Settings().Set(ctx, "serve.dot.listen", ":8853"); err != nil {
			t.Fatal(err)
		}
		if err := s.Settings().Set(ctx, "sync.peer_url", "https://main.example"); err != nil {
			t.Fatal(err)
		}
		b, err := s.ExportBundle(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// One local key the bundle names, one it does not: overwriting and
		// pruning are two different ways to lose the same setting.
		b.Settings["serve.dot.listen"] = ":9999"
		if err := s.ImportBundle(ctx, b); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{"serve.dot.listen": ":8853", "sync.peer_url": "https://main.example"} {
			if v, ok, err := s.Settings().Get(ctx, key); err != nil || !ok || v != want {
				t.Errorf("%s after an import = %q (ok %v, err %v), want this instance's own %q", key, v, ok, err, want)
			}
		}
	})
}

// TestLocalSettingKey pins §4.3's table. It is the one place that decides
// what never leaves an instance, and the cost of a wrong answer is a replica
// listening on the main's certificate paths or pointing at itself as its own
// peer.
func TestLocalSettingKey(t *testing.T) {
	for _, key := range []string{"instance.id", "serve.dot.listen", "serve.tls.cert", "sync.peer_url", "sync.token", StatsWatermarkKey} {
		if !LocalSettingKey(key) {
			t.Errorf("LocalSettingKey(%q) = false, want true — §4.3 keeps it on the instance", key)
		}
	}
	for _, key := range []string{"upstreams", "blocking.mode", "blocking.pauses", "cache.min_ttl", "instances"} {
		if LocalSettingKey(key) {
			t.Errorf("LocalSettingKey(%q) = true, want false — §4.3 syncs everything else", key)
		}
	}
}
