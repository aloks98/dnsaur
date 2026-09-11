package confsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloks98/dnsaur/internal/api"
	"github.com/aloks98/dnsaur/internal/store"
)

const (
	peerURLSetting = "sync.peer_url"
	tokenSetting   = "sync.token"
	primaryDNSKey  = "sync.primary_dns"
	// Bookkeeping the replica writes about its own pulls, with SetInternal:
	// none of it is configuration, and a version bump per poll would make
	// the box reconfigure itself every interval.
	appliedVersionSetting = "sync.applied_version"
	appliedAtSetting      = "sync.applied_at"
	lastPullAtSetting     = "sync.last_pull_at"
	lastErrorSetting      = "sync.last_error"

	versionPath  = "/api/v1/sync/version"
	bundlePath   = "/api/v1/sync/bundle"
	replicasPath = "/api/v1/sync/replicas"

	// probeTimeout bounds the version probe and the registration, both a few
	// hundred bytes; fetchTimeout bounds the bundle, which carries every
	// group, client, rule, list assignment, key and zone the main has.
	probeTimeout = 10 * time.Second
	fetchTimeout = 60 * time.Second
	// maxProbeBytes is how much of a small answer is read: the version
	// probe's body, and as much of any error response as is drained to keep
	// the connection reusable. A peer that answers a two-field document with
	// more than this is not a peer this build understands.
	maxProbeBytes = 64 << 10
	// maxBundleBytes is the same for the bundle, which is the only large
	// answer: every group, client, list, rule, key and zone definition the
	// main holds. Homelab-sized configurations are kilobytes; the cap is
	// three orders of magnitude above that, and exists so a peer that
	// answers with a stream instead of a document cannot be decoded into
	// this box's memory until it runs out.
	maxBundleBytes = 32 << 20
	// peerReadTimeout bounds PeerURL's settings read. That read is on the
	// API's write path, so it may not hang: five seconds is far beyond a
	// healthy answer and short of a stuck one.
	peerReadTimeout = 5 * time.Second

	// minIntervalSeconds mirrors the settings validator's floor. A stored
	// value below it (or unparseable) is not a reason to spin.
	minIntervalSeconds = 5
	minInterval        = minIntervalSeconds * time.Second
	// defaultInterval is defaultSettings()' sync.interval_seconds.
	defaultInterval = 30 * time.Second
	// idleInterval is how often a box with no peer looks again. It is not a
	// poll — nothing is dialled — it is the window in which setting a peer
	// URL starts a pull without a restart.
	idleInterval = 5 * time.Second
)

// Reloader is what a pull needs from the running server once a bundle is in
// the store: the subset of api.Reloader the API handlers themselves run
// after their own writes, in the order §5 names.
type Reloader interface {
	ReloadClients(ctx context.Context) error
	RecompileFilters(ctx context.Context) error
	ReloadZones(ctx context.Context) error
	ReloadSettings(ctx context.Context) error
}

// Replica is the pull loop: every interval it asks the main whether its
// config_version moved, and applies the bundle when it has.
//
// One instance runs on every box, main or replica, because which one this is
// is a setting and not a startup decision (§2): a box with no sync.peer_url
// simply never dials.
type Replica struct {
	st     store.Store
	reload Reloader
	// client has no Timeout of its own: each request carries its own
	// deadline, and the two differ by an order of magnitude.
	client              *http.Client
	now                 func() time.Time
	instanceID, dnsAddr string

	mu   sync.Mutex
	last api.SyncStatus
	// peer is the last peer URL successfully read, the answer PeerURL falls
	// back to when the store will not answer. Lifting the API's write guard
	// because one query failed is how two boxes start accepting writes.
	peer atomic.Value // string
}

func NewReplica(st store.Store, reload Reloader, instanceID, dnsAddr string) *Replica {
	return &Replica{
		st:     st,
		reload: reload,
		client: &http.Client{
			// A redirect is never followed. The peer URL is scheme and host
			// only and the paths are this API's own, so a 3xx means the
			// operator is pointed at something that is not the main —
			// following it would send the pull token to whatever answered.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now:        time.Now,
		instanceID: instanceID,
		dnsAddr:    dnsAddr,
	}
}

// PeerURL is the main this box follows, "" when it follows none. Read from
// the store on every call rather than from the last poll: it is what the
// API's write guard (§7) asks on every write, and a cached answer would let
// a replica accept writes for a whole interval after it started following.
func (r *Replica) PeerURL() string {
	ctx, cancel := context.WithTimeout(context.Background(), peerReadTimeout)
	defer cancel()
	v, _, err := r.st.Settings().Get(ctx, peerURLSetting)
	if err != nil {
		slog.Warn("reading the sync peer failed, keeping the last one known", "err", err)
		last, _ := r.peer.Load().(string)
		return last
	}
	r.peer.Store(v)
	return v
}

// Status is what the settings screen and the warning strip show. The peer is
// read live for the reason PeerURL is; everything else is what the last pull
// left behind.
func (r *Replica) Status() api.SyncStatus {
	r.mu.Lock()
	s := r.last
	r.mu.Unlock()
	s.Role = "replica"
	s.PeerURL = r.PeerURL()
	s.PlainHTTP = strings.HasPrefix(s.PeerURL, "http://")
	return s
}

// Run polls until ctx ends. It pulls once before its first wait, so a
// restart applies whatever the main changed while this box was down instead
// of waiting out an interval, and re-reads the interval and the peer URL
// every cycle, so a settings write takes effect without a restart.
func (r *Replica) Run(ctx context.Context) {
	for {
		if err := r.PullOnce(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("config sync pull failed", "err", err)
		}
		t := time.NewTimer(r.pollInterval(ctx))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// pollInterval is how long to wait before looking again: the configured
// interval while following, and idleInterval while not.
func (r *Replica) pollInterval(ctx context.Context) time.Duration {
	if r.PeerURL() == "" {
		return idleInterval
	}
	n, err := r.st.Settings().GetInt(ctx, intervalSetting)
	switch {
	case err != nil:
		return defaultInterval
	case n < minIntervalSeconds:
		// Already refused by the settings validator; a value that got in
		// another way (a hand-edited database) must not become a busy loop.
		return minInterval
	}
	return time.Duration(n) * time.Second
}

// PullOnce is one cycle. It records what happened — in memory for the status
// endpoint and in the store for the next process — and returns the same
// error it recorded, so the caller can log it once.
//
// Not safe for concurrent callers: two cycles at once would both fetch and
// both import, and the second would overwrite the first's bookkeeping. Run
// is the only caller, and it is one goroutine.
func (r *Replica) PullOnce(ctx context.Context) error {
	// The one key that decides whether there is anything to do, before the
	// whole settings table is read for the rest of them: this runs every
	// five seconds on a box that follows nobody.
	peer, _, err := r.st.Settings().Get(ctx, peerURLSetting)
	if err != nil {
		// The store this would record the failure in is the store that just
		// failed. Nothing to write, nothing applied.
		return fmt.Errorf("reading the sync peer: %w", err)
	}
	if peer == "" {
		r.mu.Lock()
		r.last = api.SyncStatus{}
		r.mu.Unlock()
		return nil
	}
	all, err := r.st.Settings().All(ctx)
	if err != nil {
		return fmt.Errorf("reading the sync settings: %w", err)
	}

	s := api.SyncStatus{Role: "replica", PeerURL: peer}
	// The last version this peer was known to hold. A probe that fails
	// leaves it standing: "behind by N" is what the Sync band shows, and a
	// main that has gone down must not read as one this box is level with.
	//
	// Only while it is the same peer, though. A replica re-pointed at
	// another main knows nothing about the new one until it answers, and
	// carrying the old one's number over would report a version from a box
	// this one no longer follows, for as long as the new peer is
	// unreachable.
	r.mu.Lock()
	if r.last.PeerURL == peer {
		s.PeerVersion = r.last.PeerVersion
	}
	r.mu.Unlock()
	s.AppliedVersion, _ = strconv.ParseInt(all[appliedVersionSetting], 10, 64)
	s.AppliedAt, _ = strconv.ParseInt(all[appliedAtSetting], 10, 64)
	// A box that has never applied anything pulls whatever the main has,
	// even if the version happens to match the zero this one starts at.
	_, applied := all[appliedVersionSetting]

	err = r.pull(ctx, peer, all, applied, &s)
	s.LastPullAt = r.now().UnixMilli()
	if err != nil {
		s.LastError = err.Error()
	}
	r.mu.Lock()
	r.last = s
	r.mu.Unlock()

	if ctx.Err() != nil {
		// Shutting down. Recording "context canceled" as the last error
		// would leave the Sync band accusing the peer of something this
		// process did to itself, and it would still be there after the
		// restart that cleared it.
		return err
	}
	// Written on every outcome: clearing the last error is how the warning
	// strip goes away on its own once a pull succeeds.
	r.record(ctx, lastPullAtSetting, strconv.FormatInt(s.LastPullAt, 10))
	r.record(ctx, lastErrorSetting, s.LastError)
	return err
}

// record writes one bookkeeping key. A failure is logged and dropped: the
// pull itself has already succeeded or failed on its own terms, and losing
// the note about it is not a reason to report the pull differently.
func (r *Replica) record(ctx context.Context, key, value string) {
	if err := r.st.Settings().SetInternal(ctx, key, value); err != nil {
		slog.Warn("recording a config sync pull failed", "key", key, "err", err)
	}
}

// pull is one cycle's work, from the version probe to the registration. It
// fills s as it goes so a failure still reports how far it got.
func (r *Replica) pull(ctx context.Context, peer string, all map[string]string, applied bool, s *api.SyncStatus) error {
	token := all[tokenSetting]
	var probe syncVersion
	if err := r.get(ctx, probeTimeout, maxProbeBytes, peer+versionPath, token, &probe); err != nil {
		return err
	}
	s.PeerVersion = probe.ConfigVersion
	if probe.InstanceID != "" && probe.InstanceID == r.instanceID {
		// Pointed at itself: applying its own bundle would be a no-op the
		// operator would never see, and the registration would make this box
		// its own replica.
		return fmt.Errorf("%s is this instance: a replica cannot follow itself", peer)
	}
	if applied && probe.ConfigVersion == s.AppliedVersion {
		// Nothing to apply, but the registration still goes out: it is also
		// the heartbeat the main measures staleness by (§6), and config
		// changes far less often than three intervals, so a replica that
		// only registered on a change would be permanently stale — and
		// would lose the implicit transfer allow that entry carries.
		return r.register(ctx, peer, token, s.AppliedVersion)
	}

	var b store.Bundle
	if err := r.get(ctx, fetchTimeout, maxBundleBytes, peer+bundlePath, token, &b); err != nil {
		return err
	}
	if err := Validate(b); err != nil {
		return fmt.Errorf("bundle from %s: %w", peer, err)
	}
	if b.ConfigVersion != probe.ConfigVersion {
		// A write landed between the two requests. Nothing is applied half
		// a version late: the next cycle sees the newer one (§3). Said out
		// loud, because a cycle that fetched a whole bundle and applied
		// none of it is otherwise indistinguishable from one that found
		// nothing to do.
		slog.Info("config sync: the peer's version moved while the bundle was being fetched, retrying next cycle",
			"peer", peer, "probed", probe.ConfigVersion, "bundle", b.ConfigVersion)
		return nil
	}
	b.Zones = DeriveZones(b.Zones, primaryDNS(all[primaryDNSKey], peer), b.SyncKey)
	if err := r.st.ImportBundle(ctx, b); err != nil {
		return fmt.Errorf("applying the bundle from %s: %w", peer, err)
	}

	// From here the config *is* applied, so every failure below is reported
	// without unwinding the version: a pull that is not marked applied would
	// be retried on every tick, reimporting a bundle that is already in the
	// store.
	s.AppliedVersion, s.AppliedAt = b.ConfigVersion, r.now().UnixMilli()
	var errs []error
	if err := r.st.Settings().SetInternal(ctx, appliedVersionSetting, strconv.FormatInt(s.AppliedVersion, 10)); err != nil {
		errs = append(errs, err)
	}
	if err := r.st.Settings().SetInternal(ctx, appliedAtSetting, strconv.FormatInt(s.AppliedAt, 10)); err != nil {
		errs = append(errs, err)
	}
	// The order the API handlers run after their own writes (§5): clients,
	// then the ruleset that groups them, then zones, then the settings the
	// forwarder and the listeners are built from.
	//
	// Recompile, not refresh: a bundle cannot change what any list URL
	// serves, and a list this box has never downloaded is fetched by the
	// refresher's own ticker, exactly as a newly created one is.
	for _, step := range []struct {
		what string
		run  func(context.Context) error
	}{
		{"clients", r.reload.ReloadClients},
		{"filters", r.reload.RecompileFilters},
		{"zones", r.reload.ReloadZones},
		{"settings", r.reload.ReloadSettings},
	} {
		if err := step.run(ctx); err != nil {
			errs = append(errs, fmt.Errorf("reloading %s after the pull: %w", step.what, err))
		}
	}
	if err := r.register(ctx, peer, token, b.ConfigVersion); err != nil {
		// Not fatal, and it does not unwind the version: the config is
		// applied. What it costs this box is the implicit AXFR allow on the
		// main (§6) until a later cycle registers, which is what reporting
		// it — rather than swallowing it — is for.
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// register tells the main this box is here and what it applied. It is also
// what turns on the main's implicit transfer allow and notify target (§6),
// so it runs on every applied pull rather than once at startup.
func (r *Replica) register(ctx context.Context, peer, token string, version int64) error {
	body, err := json.Marshal(map[string]any{
		"instance_id": r.instanceID, "dns_addr": r.dnsAddr, "version_applied": version,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, peer+replicasPath, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	setBearer(req, token)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	drain(resp)
	if resp.StatusCode/100 != 2 {
		return statusErr(peer+replicasPath, resp)
	}
	return nil
}

// syncVersion is what GET /sync/version answers.
type syncVersion struct {
	ConfigVersion int64  `json:"config_version"`
	InstanceID    string `json:"instance_id"`
}

// get reads one JSON document from the peer under its own deadline.
func (r *Replica) get(ctx context.Context, timeout time.Duration, limit int64, url, token string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	setBearer(req, token)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		drain(resp)
		return statusErr(url, resp)
	}
	return decodeLimited(resp.Body, limit, url, out)
}

// decodeLimited reads one JSON document, and refuses a body over limit
// rather than decoding the prefix that fits. The reader is given one byte
// more than the cap, so "nothing left" is exactly "there was more".
func decodeLimited(body io.Reader, limit int64, url string, out any) error {
	lr := &io.LimitedReader{R: body, N: limit + 1}
	err := json.NewDecoder(lr).Decode(out)
	if lr.N <= 0 {
		// Checked before err, because the error a truncated document
		// produces describes the truncation rather than its cause.
		return fmt.Errorf("%s: the answer is larger than the %d byte cap", url, limit)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	return nil
}

// setBearer sends the pull credential, and sends no header at all when there
// is none: an empty Authorization is a header a proxy or a peer may judge
// differently from its absence, and "no token configured" is the honest
// state of a half-configured replica.
func setBearer(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// drain reads what is left of an error response so the connection can go
// back to the idle pool, bounded because a peer answering 500 with a
// gigabyte of HTML is not something to read in full.
func drain(resp *http.Response) { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxProbeBytes)) }

// statusErr turns a refusal into the sentence the Sync band shows.
func statusErr(url string, resp *http.Response) error {
	if loc := resp.Header.Get("Location"); loc != "" && resp.StatusCode/100 == 3 {
		// CheckRedirect stopped here deliberately, so the status alone would
		// say nothing an operator could act on.
		return fmt.Errorf("%s: %s, not followed, to %q", url, resp.Status, loc)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%s: peer refused the token (%s)", url, resp.Status)
	}
	return fmt.Errorf("%s: %s", url, resp.Status)
}

// primaryDNS is the address a derived secondary transfers from: the setting
// when it names one, and otherwise the peer URL's host on port 53 (§8) —
// the main's API and its DNS are on one box in every deployment this
// fallback is for.
func primaryDNS(setting, peer string) string {
	if setting != "" {
		return setting
	}
	u, err := url.Parse(peer)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return net.JoinHostPort(u.Hostname(), "53")
}
