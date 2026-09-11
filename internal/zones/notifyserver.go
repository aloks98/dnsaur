package zones

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// Answering a peer that notifies us: the gate that decides whether an
// arriving NOTIFY is one this server should act on, and what to say when it
// is not.
//
// The rules and their order are §9.10.2 of
// docs/superpowers/specs/2026-08-08-zones-design.md, mirroring §9.5.5's
// table on the transfer side. Where the two differ, and why, is explained at
// decidePeer.

// notifyRefusal is why a NOTIFY will not be acted on, and what to say about
// it. It is refusal's twin from transferserver.go; the two are separate types
// rather than one shared one because the rcodes mean different things — see
// decidePeer's comment. The reply itself is not twinned: both end at
// writeRefusal, which takes the fields rather than either type.
type notifyRefusal struct {
	rcode    int
	tsigCode uint16 // dns.RcodeBadKey/BadSig/BadTime, or 0 for no TSIG error RR
	reason   string // the log line
}

// notifyThrottle is the shortest interval between two SOA probes for one
// zone. A primary editing ten records sends ten NOTIFYs; each gets its own
// immediate NOERROR, because that is the peer's business, and they collapse
// to one probe. Without this, "NOTIFY is cheap" becomes a probe amplifier
// pointed at our own primary.
//
// **The collapse is trailing-edge, not leading-edge**, and the difference is
// a stale zone. A NOTIFY arriving inside an open window is not dropped: the
// window remembers it and one more probe runs when the window closes. Dropped
// instead, a serial bump that lands after the window's own probe has already
// asked — the primary editing at t=0 and again at t=2s — is lost until the SOA
// refresh fires, with the peer told NOERROR so it stops retransmitting (RFC
// 1996 §3.6). The bound the throttle exists for is unchanged, because what is
// remembered is "something arrived", not how much: ten suppressed NOTIFYs are
// worth one probe between them.
//
// It is the same shape as transferStateThrottle and deliberately not the same
// constant: that one throttles a database write, this one throttles a network
// round trip to somebody else's server.
const notifyThrottle = 5 * time.Second

// deferredProbeTick is how often a deferred NOTIFY re-reads the clock while it
// waits for its window to close.
//
// One timer for the remaining duration would be the obvious shape and would be
// wrong here: the clock this server reads is injectable
// (WithNotifyServerNow), so a test that moves it would still have to wait out
// real seconds, and the wait would then be measured against a clock nothing
// else in this file uses. Polling a comparison is what makes the wait honour
// whichever clock is in use, and at this resolution it costs a few hundred
// wakeups spread over one window.
const deferredProbeTick = 20 * time.Millisecond

// Refreshes is the half of *Refresher this needs, taken as an interface so a
// test can drive the gate without a Transferrer or a live primary.
type Refreshes interface {
	Refresh(ctx context.Context, zoneID int64) (TransferResult, error)
}

// Probes is the half of *Transferrer NotifyServer needs to decide whether a
// NOTIFY's zone has actually moved before transferring it. It is an option
// (WithNotifyProbes) rather than a constructor argument because a
// NotifyServer built without one still works — it skips the probe and
// refreshes on every admitted NOTIFY, which is what some of this file's own
// tests exercise — but production wires it, and act says so loudly when it is
// missing.
type Probes interface {
	ProbeSerial(ctx context.Context, z store.Zone) (uint32, netip.AddrPort, error)
}

// NotifyServer answers a peer that notifies us. It implements
// dnssrv.Notifies, which hands it the raw dns.ResponseWriter for the same
// reason TransferServer gets one: the reply is written here rather than
// through the pipeline.
type NotifyServer struct {
	res       *Resolver
	zs        store.ZoneStore
	refresher Refreshes
	// probes is nil only in a server built without WithNotifyProbes; see
	// Probes' doc comment. App wires it.
	probes Probes
	// dnsRes is the resolver ParsePrimaries uses to look up a hostname
	// primary. nil means net.DefaultResolver — see Lookup, and see
	// ParsePrimaries' own comment on why that default belongs to the caller,
	// not to a lookup baked into the gate.
	dnsRes Lookup
	// keys resolves a zone's own tsig_key_id to the key name a verified
	// message arrives under — the source gate's second route in, for a
	// primary whose packets do not come from an address anyone would list.
	// nil is a server built without WithNotifyKeys, which fails that route
	// closed rather than opening it. App wires it.
	keys TSIGKeys
	// now is the clock admit and a BADTIME reply's timestamp read. Injected
	// for the same reason TransferServer.now is: a test should be able to
	// drive the throttle rather than wait for it.
	now func() time.Time

	// mu guards states, the per-zone throttle and resolution state behind
	// admit, and stopped, the half of the work lifetime that has to be decided
	// atomically with wg.Add. Held only across those checks — the probe, the
	// lookup and the transfer all happen well outside it, so a slow primary
	// cannot serialise other zones' NOTIFYs.
	mu      sync.Mutex
	states  map[int64]*notifyState
	stopped bool

	// work is the context every goroutine ServeNotify starts runs under, and
	// wg counts them. Together they are this server's lifetime: see Run,
	// which is what App waits on.
	work     context.Context
	stopWork context.CancelFunc
	wg       sync.WaitGroup
}

// NotifyServerOption configures a NotifyServer at construction.
type NotifyServerOption func(*NotifyServer)

// WithNotifyServerNow replaces the clock the gate reads. Production leaves it
// alone and gets time.Now.
func WithNotifyServerNow(now func() time.Time) NotifyServerOption {
	return func(n *NotifyServer) { n.now = now }
}

// WithNotifyProbes attaches the SOA probe a NOTIFY's zone is checked against
// before it is transferred. Without it, every admitted NOTIFY transfers
// unconditionally — some of this file's own tests build no probe and assert
// exactly that.
func WithNotifyProbes(p Probes) NotifyServerOption {
	return func(n *NotifyServer) { n.probes = p }
}

// WithNotifyServerResolver replaces the resolver ParsePrimaries uses to
// resolve a hostname primary. Production passes dnsaur's own forwarder (see
// Lookup); nil is what ParsePrimaries reads as net.DefaultResolver. Tests
// use it to prove a lookup was, or was not, made — in particular that
// ServeNotify never resolves a zone's primaries until decideZone has already
// accepted the NOTIFY — without touching a real network.
// WithNotifyKeys attaches the TSIG key store the source gate resolves a
// zone's own tsig_key_id through. Without it a NOTIFY from an address the
// zone does not list is refused however it was signed — the pre-NAT
// behaviour — because there is nothing to compare the verified key name
// against. Production always passes it (internal/app).
func WithNotifyKeys(keys TSIGKeys) NotifyServerOption {
	return func(n *NotifyServer) { n.keys = keys }
}

func WithNotifyServerResolver(res Lookup) NotifyServerOption {
	return func(n *NotifyServer) { n.dnsRes = res }
}

// NewNotifyServer returns the handler dnssrv routes NOTIFY to.
func NewNotifyServer(r *Resolver, zs store.ZoneStore, rf Refreshes, opts ...NotifyServerOption) *NotifyServer {
	n := &NotifyServer{
		res: r, zs: zs, refresher: rf, now: time.Now,
		states: make(map[int64]*notifyState),
	}
	// Not derived from a caller's context on purpose: the work must outlive
	// the request that started it (RFC 1996 §4.7 replies first and acts
	// after), and the only thing allowed to end it is Run.
	n.work, n.stopWork = context.WithCancel(context.Background())
	for _, opt := range opts {
		opt(n)
	}
	if n.probes == nil {
		// Once, at startup, rather than only under load: a wiring mistake
		// that drops WithNotifyProbes (a typo, a refactor) would otherwise be
		// silent right up until it mattered, and by then it is a stale zone
		// in production rather than a line in a startup log. See act's own
		// per-call warning for the other half of "not silent".
		slog.Warn("notify: built without an SOA probe, every notify will transfer unconditionally")
	}
	return n
}

// decideZone runs the checks that need nothing but the message and the
// served snapshot: qtype, apex, type, enabled. Split from decidePeer so
// ServeNotify can refuse a NOTIFY for a zone it does not hold *before*
// resolving any hostname primary — see ServeNotify's own comment on why
// that ordering matters.
//
// primaries is not among decideZone's inputs for the same reason: it is
// resolved by the caller, only once decideZone has passed. ParsePrimaries
// takes a context and may do a live DNS lookup for a hostname primary, and a
// lookup inside the gate of the server that answers DNS is the thing
// acl.go's own comment refuses for the same reason.
func (n *NotifyServer) decideZone(m *dns.Msg) (*Zone, *notifyRefusal) {
	if len(m.Question) != 1 || m.Question[0].Qtype != dns.TypeSOA {
		return nil, &notifyRefusal{rcode: dns.RcodeFormatError, reason: "not a single SOA question"}
	}
	// The snapshot, never the store: a gate that read the store would decide
	// from data this server is not currently serving. D3's gate reads the
	// same snapshot for the same reason.
	z := n.res.Snapshot().Apex(qnameOf(m))
	if z == nil {
		return nil, &notifyRefusal{rcode: dns.RcodeNotAuth, reason: "no such zone"}
	}
	// Only a type that pulls from a master is told by anyone, which is the
	// same rule the scheduler applies (pullsFromAMaster in refresh.go) and
	// for the same reason: a NOTIFY is a hint that the master has moved on,
	// and it is worth nothing to a zone with no master to re-ask.
	//
	// That is a secondary and a stub. A stub pulls its delegation by the
	// same scheduler, under the same per-zone lock, recording the same
	// attempt — Refresher.Refresh, which is what act calls, already branches
	// on the type below this gate — so a master that has just moved its
	// delegation says so exactly the way it does for a zone it AXFRs, and a
	// gate naming only "secondary" would make this the one path that made a
	// stub wait out its SOA refresh instead.
	//
	// A primary owns its data, so a NOTIFY for one is either a
	// misconfiguration or an attempt to make this server pull its own zone
	// from somewhere else. A forwarder names its upstreams outright in
	// forward_to and has nobody to ask, so there is no work a NOTIFY for one
	// could cause. Both are refused, and decidePeer's source check still
	// applies to the two that are not.
	if !pullsFromAMaster(z.Type) {
		return nil, &notifyRefusal{rcode: dns.RcodeNotAuth, reason: "zone is type " + z.Type}
	}
	if !z.Enabled {
		return nil, &notifyRefusal{rcode: dns.RcodeNotAuth, reason: "zone is disabled"}
	}
	return z, nil
}

// decidePeer runs the checks about *who sent this*, against a zone
// decideZone has already accepted and primaries the caller resolved.
//
// Where this differs from TransferServer.decide, and why: the TSIG rows are
// §9.5.5's exactly, including that BADKEY and BADSIG are unsigned while
// BADTIME is signed — writeRefusal reuses errorTSIG and signIfVerified
// rather than a second copy of that rule. The *rcode* differs: D3 answers a
// refused transfer NOTAUTH under RFC 5936 §2.2.1, which is about authority
// over the zone, while a refused NOTIFY is a policy statement by a server
// that does hold the zone and declines to be told by this peer.
func (n *NotifyServer) decidePeer(ctx context.Context, z *Zone, peer netip.Addr, primaries []netip.AddrPort, key string, tsigErr error) *notifyRefusal {
	// RFC 1996 §3.10 says to ignore a NOTIFY from a host that is not a known
	// master. dnsaur answers REFUSED instead — a deliberate divergence,
	// recorded in §9.9. Silence is indistinguishable from a firewall drop,
	// the usual cause here is a primaries list one address wrong, and a
	// NOTIFY and its refusal are both ~50 bytes, so there is no
	// amplification and 1:1 reflection is not a useful attack primitive.
	//
	// **A TSIG that verified under this zone's own key stands in for the
	// address**, because it is the stronger claim of the two: an address can
	// be spoofed and a MAC cannot, and the key named on the zone is
	// specifically the one its master signs with. Without this a primary
	// behind NAT could not notify at all — it reaches us from the NAT's
	// address, which is not the one anybody would write in `primaries`, and
	// the only remedy was to widen the list to an address that is not the
	// primary's. Nothing else is widened: a message that is unsigned, signed
	// under a key this zone does not name, or signed and not verifying is
	// refused from an unlisted source exactly as before (§9.9, §9.10).
	if !notifyPeerAllowed(primaries, peer) && !n.signedByZoneKey(ctx, z, key, tsigErr) {
		return &notifyRefusal{rcode: dns.RcodeRefused, reason: "source is not a configured primary"}
	}

	// TSIG. The rows and their error codes are §9.5.5's exactly; only the
	// rcode differs, because this refusal is policy rather than authority.
	//
	// **The dns.Err* arms are deliberately NOT gated on the zone naming a
	// key**, and D3's sibling checks tsigErr unconditionally for the same
	// reason (transferserver.go). RFC 8945 §5.2.1 and §5.2.2 make BADKEY and
	// BADSIG mandatory regardless of local policy. Gate them and a NOTIFY
	// carrying a broken MAC to a keyless zone is *accepted*: signIfVerified
	// declines to sign, so the peer receives an unsigned NOERROR it must
	// discard, and retransmits per RFC 1996 §3.6 indefinitely. The zone's own
	// key requirement decides only whether an *absent* signature is refused.
	if tsigErr != nil {
		switch {
		case errors.Is(tsigErr, dns.ErrSecret), errors.Is(tsigErr, dns.ErrKeyAlg):
			return &notifyRefusal{rcode: dns.RcodeRefused, tsigCode: dns.RcodeBadKey, reason: "unknown TSIG key"}
		case errors.Is(tsigErr, dns.ErrSig):
			return &notifyRefusal{rcode: dns.RcodeRefused, tsigCode: dns.RcodeBadSig, reason: "TSIG did not verify"}
		case errors.Is(tsigErr, dns.ErrTime):
			return &notifyRefusal{rcode: dns.RcodeRefused, tsigCode: dns.RcodeBadTime, reason: "TSIG outside the fudge window"}
		case errors.Is(tsigErr, dnssrv.ErrTSIGUnsigned):
			// An absent signature is only a refusal for a zone that requires
			// one. A keyless zone accepts an unsigned NOTIFY and falls
			// through — which is the ordinary case.
			if z.TSIGKeyID != 0 {
				return &notifyRefusal{rcode: dns.RcodeRefused, reason: "zone requires TSIG and the message is unsigned"}
			}
		default:
			// dnssrv.ErrTSIGUnavailable (this server has no key store, so
			// nothing verified the message) and a key lookup that failed
			// because the store failed. Neither is the peer's fault and
			// neither is a TSIG error.
			//
			// SERVFAIL rather than REFUSED, matching TransferServer.decide
			// for the identical error value (transferserver.go). REFUSED
			// tells a correctly configured peer we decline it, when in fact
			// we were unable — which sends the operator to the far end of
			// the connection to debug a problem that is on this one. D3
			// recorded that reasoning; a second rule for one error value in
			// one codebase would be worse than either rule alone.
			return &notifyRefusal{rcode: dns.RcodeServerFailure, reason: "TSIG check failed: " + tsigErr.Error()}
		}
	}
	return nil
}

// notifyPeerAllowed reports whether peer is one of the zone's primaries.
//
// The port is deliberately not compared. A primary's `primaries` entry names
// where *we dial it*, and a NOTIFY arrives from an ephemeral source port —
// requiring a match would refuse every real NOTIFY ever sent.
func notifyPeerAllowed(primaries []netip.AddrPort, peer netip.Addr) bool {
	peer = peer.Unmap()
	for _, ap := range primaries {
		if ap.Addr().Unmap() == peer {
			return true
		}
	}
	return false
}

// signedByZoneKey reports whether this message verified under the key the
// zone itself names — the one thing that can stand in for a source address
// (see decidePeer).
//
// Every condition is a refusal to guess. tsigErr non-nil means nothing
// verified; an empty key name is what RequireTSIG returns alongside any
// failure, so it is never a verified message; a zone naming no key has
// nothing for a signature to match; and a server with no key store attached
// cannot resolve the id, which fails closed rather than accepting.
//
// A lookup error is also false rather than an error upward: this runs on an
// unauthenticated packet's path, and the caller's next answer for a source
// nobody listed is a refusal either way.
func (n *NotifyServer) signedByZoneKey(ctx context.Context, z *Zone, key string, tsigErr error) bool {
	if tsigErr != nil || key == "" || z.TSIGKeyID == 0 || n.keys == nil {
		return false
	}
	k, ok, err := n.keys.Get(ctx, z.TSIGKeyID)
	if err != nil {
		slog.Debug("resolving a zone's tsig key for a notify failed",
			"zone", z.Name, "key", z.TSIGKeyID, "err", err)
		return false
	}
	// Canonical on both sides for the reason acl.go canonicalises: the stored
	// name, the verified name and a hand-typed one are three spellings of one
	// thing, and comparing any two literally is a match that silently fails.
	return ok && dns.CanonicalName(k.Name) == dns.CanonicalName(key)
}

// ServeNotify answers one NOTIFY, then acts on it.
//
// The order is RFC 1996's and is not an optimisation. §4.7 has the responder
// enter its refresh state and §3.6 has the sender retransmitting until it
// gets a response, so a responder that waited for a transfer before replying
// would earn itself a second NOTIFY for the transfer already in flight.
func (n *NotifyServer) ServeNotify(ctx context.Context, w dns.ResponseWriter, m *dns.Msg, key string, tsigErr error) {
	peer := peerAddr(w)

	// The cheap, purely local checks first — qtype, apex, type, enabled —
	// and only then the primaries.
	//
	// **The ordering is load-bearing, not tidiness.** ParsePrimaries
	// resolves hostname entries with a live DNS lookup and no cache
	// (primaries.go's LookupNetIP). Resolving before these checks means one
	// unauthenticated, trivially source-spoofable UDP packet drives one
	// outbound recursive lookup, bounded only by dnssrv.NotifyTimeout and
	// throttled by nothing — the throttle below covers the transfer, not
	// this. That is the hazard acl.go's own comment refuses, and lifting the
	// call out of decide into ServeNotify achieved purity without removing
	// it. This ordering makes the hostname case cost nothing for a NOTIFY that
	// was going to be refused anyway; primariesFor is what makes it cost
	// nothing for most of the ones that were not (see its own comment).
	z, ref := n.decideZone(m)
	if ref == nil {
		ref = n.decidePeer(ctx, z, peer, n.primariesFor(ctx, z, peer), key, tsigErr)
	}
	if ref != nil {
		slog.Debug("notify refused", "peer", peer, "qname", qnameOf(m),
			"rcode", dns.RcodeToString[ref.rcode], "reason", ref.reason)
		if err := writeRefusal(w, m, ref.rcode, ref.tsigCode, n.now(), key, tsigErr); err != nil {
			slog.Debug("writing a notify refusal failed", "peer", peer, "err", err)
		}
		return
	}

	reply := new(dns.Msg)
	reply.SetReply(m)
	reply.Authoritative = true
	signIfVerified(reply, m, key, tsigErr)
	if err := w.WriteMsg(reply); err != nil {
		// The peer has gone; there is nothing to act on its behalf for, but
		// the zone may still be behind, so the work goes ahead anyway.
		slog.Debug("writing a notify reply failed", "peer", peer, "err", err)
	}

	// The throttle is on the work, never on the reply above. A NOTIFY it
	// suppresses is deferred rather than dropped — see notifyThrottle — and the
	// first one suppressed in a window is the one that puts a pass on the end
	// of it. Every later one inside that window adds nothing, because the pass
	// is already owed.
	run, deferred := n.admit(z.ID)
	switch {
	case run:
	case deferred:
		slog.Debug("notify deferred to the end of the throttle window",
			"zone", z.Name, "peer", peer)
		// Not ctx, and not the request's goroutine: this waits out the rest of
		// the window before it does anything, and the request is long gone by
		// then. See startWork.
		if !n.startWork(func(workCtx context.Context) {
			if !n.claimDeferred(workCtx, z.ID) {
				return
			}
			n.pass(workCtx, z)
		}) {
			n.dropDeferred(z.ID)
			slog.Debug("notify work skipped: the server is shutting down",
				"zone", z.Name, "peer", peer)
		}
		return
	default:
		slog.Debug("notify throttled: a pass is already owed for this window",
			"zone", z.Name, "peer", peer)
		return
	}

	// Not ctx: dnssrv cancels it the moment ServeNotify returns, and a
	// transfer started under it would be cut off mid-zone. The same
	// reasoning recordAttempt uses for its own write. startWork's context
	// comes from this server's own lifetime instead, so the work outlives
	// the request and nothing else.
	if !n.startWork(func(workCtx context.Context) { n.pass(workCtx, z) }) {
		n.abandonWindow(z.ID)
		slog.Debug("notify work skipped: the server is shutting down",
			"zone", z.Name, "peer", peer)
	}
}

// pass is the work one admitted NOTIFY causes. The zone is read again rather
// than taken from the served snapshot the gate decided on: act compares
// serials and hands the row to a transfer, and both want the row as it is now.
func (n *NotifyServer) pass(ctx context.Context, z *Zone) {
	row, err := n.zs.Zone(ctx, z.ID)
	if err != nil {
		slog.Warn("reading a zone after a notify failed", "zone", z.Name, "err", err)
		return
	}
	n.act(ctx, row)
}

// Run is this server's lifetime, in the shape App gives every other
// long-lived worker it owns: a func(context.Context) in a.bg, started under
// App.wg and waited on by App.Shutdown before it closes the store.
//
// It exists because the work an admitted NOTIFY starts happens in a
// goroutine that, by design, does not inherit the request's context — so
// without this nothing at all held a reference to it, and a NOTIFY admitted
// moments before shutdown could still be probing a primary or installing a
// transferred zone while the store closed underneath it.
//
// When the context ends it does both halves, in this order: stop admitting
// new work, then cancel what is running and wait for it. Cancelling as well
// as waiting is what keeps shutdown bounded — a probe against a primary that
// has stopped answering would otherwise hold it for as long as
// dnssrv.TransferTimeout — and cancelling is safe: a transfer install is one
// transaction that rolls back whole, and the bookkeeping writes downstream
// of it already run on contexts stripped of cancellation.
//
// A NotifyServer whose Run is never called (every test that does not build
// one, and any embedder) is unaffected: nothing ever sets stopped, and work
// runs under a context nothing cancels.
func (n *NotifyServer) Run(ctx context.Context) {
	<-ctx.Done()
	n.mu.Lock()
	n.stopped = true
	n.mu.Unlock()
	n.stopWork()
	n.wg.Wait()
}

// startWork runs f in a goroutine this server's lifetime covers, and reports
// whether it started one at all. False means Run's context has already
// ended: the reply has gone out, but there is nobody left to do the work
// for, and starting a goroutine here after wg.Wait returned would both
// escape the lifetime and race Add against Wait.
//
// The Add is under mu with the stopped check, which is what makes that
// impossible rather than unlikely.
func (n *NotifyServer) startWork(f func(context.Context)) bool {
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return false
	}
	n.wg.Add(1)
	n.mu.Unlock()
	go func() {
		defer n.wg.Done()
		workCtx, cancel := context.WithTimeout(n.work, dnssrv.TransferTimeout)
		defer cancel()
		f(workCtx)
	}()
	return true
}

// act is what a NOTIFY causes, after the reply has already gone out.
//
// The order is RFC 1996's, and the middle step is the one that looks
// skippable and is not.
func (n *NotifyServer) act(ctx context.Context, z store.Zone) {
	// A zone that has never transferred is not a serial question. A secondary
	// created through the API starts at soa_serial 1, so a primary also at 1
	// would make every comparison say "not newer" and leave the zone
	// permanently empty while reporting nothing wrong.
	// n.probes != nil is not defensive padding: WithNotifyProbes is optional
	// and some of this package's own tests omit it, so without this guard act
	// panics on a nil interface.
	//
	// The fallback is to transfer unconditionally, which is the pre-probe
	// behaviour and errs the safe way: an unnecessary AXFR costs bandwidth,
	// a skipped one leaves a secondary serving stale data. It is logged
	// rather than silent, because a NotifyServer running without probes has
	// lost the whole point of the probe and nothing else would say so.
	if n.probes == nil {
		// Not silent. A NotifyServer without probes transfers on every
		// admitted NOTIFY — the whole protection the probe exists to give,
		// gone — and the only other evidence would be a load graph nobody is
		// watching.
		// See NewNotifyServer, which says the same thing once at startup.
		slog.Warn("notify: no SOA probe configured, transferring unconditionally",
			"zone", z.Name)
	}
	if z.RefreshedAt != 0 && n.probes != nil {
		remote, from, err := n.probes.ProbeSerial(ctx, z)
		if err != nil {
			slog.Warn("SOA probe after a notify failed", "zone", z.Name, "err", err)
			return
		}
		if !SerialNewer(remote, z.SOASerial) {
			// The NOTIFY was true and we were already current. RFC 1996 calls
			// it a hint, and this is the hint being correctly declined.
			slog.Debug("notify: already current",
				"zone", z.Name, "serial", z.SOASerial, "primary", from)
			return
		}
		slog.Info("notify: primary is ahead, transferring",
			"zone", z.Name, "ours", z.SOASerial, "theirs", remote, "primary", from)
	}
	if _, err := n.refresher.Refresh(ctx, z.ID); err != nil {
		slog.Warn("transfer after a notify failed", "zone", z.Name, "err", err)
	}
}

// notifyState is what this server remembers between NOTIFYs for one zone: the
// throttle window, whether a NOTIFY was suppressed inside it, and the last
// resolution of a hostname primary.
//
// All of it is process-local and none of it is correctness-bearing: losing it
// costs one extra probe or one extra lookup. Entries are never dropped, for
// the reason Refresher.stateFor gives — they are small, one per zone, and a
// map that forgot an open window while its worker still owned it would let the
// next NOTIFY open a second one.
type notifyState struct {
	// opened is when the current throttle window began; deferred records that
	// a NOTIFY arrived while it was open.
	opened   time.Time
	deferred bool
	// primaries is the last resolution of this zone's primaries, and when it
	// was made; see primariesFor. An empty list is a resolution that failed,
	// remembered exactly like one that succeeded — a name that will not
	// resolve is the case a flood would otherwise turn into one lookup per
	// packet.
	primaries   []netip.AddrPort
	primariesAt time.Time
}

// stateFor returns the zone's state, creating it on first sight. The caller
// holds n.mu.
func (n *NotifyServer) stateFor(zoneID int64) *notifyState {
	st, ok := n.states[zoneID]
	if !ok {
		st = &notifyState{}
		n.states[zoneID] = st
	}
	return st
}

// admit decides what an arriving NOTIFY's work is allowed to do.
//
// run is a pass now, and opens a window. deferred is "this NOTIFY is the one
// that owes the window a pass when it closes" — true for the first NOTIFY
// suppressed in a window and false for every later one, because the window
// already owes exactly one pass and ten suppressed NOTIFYs are worth no more
// than that. Both false is a NOTIFY that cost nothing at all.
//
// The lock is held only across the decision: the probe and the transfer happen
// well outside it, so a slow primary cannot serialise other zones.
func (n *NotifyServer) admit(zoneID int64) (run, deferred bool) {
	now := n.now()
	n.mu.Lock()
	defer n.mu.Unlock()
	st := n.stateFor(zoneID)
	if !st.opened.IsZero() && now.Sub(st.opened) < notifyThrottle {
		if st.deferred {
			return false, false
		}
		st.deferred = true
		return false, true
	}
	st.opened = now
	st.deferred = false
	return true, false
}

// abandonWindow drops the window admit just opened, for the caller that was
// given one and then could not use it: a NOTIFY admitted after Run has already
// stopped the server. Leaving it standing would throttle the zone against a
// pass that never happened.
func (n *NotifyServer) abandonWindow(zoneID int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	st := n.stateFor(zoneID)
	st.opened, st.deferred = time.Time{}, false
}

// dropDeferred forgets the pass a suppressed NOTIFY put on the end of the
// window, for the same reason abandonWindow exists: nothing is going to make
// it. The window itself is left alone — it belongs to whichever pass opened
// it, which is not this caller's to close.
func (n *NotifyServer) dropDeferred(zoneID int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.stateFor(zoneID).deferred = false
}

// claimDeferred waits out the rest of the zone's window and then takes the
// pass it owes, opening the next window as it does.
//
// False means the pass is not this goroutine's to make: ctx ended first — on
// this path the server is shutting down, and a probe nobody is waiting for is
// not worth holding shutdown open for — or an ordinary NOTIFY was admitted in
// the meantime and has already claimed it.
func (n *NotifyServer) claimDeferred(ctx context.Context, zoneID int64) bool {
	if !n.waitOutWindow(ctx, zoneID) {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	st := n.stateFor(zoneID)
	if !st.deferred {
		return false
	}
	st.opened = n.now()
	st.deferred = false
	return true
}

// waitOutWindow blocks until the zone's current throttle window has closed,
// and reports whether it got there rather than being cut short by ctx.
func (n *NotifyServer) waitOutWindow(ctx context.Context, zoneID int64) bool {
	for {
		n.mu.Lock()
		opened := n.stateFor(zoneID).opened
		n.mu.Unlock()
		left := notifyThrottle - n.now().Sub(opened)
		if left <= 0 {
			return true
		}
		if left > deferredProbeTick {
			left = deferredProbeTick
		}
		t := time.NewTimer(left)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
	}
}

// primariesFor is the address list decidePeer matches an arriving NOTIFY's
// source against.
//
// It answers from the column alone whenever it can. A zone whose primaries are
// IP literals — with or without a hostname beside them — needs no lookup at
// all for a source that is one of them, which is the ordinary case and the one
// a mixed list produces. Only a source matching no literal is worth a live
// lookup, and then only once per throttle window: uncached, one trivially
// source-spoofable UDP packet drives one outbound recursive query, which is a
// flood amplifier with a NOTIFY on the near end.
//
// Reusing a resolution for one window costs a primary that has just moved a
// few seconds of refusals, which RFC 1996 §3.6's retransmission covers. A
// failed resolution is remembered too, and for the same reason: a name that
// will not resolve is exactly the one a flood would otherwise re-ask on every
// packet.
func (n *NotifyServer) primariesFor(ctx context.Context, z *Zone, peer netip.Addr) []netip.AddrPort {
	literals, names, err := PrimaryLiterals(z.Primaries)
	if err != nil {
		// Fails closed: an unparseable list matches nobody. The API validates
		// on write, so reaching this means a hand-edited row.
		slog.Debug("a zone's primaries will not parse; refusing every notify for it",
			"zone", z.Name, "err", err)
		return nil
	}
	if !names || notifyPeerAllowed(literals, peer) {
		return literals
	}
	now := n.now()
	n.mu.Lock()
	if st := n.stateFor(z.ID); !st.primariesAt.IsZero() && now.Sub(st.primariesAt) < notifyThrottle {
		cached := st.primaries
		n.mu.Unlock()
		return cached
	}
	n.mu.Unlock()

	aps, err := ParsePrimaries(ctx, n.dnsRes, z.Primaries)
	if err != nil {
		// Debug, not Warn: this runs once per arriving packet, so a Warn here
		// is one log line per packet from any source that can spell the zone's
		// name. The zone's own transfers report the same failure against the
		// attempt that suffered it, which is where it is actionable.
		slog.Debug("resolving a zone's primaries for a notify failed",
			"zone", z.Name, "err", err)
		aps = literals
	}
	n.mu.Lock()
	st := n.stateFor(z.ID)
	st.primaries, st.primariesAt = aps, now
	n.mu.Unlock()
	return aps
}
