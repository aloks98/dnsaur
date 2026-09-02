package zones

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
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
// decidePeer's comment.
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
// It is the same shape as transferStateThrottle and deliberately not the same
// constant: that one throttles a database write, this one throttles a network
// round trip to somebody else's server.
const notifyThrottle = 5 * time.Second

// Refreshes is the half of *Refresher this needs, taken as an interface so a
// test can drive the gate without a Transferrer or a live primary.
type Refreshes interface {
	Refresh(ctx context.Context, zoneID int64) (TransferResult, error)
}

// Probes is the half of *Transferrer NotifyServer needs to decide whether a
// NOTIFY's zone has actually moved before transferring it. Declared here and
// used from Task 7 onward: a NotifyServer built without one (the ordinary
// construction until then) skips the probe and always refreshes, which is
// what this file's own tests exercise.
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
	// probes is nil until Task 7 wires WithNotifyProbes in production; see
	// Probes' doc comment.
	probes Probes
	// dnsRes is the resolver ParsePrimaries uses to look up a hostname
	// primary. nil means net.DefaultResolver — see ParsePrimaries' own
	// comment on why that default belongs to the caller, not to a lookup
	// baked into the gate.
	dnsRes *net.Resolver
	// now is the clock admit and a BADTIME reply's timestamp read. Injected
	// for the same reason TransferServer.now is: a test should be able to
	// drive the throttle rather than wait for it.
	now func() time.Time

	// mu guards lastAct, the throttle state behind admit. Held only across
	// the check — the probe and the transfer happen well outside it, so a
	// slow primary cannot serialise other zones' NOTIFYs.
	mu      sync.Mutex
	lastAct map[int64]time.Time
}

// NotifyServerOption configures a NotifyServer at construction.
type NotifyServerOption func(*NotifyServer)

// WithNotifyServerNow replaces the clock the gate reads. Production leaves it
// alone and gets time.Now.
func WithNotifyServerNow(now func() time.Time) NotifyServerOption {
	return func(n *NotifyServer) { n.now = now }
}

// WithNotifyProbes attaches the SOA probe a NOTIFY's zone is checked against
// before it is transferred (Task 7). Without it, every admitted NOTIFY
// transfers unconditionally — this file's own tests build no probe and
// assert exactly that.
func WithNotifyProbes(p Probes) NotifyServerOption {
	return func(n *NotifyServer) { n.probes = p }
}

// WithNotifyServerResolver replaces the resolver ParsePrimaries uses to
// resolve a hostname primary. Production leaves it nil, which ParsePrimaries
// reads as net.DefaultResolver (see ParsePrimaries' own doc comment). Tests
// use it to prove a lookup was, or was not, made — in particular that
// ServeNotify never resolves a zone's primaries until decideZone has already
// accepted the NOTIFY — without touching a real network.
func WithNotifyServerResolver(res *net.Resolver) NotifyServerOption {
	return func(n *NotifyServer) { n.dnsRes = res }
}

// NewNotifyServer returns the handler dnssrv routes NOTIFY to.
func NewNotifyServer(r *Resolver, zs store.ZoneStore, rf Refreshes, opts ...NotifyServerOption) *NotifyServer {
	n := &NotifyServer{
		res: r, zs: zs, refresher: rf, now: time.Now,
		lastAct: make(map[int64]time.Time),
	}
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
	// Only a secondary is told by anyone. A primary owns its data, so a
	// NOTIFY for one is either a misconfiguration or an attempt to make this
	// server pull its own zone from somewhere else.
	if !strings.EqualFold(z.Type, "secondary") {
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
func (n *NotifyServer) decidePeer(z *Zone, peer netip.Addr, primaries []netip.AddrPort, key string, tsigErr error) *notifyRefusal {
	// RFC 1996 §3.10 says to ignore a NOTIFY from a host that is not a known
	// master. dnsaur answers REFUSED instead — a deliberate divergence,
	// recorded in §9.9. Silence is indistinguishable from a firewall drop,
	// the usual cause here is a primaries list one address wrong, and a
	// NOTIFY and its refusal are both ~50 bytes, so there is no
	// amplification and 1:1 reflection is not a useful attack primitive.
	if !notifyPeerAllowed(primaries, peer) {
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
	// it. A zone whose primaries are IP literals short-circuits with no
	// lookup at all (primaries.go), which is the common case; this ordering
	// makes the hostname case cost nothing for a NOTIFY that was going to be
	// refused anyway.
	z, ref := n.decideZone(m)
	if ref == nil {
		var primaries []netip.AddrPort
		if aps, err := ParsePrimaries(ctx, n.dnsRes, z.Primaries); err == nil {
			primaries = aps
		} else {
			slog.Warn("resolving a zone's primaries for a notify failed",
				"zone", z.Name, "err", err)
		}
		ref = n.decidePeer(z, peer, primaries, key, tsigErr)
	}
	if ref != nil {
		slog.Debug("notify refused", "peer", peer, "qname", qnameOf(m),
			"rcode", dns.RcodeToString[ref.rcode], "reason", ref.reason)
		if err := n.writeRefusal(w, m, ref, key, tsigErr); err != nil {
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

	// The throttle is on the work, never on the reply above.
	if !n.admit(z.ID) {
		slog.Debug("notify throttled", "zone", z.Name, "peer", peer)
		return
	}

	// A background context, not ctx: dnssrv cancels ctx the moment
	// ServeNotify returns, and a transfer started under it would be cut off
	// mid-zone. The same reasoning recordAttempt uses for its own write.
	go func() {
		workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dnssrv.TransferTimeout)
		defer cancel()
		row, err := n.zs.Zone(workCtx, z.ID)
		if err != nil {
			slog.Warn("reading a zone after a notify failed", "zone", z.Name, "err", err)
			return
		}
		n.act(workCtx, row)
	}()
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
	// n.probes != nil is not defensive padding: WithNotifyProbes is optional,
	// every Task 6 test omits it, and production does not wire it until
	// Task 10 — so without this guard act panics on a nil interface.
	//
	// The fallback is to transfer unconditionally, which is the pre-probe
	// behaviour and errs the safe way: an unnecessary AXFR costs bandwidth,
	// a skipped one leaves a secondary serving stale data. It is logged
	// rather than silent, because a NotifyServer running without probes has
	// lost the whole point of this task and nothing else would say so.
	if n.probes == nil {
		// Not silent. A NotifyServer without probes transfers on every
		// admitted NOTIFY — this task's whole protection, gone — and the
		// only other evidence would be a load graph nobody is watching.
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

// admit reports whether this zone may do the work now, and records that it
// did. The lock is held only across the check — the probe and the transfer
// happen well outside it, so a slow primary cannot serialise other zones.
func (n *NotifyServer) admit(zoneID int64) bool {
	now := n.now()
	n.mu.Lock()
	defer n.mu.Unlock()
	if last, ok := n.lastAct[zoneID]; ok && now.Sub(last) < notifyThrottle {
		return false
	}
	n.lastAct[zoneID] = now
	return true
}

// writeRefusal sends the one message a refused NOTIFY consists of.
//
// TransferServer.writeRefusal's logic with this type's rcodes: reuses
// errorTSIG and signIfVerified from transferserver.go rather than a second
// copy, since the BADKEY/BADSIG-unsigned and BADTIME-signed rule is
// identical and two implementations of it would drift.
func (n *NotifyServer) writeRefusal(w dns.ResponseWriter, q *dns.Msg, ref *notifyRefusal, key string, tsigErr error) error {
	m := new(dns.Msg)
	m.SetRcode(q, ref.rcode)
	if ref.tsigCode != 0 {
		if rr := errorTSIG(q, ref.tsigCode, n.now()); rr != nil {
			m.Extra = append(m.Extra, rr)
			// See TransferServer.writeRefusal's comment on the same line:
			// whether this is signed is WriteMsg's own rule
			// (TsigGenerateWithProvider — sign unless BADKEY/BADSIG).
			return w.WriteMsg(m)
		}
	}
	signIfVerified(m, q, key, tsigErr)
	return w.WriteMsg(m)
}
