package app

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloks98/dnsaur/internal/api"
	"github.com/aloks98/dnsaur/internal/auth"
	"github.com/aloks98/dnsaur/internal/cache"
	"github.com/aloks98/dnsaur/internal/clients"
	"github.com/aloks98/dnsaur/internal/config"
	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/filter"
	"github.com/aloks98/dnsaur/internal/qlog"
	"github.com/aloks98/dnsaur/internal/stats"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/upstream"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/aloks98/dnsaur/web"
)

func defaultSettings() map[string]string {
	return map[string]string{
		"instance.id":           rand.Text(),
		"upstreams":             "1.1.1.1:53,1.0.0.1:53,9.9.9.9:53",
		"upstream.strategy":     "race",
		"blocking.mode":         "null-ip",
		"blocking.ttl":          "30",
		"cache.min_ttl":         "0",
		"cache.max_ttl":         "86400",
		"cache.max_entries":     "10000",
		"cache.serve_stale_for": "86400",
		"lists.refresh_hours":   "24",
		"qlog.retention_days":   "90",
		"qlog.privacy":          "full",
		"stats.retention_days":  "365",
		"serve.dot.enabled":     "false",
		"serve.dot.listen":      ":853",
		"serve.doh.enabled":     "false",
		"serve.doh.listen":      ":443",
		"serve.tls.cert":        "",
		"serve.tls.key":         "",
	}
}

// swappable lets us rebuild the tail of the pipeline (forwarder) on
// settings changes without restarting listeners.
//
// It holds the *upstream.Forwarder beside the Handler it produced, under the
// same lock, because the two have to be swapped and read as one thing. The
// pipeline needs the Handler; installing a zone reload's conditional routing
// table needs the Forwarder (SetConditional is not on Handler); and a reload
// that pushed its table onto a Forwarder that is no longer the one serving
// queries would have installed nothing at all.
type swappable struct {
	mu  sync.RWMutex
	h   dnssrv.Handler
	f   *upstream.Forwarder
	has bool // true once set has been called at least once
}

// set installs f and returns the Forwarder it displaced, or nil if this is
// the first. The caller closes it — outside the lock, because Close walks
// every upstream and the documented order is routeMu -> swappable.mu (see
// App.routeMu): doing it here would put a slow, transport-touching call
// inside the lock every query path reads through.
func (s *swappable) set(f *upstream.Forwarder) *upstream.Forwarder {
	s.mu.Lock()
	old := s.f
	s.f = f
	s.h = f.Handler()
	s.has = true
	s.mu.Unlock()
	return old
}

// forwarder returns the Forwarder currently serving queries, or nil before
// the first set. It is how App.ReloadZones reaches SetConditional: a.fwd is
// a dnssrv.Handler, which has no such method.
func (s *swappable) forwarder() *upstream.Forwarder {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.f
}

// isSet reports whether a handler has ever been installed. Used by
// applySettings to decide whether it's safe to keep the previous forwarder
// on a bad settings reload, or whether it must fall back to something usable
// because there is no previous forwarder to keep.
func (s *swappable) isSet() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.has
}

func (s *swappable) ServeDNS(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
	s.mu.RLock()
	h := s.h
	s.mu.RUnlock()
	return h.ServeDNS(ctx, req)
}

type App struct {
	cfg      *config.Config
	version  string
	st       store.Store
	registry *clients.Registry
	engine   *filter.Engine
	resolver *zones.Resolver
	// refresher keeps blocklists current; zoneRefresh keeps secondary zones
	// current. Different schedules, different stores, same shape.
	refresher   *filter.Refresher
	zoneRefresh *zones.Refresher
	// xfrOut answers AXFR and IXFR from a.st.Zones() — the D3 counterpart of
	// zoneRefresh's D2 client. Constructed once here and attached to every
	// listener in Start via dnssrv.WithTransfers, the same way the key store
	// is: live, not a snapshot, so a zone's allow_transfer edited through the
	// API governs the next transfer rather than the next restart.
	xfrOut *zones.TransferServer
	// notifier is the outbound half of NOTIFY (D4): who to tell when a zone
	// this server owns has changed. notifyIn is the inbound half: the gate
	// that decides whether an arriving NOTIFY is acted on, sharing
	// zoneRefresh so a notify-triggered transfer takes the same per-zone
	// lock a scheduled one does. Both attached to every listener in Start.
	notifier *zones.Notifier
	notifyIn *zones.NotifyServer
	logger   *qlog.Logger
	fwd      *swappable
	// dnsCache is the pipeline's cache, held here rather than left local to
	// Start because a routing change has to be able to invalidate it: an
	// entry is keyed on (qname, qtype) with no record of which route
	// produced it, so a name cached from the default upstreams before a zone
	// claimed its suffix would go on being served from that entry
	// afterwards. Written once in Start, before anything that reads it
	// exists, and never again.
	dnsCache *cache.Cache
	// routes is the conditional table last installed, kept so the next
	// install can tell which suffixes actually changed rather than purging
	// the whole cache on every record edit. Read and replaced only in
	// installConditional, whose three callers all hold routeMu — and reading
	// it is in-memory work, so routeMu still never touches the store.
	routes map[string][]string
	// routeMu serialises the two writers of the conditional routing table
	// against each other: ReloadZones, which reads which forwarder is live
	// and installs on it, and applySettings, which builds a replacement
	// forwarder, installs on it and makes it live. Each is a
	// read-modify-write spanning both objects, so serialising inside
	// SetConditional is not enough — the interleaving that loses a route
	// happens above it, here.
	//
	// Held only by those two. ServeDNS never takes it, so the query path is
	// not on this lock, and the order is always routeMu -> swappable.mu and
	// is never inverted. The cost is one mutex per zone reload — contended,
	// by construction, in exactly the interleaving it exists to order, and
	// uncontended the rest of the time.
	routeMu sync.Mutex
	// downgrade records that applySettings fell all the way to the hardcoded
	// plaintext defaults while the stored `upstreams` asked for an encrypted
	// transport — see the third rung of the ladder in applySettings and
	// UpstreamDowngrade. Server state, not a setting: it is not in the
	// settings map and GET /settings does not carry it (that response is a
	// flat key -> value map of settings, and handleSettingsGet strips even
	// the internal ones). It lives here because applySettings is the only
	// thing that can know it, and it is read through the API by the settings
	// screen, which is the only place that can act on it.
	//
	// atomic.Pointer, and nil for "no downgrade", so the query path and the
	// API handler never contend with a settings write.
	downgrade atomic.Pointer[downgradeState]
	// handler is the same pipeline every listener serves through — plain
	// :53, and (Task 8) DoT and DoH. Built once in Start, before the first
	// applySettings call, so a restart with either encrypted protocol
	// already enabled has a real handler to hand its listener rather than
	// a nil interface.
	handler dnssrv.Handler
	// serving is the encrypted-listener reconciler's own state: which of
	// DoT and DoH are actually running, and what applySettings last found
	// out trying to converge them to settings. See serve.go.
	serving   servingReconciler
	servers   []*dnssrv.Server
	apiSrv    *http.Server
	apiAddr   string
	apiCancel context.CancelFunc
	bg        []func(context.Context)
	cancel    context.CancelFunc
	ready     chan struct{}
	wg        sync.WaitGroup
	// badSettings is the last failure each settings key was warned about,
	// keyed by key — see warnUnusableSetting. sync.Map rather than a guarded
	// map because it is written from getInt, which applySettings and the
	// pruner's retention closure both reach from different goroutines, and
	// its zero value is already usable.
	badSettings sync.Map
}

func New(ctx context.Context, cfg *config.Config, version string) (*App, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	st, err := store.Open(ctx, cfg.Storage.Driver, cfg.Storage.DSN)
	if err != nil {
		return nil, err
	}
	if err := st.Settings().SeedDefaults(ctx, defaultSettings()); err != nil {
		return nil, err
	}
	if gs, err := st.Clients().Groups(ctx); err != nil {
		return nil, err
	} else if len(gs) == 0 {
		if _, err := st.Clients().AddGroup(ctx, "default"); err != nil {
			return nil, err
		}
	}
	a := &App{
		cfg:      cfg,
		version:  version,
		st:       st,
		registry: clients.NewRegistry(st.Clients()),
		engine:   filter.NewEngine(),
		resolver: zones.NewResolver(st.Zones()),
		fwd:      &swappable{},
		ready:    make(chan struct{}),
	}
	a.refresher = filter.NewRefresher(st.Filters(), st.Clients(), a.engine, cfg.DataDir)
	// The resolver every hostname in a zone's configuration is looked up
	// through: a secondary's primaries, a NOTIFY target, a stub's
	// out-of-zone nameserver. Four constructors default it independently,
	// so it is decided here once instead of in each of them.
	//
	// It is this server's own cache and forwarder, entered below the zones
	// stage — never the host machine's resolver, which on a machine running
	// dnsaur is usually dnsaur itself. See zoneLookup for what that buys and
	// what it deliberately cannot reach.
	zoneRes := a.zoneLookup()
	// Built before the Transferrer, which takes its Wake for the cascade.
	a.notifier = zones.NewNotifier(st.Zones(), st.Notifies(), st.TSIGKeys(),
		zones.WithNotifyResolver(zoneRes))
	// The transfer republishes the served snapshot itself: a zone installed
	// into the store that nothing reloaded is answering from the copy it just
	// replaced. The key store is passed live, not a snapshot, for the same
	// reason the DNS servers get it that way — a key edited through the API
	// signs the next transfer, not the next restart.
	a.zoneRefresh = zones.NewRefresher(st.Zones(),
		zones.NewTransferrer(st.Zones(), st.TSIGKeys(),
			zones.WithTransferResolver(zoneRes),
			// **ReloadZones, not resolver.Reload.** The conditional routing
			// table is derived from the served snapshot, so republishing the
			// snapshot alone publishes half a reload: a forwarder or stub zone
			// that claimed a suffix since the last full reload is in what the
			// resolver serves and absent from what the forwarder routes with,
			// and its names fall through to the default upstreams — §9.11.5's
			// failure, reached through the one path that used to skip the
			// table. Signature-compatible, and installConditional is nil-safe
			// for the window before any forwarder exists.
			zones.WithReload(a.ReloadZones),
			// The cascade: a secondary that just installed a zone may have
			// downstream secondaries of its own. Nothing cascade-specific
			// happens here — the pass compares serials, and the install has
			// just written the primary's serial verbatim.
			zones.WithNotifyWake(a.notifier.Wake)),
		// The other worker the same schedule drives: a stub pulls its
		// delegation with two ordinary queries instead of an AXFR, so it is a
		// separate object from the Transferrer — and therefore does *not*
		// inherit the reload above. It needs its own, and it needs the same
		// one.
		//
		// **ReloadZones, not resolver.Reload**, and for a stub the
		// consequence is not a corner case but the feature: its upstreams are
		// derived from the NS records the fetch has just installed, so
		// republishing the snapshot without reinstalling the routing table
		// leaves the zone claiming its suffix with no addresses behind it.
		// Every query for it SERVFAILs, forever, looking exactly like a fetch
		// that never happened — while the rows sit in the database and the
		// zone page shows the delegation.
		zones.WithStubFetcher(zones.NewStubFetcher(st.Zones(), st.TSIGKeys(),
			zones.WithStubResolver(zoneRes),
			zones.WithStubReload(a.ReloadZones))))
	// The other direction: what a.zoneRefresh's Transferrer pulls from
	// someone else's TransferServer, this one serves to a peer pulling from
	// us. It reads a.resolver's live snapshot, so a zone this server
	// authors is transferred as of its last reload, the same data an
	// ordinary query would get.
	a.xfrOut = zones.NewTransferServer(a.resolver, st.Zones())
	// The inbound half. It shares the Refresher the scheduler drives, so a
	// notify-triggered transfer takes the same per-zone lock a scheduled one
	// does rather than racing it.
	//
	// **WithNotifyProbes is not optional here even though the option is.**
	// Without it a NOTIFY skips the SOA probe and transfers on every
	// admitted message — the pre-Task-7 behaviour, silently, with a log
	// line as the only signal. A primary editing ten records would cause
	// ten full zone transfers.
	a.notifyIn = zones.NewNotifyServer(a.resolver, st.Zones(), a.zoneRefresh,
		zones.WithNotifyServerResolver(zoneRes),
		zones.WithNotifyProbes(a.zoneRefresh.Transferrer()))
	return a, nil
}

func (a *App) Store() store.Store { return a.st }

// pruneExpiredTokens deletes expired auth tokens. Errors are logged and
// swallowed: cleanup is best-effort housekeeping, not on the request path.
func (a *App) pruneExpiredTokens(ctx context.Context) {
	if err := a.st.Tokens().DeleteExpired(ctx, time.Now().UnixMilli()); err != nil {
		slog.Warn("token cleanup failed", "err", err)
	}
}

// runTokenCleanup prunes expired auth tokens once at startup and then daily,
// mirroring qlog.Pruner's cadence for the query-log retention job.
func (a *App) runTokenCleanup(ctx context.Context) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	a.pruneExpiredTokens(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.pruneExpiredTokens(ctx)
		}
	}
}

func (a *App) getSetting(ctx context.Context, key string) string {
	v, _, _ := a.st.Settings().Get(ctx, key)
	return v
}

// settingValue is getSetting for the keys where "the store could not answer"
// and "the key is unset" have to stay apart. Settings().Get already
// distinguishes them — a missing key is ("", false, nil), not an error — and
// this carries that distinction out to the caller instead of flattening both
// into "".
//
// The flattening is not a cosmetic loss: a value of "" is how blocking.mode
// means null-ip and qlog.privacy means full, so a database blip read as ""
// silently undoes a configured nxdomain, and starts writing whole client IPs
// into the query log for an install configured to anonymise them.
// readServingSettings (serve.go) refuses the same collapse for the serving
// keys; this is the same rule for the two keys applySettings applies itself.
func (a *App) settingValue(ctx context.Context, key string) (string, error) {
	v, _, err := a.st.Settings().Get(ctx, key)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", key, err)
	}
	return v, nil
}

func (a *App) getInt(ctx context.Context, key string, fallback int64) int64 {
	v, err := a.st.Settings().GetInt(ctx, key)
	if err != nil {
		a.warnUnusableSetting(key, fallback, err)
		return fallback
	}
	a.badSettings.Delete(key)
	return v
}

// warnUnusableSetting reports an integer setting that could not be used, once
// per distinct failure per key.
//
// The rate limit is what makes the log worth reading: applySettings runs on
// every settings write, and a row holding "ninety" would otherwise repeat its
// warning on each one — which is a large part of why this was silent to begin
// with. Remembering the last failure per key keeps the first occurrence and
// any change to it, and drops the repeats; a key that reads successfully
// forgets, so a value that breaks again is reported again.
func (a *App) warnUnusableSetting(key string, fallback int64, err error) {
	if prev, had := a.badSettings.Swap(key, err.Error()); had && prev == err.Error() {
		return
	}
	slog.Warn("unusable setting, falling back to the default", "key", key, "default", fallback, "err", err)
}

// buildForwarder wraps upstream.New with the same defaulted timeout/strategy
// handling applySettings needs in more than one place.
func buildForwarder(upstreams []string, strategy string) (*upstream.Forwarder, error) {
	return upstream.New(upstream.Config{Upstreams: upstreams, Strategy: strategy})
}

// conditionalRoutes maps each enabled forwarder and stub zone's apex to the
// addresses its queries go to.
//
// A zone that names no usable upstreams is still included, with an empty
// list. That is deliberate: pick returns the empty slice, every attempt
// fails, and the handler answers SERVFAIL — so the zone keeps its claim on
// the suffix instead of falling through to the default resolvers and letting
// a public answer shadow an internal name (§9.11.5). Omitting it would be the
// fall-through this design refuses.
//
// A disabled zone is skipped entirely, which releases its suffix back to the
// defaults — the same meaning "disabled" has on every other path.
//
// It walks the resolver's snapshot rather than the store, for two reasons.
// The snapshot is what the server is actually serving, so the routing table
// cannot describe a zone the resolver has not loaded yet. And a stub's
// upstreams are derived from its NS records and their glue, which live on
// zones.Zone — store.Zone carries the row only, so reading the store would
// make StubUpstreams impossible to call without a second query per zone.
func (a *App) conditionalRoutes() map[string][]string {
	routes := map[string][]string{}
	for _, z := range a.resolver.Snapshot().Zones() {
		if !z.Enabled {
			continue
		}
		switch strings.ToLower(z.Type) {
		case "forwarder":
			targets, err := zones.ParseForwardTo(z.ForwardTo)
			if err != nil {
				// Fails closed: an unparseable stored value names no
				// upstreams, so the zone SERVFAILs rather than forwarding
				// somewhere unintended. The API validates on write, so
				// reaching this means a hand-edited row.
				slog.Warn("zone forward_to will not parse; the zone will answer SERVFAIL",
					"zone", z.Name, "err", err)
				routes[z.Name] = nil
				continue
			}
			addrs := make([]string, 0, len(targets))
			for _, t := range targets {
				addrs = append(addrs, t.Addr())
			}
			routes[z.Name] = addrs
		case "stub":
			// Derived from the fetched NS set; empty until the first fetch
			// lands, which claims the suffix and SERVFAILs meanwhile.
			routes[z.Name] = zones.StubUpstreams(z)
		}
	}
	return routes
}

// installConditional pushes the served snapshot's routing table onto f, then
// drops the cached answers for every suffix whose routing that just changed.
//
// f is passed rather than read from a.fwd so applySettings can install onto a
// forwarder that has not gone live yet — see the ordering note there.
//
// **The purge is not an optimisation and the order is not arbitrary.** The
// pipeline is resolver → cache → forwarder, and only the forwarder holds the
// routing table: a cache entry is keyed on (qname, qtype) with no record of
// which route produced it, so a hit is served without pick ever being
// reached. Without the purge, a name cached from the *default* upstreams
// before a zone claimed its suffix goes on being answered from that entry —
// and that is not a corner, it is the feature's own motivating workflow, since
// a split-horizon forwarder zone is added precisely because the name resolves
// publicly today. Worse, when the claimed suffix's upstreams are all down the
// cache serves that public entry *stale* with rcode NOERROR, for
// serve_stale_for (a day, by default) past its own TTL, where §9.11.5 requires
// SERVFAIL. The mirror case is the same defect the other way: releasing a
// suffix leaves internal answers cached for names that should now resolve
// publicly.
//
// Install first, purge second. Purging first would leave a window in which
// the suffix is unclaimed and a miss re-fills the cache from the defaults,
// which is the direction that leaks.
func (a *App) installConditional(f *upstream.Forwarder) {
	if f == nil {
		return // no forwarder has ever been installed; applySettings is about to
	}
	routes := a.conditionalRoutes()
	if err := f.SetConditional(routes); err != nil {
		// Nothing was installed, so nothing changed and a.routes is left
		// alone: the next install still sees this table as new and purges it.
		slog.Error("installing conditional routes failed", "err", err)
		return
	}
	changed := changedSuffixes(a.routes, routes)
	a.routes = routes
	if len(changed) == 0 || a.dnsCache == nil {
		return
	}
	if n := a.dnsCache.Purge(changed...); n > 0 {
		slog.Debug("cache purged where routing changed", "suffixes", len(changed), "entries", n)
	}
}

// changedSuffixes lists every suffix that is in exactly one of prev and next,
// or in both against a different set of upstreams. Those are the suffixes
// whose cached answers may have been produced by a route that no longer
// applies; the rest of the table is untouched, so an ordinary record edit —
// which reloads zones and reinstalls this table — purges nothing at all.
//
// The upstreams are compared as a set rather than a list. Order decides which
// address is *tried* first, not which of them may answer, so a reordering
// invalidates nothing.
func changedSuffixes(prev, next map[string][]string) []string {
	var out []string
	for suffix, ups := range next {
		if was, ok := prev[suffix]; !ok || !sameUpstreams(was, ups) {
			out = append(out, suffix)
		}
	}
	for suffix := range prev {
		if _, ok := next[suffix]; !ok {
			out = append(out, suffix)
		}
	}
	return out
}

func sameUpstreams(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// applySettings re-reads DB settings into live components.
// downgradeState is why the running forwarder is not what the operator
// configured. Immutable once stored.
type downgradeState struct {
	reason string
}

// UpstreamDowngrade reports whether queries are travelling in the clear
// against the hardcoded default resolvers because the stored `upstreams`
// value — which asked for tls:// or https:// — could not be parsed, and why.
//
// It satisfies api.ResolverStatus. False and "" is the normal case, and is
// also what a server that has never run applySettings reports.
func (a *App) UpstreamDowngrade() (bool, string) {
	if d := a.downgrade.Load(); d != nil {
		return true, d.reason
	}
	return false, ""
}

// setDowngrade records reason, or clears the record when reason is empty.
//
// **Clearing is as load-bearing as setting.** A warning that survives the
// operator fixing the setting is worse than no warning: it teaches them to
// ignore the one banner that means their DNS is unencrypted. Every path that
// installs a forwarder built from the stored value calls this with "".
func (a *App) setDowngrade(reason string) {
	if reason == "" {
		a.downgrade.Store(nil)
		return
	}
	a.downgrade.Store(&downgradeState{reason: reason})
}

// namesEncryptedScheme reports whether raw — the stored `upstreams` string,
// unparsed, because the point is that it would not parse — asks for an
// encrypted transport anywhere in it.
//
// Deliberately a scan of the raw text rather than anything cleverer: the
// value has already been refused by the real grammar, so this is reading an
// intent out of something known to be broken, and the only honest way to do
// that is to look for the schemes by name.
func namesEncryptedScheme(raw string) bool {
	for entry := range strings.SplitSeq(raw, ",") {
		e := strings.ToLower(strings.TrimSpace(entry))
		if strings.HasPrefix(e, string(upstream.SchemeDoT)+"://") ||
			strings.HasPrefix(e, string(upstream.SchemeDoH)+"://") {
			return true
		}
	}
	return false
}

func (a *App) applySettings(ctx context.Context) {
	// Reconciling DoT/DoH is independent of whether the forwarder rebuild
	// below succeeds — a broken `upstreams` value and a DoT/DoH settings
	// change can land in the same write, or simply be stored at the same
	// time by coincidence — so it must run on every exit from this
	// function, including the early return a few lines down that keeps the
	// previous forwarder rather than only on the common path. Deferred
	// rather than called explicitly at every return: everything above it
	// in the function body has already run by the time a deferred call
	// fires, which is what gives "after the forwarder swap" (Task 8's
	// brief) on the path that reaches it, while still covering the paths
	// that return before it.
	defer a.reconcileServing(ctx)

	if mode, err := a.settingValue(ctx, "blocking.mode"); err != nil {
		slog.Warn("keeping the blocking mode already in force", "err", err)
	} else {
		a.engine.SetBlocking(mode, uint32(a.getInt(ctx, "blocking.ttl", 30)))
	}
	if err := a.registry.Reload(ctx); err != nil {
		slog.Error("client reload failed", "err", err)
	}
	if err := a.resolver.Reload(ctx); err != nil {
		slog.Error("zones reload failed", "err", err)
	}
	if a.logger != nil {
		if privacy, err := a.settingValue(ctx, "qlog.privacy"); err != nil {
			slog.Warn("keeping the query-log privacy mode already in force", "err", err)
		} else {
			a.logger.SetPrivacy(privacy)
		}
	}

	rawUpstreams := a.getSetting(ctx, "upstreams")
	upstreams := strings.Split(rawUpstreams, ",")
	strategy := a.getSetting(ctx, "upstream.strategy")
	// Empty unless the ladder below reaches its last rung with an encrypted
	// value stored; see setDowngrade, and note that it is written on every
	// successful path too, because clearing matters as much as setting.
	var downgradeReason string
	fwd, err := buildForwarder(upstreams, strategy)
	if err != nil {
		if a.fwd.isSet() {
			// A working forwarder already exists (e.g. from a previous
			// successful applySettings) — keep serving queries with it
			// rather than replacing it with something broken. The reload
			// above may still have moved a zone, so the table goes on
			// anyway: the forwarder is kept, its routing is not frozen.
			slog.Error("keeping previous upstream config", "err", err)
			a.routeMu.Lock()
			a.installConditional(a.fwd.forwarder())
			a.routeMu.Unlock()
			return
		}
		// No forwarder has ever been installed: swappable.h would stay nil
		// and every query would panic (recovered by dnssrv.Recover, but
		// resolution would be permanently dead). Fall back rather than
		// leave the pipeline unusable. First retry with the same upstream
		// addresses but the known-good default strategy, in case the
		// problem was just a bad strategy string.
		slog.Error("invalid upstream settings, retrying with default strategy", "err", err, "strategy", strategy)
		fwd, err = buildForwarder(upstreams, "race")
		if err != nil {
			// Upstream addresses themselves are unusable too: fall all the
			// way back to the hardcoded defaults so the server can resolve
			// something instead of staying dead.
			slog.Error("invalid upstream settings, using defaults", "err", err)
			// **The defaults are plaintext.** If the stored value asked for
			// tls:// or https:// then resolving through 1.1.1.1:53 is not a
			// smaller version of what the operator wanted, it is the opposite
			// of it: every query goes out in the clear, and before this
			// milestone this rung could not be reached for upstream reasons
			// at all, so nothing above the exchanger ever had to think about
			// it. §5's "never downgrade" holds inside the forwarder, and this
			// is one level above it — exactly where §5 does not look.
			//
			// The owner's call is to keep resolving rather than fail closed —
			// a homelab whose DNS stops entirely is a worse Monday than one
			// that resolves unencrypted — but not silently: the fact is
			// recorded here and the settings screen shows it until the
			// setting is fixed. An ERROR log alone is not a signal; nobody
			// reads a resolver's log on a good day.
			if namesEncryptedScheme(rawUpstreams) {
				downgradeReason = err.Error()
			}
			fwd, err = buildForwarder(strings.Split(defaultSettings()["upstreams"], ","), "race")
			if err != nil {
				// The hardcoded defaults are static and known-good; this
				// should be unreachable, but don't panic — leave the
				// forwarder unset and let Recover() keep serving SERVFAIL.
				slog.Error("default upstream config failed to build", "err", err)
				return
			}
		}
	}
	// **Before it goes live, not after.** fwd is brand new and its
	// conditional table is empty; a settings edit that installed the routes
	// after the swap would leave a window in which every forwarder and stub
	// zone's suffix resolves through the default upstreams — a split-horizon
	// name answered by the public internet, which is the failure §9.11.5
	// exists to prevent. Get it wrong and nothing fails until an unrelated
	// zone edit happens to reinstall the table.
	a.routeMu.Lock()
	a.installConditional(fwd)
	old := a.fwd.set(fwd)
	a.routeMu.Unlock()
	// Set beside the forwarder it describes, and on every path that reaches
	// here, so a later apply that succeeds clears what an earlier one
	// recorded.
	a.setDowngrade(downgradeReason)
	if old != nil && old != fwd {
		// After the swap and outside routeMu: the new forwarder is already
		// serving, so nothing waits on this.
		//
		// **In-flight queries are not drained, and this is safe anyway.**
		// swappable.set replaces the handler; a query that read s.h before
		// the swap can still be mid-Exchange on the old forwarder. What
		// makes closing it correct is that both encrypted Close
		// implementations touch *idle* connections only — dotExchanger.Close
		// empties the pool (dot.go), dohExchanger.Close calls
		// CloseIdleConnections (doh.go) — and a connection carrying an
		// in-flight query is checked out of the pool, so neither reaches it.
		// It is closed when that query finishes, by put()'s closed check.
		//
		// The reason matters: written as "nothing can still be using these",
		// this comment is exactly the premise under which someone later
		// "improves" Close into something that tears down live connections.
		_ = old.Close()
	}
}

func (a *App) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel

	// Before applySettings, not after. applySettings installs the conditional
	// routing table, and installing it is also what purges the cache of the
	// suffixes that table just claimed or released (installConditional). Built
	// afterwards, the first install would have nothing to purge and the field
	// would be written while the settings watcher below could already be
	// reading it.
	a.dnsCache = cache.New(cache.Options{
		MinTTL:        time.Duration(a.getInt(ctx, "cache.min_ttl", 0)) * time.Second,
		MaxTTL:        time.Duration(a.getInt(ctx, "cache.max_ttl", 86400)) * time.Second,
		ServeStaleFor: time.Duration(a.getInt(ctx, "cache.serve_stale_for", 86400)) * time.Second,
		MaxEntries:    int(a.getInt(ctx, "cache.max_entries", 10000)),
	})

	// Also before applySettings, and for the same reason the cache is: the
	// first applySettings call below reconciles DoT and DoH against
	// whatever the store already has — a restart with either enabled from
	// a previous run — and that reconcile needs a real pipeline handler to
	// give its listener, not the nil interface a field declared but never
	// assigned would hand it.
	instanceID := a.getSetting(ctx, "instance.id")
	a.logger = qlog.New(a.st.QueryLog(), qlog.Options{
		Privacy: a.getSetting(ctx, "qlog.privacy"), InstanceID: instanceID,
	})
	a.handler = dnssrv.Chain(a.fwd,
		a.logger.Middleware(),
		dnssrv.Recover(),
		a.registry.Middleware(),
		a.engine.Middleware(),
		a.resolver.Middleware(),
		a.dnsCache.Middleware(),
	)

	a.applySettings(ctx)
	// applySettings has, by this line, possibly started DoT and/or DoH from
	// settings a previous run stored. Every failure below therefore has to
	// take them back down: Start's contract is all-or-nothing, and
	// cmd/dnsaur/main.go does not call Shutdown on a Start error. Harmless
	// in production (the process exits and the OS reclaims the socket), but
	// internal/app's tests are one long-lived process, and a leaked listener
	// there wedges its port for every test that follows.
	stopWhatStarted := func() {
		if err := a.serving.shutdownAll(context.Background()); err != nil {
			slog.Error("stopping encrypted listeners after a failed start", "err", err)
		}
		for _, s := range a.servers {
			if err := s.Shutdown(context.Background()); err != nil {
				slog.Error("stopping a DNS listener after a failed start", "err", err)
			}
		}
		a.servers = nil
		cancel()
	}

	// Compile before binding, from the list copies already on disk and the
	// stored rules. The initial download below can take as long as the
	// slowest subscribed URL — a reboot with the WAN down spends the full
	// fetch timeout per list — and until it finished, every listener was
	// already answering, unfiltered, with usable copies sitting in the data
	// dir. This costs a few milliseconds of disk and parse; a failure is not
	// a reason to refuse to resolve.
	if err := a.refresher.Recompile(ctx); err != nil {
		slog.Error("compiling filters from the list cache at startup failed", "err", err)
	}

	for _, addr := range a.cfg.DNSListen {
		// The key store, not a snapshot of it: a key created through the API
		// is live on the next signed message rather than the next restart.
		// a.xfrOut and a.notifyIn are each the same instance on every
		// listener, so a transfer or a notify answers identically regardless
		// of which address a peer dials.
		s := dnssrv.NewServer(addr, a.handler,
			dnssrv.WithTSIGKeys(a.st.TSIGKeys()),
			dnssrv.WithTransfers(a.xfrOut),
			dnssrv.WithNotifies(a.notifyIn))
		if err := s.Start(); err != nil {
			stopWhatStarted()
			return err
		}
		a.servers = append(a.servers, s)
	}

	apiSrv := api.New(api.Deps{
		Store: a.st, Auth: auth.New(a.st.Users(), a.st.Tokens()),
		Engine: a.engine, Reloader: a, Logger: a.logger, Refresher: a.refresher,
		ZoneRefresher: a.zoneRefresh,
		// a, again: App is what knows the ladder fell to plaintext defaults.
		ResolverStatus: a,
		Version:        a.version, Static: web.Dist(),
		TrustedProxies: a.cfg.TrustedProxies,
	})
	ln, err := net.Listen("tcp", a.cfg.HTTPListen)
	if err != nil {
		stopWhatStarted()
		return err
	}
	a.apiAddr = ln.Addr().String()
	// apiCtx is the base context for every API request (via BaseContext
	// below). Cancelling it in Shutdown propagates to every in-flight
	// request's r.Context(), so long-lived handlers blocked on ctx.Done()
	// (the SSE tail handler) unblock immediately instead of making
	// apiSrv.Shutdown wait out its deadline for a connection that would
	// otherwise never go idle on its own.
	apiCtx, apiCancel := context.WithCancel(context.Background())
	a.apiCancel = apiCancel
	a.apiSrv = &http.Server{
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return apiCtx },
	}
	go func() {
		if err := a.apiSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("api server exited", "err", err)
		}
	}()

	// runCtx, not ctx: the closure outlives Start and is called on every
	// prune, so it has to read under the context that ends when the app
	// stops rather than under the caller's, which in cmd/dnsaur is cancelled
	// by SIGTERM and would make the last prune read nothing.
	//
	// It prunes stats_hourly on the same pass, on its own retention: the
	// rollups are a row per hour per name and per client, small enough to
	// keep for a year against the query log's ninety days, but not small
	// enough to keep forever.
	pruner := qlog.NewPruner(a.st.QueryLog(), a.st.Stats(),
		func() int64 { return a.getInt(runCtx, "qlog.retention_days", 90) },
		func() int64 { return a.getInt(runCtx, "stats.retention_days", 365) })
	rollups := stats.NewRunner(a.st.Stats(), a.st.Settings(), time.Minute)
	refreshEvery := time.Duration(a.getInt(ctx, "lists.refresh_hours", 24)) * time.Hour
	changes := a.st.Settings().Changes()

	a.bg = []func(context.Context){
		a.logger.Run,
		pruner.Run,
		a.runTokenCleanup,
		rollups.Run,
		// Secondary zones, on each zone's own SOA schedule. It transfers what
		// is already due before its first tick, so a restart does not leave a
		// zone that expired overnight answering nothing for another interval.
		a.zoneRefresh.Run,
		// Outbound NOTIFY. It passes once before its first tick, which finds
		// nothing on a healthy restart — every row already records delivery
		// at the current serial — and catches up anything that changed while
		// the process was down.
		a.notifier.Run,
		// The inbound half's lifetime, not a worker: it does nothing until
		// runCtx ends, and then stops NotifyServer admitting new work and
		// waits for the goroutine an admitted NOTIFY started. Being in this
		// list is what puts that goroutine inside a.wg, so Shutdown waits
		// for it before closing the store rather than pulling the store out
		// from under a transfer.
		a.notifyIn.Run,
		// Re-attempts DoT/DoH while either is enabled and not listening, so
		// a bind failure whose cause the operator has since cleared stops
		// needing an unrelated settings write to notice — see
		// runServingRetry.
		a.runServingRetry,
		func(c context.Context) { a.refresher.Run(c, refreshEvery) },
		func(c context.Context) {
			for {
				select {
				case <-c.Done():
					return
				case <-changes:
					a.applySettings(c)
					// Compile, do not download. A settings write can
					// change which rules and lists a group enforces, so
					// the ruleset has to be rebuilt; it cannot change
					// what any URL serves, so there is nothing to fetch.
					// Fetching anyway made every settings save wait on
					// the slowest subscribed URL. The ticker and the
					// list writes are what download.
					if err := a.refresher.Recompile(c); err != nil {
						slog.Error("recompiling filters after a settings change failed", "err", err)
					}
				}
			}
		},
	}
	for _, f := range a.bg {
		a.wg.Add(1)
		go func(f func(context.Context)) { defer a.wg.Done(); f(runCtx) }(f)
	}
	a.wg.Add(1)
	go func() { // initial list load; readiness gate for tests
		defer a.wg.Done()
		if err := a.refresher.RefreshAll(runCtx); err != nil {
			slog.Error("initial blocklist refresh failed", "err", err)
		}
		close(a.ready)
	}()
	return nil
}

func (a *App) WaitReady(d time.Duration) {
	select {
	case <-a.ready:
	case <-time.After(d):
	}
}

func (a *App) DNSAddr() string {
	if len(a.servers) == 0 {
		return ""
	}
	return a.servers[0].Addr()
}

func (a *App) HTTPAddr() string { return a.apiAddr }

func (a *App) ReloadClients(ctx context.Context) error { return a.registry.Reload(ctx) }

// ReloadZones rebuilds the served snapshot and installs the routing table it
// implies, in that order: the table is derived from the snapshot, so pushing
// it first would publish the previous reload's routes.
//
// A failed reload returns without touching the forwarder, which keeps the
// table already installed. Clearing every claimed suffix because one store
// read failed would send internal names to the public internet — the
// opposite of the direction §9.11.5 fails in.
func (a *App) ReloadZones(ctx context.Context) error {
	if err := a.resolver.Reload(ctx); err != nil {
		return err
	}
	// Which forwarder is live, the build, and the install are one step: a
	// settings change landing between them would install this table on the
	// forwarder it is retiring, and the table that survived would be the one
	// applySettings built from a snapshot taken before this reload's zone
	// existed. That loses a claimed suffix to the public internet, which is
	// the direction adding or enabling a zone fails in.
	a.routeMu.Lock()
	defer a.routeMu.Unlock()
	a.installConditional(a.fwd.forwarder())
	return nil
}

func (a *App) RefreshFilters(ctx context.Context) error   { return a.refresher.RefreshAll(ctx) }
func (a *App) RecompileFilters(ctx context.Context) error { return a.refresher.Recompile(ctx) }
func (a *App) NotifyZones()                               { a.notifier.Wake() }

func (a *App) RefreshList(ctx context.Context, id int64) error {
	return a.refresher.RefreshOne(ctx, id)
}
func (a *App) NextFilterRefresh() int64 { return a.refresher.NextRefresh() }

func (a *App) Shutdown(ctx context.Context) error {
	if a.apiCancel != nil {
		// Cancel in-flight request contexts first so handlers blocked on
		// r.Context().Done() (the SSE tail handler) return promptly,
		// letting apiSrv.Shutdown below complete well inside its deadline
		// instead of waiting for those connections to go idle.
		a.apiCancel()
	}
	if a.apiSrv != nil {
		_ = a.apiSrv.Shutdown(ctx)
	}
	var errs []error
	for _, s := range a.servers {
		if err := s.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	// Cancel before the encrypted listeners come down, not after. a.wg.Wait
	// below waits for an in-flight applySettings, and applySettings ends in
	// a reconcile that can start listeners. shutdownAll holds that pass's
	// own lock and marks the reconciler closed, so a racing pass can no
	// longer commit a listener behind it; cancelling first is what keeps
	// this prompt rather than what makes it correct — a pass that has not
	// yet read settings dies there, instead of making shutdownAll wait out
	// a bind attempt and a listener drain before it can take the lock.
	if a.cancel != nil {
		a.cancel()
	}
	// The encrypted listeners the reconciler is holding, if any — a.servers
	// above is only ever the plain :53 listeners built once in Start.
	if err := a.serving.shutdownAll(ctx); err != nil {
		errs = append(errs, err)
	}
	a.wg.Wait() // qlog drains its buffer on ctx cancel before returning
	if f := a.fwd.forwarder(); f != nil {
		_ = f.Close()
	}
	// Joined rather than "first wins": a socket that would not close is a
	// separate fact from a store that would not close, and the caller
	// (cmd/dnsaur) prints whatever comes back.
	errs = append(errs, a.st.Close())
	return errors.Join(errs...)
}
