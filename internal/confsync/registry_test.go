package confsync

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/auth"
	"github.com/aloks98/dnsaur/internal/store"
)

// mustPair registers one replica the way the two endpoints do: mint a code
// on this main and spend it. There is no other way into the registry — an
// entry that never paired authenticates nobody.
func mustPair(t *testing.T, g *Registry, instanceID, dnsAddr string) {
	t.Helper()
	code, _, err := g.NewPairingCode(t.Context())
	if err != nil {
		t.Fatalf("NewPairingCode: %v", err)
	}
	if _, err := g.Pair(t.Context(), code, instanceID, dnsAddr); err != nil {
		t.Fatalf("Pair(%s): %v", instanceID, err)
	}
}

func TestRegistryStaleness(t *testing.T) {
	ctx := t.Context()
	st := openStore(t)
	now := time.Unix(1_757_580_000, 0)
	g := NewRegistry(st)
	g.now = func() time.Time { return now }
	g.staleAfter = 90 * time.Second

	mustPair(t, g, "r1", "10.0.0.6:53")
	if err := g.Heartbeat(ctx, "r1", 3, false); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	rs, err := g.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rs) != 1 || rs[0].Stale || rs[0].VersionApplied != 3 || rs[0].LastSeen != now.UnixMilli() {
		t.Fatalf("fresh registration: %+v", rs)
	}

	// A second heartbeat updates the one entry rather than adding another.
	now = now.Add(30 * time.Second)
	if err := g.Heartbeat(ctx, "r1", 4, false); err != nil {
		t.Fatalf("second Heartbeat: %v", err)
	}
	if rs, _ := g.List(ctx); len(rs) != 1 || rs[0].VersionApplied != 4 || rs[0].LastSeen != now.UnixMilli() {
		t.Fatalf("after a second heartbeat: %+v", rs)
	}

	now = now.Add(2 * time.Minute)
	rs, err = g.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rs) != 1 || !rs[0].Stale {
		t.Fatalf("%+v", rs)
	}

	if err := g.Forget(ctx, "r1"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if rs, _ := g.List(ctx); len(rs) != 0 {
		t.Fatal("forget")
	}
	// Forgetting what is not registered is the state the caller asked for.
	if err := g.Forget(ctx, "r1"); err != nil {
		t.Fatalf("Forget of an unknown id: %v", err)
	}
}

// TestRegistryStatus is the main's half of GET /sync/status: the designated
// sync key by name, and the replicas.
func TestRegistryStatus(t *testing.T) {
	ctx := t.Context()
	st := openStore(t)
	g := NewRegistry(st)

	s := g.Status(ctx)
	if s.Role != "main" || s.SyncKey != "" || len(s.Replicas) != 0 {
		t.Fatalf("a main with nothing configured: %+v", s)
	}

	id, err := st.TSIGKeys().Create(ctx, store.TSIGKey{Name: "xfer.example.", Algorithm: "hmac-sha256", Secret: "c2VjcmV0"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := st.Settings().Set(ctx, "sync.tsig_key_id", strconv.FormatInt(id, 10)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	mustPair(t, g, "r1", "10.0.0.6:53")
	s = g.Status(ctx)
	if s.Role != "main" || s.SyncKey != "xfer.example." || len(s.Replicas) != 1 {
		t.Fatalf("status %+v", s)
	}
}

// TestRegistryStalenessFollowsTheInterval: §6's three intervals, read from
// the box's own sync.interval_seconds rather than frozen at 90 seconds.
func TestRegistryStalenessFollowsTheInterval(t *testing.T) {
	ctx := t.Context()
	st := openStore(t)
	g := NewRegistry(st)
	now := time.Unix(1_757_580_000, 0)
	g.now = func() time.Time { return now }
	mustPair(t, g, "r1", "10.0.0.6:53")
	if err := st.Settings().Set(ctx, "sync.interval_seconds", "300"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	now = now.Add(5 * time.Minute) // stale at the default 90s, fresh at 3 x 300s
	if rs, _ := g.List(ctx); len(rs) != 1 || rs[0].Stale {
		t.Fatalf("%+v", rs)
	}
	now = now.Add(20 * time.Minute)
	if rs, _ := g.List(ctx); len(rs) != 1 || !rs[0].Stale {
		t.Fatalf("%+v", rs)
	}
}

// TestPairSpendsTheCode: a correct code registers the replica and dies doing
// it, so the operator showing it to one box cannot be replayed by a second.
// It is presented lowercased and without its dash, which is what an operator
// typing what they read off the other screen will sometimes send (§6).
func TestPairSpendsTheCode(t *testing.T) {
	ctx := t.Context()
	g := NewRegistry(openStore(t))
	now := time.Unix(1_757_580_000, 0)
	g.now = func() time.Time { return now }

	code, expiresAt, err := g.NewPairingCode(ctx)
	if err != nil {
		t.Fatalf("NewPairingCode: %v", err)
	}
	if want := now.Add(pairingTTL).UnixMilli(); expiresAt != want {
		t.Fatalf("expiresAt %d, want %d", expiresAt, want)
	}

	typed := strings.ToLower(strings.ReplaceAll(code, "-", ""))
	secret, err := g.Pair(ctx, typed, "r1", "10.0.0.6:53")
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if secret == "" {
		t.Fatal("Pair returned no secret")
	}
	rs, err := g.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rs) != 1 || rs[0].InstanceID != "r1" || rs[0].DNSAddr != "10.0.0.6:53" {
		t.Fatalf("after pairing: %+v", rs)
	}

	if _, err := g.Pair(ctx, code, "r2", "10.0.0.7:53"); !errors.Is(err, ErrPairingRefused) {
		t.Fatalf("the same code a second time: %v", err)
	}
	if rs, _ := g.List(ctx); len(rs) != 1 {
		t.Fatalf("a refused pair registered something: %+v", rs)
	}
}

// TestPairRefusesAfterFiveWrongAttempts: the code is 40 bits, so the attempt
// count is what makes guessing it pointless. The count is in the store, not
// in memory — the last attempt here is made through a second Registry over
// the same store, which is what a restart mid-window looks like.
func TestPairRefusesAfterFiveWrongAttempts(t *testing.T) {
	ctx := t.Context()
	st := openStore(t)
	g := NewRegistry(st)

	code, _, err := g.NewPairingCode(ctx)
	if err != nil {
		t.Fatalf("NewPairingCode: %v", err)
	}
	for i := range pairingMaxAttempts {
		guesser := g
		if i == pairingMaxAttempts-1 {
			guesser = NewRegistry(st)
		}
		// O is not in the alphabet, so this is a code nobody can ever mint.
		wrong := fmt.Sprintf("WRONG-CO%d", i)
		if _, err := guesser.Pair(ctx, wrong, "r1", "10.0.0.6:53"); !errors.Is(err, ErrPairingRefused) {
			t.Fatalf("wrong code %d: %v", i, err)
		}
	}
	if _, err := g.Pair(ctx, code, "r1", "10.0.0.6:53"); !errors.Is(err, ErrPairingRefused) {
		t.Fatalf("the right code after five wrong ones: %v", err)
	}
	if rs, _ := g.List(ctx); len(rs) != 0 {
		t.Fatalf("a voided code registered something: %+v", rs)
	}
}

func TestPairRefusesAnExpiredCode(t *testing.T) {
	ctx := t.Context()
	g := NewRegistry(openStore(t))
	now := time.Unix(1_757_580_000, 0)
	g.now = func() time.Time { return now }

	code, _, err := g.NewPairingCode(ctx)
	if err != nil {
		t.Fatalf("NewPairingCode: %v", err)
	}
	now = now.Add(pairingTTL - time.Second)
	if _, err := g.Pair(ctx, code, "r1", "10.0.0.6:53"); err != nil {
		t.Fatalf("inside the window: %v", err)
	}

	code, _, err = g.NewPairingCode(ctx)
	if err != nil {
		t.Fatalf("NewPairingCode: %v", err)
	}
	now = now.Add(pairingTTL + time.Second)
	if _, err := g.Pair(ctx, code, "r2", "10.0.0.7:53"); !errors.Is(err, ErrPairingRefused) {
		t.Fatalf("past the window: %v", err)
	}
}

// TestNewPairingCodeReplacesTheLiveOne: one live code at a time (§6), so an
// operator who mints a second because the first scrolled away has not left
// two doors open.
func TestNewPairingCodeReplacesTheLiveOne(t *testing.T) {
	ctx := t.Context()
	g := NewRegistry(openStore(t))

	first, _, err := g.NewPairingCode(ctx)
	if err != nil {
		t.Fatalf("NewPairingCode: %v", err)
	}
	second, _, err := g.NewPairingCode(ctx)
	if err != nil {
		t.Fatalf("NewPairingCode: %v", err)
	}
	if _, err := g.Pair(ctx, first, "r1", "10.0.0.6:53"); !errors.Is(err, ErrPairingRefused) {
		t.Fatalf("the superseded code: %v", err)
	}
	if _, err := g.Pair(ctx, second, "r1", "10.0.0.6:53"); err != nil {
		t.Fatalf("the live code: %v", err)
	}
}

func TestAuthenticateFindsTheReplicaBySecret(t *testing.T) {
	ctx := t.Context()
	g := NewRegistry(openStore(t))

	code, _, err := g.NewPairingCode(ctx)
	if err != nil {
		t.Fatalf("NewPairingCode: %v", err)
	}
	secret, err := g.Pair(ctx, code, "r1", "10.0.0.6:53")
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}

	id, ok, err := g.Authenticate(ctx, secret)
	if err != nil || !ok || id != "r1" {
		t.Fatalf("Authenticate(secret) = %q, %v, %v", id, ok, err)
	}
	if id, ok, err := g.Authenticate(ctx, "cGxhaW5seS1ub3QtdGhlLXNlY3JldA"); err != nil || ok || id != "" {
		t.Fatalf("Authenticate(a stranger) = %q, %v, %v", id, ok, err)
	}

	// Forget revokes the secret with the entry (§6).
	if err := g.Forget(ctx, "r1"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if id, ok, err := g.Authenticate(ctx, secret); err != nil || ok || id != "" {
		t.Fatalf("Authenticate after Forget = %q, %v, %v", id, ok, err)
	}
}

// TestHeartbeatStampsLastSeenAndApplied: the version probe is the heartbeat
// (§3), and the main dates it, so a replica's clock cannot decide whether it
// looks stale.
func TestHeartbeatStampsLastSeenAndApplied(t *testing.T) {
	ctx := t.Context()
	g := NewRegistry(openStore(t))
	now := time.Unix(1_757_580_000, 0)
	g.now = func() time.Time { return now }

	code, _, err := g.NewPairingCode(ctx)
	if err != nil {
		t.Fatalf("NewPairingCode: %v", err)
	}
	if _, err := g.Pair(ctx, code, "r1", "10.0.0.6:53"); err != nil {
		t.Fatalf("Pair: %v", err)
	}

	now = now.Add(30 * time.Second)
	if err := g.Heartbeat(ctx, "r1", 42, true); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	rs, err := g.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rs) != 1 || rs[0].VersionApplied != 42 || rs[0].LastSeen != now.UnixMilli() || rs[0].DNSAddr != "10.0.0.6:53" {
		t.Fatalf("after a heartbeat: %+v", rs)
	}
	// Whether that box runs a DHCP engine is stamped from the same probe:
	// it is the only thing the main ever hears from a replica, and the DHCP
	// renderer will not pair with a box that has no engine to pair with
	// (design §6).
	if !rs[0].DHCP {
		t.Error("a heartbeat from a box running an engine did not record it")
	}
	// And it is the probe's answer every time, not a latch: an engine that
	// was turned off is a standby that has to be dropped.
	if err := g.Heartbeat(ctx, "r1", 43, false); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if rs, err = g.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	} else if rs[0].DHCP {
		t.Error("a box that stopped running an engine is still recorded as running one")
	}

	if err := g.Heartbeat(ctx, "nobody", 1, false); err == nil {
		t.Fatal("a heartbeat from an unregistered id was accepted")
	}
}

// TestListNeverExposesTheSecretHash: the hash is stored beside the entry and
// must not reach the dashboard with it.
func TestListNeverExposesTheSecretHash(t *testing.T) {
	ctx := t.Context()
	g := NewRegistry(openStore(t))
	code, _, err := g.NewPairingCode(ctx)
	if err != nil {
		t.Fatalf("NewPairingCode: %v", err)
	}
	secret, err := g.Pair(ctx, code, "r1", "10.0.0.6:53")
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}

	raw, err := json.Marshal(g.Status(ctx))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// The hash by value too, so a leak under some other field name is
	// caught as well as one under this one.
	for _, leak := range []string{"secret_hash", secret, auth.HashToken(secret)} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("the status carries %q: %s", leak, raw)
		}
	}
}

// TestPairReplacesAnExistingInstance: pairing again with the same instance id
// replaces the entry and its secret (§6), which is how an operator re-pairs a
// replica whose secret was lost.
func TestPairReplacesAnExistingInstance(t *testing.T) {
	ctx := t.Context()
	g := NewRegistry(openStore(t))

	code, _, err := g.NewPairingCode(ctx)
	if err != nil {
		t.Fatalf("NewPairingCode: %v", err)
	}
	first, err := g.Pair(ctx, code, "r1", "10.0.0.6:53")
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	code, _, err = g.NewPairingCode(ctx)
	if err != nil {
		t.Fatalf("NewPairingCode: %v", err)
	}
	second, err := g.Pair(ctx, code, "r1", "10.0.0.9:53")
	if err != nil {
		t.Fatalf("re-Pair: %v", err)
	}
	if first == second {
		t.Fatal("re-pairing handed back the same secret")
	}

	if _, ok, err := g.Authenticate(ctx, first); err != nil || ok {
		t.Fatalf("the replaced secret still authenticates: %v, %v", ok, err)
	}
	if id, ok, err := g.Authenticate(ctx, second); err != nil || !ok || id != "r1" {
		t.Fatalf("the new secret: %q, %v, %v", id, ok, err)
	}
	rs, err := g.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rs) != 1 || rs[0].DNSAddr != "10.0.0.9:53" {
		t.Fatalf("re-pairing left: %+v", rs)
	}
}

// TestPairRefusesAnEmptyInstanceID: without the id there is nothing to key
// the entry on, and an entry keyed "" would make Authenticate answer
// ("", true) — a caller that reads the bool and not the id would treat a
// stranger's secret as a replica's. Refused before the code is spent, so the
// operator's code survives the replica's bad request.
func TestPairRefusesAnEmptyInstanceID(t *testing.T) {
	ctx := t.Context()
	g := NewRegistry(openStore(t))

	code, _, err := g.NewPairingCode(ctx)
	if err != nil {
		t.Fatalf("NewPairingCode: %v", err)
	}
	if _, err := g.Pair(ctx, code, "", "10.0.0.6:53"); !errors.Is(err, ErrPairingRefused) {
		t.Fatalf("an empty instance id: %v", err)
	}
	if rs, _ := g.List(ctx); len(rs) != 0 {
		t.Fatalf("it registered something: %+v", rs)
	}
	// The code was not spent on it.
	if _, err := g.Pair(ctx, code, "r1", "10.0.0.6:53"); err != nil {
		t.Fatalf("the code afterwards: %v", err)
	}
}

// TestListReadsAReplicaRegisteredBeforePairing: sync.replicas rows written by
// a build with no secret_hash in them are still entries, and still shown.
// They authenticate nobody, which is what makes re-pairing the way back.
func TestListReadsAReplicaRegisteredBeforePairing(t *testing.T) {
	ctx := t.Context()
	st := openStore(t)
	g := NewRegistry(st)
	now := time.Unix(1_757_580_000, 0)
	g.now = func() time.Time { return now }

	const row = `{"r1":{"instance_id":"r1","dns_addr":"10.0.0.6:53","version_applied":7,"last_seen":1757580000000}}`
	if err := st.Settings().SetInternal(ctx, "sync.replicas", row); err != nil {
		t.Fatalf("SetInternal: %v", err)
	}

	rs, err := g.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rs) != 1 || rs[0].InstanceID != "r1" || rs[0].DNSAddr != "10.0.0.6:53" || rs[0].VersionApplied != 7 || rs[0].Stale {
		t.Fatalf("a pre-pairing row: %+v", rs)
	}
	for _, secret := range []string{"", auth.HashToken(""), "cGxhaW5seS1ub3QtdGhlLXNlY3JldA"} {
		if id, ok, err := g.Authenticate(ctx, secret); err != nil || ok || id != "" {
			t.Fatalf("Authenticate(%q) = %q, %v, %v", secret, id, ok, err)
		}
	}
}

// TestStatusReportsACorruptReplicasRow: sync.replicas is one JSON document,
// and a row this build cannot read is a main that admits no transfer and
// notifies nobody (§6) while every screen reads "no replicas". The failure
// has to be on the screen, not only in the log.
func TestStatusReportsACorruptReplicasRow(t *testing.T) {
	ctx := t.Context()
	st := openStore(t)
	g := NewRegistry(st)
	if err := st.Settings().SetInternal(ctx, replicasSetting, "{not json"); err != nil {
		t.Fatalf("SetInternal: %v", err)
	}

	s := g.Status(ctx)
	if s.Role != "main" || len(s.Replicas) != 0 {
		t.Fatalf("status %+v, want a main with no replicas", s)
	}
	if !strings.Contains(s.LastError, replicasSetting) {
		t.Fatalf("last_error = %q, want the unreadable %s named", s.LastError, replicasSetting)
	}
}

// TestPairRefusesACorruptRowBeforeSpendingTheCode: the entry cannot be
// written into a row nothing can parse, so the code must survive to be spent
// once the operator has cleared it.
func TestPairRefusesACorruptRowBeforeSpendingTheCode(t *testing.T) {
	ctx := t.Context()
	st := openStore(t)
	g := NewRegistry(st)

	code, _, err := g.NewPairingCode(ctx)
	if err != nil {
		t.Fatalf("NewPairingCode: %v", err)
	}
	if err := st.Settings().SetInternal(ctx, replicasSetting, "{not json"); err != nil {
		t.Fatalf("SetInternal: %v", err)
	}
	if _, err := g.Pair(ctx, code, "r1", "10.0.0.6:53"); err == nil || errors.Is(err, ErrPairingRefused) {
		t.Fatalf("Pair against a corrupt row = %v, want the row reported rather than the code refused", err)
	}

	// Cleared the way an operator clears it, and the same code still works.
	if err := st.Settings().SetInternal(ctx, replicasSetting, ""); err != nil {
		t.Fatalf("SetInternal: %v", err)
	}
	if _, err := g.Pair(ctx, code, "r1", "10.0.0.6:53"); err != nil {
		t.Fatalf("the code after the row was cleared: %v", err)
	}
}
