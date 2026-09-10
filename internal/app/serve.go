package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aloks98/dnsaur/internal/api"
	"github.com/aloks98/dnsaur/internal/dnssrv"
)

// certExpiryWarningWindow is how close to a certificate's NotAfter counts
// as "expiring soon" for CertExpiry below. A constant, not a setting: it
// exists to catch a certificate-delivery mechanism (certbot, rsync, a
// manual copy) that has quietly stopped working, and an operator who could
// tune the threshold could tune it to never fire — which defeats the point
// of having it at all.
const certExpiryWarningWindow = 14 * 24 * time.Hour

// servingState is what the reconciler achieved on its most recent pass —
// intent from settings, alongside the reality a bind attempt produced.
// Read by the status endpoint (Task 9).
type servingState struct {
	DoT protocolState
	DoH protocolState
}

// protocolState is one encrypted protocol's reconciled state. Enabled is
// what the setting says; Listening is whether a socket is actually open
// right now — and the two are allowed to disagree, which is the whole
// reason this is two fields and not one bool. A privileged port already
// taken by something else leaves Enabled true and Listening false, and so
// does a certificate that will not load at the moment a listener is
// (re)started.
//
// An unreadable certificate under an *already running* listener does not:
// reconcileOne's leave-alone branch never touches it, and it keeps serving
// from the keypair it has cached (rule 3, and
// TestServingCertificateDeletedUnderARunningListenerLeavesItServing). That
// disagreement only appears at the next start — a restart, an address
// change, a certificate-path change.
type protocolState struct {
	Enabled   bool
	Listening bool
	Addr      string
	Err       string // the bind error, when Enabled && !Listening
}

// listener is the surface dnssrv.Server and dnssrv.DoHServer both expose.
// Declared locally, rather than reused from internal/dnssrv, because
// nothing outside this reconciler needs to hold the two kinds of listener
// interchangeably — and declaring it here is what lets the diff logic
// below treat DoT and DoH identically, with no type switch anywhere in it.
type listener interface {
	Start() error
	Addr() string
	Shutdown(ctx context.Context) error
}

// running is one protocol's live listener, remembered beside the
// configuration it was actually started with.
//
// certPath/keyPath are part of that configuration, not merely metadata:
// repointing serve.tls.cert/key at a different, perfectly valid keypair
// while a listener is up must be noticed and acted on (it is a save the
// API accepts, same as an address change), and the only way reconcileOne
// can tell "the certificate changed" from "nothing changed" is to remember
// what was asked for last time and compare.
//
// The asked-for address, similarly, rather than one re-derived from
// listener.Addr() on every reconcile: those two can legitimately differ
// (an ephemeral ":0"-style address, which nothing in production configures
// but a test might), and it is the configured address a settings change is
// compared against.
type running struct {
	l                 listener
	addr              string
	certPath, keyPath string
}

// servingReconciler owns the two encrypted listeners' lifecycle: which are
// up, the servingState the last reconcile pass computed, and the one
// CertProvider both of them are built from.
//
// Its own mutex, separate from App.routeMu and swappable.mu, because it
// protects a third and unrelated piece of state that neither of those
// paths ever needs to read or write, and vice versa — so there is no
// ordering to document between them, only the one rule every method here
// follows: the lock is taken to read or swap fields, never held across
// Start, Shutdown, or any filesystem read.
type servingReconciler struct {
	// passMu serialises whole reconcile passes against each other, and
	// against shutdownAll. There are two pass callers now — the settings
	// watcher, through applySettings, and the retry loop below — and two
	// passes running at once would race each other's Start and Shutdown
	// over the same address, with the loser's listener left running and
	// unreferenced. shutdownAll takes it for the same reason, one level
	// up: a pass interleaving with it binds listeners into a reconciler
	// that has just been emptied.
	//
	// Deliberately not mu. mu guards field reads and swaps and is never
	// held across I/O; this one is held across Start and Shutdown for a
	// whole pass, by design. Keeping them separate is what lets status()
	// and certLeaf() answer instantly while a pass is draining a
	// connection — the property "the lock is never held across a socket
	// call" is about mu, and it still holds.
	passMu sync.Mutex

	// closed is written by shutdownAll and read by reconcileServing, both
	// under passMu, and is one-way: there is no reopen. It is what makes
	// the two orderings passMu leaves — pass then shutdown, shutdown then
	// pass — both correct, the second by having the pass start nothing.
	closed bool

	mu    sync.Mutex
	dot   *running
	doh   *running
	state servingState

	// certPath/keyPath and cert are the CertProvider currently shared by
	// DoT and DoH, and the paths it was built for. Rebuilt only when those
	// paths themselves change (certProviderFor), and otherwise handed out
	// to whichever protocol (re)starts, however much later — see
	// certProviderFor's comment for why two independent providers over the
	// same on-disk keypair is exactly what this avoids.
	certPath, keyPath string
	cert              *dnssrv.CertProvider
}

// snapshot returns what is currently running for each protocol. Deciding
// what to do with that is pure, in-memory work; doing it (Start, Shutdown)
// is not, so the caller reads this once, releases the lock implicitly by
// returning from this call, and only performs I/O afterwards.
func (r *servingReconciler) snapshot() (dot, doh *running) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dot, r.doh
}

// certProviderFor returns the CertProvider DoT and DoH should both build
// their tls.Config from for certPath/keyPath, building a fresh one only
// when those paths differ from what is already cached.
//
// Sharing matters because DoT and DoH read the very same two settings:
// without this, a cert path change that reaches DoT's restart before DoH's
// (or a DoT and DoH that simply started at different times) would leave
// two independent CertProvider instances, each with its own mtime cache,
// over what is meant to be one keypair — and two independent mtime caches
// can disagree briefly across a renewal, serving different certificates on
// the two transports for one handshake apiece. One instance, handed to
// whichever transport (re)starts and whenever, has only one cache to be
// stale.
//
// In-memory only — building a CertProvider does no I/O (NewCertProvider,
// certs.go) — so this is safe to call under r.mu; the eager load that
// might actually fail happens in the caller, afterwards, with no lock
// held.
func (r *servingReconciler) certProviderFor(certPath, keyPath string) *dnssrv.CertProvider {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cert == nil || r.certPath != certPath || r.keyPath != keyPath {
		r.cert = dnssrv.NewCertProvider(certPath, keyPath)
		r.certPath, r.keyPath = certPath, keyPath
	}
	return r.cert
}

// certLeaf returns the certificate the shared CertProvider currently holds
// for the configured paths. It returns nil in two distinct cases the caller
// (CertExpiry) deliberately does not distinguish, both being "no certificate
// loaded" rather than "expires soon": no provider is held, because neither
// DoT nor DoH is enabled or no certificate is configured (r.cert is nil —
// see dropCertProvider); or a provider exists but has never successfully
// read a keypair from disk.
//
// GetCertificate first, rather than Leaf alone. Leaf only advances when a
// reload happens, and a reload only happens on a handshake — so on a quiet
// resolver a renewed certificate would keep reporting the old NotAfter, and
// the "expires in N days" warning would stay up, until some client happened
// to connect. Asking the provider is what makes the mtime check run: it
// costs two Stats on a status read, and reloads only when the files have
// actually moved. Its error is deliberately unexamined here — a failed
// reload returns the cached keypair, and reporting "no certificate loaded"
// for a transient read failure is a worse answer than the expiry of the one
// still being served.
//
// The provider pointer is copied out from under r.mu before the call, so no
// file I/O ever runs with the reconciler's lock held.
func (r *servingReconciler) certLeaf() *x509.Certificate {
	r.mu.Lock()
	cert := r.cert
	r.mu.Unlock()
	if cert == nil {
		return nil
	}
	if loaded, err := cert.GetCertificate(nil); err == nil && loaded != nil {
		return loaded.Leaf
	}
	return cert.Leaf()
}

// dropCertProvider forgets the shared CertProvider, so certLeaf reports "no
// certificate loaded" again.
//
// Without it the provider outlives everything it describes: it is built by
// startDoT/startDoH and never cleared, so after both protocols are turned
// off — or after the certificate paths are blanked entirely — Leaf goes on
// returning the last keypair that loaded, and the expiry banner goes on
// nagging about a certificate nothing serves and that is no longer even
// configured, until the process restarts.
func (r *servingReconciler) dropCertProvider() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cert = nil
	r.certPath, r.keyPath = "", ""
}

// commit installs the outcome of a reconcile pass — the listeners now
// running (or nil) and the state describing them — after every Start and
// Shutdown that pass needed has already happened.
func (r *servingReconciler) commit(dot, doh *running, state servingState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dot, r.doh = dot, doh
	r.state = state
}

// status returns a copy of the state the last reconcile pass computed.
// Safe to call from any goroutine, including the API handler Task 9 adds.
func (r *servingReconciler) status() servingState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// shutdownAll stops every listener the reconciler is holding, and reports
// what would not close. The listeners are taken out from under r.mu before
// Shutdown runs on them — Shutdown can block draining an in-flight
// connection, and that must never happen while r.mu is held.
//
// passMu is held for the whole call, and closed set under it, because a
// reconcile pass is otherwise free to interleave: a pass past its settings
// read consults nothing else that shutting down changes, so it would go on
// to bind fresh listeners and commit them into the fields emptied here,
// leaving them serving against a store App.Shutdown is about to close.
// Holding the pass lock leaves only the two orderings that are correct.
// Neither caller holds passMu itself — Start's error path (app.go) and
// App.Shutdown both call in from outside any pass.
//
// The error is returned rather than swallowed because a listener that fails
// to close is precisely the failure that must not be silent: the socket
// stays bound, the protocol goes on answering queries, and the next process
// to want that address cannot have it.
func (r *servingReconciler) shutdownAll(ctx context.Context) error {
	r.passMu.Lock()
	defer r.passMu.Unlock()
	r.closed = true

	r.mu.Lock()
	dot, doh := r.dot, r.doh
	r.dot, r.doh = nil, nil
	r.mu.Unlock()

	var errs []error
	for _, run := range []*running{dot, doh} {
		if run != nil {
			if err := run.l.Shutdown(ctx); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// protoWant is one protocol's desired configuration, read fresh from
// settings on every reconcile.
type protoWant struct {
	enabled           bool
	addr              string
	certPath, keyPath string
}

// settingsSnapshot is everything reconcileServing needs from the settings
// store, read together so a single read failure can abort the whole pass
// rather than being absorbed key by key.
type settingsSnapshot struct {
	certPath, keyPath string
	dot, doh          protoWant
}

// readServingSettings reads every setting reconcileServing needs and
// reports the first read error, if any.
//
// A read error is not the same thing as a key that legitimately reads as
// "" or "false" — Settings().Get's own (string, bool, error) already makes
// that distinction (a missing key is ("", false, nil), not an error) — and
// this refuses to collapse the two the way a bare value-only read would.
// The distinction is the whole point: a transient database failure must
// not read as "the operator disabled encrypted DNS".
func (a *App) readServingSettings(ctx context.Context) (settingsSnapshot, error) {
	read := func(key string) (string, error) {
		v, _, err := a.st.Settings().Get(ctx, key)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", key, err)
		}
		return v, nil
	}

	certPath, err := read("serve.tls.cert")
	if err != nil {
		return settingsSnapshot{}, err
	}
	keyPath, err := read("serve.tls.key")
	if err != nil {
		return settingsSnapshot{}, err
	}
	dotEnabled, err := read("serve.dot.enabled")
	if err != nil {
		return settingsSnapshot{}, err
	}
	dotAddr, err := read("serve.dot.listen")
	if err != nil {
		return settingsSnapshot{}, err
	}
	dohEnabled, err := read("serve.doh.enabled")
	if err != nil {
		return settingsSnapshot{}, err
	}
	dohAddr, err := read("serve.doh.listen")
	if err != nil {
		return settingsSnapshot{}, err
	}

	return settingsSnapshot{
		certPath: certPath,
		keyPath:  keyPath,
		dot: protoWant{
			enabled: dotEnabled == "true", addr: dotAddr,
			certPath: certPath, keyPath: keyPath,
		},
		doh: protoWant{
			enabled: dohEnabled == "true", addr: dohAddr,
			certPath: certPath, keyPath: keyPath,
		},
	}, nil
}

// ServingStatus reports what the listener reconciler last achieved: DoT and
// DoH's intent (from settings) and reality (whether a socket is actually
// open), for the status endpoint Task 9 adds and for tests.
func (a *App) ServingStatus() servingState { return a.serving.status() }

// Serving satisfies api.ResolverStatus. It converts the reconciler's own
// unexported protocolState into api.ProtocolStatus on the way out — see
// ProtocolStatus's doc comment in server.go for why that type has to be
// declared over there rather than named from this interface.
func (a *App) Serving() (dot, doh api.ProtocolStatus) {
	st := a.serving.status()
	return st.DoT.toAPI(), st.DoH.toAPI()
}

// toAPI converts one protocol's reconciled state to the shape the API
// exposes it in. A separate method, rather than JSON tags on protocolState
// itself, because protocolState is internal/app's own bookkeeping (compared
// field-by-field by reconcileOne) and has no business also being a wire
// format.
func (p protocolState) toAPI() api.ProtocolStatus {
	return api.ProtocolStatus{Enabled: p.Enabled, Listening: p.Listening, Addr: p.Addr, Err: p.Err}
}

// DNSListening satisfies api.ResolverStatus: whether any socket is
// answering DNS right now, which is what GET /readyz turns on.
//
// a.servers is written once, in Start, before the API server it is read
// from exists, and never rewritten afterwards — Shutdown stops the
// listeners without clearing the slice — so this needs no lock. The
// encrypted pair is read through the reconciler, which takes its own.
func (a *App) DNSListening() bool {
	if len(a.servers) > 0 {
		return true
	}
	st := a.serving.status()
	return st.DoT.Listening || st.DoH.Listening
}

// CertExpiry satisfies api.ResolverStatus: the loaded certificate's expiry,
// and whether it falls within certExpiryWarningWindow. ok is false when no
// certificate has ever loaded successfully — see certLeaf — which is a
// different fact from "expires soon", and reporting the former as the
// latter would put a spurious warning on a fresh install with nothing
// configured yet, or on a certificate path that has never once read
// cleanly.
func (a *App) CertExpiry() (notAfter time.Time, expiringSoon, ok bool) {
	leaf := a.serving.certLeaf()
	if leaf == nil {
		return time.Time{}, false, false
	}
	return leaf.NotAfter, time.Until(leaf.NotAfter) < certExpiryWarningWindow, true
}

// reconcileServing diffs DoT and DoH's desired configuration (settings)
// against what is actually running, and performs exactly the stops and
// starts that diff calls for — spec §6.
//
// Called from applySettings, after the forwarder swap: a settings write
// that touches both the resolution path and serving does the DNS-path work
// first. Every Start and Shutdown below runs with no lock held — the
// reconciler's own mutex (in snapshot, certProviderFor and commit) is
// taken only to swap state, never around a socket operation. That is the
// rule E1's dot.go broke once already (a Close under a mutex, its
// Important review finding), carried forward here as a hard requirement
// rather than a style preference.
//
// If the settings read itself fails, nothing here runs at all: no Start,
// no Shutdown, no commit. A stale servingState — and, more to the point,
// listeners that are still up — is the correct outcome of a database
// hiccup. Rewriting servingState to "disabled" and tearing down two
// working listeners because a read failed would turn a transient,
// self-healing blip into an outage that persists until some unrelated
// settings write happens to trigger a working reconcile.
func (a *App) reconcileServing(ctx context.Context) {
	a.serving.passMu.Lock()
	defer a.serving.passMu.Unlock()

	// shutdownAll has already run, under this same lock. Starting a
	// listener now would bind a socket no shutdown path holds a reference
	// to — see closed, and
	// TestServingReconcilingAfterShutdownAllStartsNothing.
	if a.serving.closed {
		return
	}

	snap, err := a.readServingSettings(ctx)
	if err != nil {
		slog.Error("reconcile serving: reading settings failed; DoT and DoH left exactly as they are", "err", err)
		return
	}

	curDot, curDoh := a.serving.snapshot()

	newDot, dotState := reconcileOne(curDot, snap.dot, func(addr string) (listener, error) {
		return a.startDoT(addr, snap.certPath, snap.keyPath)
	})
	newDoh, dohState := reconcileOne(curDoh, snap.doh, func(addr string) (listener, error) {
		return a.startDoH(addr, snap.certPath, snap.keyPath)
	})

	a.serving.commit(newDot, newDoh, servingState{DoT: dotState, DoH: dohState})

	// Nothing wants a certificate any more: neither protocol is enabled, or
	// no keypair is configured at all. Forget the provider so the expiry
	// warning stops describing something that is no longer serving or no
	// longer named — see dropCertProvider.
	//
	// Keyed on intent, not on whether a listener came up: a protocol that is
	// enabled and merely failing to bind still wants its certificate
	// watched, and the operator still needs to know when it expires.
	if (!snap.dot.enabled && !snap.doh.enabled) || snap.certPath == "" || snap.keyPath == "" {
		a.serving.dropCertProvider()
	}
}

// servingRetryEvery is how often runServingRetry re-attempts a protocol
// that is enabled and not listening.
//
// Spec §8 promises the bind-failure warning clears "on its own … when a
// later reconcile binds successfully". Nothing used to *cause* a later
// reconcile: reconcileServing ran only from applySettings, which runs at
// startup and on a settings write, so an operator who stopped whatever was
// holding :853 watched the banner stay up until some unrelated setting
// happened to be saved. Thirty seconds is short enough that clearing it
// feels automatic and long enough that a permanently occupied port costs
// one bind attempt a minute per protocol rather than a busy loop.
// A var rather than a const only so the reconcile-retry test can shorten
// it: waiting thirty seconds for one assertion is not a test anyone runs.
// Nothing in production writes it.
var servingRetryEvery = 30 * time.Second

// runServingRetry re-runs the reconciler while any enabled protocol is not
// listening, and does nothing at all otherwise.
//
// The guard matters: reconcileServing on a converged configuration is not
// free (it reads six settings), and running it unconditionally on a timer
// would put a database read on a clock for the overwhelmingly common case
// where nothing is wrong. Reading the state it already computed costs one
// mutex.
func (a *App) runServingRetry(ctx context.Context) {
	t := time.NewTicker(servingRetryEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st := a.serving.status()
			if needsRetry(st.DoT) || needsRetry(st.DoH) {
				a.reconcileServing(ctx)
			}
		}
	}
}

// needsRetry reports whether p is the "enabled but not listening" state a
// later bind could still fix.
func needsRetry(p protocolState) bool { return p.Enabled && !p.Listening }

// reconcileOne decides and performs one protocol's transition from cur to
// want, and reports what is true of it afterwards. Three shapes, matching
// spec §6 exactly:
//
//   - Not wanted: stop it if it is running (idempotent if it already
//     isn't), report disabled.
//   - Wanted, and either nothing is running yet or the configuration
//     changed (address, certificate path, or key path): stop whatever is
//     running first — a bind cannot succeed while the old listener still
//     holds the address — then start fresh. A failed start is reported and
//     left down: never a fallback to the address or listener that was just
//     retired, because a listener serving a configuration the settings no
//     longer name is a worse lie than one that is honestly off. The
//     address that failed to bind is still reported, even though nothing
//     is listening on it, because the address is the useful half of "bind
//     :853: address already in use".
//   - Wanted, already running, same configuration: returned untouched. No
//     probe, no restart, not even a certificate re-check — this is the
//     branch a rebuild-everything reconciler skips, and it is what keeps
//     toggling one protocol from dropping a live connection on the other.
func reconcileOne(cur *running, want protoWant, start func(addr string) (listener, error)) (*running, protocolState) {
	if !want.enabled {
		if cur != nil {
			shutdownListener(cur.l)
		}
		return nil, protocolState{Enabled: false}
	}

	unchanged := cur != nil && cur.addr == want.addr &&
		cur.certPath == want.certPath && cur.keyPath == want.keyPath
	if unchanged {
		// Leave strictly alone: the desired configuration has not changed,
		// so nothing here may touch the socket — including the certificate
		// it is serving. A certificate that has since become unreadable at
		// the same path is not this listener's problem until something
		// else gives it a reason to restart (rule 3, task-8-brief.md).
		return cur, protocolState{Enabled: true, Listening: true, Addr: cur.l.Addr()}
	}
	if cur != nil {
		// The configuration changed — address, certificate, or both:
		// stop-then-start, because the new listener cannot bind while the
		// old one still holds the address.
		shutdownListener(cur.l)
	}

	l, err := start(want.addr)
	if err != nil {
		return nil, protocolState{Enabled: true, Listening: false, Addr: want.addr, Err: err.Error()}
	}
	return &running{l: l, addr: want.addr, certPath: want.certPath, keyPath: want.keyPath},
		protocolState{Enabled: true, Listening: true, Addr: l.Addr()}
}

// shutdownListener stops l against a bounded deadline of its own,
// independent of whatever context triggered the reconcile that is retiring
// it — the same "its own deadline, none of the caller's" discipline
// Server.serve already uses for zone transfers and NOTIFY (server.go).
func shutdownListener(l listener) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := l.Shutdown(ctx); err != nil {
		// Nothing here can return it — a reconcile pass has no caller to
		// report to — but a socket that would not close leaves the
		// protocol serving a configuration the settings no longer name,
		// and the address unbindable for whatever the operator asked for
		// instead. Logging it is the difference between an operator who
		// can see why the next bind says "address already in use" and one
		// who cannot.
		slog.Error("stopping an encrypted listener failed; its address may still be bound", "err", err)
	}
}

// startDoT builds and starts a DNS-over-TLS listener at addr, wired to the
// same handler, TSIG provider, transfer and NOTIFY intercepts the plain
// listeners in App.Start get. Spec §9 promises those behave identically
// regardless of which transport carried the query; this call site is what
// keeps that promise true for DoT rather than merely true in principle.
func (a *App) startDoT(addr, certPath, keyPath string) (listener, error) {
	tlsCfg, err := buildTLSConfig(a.serving.certProviderFor(certPath, keyPath))
	if err != nil {
		return nil, err
	}
	s := dnssrv.NewServer(addr, a.handler,
		dnssrv.WithTLS(tlsCfg),
		dnssrv.WithTSIGKeys(a.st.TSIGKeys()),
		dnssrv.WithTransfers(a.xfrOut),
		dnssrv.WithNotifies(a.notifyIn))
	if err := s.Start(); err != nil {
		return nil, err
	}
	return s, nil
}

// startDoH builds and starts a DNS-over-HTTPS listener at addr. No TSIG,
// transfer or NOTIFY wiring here: internal/dnssrv's DoH handler refuses
// both query types itself (doh.go) and has no miekg parse for a
// TsigProvider to verify a signature during — a limit intrinsic to the
// DoH transport, not a choice this reconciler is making.
func (a *App) startDoH(addr, certPath, keyPath string) (listener, error) {
	tlsCfg, err := buildTLSConfig(a.serving.certProviderFor(certPath, keyPath))
	if err != nil {
		return nil, err
	}
	s := dnssrv.NewDoHServer(addr, a.handler, tlsCfg)
	if err := s.Start(); err != nil {
		return nil, err
	}
	return s, nil
}

// buildTLSConfig builds a tls.Config backed by provider, having first
// confirmed the keypair it names actually loads.
//
// The eager load is the point. tls.Listen never reads the certificate at
// all — CertProvider.GetCertificate only ever runs lazily, on the first
// handshake — so without this check, "enabled but no usable certificate"
// would bind the socket successfully and report itself as Listening while
// silently refusing every connection that reaches it. Loading here folds
// that failure into the same bind-time outcome a taken port produces (spec
// §6), which is what lets reconcileOne treat both through one return path.
//
// It is only ever called from the two reconcileOne branches that are about
// to (re)start a listener — never for one already running. An
// already-running listener's own CertProvider, and its cache of the last
// keypair that loaded successfully, is never touched here — which is what
// keeps a certificate deleted, replaced badly, or made unreadable after a
// valid save from tearing down a listener that is already serving it.
func buildTLSConfig(provider *dnssrv.CertProvider) (*tls.Config, error) {
	if _, err := provider.GetCertificate(nil); err != nil {
		return nil, err
	}
	return &tls.Config{GetCertificate: provider.GetCertificate, MinVersion: tls.VersionTLS12}, nil
}
