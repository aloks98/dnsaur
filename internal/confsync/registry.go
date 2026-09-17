package confsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aloks98/dnsaur/internal/api"
	"github.com/aloks98/dnsaur/internal/auth"
	"github.com/aloks98/dnsaur/internal/store"
)

const (
	// replicasSetting holds the main's registered replicas, a JSON object
	// keyed by instance id. A setting rather than a table because it is a
	// handful of rows the operator curates by hand (§6), and because
	// sync.* is local (§4.3), so it can never travel in a bundle to a
	// replica that would then think it had replicas of its own.
	replicasSetting = "sync.replicas"
	// pairingSetting holds the one live pairing code, as a pairingState.
	// Internal for replicasSetting's reason, and never the code itself.
	pairingSetting = "sync.pairing"
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

// ErrPairingRefused is every way a pairing attempt can fail that is not the
// store failing: wrong code, expired code, voided code, no code. One error
// for all four deliberately — a guesser told which one it was learns whether
// a code is live, and an operator with the code in front of them does not
// need to be told.
var ErrPairingRefused = errors.New("pairing code refused")

// ErrNotRegistered is a heartbeat for an instance id this main does not
// know: the box was forgotten while it was mid-probe. It is a sentinel
// because the answer is the one a wrong secret gets — pair again — and not
// the one a store that will not answer gets.
var ErrNotRegistered = errors.New("no such replica is registered")

// replicaRecord is one entry as sync.replicas stores it: what the dashboard
// sees, plus the hash of the secret that replica authenticates with. The
// hash never leaves this package — List and Status hand out the api.Replica
// inside it.
type replicaRecord struct {
	api.Replica
	SecretHash string `json:"secret_hash"`
}

func NewRegistry(st store.Store) *Registry {
	return &Registry{st: st, now: time.Now, staleAfter: defaultStaleAfter}
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
	for id, rec := range all {
		// The key is the identity; a value disagreeing with it would be a
		// row nobody could remove.
		r := rec.Replica
		r.InstanceID = id
		r.Stale = r.LastSeen < cutoff
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b api.Replica) int { return strings.Compare(a.InstanceID, b.InstanceID) })
	return out, nil
}

// Status is the main's whole sync state for GET /sync/status. A store that
// will not answer does not fail the endpoint — the role and the sync key are
// still true — but it is reported as the failure it is: a registry that
// cannot be read is a main that admits no transfer and notifies nobody (§6),
// and "no replicas" is what having none looks like.
func (g *Registry) Status(ctx context.Context) api.SyncStatus {
	s := api.SyncStatus{Role: "main"}
	reps, err := g.List(ctx)
	if err != nil {
		slog.Warn("reading the registered replicas failed", "err", err)
		s.LastError = err.Error()
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

// NewPairingCode mints the code the operator carries from this box to the
// replica, replacing any code still live: one door open at a time (§6). The
// plain code is returned once and stored nowhere — sync.pairing keeps its
// hash, when it expires, and the guesses made against it.
func (g *Registry) NewPairingCode(ctx context.Context) (string, int64, error) {
	code, err := newPairingCode()
	if err != nil {
		return "", 0, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	expiresAt := g.now().Add(pairingTTL).UnixMilli()
	p := pairingState{Hash: auth.HashToken(normalizePairingCode(code)), ExpiresAt: expiresAt}
	if err := g.savePairing(ctx, p); err != nil {
		return "", 0, err
	}
	return code, expiresAt, nil
}

// Pair spends a correct code on one replica: it records instanceID at
// dnsAddr, replacing any earlier entry for that id, and returns that
// replica's secret — the only moment the plain secret exists on the main.
// Anything else is ErrPairingRefused.
func (g *Registry) Pair(ctx context.Context, code, instanceID, dnsAddr string) (string, error) {
	// Refused before the code is looked at, let alone spent: an entry keyed
	// on nothing would make Authenticate answer ("", true), and a caller
	// reading the bool alone would take a stranger for a replica.
	if instanceID == "" {
		return "", ErrPairingRefused
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	p, live, err := g.loadPairing(ctx)
	if err != nil {
		return "", err
	}
	if !live || p.Attempts >= pairingMaxAttempts || g.now().UnixMilli() > p.ExpiresAt {
		return "", ErrPairingRefused
	}
	if p.Hash != auth.HashToken(normalizePairingCode(code)) {
		// Persisted rather than counted in memory: a restart inside the ten
		// minute window would otherwise hand a guesser five more tries.
		p.Attempts++
		if err := g.savePairing(ctx, p); err != nil {
			return "", err
		}
		return "", ErrPairingRefused
	}
	secret, hash, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	// Read before the code is spent: a row this build cannot parse takes no
	// entry, and spending the code on it would cost the operator a code for
	// a pairing that was never going to land.
	all, err := g.load(ctx)
	if err != nil {
		return "", err
	}
	// Spent before the entry is written. A store that fails on the write
	// below costs the operator a fresh code; the other order would leave a
	// code that has already been used correctly live for a second box.
	if err := g.clearPairing(ctx); err != nil {
		return "", err
	}
	all[instanceID] = replicaRecord{
		Replica:    api.Replica{InstanceID: instanceID, DNSAddr: dnsAddr, LastSeen: g.now().UnixMilli()},
		SecretHash: hash,
	}
	if err := g.save(ctx, all); err != nil {
		return "", err
	}
	return secret, nil
}

// Authenticate maps the secret a replica presents to the instance id it was
// minted for. A secret nobody holds is ("", false, nil): not an error, just
// not a replica, which is what Forget leaves behind.
//
// A scan, because only the hash is stored and the registry is a handful of
// entries the operator paired by hand (§6).
func (g *Registry) Authenticate(ctx context.Context, secret string) (string, bool, error) {
	if secret == "" {
		return "", false, nil
	}
	hash := auth.HashToken(secret)
	g.mu.Lock()
	defer g.mu.Unlock()
	all, err := g.load(ctx)
	if err != nil {
		return "", false, err
	}
	for id, rec := range all {
		if rec.SecretHash == hash {
			return id, true, nil
		}
	}
	return "", false, nil
}

// Heartbeat stamps a registered replica from its version probe, which is the
// only heartbeat there is (§3). The main dates it rather than taking the
// replica's word for when it called: a clock skew must not decide whether a
// box looks stale.
//
// An id that is not registered is an error rather than a silent no-op: it is
// a replica that was forgotten here, and the answer it needs is that it has
// to pair again, not a 204.
func (g *Registry) Heartbeat(ctx context.Context, instanceID string, applied int64, dhcp bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	all, err := g.load(ctx)
	if err != nil {
		return err
	}
	rec, ok := all[instanceID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotRegistered, instanceID)
	}
	rec.LastSeen = g.now().UnixMilli()
	rec.VersionApplied = applied
	// The probe's answer every time, never a latch: an engine that was
	// turned off is a standby the main has to stop naming.
	rec.DHCP = dhcp
	all[instanceID] = rec
	return g.save(ctx, all)
}

// loadPairing reads the live code. live is false when there is none, which
// includes a row this build cannot read as a state with a hash in it.
func (g *Registry) loadPairing(ctx context.Context) (pairingState, bool, error) {
	raw, found, err := g.st.Settings().Get(ctx, pairingSetting)
	if err != nil {
		return pairingState{}, false, err
	}
	if !found || raw == "" {
		return pairingState{}, false, nil
	}
	var p pairingState
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return pairingState{}, false, fmt.Errorf("%s is not the JSON this build wrote: %w", pairingSetting, err)
	}
	return p, p.Hash != "", nil
}

func (g *Registry) savePairing(ctx context.Context, p pairingState) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return g.st.Settings().SetInternal(ctx, pairingSetting, string(raw))
}

// clearPairing spends the live code. The settings store has no delete, so an
// empty value is the deletion: loadPairing reads it as no live code.
func (g *Registry) clearPairing(ctx context.Context) error {
	return g.st.Settings().SetInternal(ctx, pairingSetting, "")
}

func (g *Registry) load(ctx context.Context) (map[string]replicaRecord, error) {
	raw, found, err := g.st.Settings().Get(ctx, replicasSetting)
	if err != nil {
		return nil, err
	}
	all := map[string]replicaRecord{}
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

func (g *Registry) save(ctx context.Context, all map[string]replicaRecord) error {
	raw, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return g.st.Settings().SetInternal(ctx, replicasSetting, string(raw))
}
