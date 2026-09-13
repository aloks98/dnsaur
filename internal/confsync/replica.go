package confsync

import (
	"bytes"
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
	// appliedPeerSetting is the peer appliedVersionSetting was applied from.
	// Version numbers are each main's own count of its own writes, so two
	// mains sit at the same number all the time: without this, a replica
	// re-pointed at a main that happens to be level reads that coincidence
	// as "nothing to apply" and serves the old main's configuration for as
	// long as the new one stays there.
	appliedPeerSetting = "sync.applied_peer"
	appliedAtSetting   = "sync.applied_at"
	lastPullAtSetting  = "sync.last_pull_at"
	lastErrorSetting   = "sync.last_error"

	versionPath = "/api/v1/sync/version"
	bundlePath  = "/api/v1/sync/bundle"
	pairPath    = "/api/v1/sync/pair"

	// probeTimeout bounds the version probe and the pairing call, both a few
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
	// maxPeerMessage is how much of what a peer says about a refusal is
	// repeated back: enough for a sentence, and short of a peer writing the
	// settings screen a paragraph.
	maxPeerMessage = 200
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
	// RefreshFilters downloads every enabled list and then compiles. A pull
	// only calls it for the lists this box has never fetched — see
	// kickFirstDownload.
	RefreshFilters(ctx context.Context) error
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

	// dnsPort is the DNS port the peer last advertised in a probe, which is
	// what a derived secondary transfers from when sync.primary_dns names no
	// override (§8). Kept in memory rather than in a setting: it describes
	// the main, it arrives with every probe, and a box that has not probed
	// yet has nothing to derive from anyway.
	dnsPort atomic.Int64

	// pullMu is held for the length of a cycle, so two can never be in
	// flight together.
	pullMu sync.Mutex
	// kick is how something that cannot wait out an interval — a pairing
	// just stored — tells the poll loop to go now. Buffered by one and
	// never sent to blockingly: a wake that finds one already queued is the
	// same wake, and the cycle it starts reads whatever the store holds by
	// the time it runs.
	kick chan struct{}

	mu   sync.Mutex
	last api.SyncStatus
	// runCtx is non-nil while Run is in its loop, which is how Follow knows
	// whether there is a loop to wake at all. Held rather than passed
	// because the wake comes from an API request whose own context ends
	// with it.
	runCtx context.Context
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
		kick:       make(chan struct{}, 1),
	}
}

// ErrFollowRefused is a main that would not spend the code: mistyped,
// expired, already used, or guessed at five times (§9). The main says which
// of those it was only as far as it is willing to, so this is every one of
// them, with the peer's own words after the colon.
var ErrFollowRefused = errors.New("the main refused the pairing code")

// Follow pairs this box with the main at peerURL, spending code, and stores
// what comes back: sync.peer_url and the per-replica secret in one settings
// write, because a peer stored without its secret is a replica that pulls a
// 401 forever (§3).
//
// It wakes the poll loop rather than waiting a pull out, so the operator
// watches the configuration arrive instead of watching a request that holds
// open for as long as a bundle fetch takes — and returns nil once the
// pairing is stored, which is the part that cannot be retried: the code is
// spent on the main either way. A first pull that fails is reported where
// every other failed pull is, in sync.last_error and the Sync band, and the
// next cycle retries.
func (r *Replica) Follow(ctx context.Context, peerURL, code string) error {
	peer, err := ParsePeerURL(strings.TrimSpace(peerURL))
	if err != nil {
		return err
	}
	// Normalised here as well as on the main, so the case and the grouping
	// dash are the operator's to get wrong on either screen.
	body, err := json.Marshal(api.PairRequest{
		Code:       normalizePairingCode(strings.TrimSpace(code)),
		InstanceID: r.instanceID,
		DNSAddr:    r.dnsAddr,
	})
	if err != nil {
		return err
	}
	result, err := r.pair(ctx, peer, body)
	if err != nil {
		return err
	}
	if result.DNSPort > 0 {
		// The pairing carries the main's DNS port too, so the secondaries
		// the pull below derives are built on it rather than on 53 until
		// the first probe that answers one.
		r.dnsPort.Store(int64(result.DNSPort))
	}
	// One write: half of it is the broken state the settings validator
	// refuses to be put in by hand.
	if err := r.st.Settings().SetMany(ctx, map[string]string{
		peerURLSetting: peer, tokenSetting: result.Secret,
	}); err != nil {
		return fmt.Errorf("storing what %s paired: %w", peer, err)
	}
	// Handed to the poll loop rather than run beside it. A cycle already in
	// flight would make a pull started here step aside (PullOnce), and that
	// pull would then be lost until the next tick — which is
	// sync.interval_seconds away now that a peer is stored, not the five
	// seconds an idle box ticks at. The loop takes the wake as soon as its
	// cycle ends.
	//
	// Only a box whose Run has not started pulls here, on its own goroutine
	// and an uncancelled context: there is no loop to tell, and no shutdown
	// to respect either.
	if !r.wake() {
		go func() {
			if err := r.PullOnce(context.Background()); err != nil {
				slog.Warn("the first pull after pairing failed", "peer", peer, "err", err)
			}
		}()
	}
	return nil
}

// wake asks the poll loop to pull now instead of at its next tick, and
// reports whether there was a loop to ask. The send never blocks: one wake
// queued is every wake queued, since the cycle it starts reads the store as
// it is by then.
func (r *Replica) wake() bool {
	r.mu.Lock()
	running := r.runCtx != nil
	r.mu.Unlock()
	if !running {
		return false
	}
	select {
	case r.kick <- struct{}{}:
	default:
	}
	return true
}

// pair is Follow's one request, kept apart so the response body is closed
// before anything is written to the store.
func (r *Replica) pair(ctx context.Context, peer string, body []byte) (api.PairResult, error) {
	var result api.PairResult
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, peer+pairPath, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// No Authorization: the code is the proof (§3). The client's
	// CheckRedirect still applies, so a 3xx is reported rather than
	// followed — a redirect here would carry the code to whatever answered.
	resp, err := r.client.Do(req)
	if err != nil {
		return result, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		return result, fmt.Errorf("%w: %s", ErrFollowRefused, peerMessage(resp))
	case http.StatusTooManyRequests:
		// The main never looked at the code: its throttle is per source
		// address and measured in a minute, so what this costs is a wait,
		// not the code.
		drain(resp)
		return result, errors.New("the main is refusing attempts for now — wait a minute and try again")
	case http.StatusNotFound:
		// A peer with no pairing route is a main too old to pair with,
		// which the operator fixes by upgrading it — not by retyping the
		// code, and not by looking for a network fault.
		drain(resp)
		return result, errors.New("that box has no pairing endpoint")
	case http.StatusConflict:
		// The other box of the pair. A replica answers the unauthenticated
		// sync routes with "managed by <url>" (§7), so the main the
		// operator meant to point at is the one named back — and only then:
		// anything else that answers 409 is reported as the status it is,
		// rather than described as a replica of whatever it said.
		if mainURL, ok := strings.CutPrefix(peerMessage(resp), "managed by "); ok {
			return result, fmt.Errorf("that box is a replica of %s", mainURL)
		}
		return result, statusErr(peer+pairPath, resp)
	default:
		drain(resp)
		return result, statusErr(peer+pairPath, resp)
	}
	if err := decodeLimited(resp.Body, maxProbeBytes, peer+pairPath, &result); err != nil {
		return result, err
	}
	if result.Secret == "" {
		return result, fmt.Errorf("%s answered no secret", peer+pairPath)
	}
	return result, nil
}

// peerMessage is what the peer said about a refusal — the {"error": …} every
// handler in this API answers with — and its status when it said nothing
// this build can read. Clipped, because it ends up in an error the settings
// screen shows and a peer is not the judge of how long that line is.
func peerMessage(resp *http.Response) string {
	var e struct {
		Error string `json:"error"`
	}
	if err := decodeLimited(resp.Body, maxProbeBytes, "", &e); err != nil || e.Error == "" {
		return resp.Status
	}
	// Runes, so a clip never lands in the middle of one.
	if msg := []rune(e.Error); len(msg) > maxPeerMessage {
		return string(msg[:maxPeerMessage]) + "…"
	}
	return e.Error
}

// ParsePeerURL is the grammar for the main a box follows: an absolute http
// or https URL, scheme and host and nothing else, returned with the one
// trailing slash an operator pastes taken back off. The pull loop joins
// "/api/v1/sync/..." onto the result, so a path, a query or a fragment would
// either be dropped silently or build a URL nobody meant, and credentials in
// it would be a second secret in a field nothing treats as one.
func ParsePeerURL(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("must be an absolute http or https URL")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("must be a scheme and host only, with no path, query or credentials")
	}
	return u.Scheme + "://" + u.Host, nil
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
	r.mu.Lock()
	r.runCtx = ctx
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.runCtx = nil
		r.mu.Unlock()
	}()
	for {
		if err := r.PullOnce(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("config sync pull failed", "err", err)
		}
		t := time.NewTimer(r.pollInterval(ctx))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-r.kick:
			// Something that cannot wait out an interval. Queued rather
			// than run by whoever wanted it, so a wake that arrived while
			// this loop was mid-cycle is read here instead of being lost to
			// PullOnce's lock.
			t.Stop()
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
// Only one cycle runs at a time: two would both fetch and both import, the
// later bookkeeping winning. A caller that finds one already running waits
// for it and then runs its own — which the probe makes cheap, because a
// version the cycle just applied is "unchanged → done". Waiting rather than
// stepping aside is what lets a direct caller (a test, an operator action)
// rely on the pull having happened when PullOnce returns. What a pairing
// needs pulled is handed to the poll loop through wake, which the loop reads
// as soon as the cycle in flight ends.
func (r *Replica) PullOnce(ctx context.Context) error {
	r.pullMu.Lock()
	defer r.pullMu.Unlock()
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
		// Clearing sync.peer_url is the promotion (§7): this box owns its
		// configuration from here, and what it last took from a main is a
		// number it no longer follows. Only when there is one to clear —
		// this runs every idle cycle, on every main.
		if v, _, err := r.st.Settings().Get(ctx, appliedVersionSetting); err == nil && v != "" {
			r.record(ctx, appliedVersionSetting, "")
			r.record(ctx, appliedPeerSetting, "")
		}
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
	// even if the version happens to match the zero this one starts at —
	// and so does one whose applied version came from a different peer,
	// whose numbering has nothing to do with this one's. A box upgraded
	// from a build that recorded no peer reads as never-applied too, which
	// costs it one bundle it already had.
	applied := all[appliedVersionSetting] != "" && all[appliedPeerSetting] == peer

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

// pull is one cycle's work, from the version probe to the reloads. It fills
// s as it goes so a failure still reports how far it got.
func (r *Replica) pull(ctx context.Context, peer string, all map[string]string, applied bool, s *api.SyncStatus) error {
	token := all[tokenSetting]
	// The probe is the heartbeat (§3): the version it carries is what the
	// main stamps this box's registry entry with, so it reports a version
	// only while that version is this peer's own count — a number applied
	// from another main describes nothing the peer can read.
	reported := int64(0)
	if applied {
		reported = s.AppliedVersion
	}
	probeURL := peer + versionPath + "?applied=" + strconv.FormatInt(reported, 10)
	var probe syncVersion
	if err := r.get(ctx, probeTimeout, maxProbeBytes, probeURL, token, &probe); err != nil {
		return err
	}
	s.PeerVersion = probe.ConfigVersion
	if probe.DNSPort > 0 {
		// Kept rather than used and dropped, so a probe that answers without
		// one — an older main — does not move every derived secondary to
		// port 53 for a cycle.
		r.dnsPort.Store(int64(probe.DNSPort))
	}
	if probe.InstanceID != "" && probe.InstanceID == r.instanceID {
		// Pointed at itself: applying its own bundle would be a no-op the
		// operator would never see, and this box would go on refusing its
		// own writes as configuration managed by itself (§7).
		return fmt.Errorf("%s reports this box's own instance.id: a replica cannot follow itself "+
			"(a database restored from the main shares its instance.id — a replica has to be a fresh install)", peer)
	}
	if applied && probe.ConfigVersion == s.AppliedVersion {
		// Nothing to apply, and nothing else to say: the probe above was
		// the heartbeat, and the main has already stamped this box from it.
		return nil
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
	b.Zones = DeriveZones(b.Zones, primaryDNS(all[primaryDNSKey], peer, int(r.dnsPort.Load())), b.SyncKey)
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
	if err := r.st.Settings().SetInternal(ctx, appliedPeerSetting, peer); err != nil {
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
	r.kickFirstDownload(ctx)
	return errors.Join(errs...)
}

// kickFirstDownload starts a background refresh when the bundle brought a
// list this box has never fetched.
//
// A bundle carries a list's URL, not its contents, and Recompile builds the
// ruleset from the copies already on disk — so a replica that has just
// started following a main has every list it was given and no copy of any of
// them. Left to its own refresher, it serves a LAN with blocking configured
// and nothing blocked until lists.refresh_hours comes round, which is a day
// by default. This is the same background download the API runs when a list
// is created, for the same reason.
//
// Not awaited and not fatal: the configuration is applied either way, and a
// list server that is slow or down must not hold up the cycle or make it
// report a failure. It runs on the pull's own context, so a shutdown stops
// it rather than leaving a download writing into a closing store.
func (r *Replica) kickFirstDownload(ctx context.Context) {
	lists, err := r.st.Filters().Lists(ctx)
	if err != nil {
		slog.Warn("config sync: checking which lists still need downloading failed", "err", err)
		return
	}
	for _, l := range lists {
		// LastRefreshed is 0 until a download of this box's own succeeds,
		// which is exactly the state an imported row starts in.
		if l.Enabled && l.LastRefreshed == 0 {
			go func() {
				if err := r.reload.RefreshFilters(ctx); err != nil {
					slog.Error("config sync: downloading the lists the bundle brought failed", "err", err)
				}
			}()
			return
		}
	}
}

// syncVersion is what GET /sync/version answers: the counter that says
// whether anything changed, the id of the box that answered — so a replica
// pointed at itself can see that it is — and the port that box serves DNS on.
type syncVersion struct {
	ConfigVersion int64  `json:"config_version"`
	InstanceID    string `json:"instance_id"`
	DNSPort       int    `json:"dns_port"`
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

// primaryDNS is the address a derived secondary transfers from: the
// override when it names one, and otherwise the peer URL's host on the port
// the main advertised in its probe (§8) — the main's API and its DNS are on
// one box in every deployment this fallback is for. Port 53 when the main
// advertised none, which is what it serves unless it says otherwise.
func primaryDNS(override, peer string, port int) string {
	if override != "" {
		return override
	}
	u, err := url.Parse(peer)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	if port == 0 {
		port = 53
	}
	return net.JoinHostPort(u.Hostname(), strconv.Itoa(port))
}
