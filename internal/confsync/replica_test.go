package confsync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
type countingReloader struct{ clients, filters, zones, settings int }

func (c *countingReloader) ReloadClients(context.Context) error    { c.clients++; return nil }
func (c *countingReloader) RecompileFilters(context.Context) error { c.filters++; return nil }
func (c *countingReloader) ReloadZones(context.Context) error      { c.zones++; return nil }
func (c *countingReloader) ReloadSettings(context.Context) error   { c.settings++; return nil }

// fakeMain serves the three endpoints a pull uses, out of a real store.
// token is what GET /sync/bundle demands; register is POST /sync/replicas.
func fakeMain(t *testing.T, st store.Store, token string, register http.HandlerFunc) *httptest.Server {
	t.Helper()
	ctx := t.Context()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/sync/version", func(w http.ResponseWriter, _ *http.Request) {
		v, _ := st.Settings().ConfigVersion(ctx)
		_ = json.NewEncoder(w).Encode(map[string]any{"config_version": v, "instance_id": "main-1"})
	})
	mux.HandleFunc("GET /api/v1/sync/bundle", func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b, err := st.ExportBundle(ctx)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(b)
	})
	mux.HandleFunc("POST /api/v1/sync/replicas", register)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestReplicaPullsAppliesAndRegisters(t *testing.T) {
	ctx := t.Context()
	mainSt := openStore(t)
	gid, err := mainSt.Clients().AddGroup(ctx, "kids")
	if err != nil {
		t.Fatalf("AddGroup: %v", err)
	}
	var mu sync.Mutex
	var registered []api.Replica
	ts := fakeMain(t, mainSt, "tok", func(w http.ResponseWriter, r *http.Request) {
		var rep api.Replica
		_ = json.NewDecoder(r.Body).Decode(&rep)
		mu.Lock()
		registered = append(registered, rep)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	registrations := func() []api.Replica {
		mu.Lock()
		defer mu.Unlock()
		return append([]api.Replica(nil), registered...)
	}

	rep := openStore(t)
	mustSet(t, rep, "sync.peer_url", ts.URL)
	mustSet(t, rep, "sync.token", "tok")
	mustSet(t, rep, "sync.primary_dns", "10.0.0.5:53")
	reloads := &countingReloader{}
	r := NewReplica(rep, reloads, "replica-1", "10.0.0.6:53")

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
	if reg := registrations(); len(reg) != 1 || reg[0].InstanceID != "replica-1" || reg[0].DNSAddr != "10.0.0.6:53" {
		t.Fatalf("registered %+v", reg)
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
	// The registration is the heartbeat the main's staleness is measured
	// against, so it goes out on every cycle, not only on an applied one.
	if reg := registrations(); len(reg) != 2 {
		t.Fatalf("registrations %+v, want a heartbeat on the unchanged cycle", reg)
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
// sync.primary_dns unset, a derived secondary transfers from the peer URL's
// host on port 53 rather than from nothing.
func TestReplicaDerivesPrimaryDNSFromPeer(t *testing.T) {
	ctx := t.Context()
	mainSt := openStore(t)
	if _, err := mainSt.Zones().AddZone(ctx, store.Zone{Name: "e412.in", Type: "primary"}); err != nil {
		t.Fatalf("AddZone: %v", err)
	}
	ts := fakeMain(t, mainSt, "", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	rep := openStore(t)
	mustSet(t, rep, "sync.peer_url", ts.URL)
	r := NewReplica(rep, &countingReloader{}, "replica-1", "10.0.0.6:53")
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

// TestReplicaAppliesEvenWhenRegisterFails: the config is in the store, so
// the version is marked applied and the next cycle does not import it a
// second time; the registration that did not land is still reported,
// because until it does the main will not allow this box's transfers (§6).
func TestReplicaAppliesEvenWhenRegisterFails(t *testing.T) {
	ctx := t.Context()
	mainSt := openStore(t)
	if _, err := mainSt.Clients().AddGroup(ctx, "kids"); err != nil {
		t.Fatalf("AddGroup: %v", err)
	}
	ts := fakeMain(t, mainSt, "", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	rep := openStore(t)
	mustSet(t, rep, "sync.peer_url", ts.URL)
	reloads := &countingReloader{}
	r := NewReplica(rep, reloads, "replica-1", "10.0.0.6:53")

	if err := r.PullOnce(ctx); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("PullOnce = %v, want the registration failure", err)
	}
	gs, _ := rep.Clients().Groups(ctx)
	if len(gs) != 1 || gs[0].Name != "kids" {
		t.Fatalf("bundle not applied: %+v", gs)
	}
	if st := r.Status(); !strings.Contains(st.LastError, "500") || st.AppliedVersion != st.PeerVersion {
		t.Fatalf("status %+v", st)
	}
	// Applied is applied: a second cycle imports nothing, and goes on
	// reporting the registration it still cannot make.
	err := r.PullOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "500") || reloads.clients != 1 {
		t.Fatalf("second cycle: err %v, reloads %+v", err, reloads)
	}
}

// TestReplicaIdlesWithoutPeer: a main runs the same worker. It must not
// pull, and it must come back to look again soon enough that setting a peer
// starts a pull without a restart.
func TestReplicaIdlesWithoutPeer(t *testing.T) {
	ctx := t.Context()
	st := openStore(t)
	r := NewReplica(st, &countingReloader{}, "main-1", "10.0.0.5:53")
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
	ts := fakeMain(t, st, "", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mustSet(t, st, "sync.peer_url", ts.URL)
	r := NewReplica(st, &countingReloader{}, "main-1", "10.0.0.5:53")
	if err := r.PullOnce(ctx); err == nil || !strings.Contains(err.Error(), "this instance") {
		t.Fatalf("PullOnce = %v, want a refusal to follow itself", err)
	}
}

func mustSet(t *testing.T, st store.Store, key, value string) {
	t.Helper()
	if err := st.Settings().Set(t.Context(), key, value); err != nil {
		t.Fatalf("Set(%s): %v", key, err)
	}
}
