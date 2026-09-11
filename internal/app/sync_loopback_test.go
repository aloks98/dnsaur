package app

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/api"
	"github.com/aloks98/dnsaur/internal/auth"
	"github.com/aloks98/dnsaur/internal/config"
	"github.com/aloks98/dnsaur/internal/store"
)

// Config sync end to end: a main and a replica, both real Apps, in one
// process. Every other test in this milestone has one half of a seam mocked
// — the pull loop against an httptest peer, the registry against a fake
// store, the notifier against a socket that only counts packets. Here both
// halves are ours at once, which is the only place the pieces Tasks 1–5
// built can be caught disagreeing: a bundle the replica imports but cannot
// derive a transferable zone from, a registration the main records in a
// shape its own transfer gate will not match, a NOTIFY signed with a key the
// other end resolves differently.
//
// Spec: docs/superpowers/specs/2026-09-11-config-sync-design.md — §3 (the
// version probe and the bundle), §5 (apply order), §6 (a registered replica
// transfers and is notified with no ACL edit), §7 (a replica refuses writes
// to synced configuration).

const (
	// syncZone is the main's own primary, and therefore the replica's
	// derived secondary (§4.2).
	syncZone = "e412.in"
	// syncKeyName is the key sync.tsig_key_id designates: the one a
	// replica's AXFR is signed with and a NOTIFY to it is signed under.
	syncKeyName = "sync.e412.in."
	// notifyWait bounds the one leg that is not driven by the test: the
	// replica transferring because a NOTIFY told it to. It is longer than
	// the five seconds the rest of this file waits because the replica's
	// inbound throttle (zones.notifyThrottle) defers a NOTIFY that arrives
	// inside an open window to the end of it, and the main's own notifier
	// ticks every five seconds — so a background pass can open that window
	// just before the pass this test drives.
	notifyWait = 10 * time.Second
)

func TestReplicaFollowsMainAndTransfersItsZones(t *testing.T) {
	ctx := t.Context()
	// A mock for the default upstreams on both boxes, so nothing in this
	// test can reach the public internet: "upstreams" is synced, so the
	// replica inherits it with the bundle.
	pub := mockDNS(t, answerA("9.9.9.9"))
	main := newTestApp(t, withUpstreams(pub))
	replica := newTestAppWith(t, replicaConfig(t), withUpstreams(pub))

	// --- the main: a sync key, a primary zone, and the token the replica pulls with.
	keyID := mustTSIGKey(t, main, syncKeyName)
	mustSetting(t, main, "sync.tsig_key_id", strconv.FormatInt(keyID, 10))
	zoneID := mustAddZone(t, main, store.Zone{
		Name: syncZone, Type: "primary", Enabled: true,
		SOANS: "ns1." + syncZone, SOAMbox: "hostmaster." + syncZone,
		SOASerial: 42, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
		SOAMinimum: 900, SOATTL: 900,
	})
	mustAddRecord(t, main, zoneID, "@", "NS", "ns1."+syncZone+".")
	mustAddRecord(t, main, zoneID, "ns1", "A", "10.20.0.1")
	mustAddRecord(t, main, zoneID, "www", "A", "10.20.0.5")
	mustReloadZones(t, main)
	// The premise of §6: nobody is allowed to transfer this zone by ACL, so
	// a transfer that succeeds below succeeded because the replica is
	// registered — not because the fixture left the door open.
	if acl := mustZone(t, main, zoneID).AllowTransfer; acl != "" {
		t.Fatalf("the main's zone allows transfers to %q; §6's implicit allow would not be what let the replica in", acl)
	}
	token := writeAPIToken(t, main)

	// --- the replica: three settings, one pull.
	mustSetMany(t, replica, map[string]string{
		"sync.peer_url":    httpURL(t, main),
		"sync.token":       token,
		"sync.primary_dns": main.DNSAddr(),
	})
	mustPull(t, replica)

	// §4.2: the main's primary arrives as a secondary of the main, signed
	// with the sync key, under the main's own id.
	got := zoneNamed(t, replica, syncZone)
	if got.ID != zoneID {
		t.Fatalf("the derived zone has id %d, want the main's %d", got.ID, zoneID)
	}
	if got.Type != "secondary" || got.Primaries != main.DNSAddr() || got.TSIGKeyID != keyID {
		t.Fatalf("derived zone = {type:%q primaries:%q tsig_key_id:%d}, want a secondary of %q under key %d",
			got.Type, got.Primaries, got.TSIGKeyID, main.DNSAddr(), keyID)
	}
	// The key itself travelled, or the AXFR below could not be signed.
	k, found, err := replica.Store().TSIGKeys().Get(ctx, keyID)
	if err != nil || !found {
		t.Fatalf("the sync key did not reach the replica: found=%v err=%v", found, err)
	}
	if k.Name != syncKeyName {
		t.Fatalf("the sync key arrived as %q, want %q", k.Name, syncKeyName)
	}

	// §6: the pull registered this box, and the main reports it over its own
	// API with the version it applied.
	version := mustConfigVersion(t, main)
	instanceID := mustGetSetting(t, replica, "instance.id")
	status := mainStatus(t, main, token)
	if status.Role != "main" || len(status.Replicas) != 1 {
		t.Fatalf("GET /sync/status on the main = {role:%q replicas:%d}, want one registered replica", status.Role, len(status.Replicas))
	}
	if r := status.Replicas[0]; r.InstanceID != instanceID || r.DNSAddr != replica.DNSAddr() ||
		r.VersionApplied != version || r.Stale {
		t.Fatalf("registered replica = %+v, want {instance_id:%q dns_addr:%q version_applied:%d stale:false}",
			r, instanceID, replica.DNSAddr(), version)
	}
	if status.SyncKey != syncKeyName {
		t.Fatalf("the main reports sync_key %q, want %q", status.SyncKey, syncKeyName)
	}

	// The transfer §6 exists for: no ACL names the replica, and the AXFR is
	// let through on the strength of the registration and the sync key.
	if _, err := replica.zoneRefresh.Refresh(ctx, zoneID); err != nil {
		t.Fatalf("the replica could not transfer %s from the main: %v", syncZone, err)
	}
	if a := digA(t, replica.DNSAddr(), "www."+syncZone); a != "10.20.0.5" {
		t.Fatalf("the replica answers www.%s with %s, want 10.20.0.5", syncZone, a)
	}

	// --- a record added on the main, announced by NOTIFY.
	mustAddRecord(t, main, zoneID, "nas", "A", "10.20.0.6")
	if err := main.Store().Zones().BumpSerial(ctx, zoneID); err != nil {
		t.Fatal(err)
	}
	mustReloadZones(t, main)
	serial := uint32(mustZone(t, main, zoneID).SOASerial)
	if err := main.notifier.Pass(ctx); err != nil {
		t.Fatalf("the main's notify pass failed: %v", err)
	}
	// The replica's gate admitted it: a refused NOTIFY is answered with a
	// refusal rcode, which the sender records as a failed round — last_error
	// set and notified_serial left where it was.
	row := notifyRow(t, main, zoneID, replica.DNSAddr())
	if row.NotifiedSerial != serial || row.LastError != "" {
		t.Fatalf("the replica did not admit the NOTIFY: notified_serial=%d (want %d), last_error=%q",
			row.NotifiedSerial, serial, row.LastError)
	}
	// And acted on it: the transfer below is the replica's own, driven by
	// the NOTIFY rather than by this test.
	waitUntil(t, notifyWait, "the replica to answer nas."+syncZone+" after the NOTIFY", func() bool {
		return digAQuiet(replica.DNSAddr(), "nas."+syncZone) == "10.20.0.6"
	})
	if s := uint32(zoneNamed(t, replica, syncZone).SOASerial); s != serial {
		t.Fatalf("the replica holds serial %d after the notified transfer, want the main's %d", s, serial)
	}

	// --- a synced-table write, not a settings write: it has to move
	// config_version, or the replica's next probe finds nothing to pull and
	// the group never arrives.
	before := mustConfigVersion(t, main)
	groupID, err := main.Store().Clients().AddGroup(ctx, "kids")
	if err != nil {
		t.Fatal(err)
	}
	after := mustConfigVersion(t, main)
	if after <= before {
		t.Fatalf("adding a group left config_version at %d (was %d); a replica polls that counter", after, before)
	}
	mustPull(t, replica)
	if name := groupNamed(t, replica, groupID); name != "kids" {
		t.Fatalf("group %d on the replica is %q, want the main's %q", groupID, name, "kids")
	}
	if s := replica.Status(); s.Role != "replica" || s.AppliedVersion != after {
		t.Fatalf("replica status = {role:%q applied_version:%d}, want a replica at the main's %d",
			s.Role, s.AppliedVersion, after)
	}

	// --- §7: the replica holds the config it was given, and says so.
	code, body := apiPost(t, httpURL(t, replica)+"/api/v1/groups", writeAPIToken(t, replica), `{"name":"local"}`)
	if code != http.StatusConflict || !strings.Contains(body, "managed by") {
		t.Fatalf("POST /groups on the replica = %d %q, want 409 naming the main it is managed by", code, body)
	}
}

// replicaConfig is the fixture's config with the DNS listener on a port
// chosen in advance.
//
// The address a box registers with its main is its configured dns_listen
// verbatim (App.New), so the fixture's "127.0.0.1:0" would have the main
// record a port nothing listens on: the transfer gate compares addresses
// only and would still let the AXFR through, but no NOTIFY could ever be
// delivered. Reserving the port and handing it over closes that gap, and a
// port taken between the reservation and the bind cannot pass unnoticed:
// the registration assertion compares what the main recorded with the
// address the App is actually listening on.
func replicaConfig(t *testing.T) *config.Config {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	if err := pc.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := testConfigOn(t, "sqlite")
	cfg.DNSListen = []string{addr}
	return cfg
}

// mustTSIGKey creates a key both boxes will sign with; the replica gets its
// secret in the bundle.
func mustTSIGKey(t *testing.T, a *App, name string) int64 {
	t.Helper()
	id, err := a.Store().TSIGKeys().Create(t.Context(), store.TSIGKey{
		Name: name, Algorithm: "hmac-sha256.",
		Secret: base64.StdEncoding.EncodeToString([]byte("a-config-sync-secret-of-length")),
	})
	if err != nil {
		t.Fatalf("creating the sync key: %v", err)
	}
	return id
}

func mustSetMany(t *testing.T, a *App, values map[string]string) {
	t.Helper()
	if err := a.Store().Settings().SetMany(t.Context(), values); err != nil {
		t.Fatalf("SetMany(%v): %v", values, err)
	}
}

func mustGetSetting(t *testing.T, a *App, key string) string {
	t.Helper()
	v, found, err := a.Store().Settings().Get(t.Context(), key)
	if err != nil || !found {
		t.Fatalf("Get(%s): found=%v err=%v", key, found, err)
	}
	return v
}

func mustAddRecord(t *testing.T, a *App, zoneID int64, name, typ, rdata string) {
	t.Helper()
	_, err := a.Store().Zones().AddRecord(t.Context(), store.ZoneRecord{
		ZoneID: zoneID, Name: name, Type: typ, TTL: 300, RData: rdata, Enabled: true,
	})
	if err != nil {
		t.Fatalf("AddRecord(%s %s): %v", name, typ, err)
	}
}

func mustConfigVersion(t *testing.T, a *App) int64 {
	t.Helper()
	v, err := a.Store().Settings().ConfigVersion(t.Context())
	if err != nil {
		t.Fatalf("ConfigVersion: %v", err)
	}
	return v
}

// mustPull runs one probe/bundle/apply/register cycle, the same one the
// replica's own loop runs every interval.
func mustPull(t *testing.T, a *App) {
	t.Helper()
	if err := a.replica.PullOnce(t.Context()); err != nil {
		t.Fatalf("PullOnce: %v", err)
	}
}

func zoneNamed(t *testing.T, a *App, name string) store.Zone {
	t.Helper()
	all, err := a.Store().Zones().Zones(t.Context())
	if err != nil {
		t.Fatalf("Zones: %v", err)
	}
	for _, z := range all {
		if z.Name == name {
			return z
		}
	}
	t.Fatalf("no zone %q; the store holds %d", name, len(all))
	return store.Zone{}
}

func groupNamed(t *testing.T, a *App, id int64) string {
	t.Helper()
	all, err := a.Store().Clients().Groups(t.Context())
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	for _, g := range all {
		if g.ID == id {
			return g.Name
		}
	}
	t.Fatalf("no group %d; the store holds %d", id, len(all))
	return ""
}

// notifyRow is what the main recorded about one NOTIFY target: the sender's
// own account of whether the other end took it.
func notifyRow(t *testing.T, a *App, zoneID int64, target string) store.ZoneNotify {
	t.Helper()
	rows, err := a.Store().Notifies().ByZone(t.Context(), zoneID)
	if err != nil {
		t.Fatalf("ByZone(%d): %v", zoneID, err)
	}
	for _, r := range rows {
		if r.Target == target {
			return r
		}
	}
	t.Fatalf("no notify row for %q; the zone has %d", target, len(rows))
	return store.ZoneNotify{}
}

// writeAPIToken mints a write-scoped API token for a box's admin account,
// creating the account on first use.
func writeAPIToken(t *testing.T, a *App) string {
	t.Helper()
	ctx := t.Context()
	svc := auth.New(a.Store().Users(), a.Store().Tokens())
	// ErrSetupDone on the second call for the same box, which is not a
	// failure: the account is what this needs, not its creation.
	_ = svc.CreateAdmin(ctx, "admin", "password123")
	u, found, err := a.Store().Users().ByUsername(ctx, "admin")
	if err != nil || !found {
		t.Fatalf("reading the admin account: found=%v err=%v", found, err)
	}
	_, plain, err := svc.CreateAPIToken(ctx, u.ID, "sync", "write", 0)
	if err != nil {
		t.Fatalf("minting an API token: %v", err)
	}
	return plain
}

// httpURL is the peer URL shape sync.peer_url takes: scheme and host, no
// path. HTTPAddr reports the wildcard the fixture binds, so the loopback
// address is named explicitly rather than dialled through "[::]".
func httpURL(t *testing.T, a *App) string {
	t.Helper()
	_, port, err := net.SplitHostPort(a.HTTPAddr())
	if err != nil {
		t.Fatalf("HTTPAddr %q: %v", a.HTTPAddr(), err)
	}
	return "http://127.0.0.1:" + port
}

// mainStatus reads GET /sync/status over the main's real listener.
func mainStatus(t *testing.T, a *App, token string) api.SyncStatus {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, httpURL(t, a)+"/api/v1/sync/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sync/status: %s", resp.Status)
	}
	var s api.SyncStatus
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatalf("decoding /sync/status: %v", err)
	}
	return s
}

// apiPost issues an authenticated POST and returns the status and body, for
// the one call whose refusal is the point.
func apiPost(t *testing.T, url, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}
