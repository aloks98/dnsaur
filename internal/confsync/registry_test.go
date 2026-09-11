package confsync

import (
	"strconv"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/api"
	"github.com/aloks98/dnsaur/internal/store"
)

func TestRegistryStaleness(t *testing.T) {
	ctx := t.Context()
	st := openStore(t)
	now := time.Unix(1_757_580_000, 0)
	g := NewRegistry(st)
	g.now = func() time.Time { return now }
	g.staleAfter = 90 * time.Second

	if err := g.Register(ctx, api.Replica{InstanceID: "r1", DNSAddr: "10.0.0.6:53", VersionApplied: 3}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	rs, err := g.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rs) != 1 || rs[0].Stale || rs[0].VersionApplied != 3 || rs[0].LastSeen != now.UnixMilli() {
		t.Fatalf("fresh registration: %+v", rs)
	}

	// Re-registering the same id updates it rather than adding a second.
	now = now.Add(30 * time.Second)
	if err := g.Register(ctx, api.Replica{InstanceID: "r1", DNSAddr: "10.0.0.6:53", VersionApplied: 4}); err != nil {
		t.Fatalf("re-Register: %v", err)
	}
	if rs, _ := g.List(ctx); len(rs) != 1 || rs[0].VersionApplied != 4 || rs[0].LastSeen != now.UnixMilli() {
		t.Fatalf("re-registration: %+v", rs)
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
	if err := g.Register(ctx, api.Replica{InstanceID: "r1", DNSAddr: "10.0.0.6:53"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
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
	if err := g.Register(ctx, api.Replica{InstanceID: "r1", DNSAddr: "10.0.0.6:53"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
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
