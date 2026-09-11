package confsync

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aloks98/dnsaur/internal/api"
	"github.com/aloks98/dnsaur/internal/store"
)

const (
	// replicasSetting holds the main's registered replicas, a JSON object
	// keyed by instance id. A setting rather than a table because it is a
	// handful of rows the operator curates by hand (§6), and because
	// sync.* is local (§4.3), so it can never travel in a bundle to a
	// replica that would then think it had replicas of its own.
	replicasSetting = "sync.replicas"
	// syncKeySetting is the TSIG key the main designates for its replicas'
	// transfers; the status band shows it by name.
	syncKeySetting = "sync.tsig_key_id"
	// intervalSetting is the replica poll period. The main reads it too:
	// staleness is three of them (§6).
	intervalSetting = "sync.interval_seconds"
	// defaultStaleAfter is three times the default interval, used when this
	// box has no usable sync.interval_seconds of its own.
	defaultStaleAfter = 90 * time.Second
)

// Registry is the main's half of the sync subsystem: which replicas have
// registered, when each was last heard from, and which version it applied.
//
// Everything it knows lives in one settings row, written with SetInternal:
// a registration arrives every interval from every replica, and bumping
// config_version on each would make every replica pull a bundle it already
// has — a loop the size of the fleet, forever.
type Registry struct {
	st store.Store
	// mu serialises the read-modify-write of that one row against itself.
	// Two replicas registering in the same instant are two handlers on two
	// goroutines, and the store has no compare-and-set for a setting.
	mu         sync.Mutex
	now        func() time.Time
	staleAfter time.Duration
}

func NewRegistry(st store.Store) *Registry {
	return &Registry{st: st, now: time.Now, staleAfter: defaultStaleAfter}
}

// Register records a replica that has just applied a version, replacing any
// earlier entry for the same instance id.
//
// The main dates the registration itself: a replica reporting when it thinks
// it called would let a clock skew decide whether it looks stale.
func (g *Registry) Register(ctx context.Context, r api.Replica) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	all, err := g.load(ctx)
	if err != nil {
		return err
	}
	r.LastSeen = g.now().UnixMilli()
	r.Stale = false
	all[r.InstanceID] = r
	return g.save(ctx, all)
}

// Forget removes one replica. An id that is not registered is already in the
// state the caller asked for, so it is not an error and writes nothing.
func (g *Registry) Forget(ctx context.Context, instanceID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	all, err := g.load(ctx)
	if err != nil {
		return err
	}
	if _, ok := all[instanceID]; !ok {
		return nil
	}
	delete(all, instanceID)
	return g.save(ctx, all)
}

// List returns every registered replica, ordered by instance id so the Sync
// band does not reshuffle between polls, with Stale computed as of now.
func (g *Registry) List(ctx context.Context) ([]api.Replica, error) {
	g.mu.Lock()
	all, err := g.load(ctx)
	g.mu.Unlock()
	if err != nil {
		return nil, err
	}
	stale := g.staleness(ctx)
	cutoff := g.now().Add(-stale).UnixMilli()
	out := make([]api.Replica, 0, len(all))
	for id, r := range all {
		// The key is the identity; a value disagreeing with it would be a
		// row nobody could remove.
		r.InstanceID = id
		r.Stale = r.LastSeen < cutoff
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b api.Replica) int { return strings.Compare(a.InstanceID, b.InstanceID) })
	return out, nil
}

// Status is the main's whole sync state for GET /sync/status. A store that
// will not answer is logged and reported as "no replicas" rather than
// failing the endpoint: the role and the sync key are still true.
func (g *Registry) Status(ctx context.Context) api.SyncStatus {
	s := api.SyncStatus{Role: "main"}
	reps, err := g.List(ctx)
	if err != nil {
		slog.Warn("reading the registered replicas failed", "err", err)
	} else {
		s.Replicas = reps
	}
	s.SyncKey = g.SyncKeyName(ctx)
	return s
}

// SyncKeyName resolves sync.tsig_key_id to the key's owner name, "" when no
// key is designated or the id names none. Exported because the zones hooks
// in internal/app are the same question asked on the DNS side: the key a
// registered replica's transfer must be signed with, and the one a NOTIFY to
// it is signed under (§6).
func (g *Registry) SyncKeyName(ctx context.Context) string {
	id, err := g.st.Settings().GetInt(ctx, syncKeySetting)
	if err != nil || id == 0 {
		return ""
	}
	k, found, err := g.st.TSIGKeys().Get(ctx, id)
	if err != nil || !found {
		return ""
	}
	return k.Name
}

// staleness is §6's three intervals, from this box's own
// sync.interval_seconds — the period the operator set for the pair — and
// g.staleAfter when that value is unset or unusable.
func (g *Registry) staleness(ctx context.Context) time.Duration {
	if n, err := g.st.Settings().GetInt(ctx, intervalSetting); err == nil && n >= minIntervalSeconds {
		return 3 * time.Duration(n) * time.Second
	}
	return g.staleAfter
}

func (g *Registry) load(ctx context.Context) (map[string]api.Replica, error) {
	raw, found, err := g.st.Settings().Get(ctx, replicasSetting)
	if err != nil {
		return nil, err
	}
	all := map[string]api.Replica{}
	if !found || raw == "" {
		return all, nil
	}
	if err := json.Unmarshal([]byte(raw), &all); err != nil {
		// Not repaired by overwriting it: an operator who can see the row
		// can clear it, and silently dropping registrations would silently
		// drop the implicit AXFR allow that depends on them (§6).
		return nil, fmt.Errorf("%s is not the JSON this build wrote: %w", replicasSetting, err)
	}
	return all, nil
}

func (g *Registry) save(ctx context.Context, all map[string]api.Replica) error {
	raw, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return g.st.Settings().SetInternal(ctx, replicasSetting, string(raw))
}
