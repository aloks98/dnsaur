package app

import (
	"context"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aloks98/dnsaur/internal/cache"
	"github.com/aloks98/dnsaur/internal/clients"
	"github.com/aloks98/dnsaur/internal/config"
	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/filter"
	"github.com/aloks98/dnsaur/internal/qlog"
	"github.com/aloks98/dnsaur/internal/records"
	"github.com/aloks98/dnsaur/internal/stats"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/upstream"
	"github.com/google/uuid"
)

func defaultSettings() map[string]string {
	return map[string]string{
		"instance.id":           uuid.NewString(),
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
	}
}

// swappable lets us rebuild the tail of the pipeline (forwarder) on
// settings changes without restarting listeners.
type swappable struct {
	mu  sync.RWMutex
	h   dnssrv.Handler
	has bool // true once set has been called at least once
}

func (s *swappable) set(h dnssrv.Handler) {
	s.mu.Lock()
	s.h = h
	s.has = true
	s.mu.Unlock()
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
	cfg       *config.Config
	st        store.Store
	registry  *clients.Registry
	engine    *filter.Engine
	resolver  *records.Resolver
	refresher *filter.Refresher
	logger    *qlog.Logger
	fwd       *swappable
	servers   []*dnssrv.Server
	bg        []func(context.Context)
	cancel    context.CancelFunc
	ready     chan struct{}
	wg        sync.WaitGroup
}

func New(ctx context.Context, cfg *config.Config) (*App, error) {
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
		st:       st,
		registry: clients.NewRegistry(st.Clients()),
		engine:   filter.NewEngine(),
		resolver: records.NewResolver(st.Records()),
		fwd:      &swappable{},
		ready:    make(chan struct{}),
	}
	a.refresher = filter.NewRefresher(st.Filters(), st.Clients(), a.engine, cfg.DataDir)
	return a, nil
}

func (a *App) Store() store.Store { return a.st }

func (a *App) getSetting(ctx context.Context, key string) string {
	v, _, _ := a.st.Settings().Get(ctx, key)
	return v
}

func (a *App) getInt(ctx context.Context, key string, fallback int64) int64 {
	v, err := a.st.Settings().GetInt(ctx, key)
	if err != nil {
		return fallback
	}
	return v
}

// parseUpstreams splits a comma-separated upstreams setting into a clean
// address list: entries are trimmed, empties are dropped (logged at debug),
// and a missing port gets ":53" appended. Bare IPv6 literals (no brackets,
// no port) are bracketed first so the appended port parses correctly.
func parseUpstreams(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			slog.Debug("skipping empty upstream entry")
			continue
		}
		if _, _, err := net.SplitHostPort(p); err != nil {
			if strings.Contains(p, ":") && !strings.HasPrefix(p, "[") {
				p = "[" + p + "]:53" // bare IPv6 literal, e.g. "::1"
			} else {
				p += ":53"
			}
		}
		out = append(out, p)
	}
	return out
}

// buildForwarder wraps upstream.New with the same defaulted timeout/strategy
// handling applySettings needs in more than one place.
func buildForwarder(upstreams []string, strategy string) (*upstream.Forwarder, error) {
	return upstream.New(upstream.Config{Upstreams: upstreams, Strategy: strategy})
}

// applySettings re-reads DB settings into live components.
func (a *App) applySettings(ctx context.Context) {
	mode := a.getSetting(ctx, "blocking.mode")
	a.engine.SetBlocking(mode, uint32(a.getInt(ctx, "blocking.ttl", 30)))
	if err := a.registry.Reload(ctx); err != nil {
		slog.Error("client reload failed", "err", err)
	}
	if err := a.resolver.Reload(ctx); err != nil {
		slog.Error("records reload failed", "err", err)
	}
	if a.logger != nil {
		a.logger.SetPrivacy(a.getSetting(ctx, "qlog.privacy"))
	}

	upstreams := parseUpstreams(a.getSetting(ctx, "upstreams"))
	strategy := a.getSetting(ctx, "upstream.strategy")
	fwd, err := buildForwarder(upstreams, strategy)
	if err != nil {
		if a.fwd.isSet() {
			// A working forwarder already exists (e.g. from a previous
			// successful applySettings) — keep serving queries with it
			// rather than replacing it with something broken.
			slog.Error("keeping previous upstream config", "err", err)
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
			fwd, err = buildForwarder(parseUpstreams(defaultSettings()["upstreams"]), "race")
			if err != nil {
				// The hardcoded defaults are static and known-good; this
				// should be unreachable, but don't panic — leave the
				// forwarder unset and let Recover() keep serving SERVFAIL.
				slog.Error("default upstream config failed to build", "err", err)
				return
			}
		}
	}
	a.fwd.set(fwd.Handler())
}

func (a *App) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel

	a.applySettings(ctx)

	instanceID := a.getSetting(ctx, "instance.id")
	a.logger = qlog.New(a.st.QueryLog(), qlog.Options{
		Privacy: a.getSetting(ctx, "qlog.privacy"), InstanceID: instanceID,
	})
	dnsCache := cache.New(cache.Options{
		MinTTL:        time.Duration(a.getInt(ctx, "cache.min_ttl", 0)) * time.Second,
		MaxTTL:        time.Duration(a.getInt(ctx, "cache.max_ttl", 86400)) * time.Second,
		ServeStaleFor: time.Duration(a.getInt(ctx, "cache.serve_stale_for", 86400)) * time.Second,
		MaxEntries:    int(a.getInt(ctx, "cache.max_entries", 10000)),
	})
	handler := dnssrv.Chain(a.fwd,
		a.logger.Middleware(),
		dnssrv.Recover(),
		a.registry.Middleware(),
		a.engine.Middleware(),
		a.resolver.Middleware(),
		dnsCache.Middleware(),
	)
	for _, addr := range a.cfg.DNSListen {
		s := dnssrv.NewServer(addr, handler)
		if err := s.Start(); err != nil {
			return err
		}
		a.servers = append(a.servers, s)
	}

	pruner := qlog.NewPruner(a.st.QueryLog(), func() int64 { return a.getInt(ctx, "qlog.retention_days", 90) })
	rollups := stats.NewRunner(a.st.Stats(), a.st.Settings(), time.Minute)
	refreshEvery := time.Duration(a.getInt(ctx, "lists.refresh_hours", 24)) * time.Hour
	changes := a.st.Settings().Changes()

	a.bg = []func(context.Context){
		a.logger.Run,
		pruner.Run,
		rollups.Run,
		func(c context.Context) { a.refresher.Run(c, refreshEvery) },
		func(c context.Context) {
			for {
				select {
				case <-c.Done():
					return
				case <-changes:
					a.applySettings(c)
					if err := a.refresher.RefreshAll(c); err != nil {
						slog.Error("refresh after settings change failed", "err", err)
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

func (a *App) Shutdown(ctx context.Context) error {
	for _, s := range a.servers {
		_ = s.Shutdown(ctx)
	}
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait() // qlog drains its buffer on ctx cancel before returning
	return a.st.Close()
}
