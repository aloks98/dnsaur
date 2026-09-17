package app

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/api"
	"github.com/aloks98/dnsaur/internal/auth"
	"github.com/aloks98/dnsaur/internal/config"
	"github.com/aloks98/dnsaur/internal/filter"
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
	// An hour between the replica's own polls, so its background loop can
	// overlap the pulls this test drives at most once.
	replica := newTestAppWith(t, replicaConfig(t), withUpstreams(pub),
		withSetting("sync.interval_seconds", "3600"))

	// --- the main: a primary zone nobody may transfer yet. No sync key is
	// made here: §6 says the first pairing makes it, so a key present before
	// the follow below would hide a main that never creates one.
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

	// --- the replica follows: a code off the main's screen, typed into this
	// box's Sync band, both over the real listeners. The override goes in
	// first because it has to be in place before the pull the follow kicks —
	// the fixture binds DNS on port 0, so the port the main advertises is no
	// address to transfer from (§8).
	instanceID := mustGetSetting(t, replica, "instance.id")
	mustSetMany(t, replica, map[string]string{"sync.primary_dns": main.DNSAddr()})
	mustFollow(t, replica, main)

	// §6: the box is registered by the pairing alone — no separate call —
	// and the pairing is also what designated the key the transfer below is
	// signed with.
	status := mainStatus(t, main, token)
	if status.Role != "main" || len(status.Replicas) != 1 {
		t.Fatalf("GET /sync/status after the follow = {role:%q replicas:%d}, want one registered replica",
			status.Role, len(status.Replicas))
	}
	if r := status.Replicas[0]; r.InstanceID != instanceID || r.DNSAddr != replica.DNSAddr() {
		t.Fatalf("registered replica = %+v, want {instance_id:%q dns_addr:%q}", r, instanceID, replica.DNSAddr())
	}
	syncKeyName := status.SyncKey
	keyID, err := strconv.ParseInt(mustGetSetting(t, main, "sync.tsig_key_id"), 10, 64)
	if err != nil || keyID == 0 || syncKeyName == "" {
		t.Fatalf("the pairing designated sync key %d (%q), err %v; want a key it created itself",
			keyID, syncKeyName, err)
	}

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

	// The entry catches up on the version this box applied: a probe carries
	// what the replica had installed when it made the call, so the number
	// lands on the cycle after the one that applied the bundle, and the
	// probe is the only thing that ever stamps it.
	mustPull(t, replica)
	version := mustConfigVersion(t, main)
	status = mainStatus(t, main, token)
	if len(status.Replicas) != 1 {
		t.Fatalf("the main lists %d replicas, want the one that paired", len(status.Replicas))
	}
	if r := status.Replicas[0]; r.VersionApplied != version || r.Stale {
		t.Fatalf("registered replica = %+v, want {version_applied:%d stale:false}", r, version)
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

	// --- §6's Forget, which revokes the secret with the entry. The next
	// pull is refused at the probe, and what the Sync band shows for it is
	// the refusal rather than silence: an operator who removed the wrong box
	// has to be able to see why it stopped.
	code, body = apiSend(t, http.MethodDelete,
		httpURL(t, main)+"/api/v1/sync/replicas/"+instanceID, token, "")
	if code != http.StatusNoContent {
		t.Fatalf("DELETE /sync/replicas/%s = %d %q, want 204", instanceID, code, body)
	}
	if err := replica.replica.PullOnce(ctx); err == nil {
		t.Fatal("the replica pulled with a secret the main had forgotten")
	}
	if e := mustGetSetting(t, replica, "sync.last_error"); !strings.Contains(e, "401") {
		t.Fatalf("sync.last_error = %q after the main forgot this replica, want the 401 its probe got", e)
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
//
// Both transports are probed, because dnssrv binds UDP and TCP on the port
// it is given and retries neither: a port free for one and taken for the
// other is a start that fails rather than a test that adapts.
func replicaConfig(t *testing.T) *config.Config {
	t.Helper()
	return boxConfig(t, "127.0.0.1")
}

// boxConfig is replicaConfig with the address named, for the DHCP loopback:
// its two boxes have to sit on different hosts, because what they render for
// each other's HA peer is built from those hosts on one side and from the
// pairing on the other, and two boxes on one address could not tell the two
// apart.
func boxConfig(t *testing.T, host string) *config.Config {
	t.Helper()
	pc, err := net.ListenPacket("udp", host+":0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	addr := pc.LocalAddr().String()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		_ = pc.Close()
		t.Fatalf("the free udp port %s is taken on tcp: %v", addr, err)
	}
	if err := pc.Close(); err != nil {
		t.Fatalf("closing the probe socket: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("closing the probe listener: %v", err)
	}
	cfg := testConfigOn(t, "sqlite")
	cfg.DNSListen = []string{addr}
	return cfg
}

// mustFollow pairs a box with its main the way an operator does: a code
// minted on the main's screen, typed into the replica's Sync band, both over
// the real listeners and with nothing written into either store by hand.
//
// It returns once the pull that Follow kicks has finished. Follow answers as
// soon as the pairing is stored, on purpose, so a test that read the
// replica's configuration straight away would be reading it half-applied.
func mustFollow(t *testing.T, replica, main *App) {
	t.Helper()
	body := `{"peer_url":` + strconv.Quote(httpURL(t, main)) +
		`,"code":` + strconv.Quote(pairingCode(t, main)) + `}`
	if code, got := apiPost(t, httpURL(t, replica)+"/api/v1/sync/follow",
		writeAPIToken(t, replica), body); code != http.StatusNoContent {
		t.Fatalf("POST /sync/follow = %d %q, want 204", code, got)
	}
	waitFor(t, "the first pull after following to finish", func() bool {
		all, err := replica.Store().Settings().All(t.Context())
		return err == nil && all["sync.applied_version"] != "" && all["sync.last_pull_at"] != ""
	})
}

// pairingCode mints one code over the main's own listener, the way the Sync
// band's "Add replica" does.
func pairingCode(t *testing.T, main *App) string {
	t.Helper()
	code, body := apiPost(t, httpURL(t, main)+"/api/v1/sync/pairing-code", writeAPIToken(t, main), "")
	if code != http.StatusOK {
		t.Fatalf("POST /sync/pairing-code = %d %q, want 200", code, body)
	}
	var minted struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &minted); err != nil || minted.Code == "" {
		t.Fatalf("POST /sync/pairing-code answered %q: %v", body, err)
	}
	return minted.Code
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

// mustPull runs one probe/bundle/apply cycle, the same one the replica's own
// loop runs every interval.
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
	return apiSend(t, http.MethodPost, url, token, body)
}

// apiSend is apiPost for the other methods: a real request over the box's own
// listener, answered by the same handler chain a browser reaches.
func apiSend(t *testing.T, method, url, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
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

// TestReplicaFollowsBlockingPauses is §4.3's `blocking.pauses`: a pause is
// configuration like any other, so the box that holds the config decides it
// and every replica serves it. Both halves have to work for that — the pause
// has to move config_version, or the replica's next probe finds nothing to
// pull, and the reload has to put the row back into the engine, or the
// replica imports a pause it never applies.
func TestReplicaFollowsBlockingPauses(t *testing.T) {
	pub := mockDNS(t, answerA("9.9.9.9"))
	main := newTestApp(t, withUpstreams(pub))
	replica := newTestAppWith(t, replicaConfig(t), withUpstreams(pub),
		withSetting("sync.interval_seconds", "3600"))
	mustSetMany(t, replica, map[string]string{"sync.primary_dns": main.DNSAddr()})
	mustFollow(t, replica, main)

	// Paused on the main, the way POST /blocking/pause pauses it.
	main.engine.Pause(filter.PauseGlobal, 0, 30*time.Minute)
	mustPull(t, replica)
	until, kind := replica.engine.PausedUntil(0, 0)
	if until.IsZero() || kind != filter.PauseGlobal {
		t.Fatalf("the replica reports pause {until:%v scope:%q}, want the main's global pause", until, kind)
	}
	if want, _ := main.engine.PausedUntil(0, 0); !until.Equal(want) {
		t.Fatalf("the replica's pause ends at %v, want the main's %v", until, want)
	}

	// And resumed: a pause that cannot be cleared is worse than one that
	// never arrived.
	main.engine.Pause(filter.PauseGlobal, 0, 0)
	mustPull(t, replica)
	if until, _ := replica.engine.PausedUntil(0, 0); !until.IsZero() {
		t.Fatalf("the replica is still paused until %v after the main resumed", until)
	}
}

// TestAFollowingReplicaFetchesTheListsItWasGiven: a bundle carries a list's
// URL, never its contents, and the replica's own refresher runs on
// lists.refresh_hours — a day, by default. So a box that has just started
// following a main has every list it was given and no copy of any of them,
// and without a download kicked by the pull it serves a LAN with blocking
// configured and nothing blocked until that ticker comes round.
func TestAFollowingReplicaFetchesTheListsItWasGiven(t *testing.T) {
	ctx := t.Context()
	const blocked = "ads.example.com"
	listSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("0.0.0.0 " + blocked + "\n"))
	}))
	t.Cleanup(listSrv.Close)

	pub := mockDNS(t, answerA("9.9.9.9"))
	main := newTestApp(t, withUpstreams(pub))
	lid, err := main.Store().Filters().AddList(ctx, store.List{URL: listSrv.URL, Kind: "block", Enabled: true})
	if err != nil {
		t.Fatalf("AddList: %v", err)
	}
	// Group 1 is the default group every unknown client lands in.
	if err := main.Store().Filters().AssignList(ctx, 1, lid); err != nil {
		t.Fatalf("AssignList: %v", err)
	}

	replica := newTestAppWith(t, replicaConfig(t), withUpstreams(pub), withLoopbackLists(),
		withSetting("sync.interval_seconds", "3600"))
	mustSetMany(t, replica, map[string]string{"sync.primary_dns": main.DNSAddr()})
	// Nothing here waits on the download: the pull kicks it and returns, so
	// what the test waits for is the replica's answer changing.
	mustFollow(t, replica, main)
	if l, err := replica.Store().Filters().Lists(ctx); err != nil || len(l) != 1 || l[0].ID != lid {
		t.Fatalf("the list did not travel: %+v (err %v)", l, err)
	}
	waitFor(t, "the replica to block "+blocked+" from the list it was given", func() bool {
		return digAQuiet(replica.DNSAddr(), blocked) == "0.0.0.0"
	})
}
