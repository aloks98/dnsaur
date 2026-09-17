package confsync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/api"
	"github.com/aloks98/dnsaur/internal/store"
)

// openStore is the in-test store both halves of a pull run against.
// internal/storetest only starts postgres; sqlite needs no helper beyond
// this one, and the store's own tests are what cover the two dialects.
func openStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), "sqlite", t.TempDir()+"/t.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// countingReloader counts what a pull asked to be reloaded. Called from the
// test's own goroutine only, so plain ints are enough.
type countingReloader struct {
	clients, filters, zones, settings int

	mu        sync.Mutex
	downloads int
}

func (c *countingReloader) ReloadClients(context.Context) error    { c.clients++; return nil }
func (c *countingReloader) RecompileFilters(context.Context) error { c.filters++; return nil }
func (c *countingReloader) ReloadZones(context.Context) error      { c.zones++; return nil }
func (c *countingReloader) ReloadSettings(context.Context) error   { c.settings++; return nil }

// RefreshFilters is the one a pull calls off its own goroutine, so it is
// counted under a lock rather than with the plain ints above.
func (c *countingReloader) RefreshFilters(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.downloads++
	return nil
}

func (c *countingReloader) downloadCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.downloads
}

// pairCode is the code the fixture main pairs on; anything else is what a
// main with no live code, or the wrong code, refuses (§6).
const pairCode = "K7PQ-4M2X"

// mainFixture is the main a pull runs against: the endpoints a replica
// calls, served out of a real store, with what arrived on each recorded so
// a test can say what went over the wire.
type mainFixture struct {
	*httptest.Server
	// dnsPort is what the probe advertises, settable mid-test because the
	// replica is supposed to keep the last port a main named.
	dnsPort atomic.Int64
	// hold and release are the main sitting on a bundle: a test that needs
	// a pull still in flight closes release when it is done with it.
	hold    atomic.Bool
	release chan struct{}
	once    sync.Once

	mu      sync.Mutex
	probes  []probeRecord
	pairs   []api.PairRequest
	bundles int
	stray   []string
}

// probeRecord is one version probe: the query it carried — the version this
// box has applied, which is the heartbeat (§3) — and the credential it
// presented.
type probeRecord struct{ query, auth string }

// fakeMain serves what a replica calls, answering dnsPort as the DNS port it
// advertises. token is what GET /sync/bundle demands.
func fakeMain(t *testing.T, st store.Store, token string, dnsPort int) *mainFixture {
	t.Helper()
	ctx := t.Context()
	f := &mainFixture{release: make(chan struct{})}
	f.dnsPort.Store(int64(dnsPort))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/sync/version", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.probes = append(f.probes, probeRecord{query: r.URL.RawQuery, auth: r.Header.Get("Authorization")})
		f.mu.Unlock()
		v, _ := st.Settings().ConfigVersion(ctx)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"config_version": v, "instance_id": "main-1", "dns_port": f.dnsPort.Load(),
		})
	})
	mux.HandleFunc("GET /api/v1/sync/bundle", func(w http.ResponseWriter, r *http.Request) {
		if f.hold.Load() {
			<-f.release
		}
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.bundles++
		f.mu.Unlock()
		b, err := st.ExportBundle(ctx)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(b)
	})
	mux.HandleFunc("POST /api/v1/sync/pair", func(w http.ResponseWriter, r *http.Request) {
		var req api.PairRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.pairs = append(f.pairs, req)
		f.mu.Unlock()
		if normalizePairingCode(req.Code) != normalizePairingCode(pairCode) {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "pairing code refused"})
			return
		}
		_ = json.NewEncoder(w).Encode(api.PairResult{Secret: "s3", DNSPort: int(f.dnsPort.Load())})
	})
	// Everything else, POST /sync/replicas first of all: the probe is the
	// heartbeat now (§3) and this build registers nowhere.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.stray = append(f.stray, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(func() {
		f.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.stray) != 0 {
			t.Errorf("the replica called %v; the only calls this build makes are pair, the probe and the bundle", f.stray)
		}
	})
	return f
}

// holdBundle makes this main sit on the next bundle request until
// releaseBundle, which is idempotent so a test can both release it and leave
// the cleanup to.
func (f *mainFixture) holdBundle()    { f.hold.Store(true) }
func (f *mainFixture) releaseBundle() { f.once.Do(func() { close(f.release) }) }

// state is what the fixture recorded, copied under its lock.
func (f *mainFixture) state() (probes []probeRecord, pairs []api.PairRequest, bundles int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]probeRecord(nil), f.probes...), append([]api.PairRequest(nil), f.pairs...), f.bundles
}

func TestReplicaPullsAndApplies(t *testing.T) {
	ctx := t.Context()
	mainSt := openStore(t)
	gid, err := mainSt.Clients().AddGroup(ctx, "kids")
	if err != nil {
		t.Fatalf("AddGroup: %v", err)
	}
	ts := fakeMain(t, mainSt, "tok", 5353)

	rep := openStore(t)
	mustSet(t, rep, "sync.peer_url", ts.URL)
	mustSet(t, rep, "sync.token", "tok")
	mustSet(t, rep, "sync.primary_dns", "10.0.0.5:53")
	reloads := &countingReloader{}
	r := NewReplica(rep, reloads, "replica-1", "10.0.0.6:53", false)

	if err := r.PullOnce(ctx); err != nil {
		t.Fatalf("PullOnce: %v", err)
	}
	gs, err := rep.Clients().Groups(ctx)
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	found := false
	for _, g := range gs {
		if g.ID == gid && g.Name == "kids" {
			found = true
		}
	}
	if !found {
		t.Fatalf("group not applied: %+v", gs)
	}
	if reloads.clients != 1 || reloads.filters != 1 || reloads.zones != 1 || reloads.settings != 1 {
		t.Fatalf("reloads %+v", reloads)
	}
	// This bundle carries no lists, so nothing needs downloading: the kick
	// is for the copies a replica does not have, not a refresh per pull.
	if n := reloads.downloadCount(); n != 0 {
		t.Fatalf("a bundle with no lists kicked %d downloads", n)
	}
	st := r.Status()
	if st.Role != "replica" || st.AppliedVersion != st.PeerVersion || st.LastError != "" || !st.PlainHTTP {
		t.Fatalf("status %+v", st)
	}
	if st.PeerURL != ts.URL || st.LastPullAt == 0 || st.AppliedAt == 0 {
		t.Fatalf("status %+v", st)
	}

	// Unchanged version: no bundle fetch, no reload.
	if err := r.PullOnce(ctx); err != nil {
		t.Fatalf("second PullOnce: %v", err)
	}
	if reloads.clients != 1 {
		t.Fatal("re-applied an unchanged version")
	}
	// The probe is the whole unchanged cycle: no bundle, and nothing else
	// called — the main stamped this box from the probe itself (§3).
	if _, _, bundles := ts.state(); bundles != 1 {
		t.Fatalf("%d bundle fetches, want the one the first cycle made", bundles)
	}

	// A failing bundle keeps the last config and records the error.
	mustSet(t, rep, "sync.token", "wrong")
	mustSet(t, mainSt, "blocking.mode", "nxdomain") // moves the version
	if err := r.PullOnce(ctx); err == nil {
		t.Fatal("pull with a bad token succeeded")
	}
	if v, _, _ := rep.Settings().Get(ctx, "blocking.mode"); v == "nxdomain" {
		t.Fatal("applied despite the failed pull")
	}
	if st := r.Status(); !strings.Contains(st.LastError, "401") {
		t.Fatalf("last_error %q", st.LastError)
	}
	// Recorded on the box, not only in memory.
	if v, _, _ := rep.Settings().Get(ctx, "sync.last_error"); !strings.Contains(v, "401") {
		t.Fatalf("stored last_error %q", v)
	}
	// And it clears itself once a pull succeeds again.
	mustSet(t, rep, "sync.token", "tok")
	if err := r.PullOnce(ctx); err != nil {
		t.Fatalf("recovering PullOnce: %v", err)
	}
	if st := r.Status(); st.LastError != "" {
		t.Fatalf("last_error %q after a good pull", st.LastError)
	}
	if v, _, _ := rep.Settings().Get(ctx, "blocking.mode"); v != "nxdomain" {
		t.Fatalf("blocking.mode = %q, want the main's", v)
	}

	// A change that is not a settings write moves the version too, so the
	// next cycle picks it up — the main adding a group is the commonest
	// config change there is, and nothing about it touches settings.
	guests, err := mainSt.Clients().AddGroup(ctx, "guests")
	if err != nil {
		t.Fatalf("AddGroup: %v", err)
	}
	if err := r.PullOnce(ctx); err != nil {
		t.Fatalf("PullOnce after a group was added: %v", err)
	}
	gs, err = rep.Clients().Groups(ctx)
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	found = false
	for _, g := range gs {
		if g.ID == guests && g.Name == "guests" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a group added on the main was not pulled: %+v", gs)
	}
}

// TestReplicaDerivesPrimaryDNSFromPeer pins §8's fallback: with
// sync.primary_dns unset and a peer that advertises no DNS port, a derived
// secondary transfers from the peer URL's host on port 53 rather than from
// nothing.
func TestReplicaDerivesPrimaryDNSFromPeer(t *testing.T) {
	ctx := t.Context()
	mainSt := openStore(t)
	if _, err := mainSt.Zones().AddZone(ctx, store.Zone{Name: "e412.in", Type: "primary"}); err != nil {
		t.Fatalf("AddZone: %v", err)
	}
	ts := fakeMain(t, mainSt, "", 0)

	rep := openStore(t)
	mustSet(t, rep, "sync.peer_url", ts.URL)
	r := NewReplica(rep, &countingReloader{}, "replica-1", "10.0.0.6:53", false)
	if err := r.PullOnce(ctx); err != nil {
		t.Fatalf("PullOnce: %v", err)
	}
	zs, err := rep.Zones().Zones(ctx)
	if err != nil {
		t.Fatalf("Zones: %v", err)
	}
	host, _, _ := strings.Cut(strings.TrimPrefix(ts.URL, "http://"), ":")
	for _, z := range zs {
		if z.Name != "e412.in" {
			continue
		}
		if z.Type != "secondary" || z.Primaries != host+":53" {
			t.Fatalf("derived zone %+v, want a secondary of %s:53", z, host)
		}
		return
	}
	t.Fatalf("zone not applied: %+v", zs)
}

// TestReplicaSkipsABundleThatMovedUnderTheProbe: a write landing between the
// two requests makes the bundle a different version from the one the probe
// reported. Applying it would stamp this box with a version it does not
// hold, so the cycle stops and the next one picks the newer one up (§3).
func TestReplicaSkipsABundleThatMovedUnderTheProbe(t *testing.T) {
	ctx := t.Context()
	mainSt := openStore(t)
	if _, err := mainSt.Clients().AddGroup(ctx, "kids"); err != nil {
		t.Fatalf("AddGroup: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/sync/version", func(w http.ResponseWriter, _ *http.Request) {
		v, _ := mainSt.Settings().ConfigVersion(ctx)
		_ = json.NewEncoder(w).Encode(map[string]any{"config_version": v, "instance_id": "main-1"})
	})
	mux.HandleFunc("GET /api/v1/sync/bundle", func(w http.ResponseWriter, _ *http.Request) {
		b, _ := mainSt.ExportBundle(ctx)
		// The write that landed between the probe and this read.
		b.ConfigVersion++
		_ = json.NewEncoder(w).Encode(b)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	rep := openStore(t)
	mustSet(t, rep, "sync.peer_url", ts.URL)
	reloads := &countingReloader{}
	r := NewReplica(rep, reloads, "replica-1", "10.0.0.6:53", false)

	if err := r.PullOnce(ctx); err != nil {
		t.Fatalf("PullOnce: %v", err)
	}
	if gs, _ := rep.Clients().Groups(ctx); len(gs) != 0 {
		t.Fatalf("a bundle that moved under the probe was applied: %+v", gs)
	}
	if reloads.clients != 0 {
		t.Fatalf("reloads %+v", reloads)
	}
	if v, found, _ := rep.Settings().Get(ctx, "sync.applied_version"); found {
		t.Fatalf("sync.applied_version = %q, want no version marked applied", v)
	}
}

// TestReplicaBlanksTheAppliedVersionOnPromotion: clearing sync.peer_url is
// the promotion, and this box now owns its configuration. The version it
// last took from a main describes a counter it no longer follows — left
// standing, it is what the first pull after a later re-point compares
// against.
func TestReplicaBlanksTheAppliedVersionOnPromotion(t *testing.T) {
	ctx := t.Context()
	mainSt := openStore(t)
	if _, err := mainSt.Clients().AddGroup(ctx, "kids"); err != nil {
		t.Fatalf("AddGroup: %v", err)
	}
	ts := fakeMain(t, mainSt, "", 5353)
	rep := openStore(t)
	mustSet(t, rep, "sync.peer_url", ts.URL)
	r := NewReplica(rep, &countingReloader{}, "replica-1", "10.0.0.6:53", false)
	if err := r.PullOnce(ctx); err != nil {
		t.Fatalf("PullOnce: %v", err)
	}
	if v, _, _ := rep.Settings().Get(ctx, appliedVersionSetting); v == "" {
		t.Fatal("nothing was marked applied; this test needs a version to clear")
	}

	mustSet(t, rep, "sync.peer_url", "")
	if err := r.PullOnce(ctx); err != nil {
		t.Fatalf("PullOnce after the promotion: %v", err)
	}
	if v, _, _ := rep.Settings().Get(ctx, appliedVersionSetting); v != "" {
		t.Fatalf("sync.applied_version = %q after a promotion, want it blank", v)
	}
}

// TestReplicaRefetchesWhenRepointedToTheSameVersion: version numbers are
// each main's own count of its own writes, so two mains are level by
// coincidence all the time. A replica moved from one to the other must not
// read that coincidence as "nothing to apply" and go on serving the old
// main's configuration forever.
func TestReplicaRefetchesWhenRepointedToTheSameVersion(t *testing.T) {
	ctx := t.Context()
	first, second := openStore(t), openStore(t)
	if _, err := first.Clients().AddGroup(ctx, "first-main"); err != nil {
		t.Fatalf("AddGroup: %v", err)
	}
	if _, err := second.Clients().AddGroup(ctx, "second-main"); err != nil {
		t.Fatalf("AddGroup: %v", err)
	}
	v1, _ := first.Settings().ConfigVersion(ctx)
	v2, _ := second.Settings().ConfigVersion(ctx)
	if v1 != v2 {
		t.Fatalf("the two mains are at %d and %d; this test is about the version being the same", v1, v2)
	}
	ts1 := fakeMain(t, first, "", 5353)
	ts2 := fakeMain(t, second, "", 5353)

	rep := openStore(t)
	mustSet(t, rep, "sync.peer_url", ts1.URL)
	r := NewReplica(rep, &countingReloader{}, "replica-1", "10.0.0.6:53", false)
	if err := r.PullOnce(ctx); err != nil {
		t.Fatalf("PullOnce: %v", err)
	}
	if got := groupNames(t, rep); len(got) != 1 || got[0] != "first-main" {
		t.Fatalf("groups after the first pull = %v", got)
	}

	mustSet(t, rep, "sync.peer_url", ts2.URL)
	if err := r.PullOnce(ctx); err != nil {
		t.Fatalf("PullOnce after the re-point: %v", err)
	}
	if got := groupNames(t, rep); len(got) != 1 || got[0] != "second-main" {
		t.Fatalf("groups after the re-point = %v, want the new main's", got)
	}
	// And the heartbeat says so: the version this box held was the first
	// main's count of its own writes, and reporting it to the second would
	// put a number in its Sync band that this box has applied none of.
	if probes, _, _ := ts2.state(); len(probes) != 1 || probes[0].query != "applied=0" {
		t.Fatalf("the new main saw %+v, want one probe reporting nothing applied", probes)
	}
}

// groupNames is what a bundle brought, in the only shape these tests compare.
func groupNames(t *testing.T, st store.Store) []string {
	t.Helper()
	gs, err := st.Clients().Groups(t.Context())
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Name)
	}
	return out
}

// TestReplicaKeepsThePeerVersionWhenTheProbeFails: "behind by N" is what the
// Sync band shows, and a main that has gone down must not make the replica
// report that it is level with it.
func TestReplicaKeepsThePeerVersionWhenTheProbeFails(t *testing.T) {
	ctx := t.Context()
	mainSt := openStore(t)
	if _, err := mainSt.Clients().AddGroup(ctx, "kids"); err != nil {
		t.Fatalf("AddGroup: %v", err)
	}
	ts := fakeMain(t, mainSt, "", 5353)
	rep := openStore(t)
	mustSet(t, rep, "sync.peer_url", ts.URL)
	r := NewReplica(rep, &countingReloader{}, "replica-1", "10.0.0.6:53", false)

	if err := r.PullOnce(ctx); err != nil {
		t.Fatalf("PullOnce: %v", err)
	}
	want := r.Status().PeerVersion
	if want == 0 {
		t.Fatal("the main reported version 0; this test needs a version to lose")
	}

	ts.Close() // the main goes away
	if err := r.PullOnce(ctx); err == nil {
		t.Fatal("a pull against a dead peer succeeded")
	}
	st := r.Status()
	if st.PeerVersion != want {
		t.Fatalf("peer_version = %d after a failed probe, want the last one known (%d)", st.PeerVersion, want)
	}
	if st.LastError == "" || st.AppliedVersion != want {
		t.Fatalf("status %+v", st)
	}

	// Re-pointed at another main: the version carried forward belonged to
	// the old one, and reporting it against a new peer that has not
	// answered yet would be a number from the wrong box.
	mustSet(t, rep, "sync.peer_url", "http://127.0.0.1:1")
	if err := r.PullOnce(ctx); err == nil {
		t.Fatal("a pull against a dead peer succeeded")
	}
	if st := r.Status(); st.PeerVersion != 0 || st.LastError == "" {
		t.Fatalf("status %+v after re-pointing, want no peer version yet", st)
	}
}

// TestDecodeLimitedRefusesAnOversizeBody: a peer that answers with more than
// the cap is an error naming the cap, not a truncated document decoded as if
// it were the whole thing.
func TestDecodeLimitedRefusesAnOversizeBody(t *testing.T) {
	var out map[string]string
	body := `{"key":"` + strings.Repeat("x", 64) + `"}`
	if err := decodeLimited(strings.NewReader(body), 16, "http://main.example/b", &out); err == nil ||
		!strings.Contains(err.Error(), "16") {
		t.Fatalf("err = %v, want a refusal naming the cap", err)
	}
	out = nil
	if err := decodeLimited(strings.NewReader(body), 1<<20, "http://main.example/b", &out); err != nil {
		t.Fatalf("a body inside the cap: %v", err)
	}
	if out["key"] == "" {
		t.Fatalf("decoded %+v", out)
	}
}

// TestReplicaIdlesWithoutPeer: a main runs the same worker. It must not
// pull, and it must come back to look again soon enough that setting a peer
// starts a pull without a restart.
func TestReplicaIdlesWithoutPeer(t *testing.T) {
	ctx := t.Context()
	st := openStore(t)
	r := NewReplica(st, &countingReloader{}, "main-1", "10.0.0.5:53", false)
	if err := r.PullOnce(ctx); err != nil {
		t.Fatalf("PullOnce on a main: %v", err)
	}
	if got := r.PeerURL(); got != "" {
		t.Fatalf("PeerURL = %q on a main", got)
	}
	if d := r.pollInterval(ctx); d != idleInterval {
		t.Fatalf("idle interval = %s, want %s", d, idleInterval)
	}
	mustSet(t, st, "sync.peer_url", "https://main.example")
	mustSet(t, st, "sync.interval_seconds", "45")
	if got := r.PeerURL(); got != "https://main.example" {
		t.Fatalf("PeerURL = %q", got)
	}
	if d := r.pollInterval(ctx); d != 45*time.Second {
		t.Fatalf("poll interval = %s, want 45s", d)
	}
	mustSet(t, st, "sync.interval_seconds", "0")
	if d := r.pollInterval(ctx); d != minInterval {
		t.Fatalf("poll interval = %s for a refused value, want %s", d, minInterval)
	}
}

// TestReplicaRefusesToFollowItself: a peer URL pointed at this very box
// would make it its own replica, and no error would ever say so.
func TestReplicaRefusesToFollowItself(t *testing.T) {
	ctx := t.Context()
	st := openStore(t)
	ts := fakeMain(t, st, "", 5353)
	mustSet(t, st, "sync.peer_url", ts.URL)
	r := NewReplica(st, &countingReloader{}, "main-1", "10.0.0.5:53", false)
	// Naming instance.id is the point: the way a box ends up pointed at
	// itself in practice is a "replica" restored from the main's database,
	// and the error has to be the thing that explains that.
	if err := r.PullOnce(ctx); err == nil || !strings.Contains(err.Error(), "instance.id") ||
		!strings.Contains(err.Error(), "follow itself") {
		t.Fatalf("PullOnce = %v, want a refusal naming instance.id", err)
	}
}

func mustSet(t *testing.T, st store.Store, key, value string) {
	t.Helper()
	if err := st.Settings().Set(t.Context(), key, value); err != nil {
		t.Fatalf("Set(%s): %v", key, err)
	}
}

// TestFollowStoresThePeerAndSecret is the pairing flow end to end (§3): the
// operator types the main's URL and the code it showed, and the box comes
// back following it, holding a secret it never typed, with the main's
// configuration already applied.
func TestFollowStoresThePeerAndSecret(t *testing.T) {
	ctx := t.Context()
	mainSt := openStore(t)
	if _, err := mainSt.Clients().AddGroup(ctx, "kids"); err != nil {
		t.Fatalf("AddGroup: %v", err)
	}
	// The bundle is served to the paired secret only, so a pull that works
	// is proof the secret was stored before it ran.
	ts := fakeMain(t, mainSt, "s3", 5353)

	rep := openStore(t)
	r := NewReplica(rep, &countingReloader{}, "replica-1", "10.0.0.6:53", false)
	// The trailing slash an operator pastes is theirs to get wrong.
	if err := r.Follow(ctx, ts.URL+"/", pairCode); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	if v, _, _ := rep.Settings().Get(ctx, peerURLSetting); v != ts.URL {
		t.Errorf("sync.peer_url = %q, want %q", v, ts.URL)
	}
	if v, _, _ := rep.Settings().Get(ctx, tokenSetting); v != "s3" {
		t.Errorf("sync.token = %q, want the secret the main minted", v)
	}
	if _, pairs, _ := ts.state(); len(pairs) != 1 || pairs[0].InstanceID != "replica-1" ||
		pairs[0].DNSAddr != "10.0.0.6:53" {
		t.Fatalf("the main saw %+v", pairs)
	}
	// Following kicks a pull rather than waiting one out, so the operator
	// watches the configuration arrive instead of watching a request that
	// holds open for as long as a bundle fetch takes. sync.last_error is
	// the last thing a cycle writes, so its presence is the cycle ending.
	waitFor(t, "the pull following kicked", func() bool {
		_, found, _ := rep.Settings().Get(ctx, lastErrorSetting)
		return found
	})
	if probes, _, bundles := ts.state(); len(probes) != 1 || bundles != 1 {
		t.Fatalf("%d probes and %d bundle fetches, want one pull", len(probes), bundles)
	}
	if got := groupNames(t, rep); len(got) != 1 || got[0] != "kids" {
		t.Fatalf("groups after following = %v", got)
	}
}

// TestFollowRefusedLeavesSettingsUntouched: a mistyped code is the commonest
// thing that happens here, and it must leave the box exactly as it was — a
// peer URL stored against no secret is a replica that pulls a 401 forever.
func TestFollowRefusedLeavesSettingsUntouched(t *testing.T) {
	ctx := t.Context()
	ts := fakeMain(t, openStore(t), "s3", 5353)
	rep := openStore(t)
	r := NewReplica(rep, &countingReloader{}, "replica-1", "10.0.0.6:53", false)

	err := r.Follow(ctx, ts.URL, "K7PQ-4M2Y")
	if !errors.Is(err, ErrFollowRefused) {
		t.Fatalf("Follow with a wrong code = %v, want ErrFollowRefused", err)
	}
	// The main's own words, so the operator is told which of the four
	// refusals it was as far as the main is willing to say (§6).
	if !strings.Contains(err.Error(), "pairing code refused") {
		t.Errorf("error %q, want the main's message in it", err)
	}
	for _, key := range []string{peerURLSetting, tokenSetting} {
		if v, _, _ := rep.Settings().Get(ctx, key); v != "" {
			t.Errorf("%s = %q after a refusal, want it untouched", key, v)
		}
	}
	if probes, _, bundles := ts.state(); len(probes) != 0 || bundles != 0 {
		t.Errorf("a refused pairing pulled anyway: %d probes, %d bundles", len(probes), bundles)
	}
}

// TestFollowRejectsAPeerURLWithAPath: the pull loop joins "/api/v1/sync/..."
// onto the peer URL, so anything but a scheme and a host builds a URL nobody
// meant — refused here, before the code is sent anywhere.
func TestFollowRejectsAPeerURLWithAPath(t *testing.T) {
	ctx := t.Context()
	ts := fakeMain(t, openStore(t), "s3", 5353)
	rep := openStore(t)
	r := NewReplica(rep, &countingReloader{}, "replica-1", "10.0.0.6:53", false)

	err := r.Follow(ctx, ts.URL+"/x", pairCode)
	if err == nil || errors.Is(err, ErrFollowRefused) {
		t.Fatalf("Follow = %v, want the URL refused before anything is dialled", err)
	}
	if !strings.Contains(err.Error(), "scheme and host") {
		t.Errorf("error %q, want it to say what a peer URL may be", err)
	}
	if probes, pairs, bundles := ts.state(); len(probes)+len(pairs)+bundles != 0 {
		t.Errorf("the code was sent anyway: %d probes, %d pairs, %d bundles", len(probes), len(pairs), bundles)
	}
	if v, _, _ := rep.Settings().Get(ctx, peerURLSetting); v != "" {
		t.Errorf("sync.peer_url = %q, want it untouched", v)
	}
}

// TestProbeCarriesAppliedVersionAndBearer: the probe is the heartbeat (§3).
// It is the only call left that tells the main this box is alive and what it
// holds, so every one of them carries both.
func TestProbeCarriesAppliedVersionAndBearer(t *testing.T) {
	ctx := t.Context()
	mainSt := openStore(t)
	if _, err := mainSt.Clients().AddGroup(ctx, "kids"); err != nil {
		t.Fatalf("AddGroup: %v", err)
	}
	ts := fakeMain(t, mainSt, "tok", 5353)
	rep := openStore(t)
	mustSet(t, rep, peerURLSetting, ts.URL)
	mustSet(t, rep, tokenSetting, "tok")
	r := NewReplica(rep, &countingReloader{}, "replica-1", "10.0.0.6:53", false)

	if err := r.PullOnce(ctx); err != nil {
		t.Fatalf("PullOnce: %v", err)
	}
	applied := r.Status().AppliedVersion
	if applied == 0 {
		t.Fatal("nothing was applied; this test needs a version to report")
	}
	if err := r.PullOnce(ctx); err != nil {
		t.Fatalf("second PullOnce: %v", err)
	}
	probes, _, _ := ts.state()
	if len(probes) != 2 {
		t.Fatalf("%d probes, want one per cycle", len(probes))
	}
	// The first probe is made before anything is applied, the second after.
	want := []string{"applied=0", "applied=" + strconv.FormatInt(applied, 10)}
	for i, p := range probes {
		if p.query != want[i] {
			t.Errorf("probe %d query = %q, want %q", i, p.query, want[i])
		}
		if p.auth != "Bearer tok" {
			t.Errorf("probe %d authorization = %q, want the secret", i, p.auth)
		}
	}
}

// TestProbeSaysWhetherThisBoxRunsAnEngine: the probe is the only thing the
// main ever hears from a replica, and whether that box runs a DHCP engine is
// a bootstrap key on it — so this is how the main learns it. A main that
// named a standby with no engine would hand Kea a hot-standby pair whose
// partner answers nothing, and the primary would wait out max-response-delay
// for it on every client (design §6).
func TestProbeSaysWhetherThisBoxRunsAnEngine(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct {
		name  string
		dhcp  bool
		query string
	}{
		{name: "no engine", dhcp: false, query: "applied=0"},
		{name: "an engine", dhcp: true, query: "applied=0&dhcp=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := fakeMain(t, openStore(t), "tok", 5353)
			rep := openStore(t)
			mustSet(t, rep, peerURLSetting, ts.URL)
			mustSet(t, rep, tokenSetting, "tok")
			r := NewReplica(rep, &countingReloader{}, "replica-1", "10.0.0.6:53", tc.dhcp)

			if err := r.PullOnce(ctx); err != nil {
				t.Fatalf("PullOnce: %v", err)
			}
			probes, _, _ := ts.state()
			if len(probes) != 1 || probes[0].query != tc.query {
				t.Fatalf("probes = %+v, want one with query %q", probes, tc.query)
			}
		})
	}
}

// TestProbeDNSPortDrivesPrimaryDNS: §8's fallback is the peer's host on the
// port the main advertises in the probe, and sync.primary_dns is an override
// for the deployment where the main's API and its DNS are not one address.
func TestProbeDNSPortDrivesPrimaryDNS(t *testing.T) {
	ctx := t.Context()
	mainSt := openStore(t)
	if _, err := mainSt.Zones().AddZone(ctx, store.Zone{Name: "e412.in", Type: "primary"}); err != nil {
		t.Fatalf("AddZone: %v", err)
	}
	ts := fakeMain(t, mainSt, "", 5353)
	host, _, _ := strings.Cut(strings.TrimPrefix(ts.URL, "http://"), ":")

	rep := openStore(t)
	mustSet(t, rep, peerURLSetting, ts.URL)
	r := NewReplica(rep, &countingReloader{}, "replica-1", "10.0.0.6:53", false)
	if err := r.PullOnce(ctx); err != nil {
		t.Fatalf("PullOnce: %v", err)
	}
	if got := zonePrimaries(t, rep, "e412.in"); got != host+":5353" {
		t.Fatalf("primaries = %q, want the port the main advertised (%s:5353)", got, host)
	}

	mustSet(t, rep, primaryDNSKey, "10.0.0.5:5300")
	// A write on the main, so the next cycle has a bundle to apply.
	if _, err := mainSt.Clients().AddGroup(ctx, "kids"); err != nil {
		t.Fatalf("AddGroup: %v", err)
	}
	if err := r.PullOnce(ctx); err != nil {
		t.Fatalf("PullOnce with an override: %v", err)
	}
	if got := zonePrimaries(t, rep, "e412.in"); got != "10.0.0.5:5300" {
		t.Fatalf("primaries = %q, want the override", got)
	}
}

// zonePrimaries is where a derived secondary transfers from.
func zonePrimaries(t *testing.T, st store.Store, name string) string {
	t.Helper()
	zs, err := st.Zones().Zones(t.Context())
	if err != nil {
		t.Fatalf("Zones: %v", err)
	}
	for _, z := range zs {
		if z.Name == name {
			if z.Type != "secondary" {
				t.Fatalf("zone %s is a %s, want the derived secondary", name, z.Type)
			}
			return z.Primaries
		}
	}
	t.Fatalf("zone %s was not applied: %+v", name, zs)
	return ""
}

// TestParsePeerURL is the grammar both the Follow action and the settings
// validator judge a peer by: a scheme and a host, nothing else, with the one
// trailing slash an operator pastes taken back off.
func TestParsePeerURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://main.lan", "https://main.lan"},
		{"http://10.0.0.5:8080", "http://10.0.0.5:8080"},
		{"https://main.lan/", "https://main.lan"},
	} {
		got, err := ParsePeerURL(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParsePeerURL(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{
		"", "main.lan", "ftp://main.lan", "https://", "http://h:1/x",
		"https://main.lan//", "https://main.lan?a=1", "https://main.lan#f",
		"https://user:pw@main.lan",
	} {
		if got, err := ParsePeerURL(bad); err == nil {
			t.Errorf("ParsePeerURL(%q) = %q, want a refusal", bad, got)
		}
	}
}

// waitFor polls until ok, for the one thing in this package that happens off
// the test's own goroutine: the pull Follow kicks.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestFollowMapsThePeersAnswer: what the main said decides whether the
// operator is looking at a code to retype or at a main that is broken, and
// only the first of those is ErrFollowRefused. Nothing is stored either way.
func TestFollowMapsThePeersAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		refused bool
		want    string
	}{
		{"wrong code", http.StatusForbidden, `{"error":"pairing code refused"}`, true, "pairing code refused"},
		// The three below are not the code being wrong, and an operator
		// told to retype it would be sent to spend another one for nothing.
		{"no pairing route", http.StatusNotFound, `{"error":"not found"}`, false, "that box has no pairing endpoint"},
		{"rate limited", http.StatusTooManyRequests, `{"error":"too many attempts"}`, false,
			"the main is refusing attempts for now"},
		{"the peer is itself a replica", http.StatusConflict, `{"error":"managed by https://main.lan"}`, false,
			"that box is a replica of https://main.lan"},
		// A 409 from something that is not that guard names no main, and
		// must not be read as though it had.
		{"a 409 from something else", http.StatusConflict, `{"error":"nope"}`, false, "409 Conflict"},
		{"the main is broken", http.StatusInternalServerError, `{"error":"boom"}`, false, "500"},
		{"a correct code and no secret", http.StatusOK, `{"dns_port":5353}`, false, "no secret"},
		{"a peer with a lot to say", http.StatusForbidden,
			`{"error":"` + strings.Repeat("x", 500) + `"}`, true, strings.Repeat("x", 200) + "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(ts.Close)
			rep := openStore(t)
			r := NewReplica(rep, &countingReloader{}, "replica-1", "10.0.0.6:53", false)

			err := r.Follow(t.Context(), ts.URL, pairCode)
			if err == nil {
				t.Fatalf("Follow against %s succeeded", tc.name)
			}
			if got := errors.Is(err, ErrFollowRefused); got != tc.refused {
				t.Errorf("errors.Is(%v, ErrFollowRefused) = %v, want %v", err, got, tc.refused)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q, want %q in it", err, tc.want)
			}
			// A peer is not the judge of how long the line the settings
			// screen shows is.
			if n := len([]rune(err.Error())); n > 300 {
				t.Errorf("the error is %d runes long: %q", n, err)
			}
			for _, key := range []string{peerURLSetting, tokenSetting} {
				if v, _, _ := rep.Settings().Get(t.Context(), key); v != "" {
					t.Errorf("%s = %q, want nothing stored", key, v)
				}
			}
		})
	}
}

// TestProbeKeepsTheLastAdvertisedDNSPort: the port arrives with the probe and
// nowhere else, so a main that answers one without it — an older build — must
// not move every derived secondary to port 53 for a cycle.
func TestProbeKeepsTheLastAdvertisedDNSPort(t *testing.T) {
	ctx := t.Context()
	mainSt := openStore(t)
	if _, err := mainSt.Zones().AddZone(ctx, store.Zone{Name: "e412.in", Type: "primary"}); err != nil {
		t.Fatalf("AddZone: %v", err)
	}
	ts := fakeMain(t, mainSt, "", 5353)
	host, _, _ := strings.Cut(strings.TrimPrefix(ts.URL, "http://"), ":")

	rep := openStore(t)
	mustSet(t, rep, peerURLSetting, ts.URL)
	r := NewReplica(rep, &countingReloader{}, "replica-1", "10.0.0.6:53", false)
	if err := r.PullOnce(ctx); err != nil {
		t.Fatalf("PullOnce: %v", err)
	}
	if got := zonePrimaries(t, rep, "e412.in"); got != host+":5353" {
		t.Fatalf("primaries = %q, want %s:5353", got, host)
	}

	ts.dnsPort.Store(0)
	// A write on the main, so the next cycle has a bundle to derive from.
	if _, err := mainSt.Clients().AddGroup(ctx, "kids"); err != nil {
		t.Fatalf("AddGroup: %v", err)
	}
	if err := r.PullOnce(ctx); err != nil {
		t.Fatalf("second PullOnce: %v", err)
	}
	if got := zonePrimaries(t, rep, "e412.in"); got != host+":5353" {
		t.Fatalf("primaries = %q after a probe with no port, want the last one advertised (%s:5353)", got, host)
	}
}

// TestFollowDoesNotWaitForThePull: Follow answers once the pairing is stored,
// which is the part that cannot be retried. A main that is slow with a bundle
// would otherwise hold the request open past the API's own deadline, and a
// box that had already paired would report that it had not.
func TestFollowDoesNotWaitForThePull(t *testing.T) {
	ctx := t.Context()
	mainSt := openStore(t)
	if _, err := mainSt.Clients().AddGroup(ctx, "kids"); err != nil {
		t.Fatalf("AddGroup: %v", err)
	}
	ts := fakeMain(t, mainSt, "s3", 5353)
	ts.holdBundle()
	// Registered after the fixture's own cleanup, so it runs before it: a
	// blocked handler would hold the server's Close open.
	t.Cleanup(ts.releaseBundle)

	rep := openStore(t)
	r := NewReplica(rep, &countingReloader{}, "replica-1", "10.0.0.6:53", false)
	start := time.Now()
	if err := r.Follow(ctx, ts.URL, pairCode); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Follow took %s while the main sat on the bundle, want it back as soon as the pairing is stored", d)
	}
	if v, _, _ := rep.Settings().Get(ctx, tokenSetting); v != "s3" {
		t.Fatalf("sync.token = %q, want the pairing stored before Follow answered", v)
	}

	// And the pull it kicked is the thing that was waiting.
	ts.releaseBundle()
	waitFor(t, "the kicked pull to finish", func() bool {
		_, found, _ := rep.Settings().Get(ctx, lastErrorSetting)
		return found
	})
	if got := groupNames(t, rep); len(got) != 1 || got[0] != "kids" {
		t.Fatalf("groups after the kicked pull = %v", got)
	}
}

// TestPullOnceWaitsForTheCycleInFlight: the poll loop's own cycle and a
// direct call are two callers, and two cycles at once would both fetch and
// both import, the later bookkeeping winning. The second waits for the first
// and then runs its own cycle — a probe that finds the version just applied
// and fetches nothing — so a caller can rely on the pull having happened
// when PullOnce returns.
func TestPullOnceWaitsForTheCycleInFlight(t *testing.T) {
	ctx := t.Context()
	mainSt := openStore(t)
	if _, err := mainSt.Clients().AddGroup(ctx, "kids"); err != nil {
		t.Fatalf("AddGroup: %v", err)
	}
	ts := fakeMain(t, mainSt, "", 5353)
	ts.holdBundle()
	// Registered after the fixture's own cleanup so it runs before it: a
	// blocked handler would hold the server's Close open.
	t.Cleanup(ts.releaseBundle)

	rep := openStore(t)
	mustSet(t, rep, peerURLSetting, ts.URL)
	r := NewReplica(rep, &countingReloader{}, "replica-1", "10.0.0.6:53", false)

	first := make(chan error, 1)
	go func() { first <- r.PullOnce(ctx) }()
	// The probe is the first thing a cycle puts on the wire, so one probe
	// recorded is the first caller holding the cycle.
	waitFor(t, "the first pull to reach the main", func() bool {
		probes, _, _ := ts.state()
		return len(probes) == 1
	})

	second := make(chan error, 1)
	go func() { second <- r.PullOnce(ctx) }()
	select {
	case err := <-second:
		t.Fatalf("the second PullOnce returned %v while the first still held the cycle, want it to wait", err)
	case <-time.After(500 * time.Millisecond):
	}

	ts.releaseBundle()
	if err := <-first; err != nil {
		t.Fatalf("the first PullOnce: %v", err)
	}
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("the second PullOnce: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second PullOnce never ran after the first released the cycle")
	}
	if probes, _, bundles := ts.state(); len(probes) != 2 || bundles != 1 {
		t.Fatalf("%d probes and %d bundle fetches, want the second cycle to have probed and fetched nothing", len(probes), bundles)
	}
}

func TestFollowSeedsTheDNSPortFromThePairing(t *testing.T) {
	ctx := t.Context()
	// Only the pairing route: the probe the kicked pull makes gets a 404,
	// so the port under test can only have come from the pairing.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pairPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(api.PairResult{Secret: "s3", DNSPort: 5353})
	}))
	t.Cleanup(ts.Close)

	rep := openStore(t)
	r := NewReplica(rep, &countingReloader{}, "replica-1", "10.0.0.6:53", false)
	if err := r.Follow(ctx, ts.URL, pairCode); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	// sync.last_error is the last thing a cycle writes, so its presence is
	// the kicked pull done with the store this test is about to close.
	waitFor(t, "the pull following kicked", func() bool {
		_, found, _ := rep.Settings().Get(ctx, lastErrorSetting)
		return found
	})
	if got := r.dnsPort.Load(); got != 5353 {
		t.Fatalf("dnsPort = %d after pairing, want the 5353 the main answered", got)
	}
}

// TestFollowWakesThePollLoopInsteadOfPullingBesideIt: a pull started beside
// the loop is lost whenever the loop is mid-cycle — PullOnce steps aside —
// and the next tick is sync.interval_seconds away, because storing the peer
// is what takes the box off the five-second idle beat. So the pairing hands
// the pull to the loop instead, and the loop takes it the moment its cycle
// ends.
func TestFollowWakesThePollLoopInsteadOfPullingBesideIt(t *testing.T) {
	ctx := t.Context()
	mainSt := openStore(t)
	ts := fakeMain(t, mainSt, "s3", 5353)

	rep := openStore(t)
	// An hour: a pairing that has to wait out an interval waits past
	// anything this test will sit through.
	mustSet(t, rep, intervalSetting, "3600")
	mustSet(t, rep, peerURLSetting, ts.URL)
	mustSet(t, rep, tokenSetting, "s3")
	r := NewReplica(rep, &countingReloader{}, "replica-1", "10.0.0.6:53", false)

	ts.holdBundle()
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); r.Run(runCtx) }()
	t.Cleanup(func() { stop(); <-done })
	// Registered last so it runs first: the fixture's own cleanup closes a
	// server whose bundle handler is still sitting on the release.
	t.Cleanup(ts.releaseBundle)

	waitFor(t, "the loop's first cycle to reach the bundle", func() bool {
		probes, _, _ := ts.state()
		return len(probes) == 1
	})
	// A write the cycle in flight cannot carry: it probed before this
	// landed, so the bundle it is about to read is a version newer than the
	// one it asked about and none of it is applied (§3).
	if _, err := mainSt.Clients().AddGroup(ctx, "kids"); err != nil {
		t.Fatalf("AddGroup: %v", err)
	}

	if err := r.Follow(ctx, ts.URL, pairCode); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	// Left with the loop rather than run beside it. The loop is still inside
	// the cycle above, so nothing can have taken this yet.
	if len(r.kick) != 1 {
		t.Fatal("the pairing left the poll loop nothing to wake on: its pull is lost to the cycle in flight")
	}

	ts.releaseBundle()
	waitFor(t, "the pull the pairing asked for", func() bool {
		got := groupNames(t, rep)
		return len(got) == 1 && got[0] == "kids"
	})
}
