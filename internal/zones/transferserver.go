package zones

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// Serving a zone to another nameserver: the gate that decides whether a
// transfer happens at all, and what is said when it does not.
//
// The rules and their order are §9.5.5 of
// docs/superpowers/specs/2026-08-08-zones-design.md, and each is a separate
// rcode on purpose. An operator debugging a secondary that has stopped
// updating reads them as different sentences: NOTAUTH says "you are asking
// the wrong server", REFUSED says "you are asking the right one and it will
// not", SERVFAIL says "ask again, something here is wrong", and a TSIG error
// names which half of the authentication failed.

// TransferServer answers AXFR and IXFR from the served snapshot. It
// implements dnssrv.Transfers, which hands it the raw dns.ResponseWriter
// rather than a pipeline Request: a transfer is a sequence of messages on one
// connection, which the pipeline's one-Response-per-query shape cannot
// express.
type TransferServer struct {
	res *Resolver
	// zs is where a transfer request's outcome is recorded
	// (NoteTransferRequest). Taken at construction because recording is the
	// same object's job, and passing the store later would mean a second
	// constructor.
	zs store.ZoneStore
	// now is the clock three things read: whether a secondary's data has
	// expired, what time this server thinks it is when it tells a peer so in
	// a BADTIME reply, and — §9.5.8 — the timestamp note stamps onto a served
	// or refused transfer and the moment a served transfer's duration is
	// measured from. Injected for the same reason Resolver.now is — crossing
	// a deadline is something a test should be able to drive rather than
	// wait for — and reachable from outside the package, since this
	// package's tests are external.
	now func() time.Time
	// slots is the concurrency cap (§9.5.7): one buffered channel, filled to
	// its capacity while a transfer streams and drained when it stops,
	// however it stops. Acquired non-blockingly (a select against default)
	// rather than waited on, because a peer queued behind a full channel
	// would be a goroutine held by exactly the thing this cap exists to
	// bound.
	//
	// The reason is a library finding, not caution: dns.Server.WriteTimeout
	// is documented at miekg/dns@v1.1.72/server.go:220-221 as "the
	// net.Conn.SetWriteTimeout value for new connections, defaults to
	// 2 * time.Second" — but nothing in the server ever applies it. There is
	// no SetWriteDeadline call anywhere on the serve path, and
	// dns.ResponseWriter exposes no connection to set one on. So a peer that
	// stops reading blocks WriteMsg indefinitely, holding a goroutine and
	// that zone's built RR slice forever. Bounding how many such transfers
	// can exist at once is the only lever the library leaves; a reader who
	// sees WriteTimeout in the library's docs would otherwise assume this
	// cap is redundant with it.
	slots chan struct{}
	// replicaAllow is the config-sync clause beside the ACL: §6 of
	// docs/superpowers/specs/2026-09-11-config-sync-design.md lets a replica
	// registered with this main transfer any primary zone on the strength of
	// the sync key, so registering one does not mean editing every zone's
	// allow_transfer. nil — a main with no replicas, and every other
	// build — means the ACL is the whole of the rule.
	replicaAllow ReplicaAllow
	// hold, when set, runs once a transfer has taken its slot and is about
	// to stream, before anything is written. It exists for tests: this
	// package's tests are external (package zones_test, see now above), so
	// an unexported field alone would be unreachable, and WithTransferHook
	// is exported for the same reason WithTransferServerNow is. Production
	// leaves it nil and nothing runs.
	hold func()
	// mu guards lastNote, the throttle state behind note (§9.5.8). It is
	// held only long enough to check the throttle and record what this call
	// is about to write; the write to zs itself happens after mu is
	// released, so a slow database write cannot serialize concurrent
	// transfers of different zones behind one another.
	mu sync.Mutex
	// lastNote is, per zone ID, the outcome and time of the last write note
	// actually made — not of the last call, which the throttle may have
	// dropped. note compares its own call against this to decide whether the
	// throttle applies or the changed-outcome exception overrides it.
	lastNote map[int64]noteState
}

// DefaultMaxConcurrentTransfers is the concurrency cap (§9.5.7) in effect
// when WithMaxConcurrentTransfers is not given.
const DefaultMaxConcurrentTransfers = 4

// TransferServerOption configures a TransferServer at construction.
type TransferServerOption func(*TransferServer)

// WithTransferServerNow replaces the clock the gate reads. Production leaves
// it alone and gets time.Now.
//
// It deliberately does not move the clock a reply's TSIG signature is stamped
// with: that timestamp is checked by the *peer* against its own clock, so a
// test clock there would make every signed reply BADTIME at the far end.
func WithTransferServerNow(now func() time.Time) TransferServerOption {
	return func(t *TransferServer) { t.now = now }
}

// WithMaxConcurrentTransfers overrides the concurrency cap (§9.5.7). Given a
// zone read almost never (a config reload at most), the size is fixed at
// construction rather than reachable through a reload the way the ACL is.
//
// Anything below 1 becomes 1. This is exported, so n arrives from outside,
// and both smaller values are worse than useless: a negative one panics
// inside make at construction, and a zero one would make slots unbuffered, so
// the non-blocking send below always takes its default branch and every
// transfer this server ever serves is SERVFAIL. One is the smallest number
// that is still a cap.
func WithMaxConcurrentTransfers(n int) TransferServerOption {
	return func(t *TransferServer) {
		if n < 1 {
			n = 1
		}
		t.slots = make(chan struct{}, n)
	}
}

// ReplicaAllow answers whether a transfer of a primary zone, verified under
// keyName from peer, is a registered replica's pull. internal/app answers it
// from the replica registry: the key is the one sync.tsig_key_id designates,
// and the peer is the host of some registered replica's dns_addr.
type ReplicaAllow func(keyName string, peer netip.Addr) bool

// WithReplicaAllow installs that clause. Production passes it on a main;
// without it a transfer is decided by allow_transfer alone.
func WithReplicaAllow(f ReplicaAllow) TransferServerOption {
	return func(t *TransferServer) { t.replicaAllow = f }
}

// WithTransferHook installs the hook a test holds a transfer open with; see
// TransferServer.hold. Production has no caller for this.
func WithTransferHook(fn func()) TransferServerOption {
	return func(t *TransferServer) { t.hold = fn }
}

// NewTransferServer returns the handler dnssrv routes AXFR and IXFR to.
func NewTransferServer(r *Resolver, zs store.ZoneStore, opts ...TransferServerOption) *TransferServer {
	t := &TransferServer{
		res: r, zs: zs, now: time.Now,
		slots:    make(chan struct{}, DefaultMaxConcurrentTransfers),
		lastNote: make(map[int64]noteState),
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// refusal is why a transfer will not happen, and what to say about it.
type refusal struct {
	rcode    int
	tsigCode uint16 // dns.RcodeBadKey/BadSig/BadTime, or 0 for no TSIG error RR
	reason   string // the log line, and last_xfr_error once outcomes are recorded
}

// ServeTransfer answers one transfer query. It returns nothing because the
// interface owns the whole reply, so the work is done by serve, which can
// report why it stopped.
func (t *TransferServer) ServeTransfer(ctx context.Context, w dns.ResponseWriter, q *dns.Msg, key string, tsigErr error) {
	if err := t.serve(ctx, w, q, key, tsigErr); err != nil {
		// Nothing above can act on it: the connection is this handler's, and
		// a peer that has gone away mid-transfer is the ordinary case rather
		// than a fault of this server.
		slog.Warn("zone transfer ended early", "qname", qnameOf(q), "peer", peerAddr(w), "err", err)
	}
}

func (t *TransferServer) serve(ctx context.Context, w dns.ResponseWriter, q *dns.Msg, key string, tsigErr error) error {
	// ctx carries this transfer's own deadline (dnssrv.TransferTimeout),
	// which replaces the pipeline's five seconds. stream checks it between
	// envelopes, which is the only seam there is: dns.ResponseWriter exposes
	// no connection to attach a write deadline to, so a WriteMsg that has
	// already blocked stays blocked (§9.5.7).
	peer := peerAddr(w)
	z, ref := t.decide(q, peer, key, tsigErr)

	// Which transport this arrived on decides how a peer the gate approved is
	// answered, and the gate runs first: a peer outside the ACL is refused
	// rather than told this server's current serial, and an apex this server
	// does not hold is still NOTAUTH rather than a remark about UDP. §9.5.5's
	// table lists the AXFR-over-UDP row among the gate's own, which would
	// answer NOTIMP to a peer that is not allowed to learn anything; it is
	// here instead, so authorisation is decided in one place for every
	// transport. A query the gate approved carries exactly one question.
	_, isUDP := w.RemoteAddr().(*net.UDPAddr)
	if ref == nil && isUDP && q.Question[0].Qtype == dns.TypeAXFR {
		// RFC 5936 §4.2: "AXFR sessions over UDP transport are not defined."
		// It names no rcode, so NOTIMP is ours, and it is the honest one for
		// a transport this server does not implement.
		ref = &refusal{rcode: dns.RcodeNotImplemented, reason: "AXFR over UDP is not defined"}
	}

	if ref != nil {
		return t.refuse(ctx, w, q, z, peer, isUDP, key, tsigErr, ref)
	}

	if isUDP {
		slog.Info("zone transfer answered with a serial",
			"zone", z.Name, "peer", peer, "key", key, "serial", z.SOASerial, "transport", "udp")
		return answerSOAOnly(w, q, z, key, tsigErr)
	}

	// RFC 1995 §2: "If an IXFR query with the same or newer version number
	// than that of the server is received, it is replied to with a single SOA
	// record of the server's current version."
	//
	// §4's permission to answer an IXFR with a whole AXFR — which is what this
	// server does for a client that is genuinely behind, having no journal to
	// compute a delta from — is about a client that needs the data. A
	// secondary that is already current asks again on every refresh timer it
	// has, and answering each of those with the entire zone spends exactly the
	// bandwidth IXFR exists to save, on the peer that needed nothing.
	//
	// A query with no readable SOA of this zone in its authority section is
	// not this case and falls through to the whole zone: §3 is where the
	// client's serial goes, and without one there is nothing to compare.
	if q.Question[0].Qtype == dns.TypeIXFR {
		if serial, ok := clientSerial(q); ok && !SerialNewer(z.SOASerial, serial) {
			slog.Info("zone transfer answered with a serial",
				"zone", z.Name, "peer", peer, "key", key, "serial", z.SOASerial,
				"transport", "tcp", "client_serial", serial)
			// Recorded like any other transfer request that arrived over TCP:
			// a peer asked and was answered, which is what last_xfr_at and
			// last_xfr_peer say (§9.5.8).
			t.note(context.WithoutCancel(ctx), z, peer, "")
			return answerSOAOnly(w, q, z, key, tsigErr)
		}
	}

	// The slot is taken here, after both UDP branches above have already
	// returned, and nowhere earlier. A UDP AXFR is answered NOTIMP and a UDP
	// IXFR with a single SOA, neither streaming anything; taking the slot
	// before this point would hold one of the four for a reply that streams
	// nothing, and worse, would answer an over-capacity UDP AXFR SERVFAIL
	// where NOTIMP belongs (§9.5.5).
	select {
	case t.slots <- struct{}{}:
	default:
		// Not a queue: a peer waiting on a slot is a goroutine held by
		// exactly the thing the cap exists to bound. SERVFAIL and it
		// retries on its own SOA schedule. z is the gate's own approved
		// zone, not a second lookup, so this refusal is recorded against
		// the same row a served transfer of it would be. isUDP is always
		// false here — both UDP branches above have already returned — but
		// it is threaded through rather than hardcoded, so refuse's gate
		// stays the one place that rule is stated (see refuse's comment).
		ref := &refusal{rcode: dns.RcodeServerFailure, reason: "at the concurrent transfer limit"}
		return t.refuse(ctx, w, q, z, peer, isUDP, key, tsigErr, ref)
	}
	// freeSlot returns this transfer's slot, and only ever once: every path
	// below calls it explicitly the moment its wire work is done, and this
	// defer catches a panic — or some path added later — that skips past that
	// point instead. "drained when it stops, however it stops" (slots' own doc
	// comment): a slot already freed must stay freed.
	released := false
	freeSlot := func() {
		if released {
			return
		}
		released = true
		<-t.slots
	}
	defer freeSlot()

	// The zone is rendered here, holding the slot, and not before taking it.
	// zoneAnswer is one dns.NewRR text parse per record (ToRR), and the slice
	// it returns is exactly what a stalled peer goes on holding — the two
	// things §9.5.7's cap is the bound on, in its own words "a goroutine and
	// that zone's built RR slice". Rendered before the slot, neither would be
	// bounded by anything at all: dns.Server caps nothing about concurrent TCP
	// connections, so N peers the ACL admits would render N whole zones at
	// once, and every one over the cap would be built, refused and thrown
	// away. Both UDP branches have returned above, so a UDP IXFR still never
	// renders a zone it does not send.
	//
	// One visible consequence: a zone holding an unrenderable row, asked for
	// while the cap is full, is now refused by the cap rather than by the row.
	// The same SERVFAIL either way — a different reason in the log and in
	// last_xfr_error.
	answer, err := zoneAnswer(z)
	if err != nil {
		// A record that will not render aborts the transfer rather than being
		// skipped. A zone silently missing a record is worse on a secondary
		// than a transfer that visibly failed, because nothing downstream will
		// ever notice. The slot goes back first, for the reason the served
		// path frees it before its own note: refuse writes to the store.
		// isUDP is always false here for the same reason it is at the cap
		// refusal above.
		freeSlot()
		return t.refuse(ctx, w, q, z, peer, isUDP, key, tsigErr, &refusal{rcode: dns.RcodeServerFailure, reason: err.Error()})
	}

	if t.hold != nil {
		// Test-only; see WithTransferHook. Runs after the slot is taken so a
		// test can hold it open and prove a transfer over the cap is
		// refused rather than queued.
		t.hold()
	}

	start := t.now()
	// The deadline reaches a write that has already blocked only through here.
	// stream checks ctx between envelopes, which catches a peer that is merely
	// slow; a peer that has stopped reading blocks *inside* WriteMsg, where
	// there is no deadline the library will ever apply (see slots). Closing the
	// writer closes the connection under it, which is what makes that write
	// fail — the same lever Transferrer.fetch pulls from the client side of a
	// transfer. Without it the cap is a bound on nothing: four such peers hold
	// all four slots until their TCP connections die on their own, which for a
	// peer that is simply not reading may be never.
	stopWatchdog := watchStalledWrite(ctx, w)
	envelopes, err := stream(ctx, w, q, answer, key, tsigErr)
	// Before freeSlot, and unconditionally: past this point the stream is over
	// and the connection is the server's to keep, so a ctx that ends a moment
	// later (dnssrv cancels it the instant ServeTransfer returns) must not
	// close a connection that is no longer being written to.
	stopWatchdog()
	// The slot bounds a stalled WriteMsg (§9.5.7); it has nothing to do with
	// the bookkeeping below, which is a synchronous database write. Freeing
	// it here, the instant the wire work is done, is what keeps a slow
	// NoteTransferRequest from holding a scarce transfer slot hostage for a
	// peer that has already received everything and moved on — which a
	// defer running only at the end of serve would otherwise do.
	freeSlot()
	if err != nil {
		// A stream that errors partway (a peer gone mid-transfer) is not a
		// served transfer: ServeTransfer's own "zone transfer ended early"
		// warning covers it, and nothing here claims otherwise.
		return err
	}

	// dur and the envelope count are only known once the stream has
	// actually finished, which is why the served log and the state write
	// both happen after it rather than before, unlike every refusal above.
	dur := t.now().Sub(start)
	slog.Info("zone transfer served",
		"zone", z.Name, "peer", peer, "key", key, "serial", z.SOASerial,
		"records", len(answer), "envelopes", envelopes, "dur", dur)
	// context.WithoutCancel(ctx): the transfer is over by the time this
	// runs, and dnssrv cancels ctx the moment ServeTransfer returns
	// (server.go's deferred cancel) — sooner still if the peer that just
	// received its last envelope hangs up immediately. Either must not
	// cancel the record of a transfer that already happened.
	//
	// Unconditionally TCP: both UDP branches above (the AXFR-over-UDP
	// refusal and the IXFR single-SOA reply) return before this point, so
	// nothing reaches here except a stream that just went out over the TCP
	// connection this handler owns. See refuse's comment for where that
	// rule is stated for the refusal side.
	t.note(context.WithoutCancel(ctx), z, peer, "")
	return nil
}

// refuse logs and records one refusal — the three places serve can decide
// not to transfer funnel through it: the gate itself, the concurrency cap,
// and an unrenderable zone row, none of which know about either of the
// others. z is the zone the refusal is about, when the gate got far enough
// to know one; several refusals (a malformed question, an apex this server
// holds no zone for) have none, and are logged but not recorded, since there
// is no row to write them to.
//
// isUDP gates whether a known zone's refusal is recorded at all, and this is
// where that gate lives — not inside note, and not repeated at each of
// refuse's three call sites. §9.5.8: a zone transfer is a TCP protocol, so a
// UDP arrival is not a transfer attempt, it is a peer using the wrong
// transport, and recording it would let that peer overwrite last_xfr_peer —
// what an operator's screen calls "who last asked" — with a source address
// that arrived unauthenticated over UDP and is trivially spoofed. TCP-only
// recording is what lets docs/api.md drop that caveat rather than repeat it:
// last_xfr_peer now names a peer that completed a TCP handshake, not merely
// one that sent a UDP datagram claiming an address. It is also what turns
// the UDP-IXFR single-SOA probe (RFC 1995 §2) from a special case — "this
// one refusal shape is logged but never recorded" — into a plain instance of
// the one rule every UDP arrival already follows. note itself stays a plain,
// unconditional write once called: putting the check here, at the point
// that already decides *whether* there is a row to write to (the z != nil
// branch below), means a reader looking for "when does this write happen"
// has one branch to read instead of two.
//
// The log line's first field is "zone" when z is known and "qname"
// otherwise — never a "zone" holding a name this server does not actually
// hold a zone for, and never silently blank for the common case (an
// unrecognised apex) where qnameOf(q) is the only name there is to show. The
// log itself is never gated on transport: it is the only record a UDP
// attempt gets, now that the store write is not (§9.5.8).
func (t *TransferServer) refuse(ctx context.Context, w dns.ResponseWriter, q *dns.Msg, z *Zone, peer netip.Addr, isUDP bool, key string, tsigErr error, ref *refusal) error {
	attrs := []any{"peer", peer, "key", key, "rcode", dns.RcodeToString[ref.rcode], "reason", ref.reason}
	if z != nil {
		if !isUDP {
			t.note(ctx, z, peer, ref.reason)
		}
		attrs = append([]any{"zone", z.Name}, attrs...)
	} else {
		attrs = append([]any{"qname", qnameOf(q)}, attrs...)
	}
	// Warn is for the refusal an operator is hunting: a peer the ACL turned
	// away from a zone this server holds, which is what "ns2 has stopped
	// updating" nearly always is. The other two rows are not that. An apex
	// this server holds no zone for, and a transfer arriving over UDP, are
	// both one unauthenticated packet away for any source on the network — so
	// warning about them is a log line per packet that somebody else decides
	// to write. The NOTIFY gate's twin already reads its refusals out at debug
	// for exactly this reason.
	level := slog.LevelWarn
	if z == nil || isUDP {
		level = slog.LevelDebug
	}
	slog.Log(ctx, level, "zone transfer refused", attrs...)
	return writeRefusal(w, q, ref.rcode, ref.tsigCode, t.now(), key, tsigErr)
}

// noteState is what note remembers about the last write it actually made
// for one zone: when, and under what outcome. The next call compares
// against it to decide whether the throttle applies or the changed-outcome
// exception does.
type noteState struct {
	at     time.Time
	reason string
}

// transferStateThrottle bounds how often a peer can make this server write
// the zone row. Both outcomes are recorded (see refuse and the success path
// in serve) — a refusal is the diagnostic an operator actually needs, since
// "ns2 is not updating" is usually "ns2 is not in allow_transfer" and the
// log is not where an operator looks first — which means an unauthenticated
// peer in a loop would otherwise become an UPDATE loop against sqlite's
// single connection (store.SetMaxOpenConns(1), store/store.go). The log
// line above is never throttled: only the database write below is.
const transferStateThrottle = 10 * time.Second

// note itself has no notion of transport, and every call reaching it writes
// unconditionally, subject only to the throttle below — it does not
// re-derive "was this TCP" and does not need to. That question is already
// settled by the time either of note's two callers reaches it: the served
// path in serve is reachable only after both UDP branches have returned, and
// refuse (§9.5.8) checks isUDP before calling note at all. A reader auditing
// "does a UDP request ever get recorded" wants refuse's comment, not this
// one.
//
// note records one outbound transfer attempt against z's row, subject to
// transferStateThrottle — except that a served↔refused *transition* is
// written whatever the window says. A run of refusals followed by a success
// would otherwise leave the screen saying "refused" for up to ten seconds
// after the thing started working, which is precisely when somebody is
// watching it; that exception is what keeps the throttle honest rather than
// merely cheap.
//
// The override is deliberately narrower than "any changed reason". Two
// different refusal reasons for the same zone and peer are still both
// refusals — not the transition an operator watching the screen is waiting
// for — and a peer that alternates between them (an unsigned request, then
// one signed under a key this server does not hold, say) would otherwise
// turn the override into an unthrottled write on every request, which is
// exactly the UPDATE loop the throttle exists to prevent. That much needs no
// time floor to reason about: two refusals never override each other,
// however close together they land.
//
// It does not extend to served↔refused, and the claim is narrowed to the
// case it covers rather than dropped. A peer inside an *address* ACL can
// alternate a plain AXFR with one signed under a key this server does not
// hold, make every request a transition, and get a write per request. That is
// left alone on purpose: the served half of each pair renders and streams the
// whole zone, so this write rate is bounded by something already far more
// expensive than the write — and the alternative, a floor under the
// transition too, brings back the ten seconds of stale "refused" the override
// exists to remove.
//
// reason is "" for a served transfer and the refusal reason otherwise,
// matching store.ZoneStore.NoteTransferRequest's errText.
func (t *TransferServer) note(ctx context.Context, z *Zone, peer netip.Addr, reason string) {
	now := t.now()
	t.mu.Lock()
	last, seen := t.lastNote[z.ID]
	changed := !seen || (last.reason == "") != (reason == "")
	if !changed && now.Sub(last.at) < transferStateThrottle {
		t.mu.Unlock()
		return
	}
	t.lastNote[z.ID] = noteState{at: now, reason: reason}
	t.mu.Unlock()

	// peer.String(): netip.Addr carries no port at all — peerAddr already
	// discarded it, since an ephemeral source port is a different number
	// every time and identifies nothing.
	if err := t.zs.NoteTransferRequest(ctx, z.ID, now.UnixMilli(), peer.String(), reason); err != nil {
		// The records are what the transfer is for; the bookkeeping is not
		// worth failing it over. Logged and nothing more: the caller has
		// already written (or is about to write) the actual reply.
		//
		// "request", not "served": most of what reaches here is a refusal,
		// and an operator reading "recording a served transfer failed" after
		// refusing a peer would be looking for a transfer that never happened.
		slog.Warn("recording a transfer request failed", "zone", z.Name, "err", err)
	}
}

// decide runs the gate over one query. ref is nil when the transfer is
// approved and names why not otherwise; the order is §9.5.5's, and every
// branch carries the rule it enforces.
//
// z is not "the zone to transfer" — it is whatever zone the query resolved
// to, if any, whether or not ref is also set. serve needs that: §9.5.8
// records a refusal against the zone it was about, not only a success, and
// z is nil only for the refusals that precede a zone lookup (a malformed
// question) or find no zone at all (an apex this server holds nothing for)
// — there being nothing to record those against.
func (t *TransferServer) decide(q *dns.Msg, peer netip.Addr, key string, tsigErr error) (*Zone, *refusal) {
	// A transfer names exactly one zone, in class IN, as one of the two
	// transfer types. Nothing below can be answered without those, so this
	// is FORMERR rather than a refusal about the zone.
	//
	// Not reachable through a listener, and the reason is worth naming here
	// rather than leaving a reader to assume this row is what answers a
	// malformed transfer: miekg answers it first. Its accept function rejects
	// any message whose header QDCOUNT is not 1 with FORMERR before the
	// handler runs (dnssrv.isTransferQuery's comment has the citations), so
	// the wire delivers §9.5.5's first row the rcode it asks for and this is
	// the guard for a direct caller of the exported ServeTransfer.
	if len(q.Question) != 1 {
		return nil, &refusal{rcode: dns.RcodeFormatError, reason: fmt.Sprintf("a transfer query carries one question, this one carries %d", len(q.Question))}
	}
	question := q.Question[0]
	if question.Qclass != dns.ClassINET {
		return nil, &refusal{rcode: dns.RcodeFormatError, reason: fmt.Sprintf("question class %s is not IN", dns.Class(question.Qclass))}
	}
	switch question.Qtype {
	case dns.TypeAXFR, dns.TypeIXFR:
		// IXFR is answered with a full AXFR, which RFC 1995 §2 permits —
		// "the server may choose to transfer the entire zone just as in a
		// normal full zone transfer" — and is what this server does until
		// there is a journal to compute a delta from.
	default:
		return nil, &refusal{rcode: dns.RcodeFormatError, reason: fmt.Sprintf("qtype %s is neither AXFR nor IXFR", dns.Type(question.Qtype))}
	}

	// Apex, not Find: an AXFR names an apex exactly, and walking suffixes
	// would answer a query for sub.e412.in with e412.in — a zone the peer did
	// not ask for and may not be permitted.
	//
	// RFC 5936 §2.2.1 for the rcode: "If a server is not authoritative for
	// the queried zone, the server SHOULD set the value to NotAuth(9)."
	// REFUSED is kept for the ACL denial below, where it means what it says:
	// a policy refusal by a server that does hold the zone.
	z := t.res.Snapshot().Apex(question.Name)
	switch {
	case z == nil:
		return nil, &refusal{rcode: dns.RcodeNotAuth, reason: "no zone at that apex"}
	case !z.Enabled:
		return z, &refusal{rcode: dns.RcodeNotAuth, reason: "zone is disabled"}
	}
	switch strings.ToLower(z.Type) {
	case "primary", "secondary":
	default:
		// internal holds the RFC 6303 built-ins, which are empty by
		// construction; forwarder and stub name somewhere else to ask rather
		// than holding data. None of them has a zone to hand over.
		return z, &refusal{rcode: dns.RcodeNotAuth, reason: fmt.Sprintf("zone type %s holds no zone to transfer", z.Type)}
	}

	// A secondary holds its primary's data on loan. Before its first transfer
	// and past expires_at it cannot vouch for what it holds, and the same
	// judgement queries get (Zone.Serving) applies here: a copy this server
	// cannot vouch for is one it does not hand on.
	if strings.EqualFold(z.Type, "secondary") && !z.Serving(t.now().UnixMilli()) {
		reason := "secondary has expired: it can no longer confirm its data is current"
		if z.RefreshedAt == 0 {
			reason = "secondary has never transferred: it holds nothing to serve"
		}
		return z, &refusal{rcode: dns.RcodeServerFailure, reason: reason}
	}

	if tsigErr != nil {
		switch {
		case errors.Is(tsigErr, dnssrv.ErrTSIGUnsigned):
			// Not a failure. An unsigned request is an ACL outcome: it falls
			// through with no key name, and if every entry is a key: entry it
			// matches nothing and is REFUSED below. Answering NOTAUTH here
			// would claim the peer's key was wrong when the peer offered
			// none.
			key = ""
		case errors.Is(tsigErr, dns.ErrSecret), errors.Is(tsigErr, dns.ErrKeyAlg):
			// A key is the triple (name, algorithm, secret), so a name this
			// server does not hold and a name under an algorithm it was not
			// created with are the same answer: we have no such key.
			return z, &refusal{rcode: dns.RcodeNotAuth, tsigCode: dns.RcodeBadKey, reason: "TSIG key is unknown or names the wrong algorithm"}
		case errors.Is(tsigErr, dns.ErrSig):
			return z, &refusal{rcode: dns.RcodeNotAuth, tsigCode: dns.RcodeBadSig, reason: "TSIG signature did not verify"}
		case errors.Is(tsigErr, dns.ErrTime):
			return z, &refusal{rcode: dns.RcodeNotAuth, tsigCode: dns.RcodeBadTime, reason: "TSIG time is outside the fudge window"}
		default:
			// dnssrv.ErrTSIGUnavailable (nothing verified this message) and a
			// key lookup that failed because the store failed. Neither is the
			// peer's fault and neither is a TSIG error: telling a correctly
			// configured peer its key is bad, because our database was
			// briefly unavailable, sends the operator to the wrong end of the
			// system. So SERVFAIL, carrying no TSIG error record at all.
			return z, &refusal{rcode: dns.RcodeServerFailure, reason: fmt.Sprintf("TSIG could not be checked: %v", tsigErr)}
		}
	}

	entries, err := ParseACL(z.AllowTransfer)
	if err != nil {
		// Fails closed. The API validates on write, so an unparseable value
		// means a hand-edited database; honouring the entries before the bad
		// one would make a typo silently widen the ACL.
		return z, &refusal{rcode: dns.RcodeRefused, reason: fmt.Sprintf("allow_transfer does not parse, refusing every transfer: %v", err)}
	}
	if !ACLAllows(entries, peer, key) && !t.replicaMayPull(z, peer, key, tsigErr) {
		return z, &refusal{rcode: dns.RcodeRefused, reason: "peer matches no allow_transfer entry"}
	}
	return z, nil
}

// replicaMayPull is §6's clause, asked only once allow_transfer has already
// refused: it widens who may transfer and never narrows it, so a zone whose
// ACL admits the peer never reaches this at all.
//
// Primary zones only. A secondary holds another server's data on loan, and
// who may have a copy of that is the operator's allow_transfer to say; the
// remaining types were refused further up, having no zone to hand over.
//
// The signature is the whole of the authentication: key is non-empty only
// when TSIG verified (dnssrv.RequireTSIG returns the name or an error, never
// both), and tsigErr is checked beside it so this stays closed if that ever
// stops being true.
func (t *TransferServer) replicaMayPull(z *Zone, peer netip.Addr, key string, tsigErr error) bool {
	return t.replicaAllow != nil && key != "" && tsigErr == nil &&
		strings.EqualFold(z.Type, "primary") && t.replicaAllow(key, peer)
}

// zoneAnswer builds the whole answer section of a transfer: the SOA, every
// record, and the same SOA again.
//
// RFC 5936 §2.2: "The first message MUST begin with the SOA resource record
// of the zone, and the last message MUST conclude with the same SOA resource
// record."
//
// Records are ordered by name so a zone transfers the same way twice; the map
// they are held in has no order of its own, and a stream that reshuffles
// between transfers is one nobody can diff.
func zoneAnswer(z *Zone) ([]dns.RR, error) {
	soa := z.SOA()
	names := make([]string, 0, len(z.Records))
	for name := range z.Records {
		names = append(names, name)
	}
	slices.Sort(names)

	out := make([]dns.RR, 0, len(z.Records)+2)
	out = append(out, soa)
	for _, name := range names {
		for _, rec := range z.Records[name] {
			if strings.EqualFold(rec.Type, "SOA") {
				// A zone's SOA comes from the zone row, and the stream has
				// exactly two places for one. A stored SOA-typed row has
				// nowhere to go: in the body it would be an intermediate
				// message's SOA, which §2.2 forbids, and at the ends it would
				// contradict the SOA already there. So it is left out, and
				// nothing is lost that the peer does not receive anyway —
				// the zone row's SOA is transferred for that same name.
				//
				// Nothing writes such a row today. A hand-edited database is
				// where it comes from, which is the same provenance
				// ParseACL's fail-closed branch is written for.
				continue
			}
			// ToRR is the renderer the query path uses, so what a secondary
			// receives is what a querier is answered with rather than a
			// second spelling of it.
			rr, err := ToRR(RecordFQDN(z.Name, name), rec)
			if err != nil {
				return nil, fmt.Errorf("zone %s: record %s %s will not render: %w", z.Name, name, rec.Type, err)
			}
			out = append(out, rr)
		}
	}
	return append(out, soa), nil
}

// envelopeTargetBytes is what one message of a transfer is filled to. Well
// under the 65535 a DNS message can hold, and comfortably inside RFC 5936
// §2.2's "sufficient number of RRs to reasonably amortize the per-message
// overhead, up to the largest number that will fit within a DNS message".
const envelopeTargetBytes = 16 << 10

// maxTSIGMACLen is the largest MAC any supported algorithm produces (SHA-512,
// 64 bytes). A stub TSIG carries none yet — WriteMsg fills it in — so an
// envelope that will be signed has to leave room for one.
//
// dnssrv keeps the same number for the same reason and does not export it:
// one constant written twice, each next to the reservation it is part of,
// beats an export that invites a third caller to reserve bytes it does not
// understand.
const maxTSIGMACLen = 64

// stream writes the transfer itself: RFC 5936's sequence of messages, filled
// to envelopeTargetBytes, with the zone's SOA at the two ends and nowhere
// between.
//
// dns.Transfer.Out is deliberately not used, and §9.5.6 records why. It
// builds each message itself and only ever appends to Answer, so there is no
// seam at which §2.2.5's OPT could go on the first one; it also leaves
// Compress unset. Nothing is given up by writing the loop here, because the
// TSIG stream state is not Transfer's: the running MAC and the timers-only
// flag are fields on miekg's response (server.go:755, server.go:824-827),
// reached through the same two ResponseWriter methods Out calls.
//
// answer is never empty — zoneAnswer's pair of SOAs is its floor — so this
// always writes at least one message.
//
// The int it returns is how many envelopes were written, for the served log
// line in serve — a stream that errors partway returns however many made it
// out before the error, which that caller discards, since a partial
// transfer is not the "served" that log line reports.
func stream(ctx context.Context, w dns.ResponseWriter, q *dns.Msg, answer []dns.RR, key string, tsigErr error) (int, error) {
	edns := q.IsEdns0() != nil
	req := requestTSIG(q, key, tsigErr)
	envelopes := batch(answer, envelopeBudget(q, key, req))

	for i, records := range envelopes {
		// §9.5.7's deadline, checked at the one seam there is. It catches a
		// transfer that is merely slow; a peer that has stopped reading
		// blocks inside WriteMsg with no deadline the library will ever
		// apply, which is what the concurrency cap is for instead.
		if err := ctx.Err(); err != nil {
			return i, err
		}

		m := new(dns.Msg)
		m.SetReply(q)
		m.Authoritative = true
		m.Compress = true
		m.Answer = records
		if i == 0 && edns {
			// RFC 5936 §2.2.5: "If the client has supplied an EDNS OPT RR in
			// the AXFR query and if the server supports EDNS as well, it
			// SHOULD include one OPT RR in the first response message and MAY
			// do so in subsequent response messages." One, on the first: the
			// SHOULD is met and every later envelope keeps those eleven bytes
			// for records. This is the AXFR-specific reading of RFC 6891
			// §6.1.1's "compliant responders MUST include an OPT record in
			// their respective responses" — one response here is the whole
			// stream, and §2.2.5 is explicit that the rest may omit it.
			//
			// The size advertised is this server's own receive buffer, not a
			// bound on anything being sent: a transfer is TCP, which has no
			// datagram to fit inside.
			m.SetEdns0(dns.DefaultMsgSize, false)
		}
		if req != nil {
			// RFC 8945 §5.3.1: "The TSIG MUST be included on all DNS messages
			// in the response." A fresh stub per message, because WriteMsg
			// fills this one in and chains its MAC into the next.
			m.Extra = append(m.Extra, dnssrv.ReplyTSIG(m, key, req))
		}
		if err := w.WriteMsg(m); err != nil {
			return i, err
		}
		// After the first, §5.3.1 digests only the timers: "Note that only
		// the timers are included in the second and subsequent messages, not
		// all the TSIG variables." The flag lives on the writer, next to the
		// running MAC it belongs with, so it is set here rather than carried.
		w.TsigTimersOnly(true)
	}
	return len(envelopes), nil
}

// envelopeBudget is how many bytes of records one envelope may carry: the
// target, less everything a message holds besides its answer section.
//
// The header, question and OPT come off exactly. The signature comes off as
// an upper bound, which is what it has to be — WriteMsg grows the stub by a
// MAC of up to maxTSIGMACLen bytes after the size is fixed, so the room must
// already be there.
func envelopeBudget(q *dns.Msg, key string, req *dns.TSIG) int {
	probe := new(dns.Msg)
	probe.SetReply(q)
	if q.IsEdns0() != nil {
		probe.SetEdns0(dns.DefaultMsgSize, false)
	}
	budget := envelopeTargetBytes - probe.Len()
	if req != nil {
		budget -= dns.Len(dnssrv.ReplyTSIG(probe, key, req)) + maxTSIGMACLen
	}
	return budget
}

// batch fills envelopes from rrs, closing one when the next record would take
// it past budget.
//
// Size is accumulated with dns.Len, which measures a record *uncompressed*,
// while the message that carries it is packed with Compress set. The estimate
// is therefore an upper bound and the envelope always fits, at the price of
// slightly under-filled messages — which RFC 5936 §2.2 permits, since it asks
// for a number of RRs that amortizes the per-message overhead rather than for
// a maximum. Packing after each record to measure exactly is quadratic in a
// zone's record count, to recover bytes nobody counts.
//
// A record larger than budget goes out alone in its own envelope rather than
// being dropped: budget is an internal target, not the 65535 a message cannot
// exceed, and silently losing a record is the thing zoneAnswer refuses to do.
func batch(rrs []dns.RR, budget int) [][]dns.RR {
	var (
		out     [][]dns.RR
		current []dns.RR
		size    int
	)
	for _, rr := range rrs {
		n := dns.Len(rr)
		if len(current) > 0 && size+n > budget {
			out, current, size = append(out, current), nil, 0
		}
		current, size = append(current, rr), size+n
	}
	if len(current) > 0 {
		out = append(out, current)
	}
	return out
}

// answerSOAOnly answers a UDP IXFR with this server's current serial and
// nothing else.
//
// RFC 1995 §2: "If the UDP reply does not fit, the query is responded to with
// a single SOA record of the server's current version to inform the client
// that a TCP query should be initiated." This server answers IXFR with the
// whole zone (§9.5.5), so the reply does not fit for any zone worth
// transferring, and the single SOA is the answer to every UDP IXFR rather
// than a size-dependent branch.
func answerSOAOnly(w dns.ResponseWriter, q *dns.Msg, z *Zone, key string, tsigErr error) error {
	m := new(dns.Msg)
	m.SetReply(q)
	m.Authoritative = true
	m.Compress = true
	m.Answer = []dns.RR{z.SOA()}
	if q.IsEdns0() != nil {
		// RFC 6891 §6.1.1: "If an OPT record is present in a received
		// request, compliant responders MUST include an OPT record in their
		// respective responses." dnssrv.serve applies that to every reply the
		// pipeline writes; the transfer branch returns before it, so that an
		// OPT is not stamped onto every envelope of a stream, which leaves
		// the one datagram this handler answers *successfully* to echo it
		// here. A refusal — writeRefusal, including the NOTIMP a UDP AXFR
		// gets — is also one datagram this handler sends, and echoes no OPT;
		// that gap is pre-existing and deliberately deferred, not something
		// this comment claims to cover.
		//
		// 1232 and not the envelope's dns.DefaultMsgSize: this is the size
		// this server advertises on every other UDP reply it sends
		// (dnssrv/server.go), and a peer that reads it and sizes its next
		// query to match should get one answer from this server rather than
		// two. Over TCP the field bounds nothing and the difference does not
		// arise. It bounds nothing being sent here either — what this reply
		// may occupy comes from the *client's* advertised size, which
		// FitUDPReply reads off the request.
		m.SetEdns0(1232, false)
	}

	// The signature's bytes leave the datagram budget before the reply is
	// trimmed, and the stub goes on after — dnssrv.FitUDPReply documents why
	// that order is the whole point. A single SOA fits with room to spare for
	// any ordinary zone; a long apex under a long key name is what the
	// reservation is here for.
	var sig *dns.TSIG
	if req := requestTSIG(q, key, tsigErr); req != nil {
		sig = dnssrv.ReplyTSIG(m, key, req)
	}
	dnssrv.FitUDPReply(m, q, sig)
	if sig != nil {
		m.Extra = append(m.Extra, sig)
	}
	return w.WriteMsg(m)
}

// writeRefusal sends the one message a refused transfer or a refused NOTIFY
// consists of. Both gates end here, which is why it takes the two fields of
// a refusal rather than either refusal type: the rcodes mean different
// things on the two sides (see decidePeer), the reply they produce does not.
//
// now is the caller's own clock — t.now() or n.now() — for the same reason
// errorTSIG takes one: a BADTIME reply's timestamps are what a test drives.
//
// Deliberately not fitted to the client's datagram budget the way
// answerSOAOnly is: a refusal is a header, a question and at most a TSIG
// error record, and the TSIG-error branch returns before any fitting could
// run in any case — truncating a reply whose entire content is "your
// signature was not accepted" is not obviously the right answer, and nothing
// has asked for it.
func writeRefusal(w dns.ResponseWriter, q *dns.Msg, rcode int, tsigCode uint16, now time.Time, key string, tsigErr error) error {
	m := new(dns.Msg)
	m.SetRcode(q, rcode)
	if tsigCode != 0 {
		if rr := errorTSIG(q, tsigCode, now); rr != nil {
			m.Extra = append(m.Extra, rr)
			// Whether this one is signed is decided by which error it
			// carries, and WriteMsg already applies exactly that rule:
			// TsigGenerateWithProvider signs the reply unless the error is
			// BADKEY or BADSIG (tsig.go:196, "Sign unless there is a key or
			// MAC validation error (RFC 8945 5.3.2)"). See errorTSIG for why
			// the three are not alike. Going around it to force one shape on
			// all three is what this used to do, and it was wrong.
			return w.WriteMsg(m)
		}
	}
	// RFC 8945 §5.3: "When a server has generated a response to a signed
	// request, it signs the response using the same algorithm and key." A
	// refusal is still a response, and an unsigned one is a refusal an
	// off-path attacker could have forged — a peer that required
	// authentication would be right to ignore it, and would then never learn
	// why its transfer is failing.
	signIfVerified(m, q, key, tsigErr)
	return w.WriteMsg(m)
}

// errorTSIG builds the TSIG record that names why a signature was not
// accepted, or nil if the request carried no TSIG to answer (unreachable
// through either gate, which only sets a TSIG error code for a verdict that
// required a TSIG to reach).
//
// now is the caller's own clock (t.now() or n.now()) rather than time.Now(),
// for the same reason every other timestamp in this file is injected — a
// test has to be able to drive a BADTIME reply's Other Data without waiting
// on the wall clock.
//
// The three codes are deliberately not treated alike, and the difference is
// the rule rather than an inconsistency to tidy away. It follows from what
// the server was able to establish before it answered.
//
// BADKEY and BADSIG go back **unsigned**. RFC 8945 says so where each error
// is raised, and in the same words: §5.2.1, "This response MUST be unsigned
// as specified in Section 5.3.2", and §5.2.2, "This response MUST be
// unsigned, as specified in Section 5.3.2." (§5.3.2 is where both point, and
// it is the general shape rule for an error return rather than the sentence
// about either code — which is how it came to be miscited here for both.)
// There is nothing to sign with: the key is unknown, or the MAC did not
// verify, so any signature would assert an authenticity the server never
// established. That is the same conclusion dnssrv/server.go reached for the
// ordinary reply path.
//
// BADTIME goes back **signed**, and §5.2.3 is as explicit about that: "A
// response indicating a BADTIME error MUST be signed by the same key as the
// request. It MUST include the client's current time in the Time Signed
// field, the server's current time (an unsigned 48-bit integer) in the Other
// Data field, and 6 in the Other Len field." Here the key and the MAC did
// verify and only the two clocks disagree, so the server both can sign and
// must: the whole point of the reply is to tell a skewed peer what time it is
// here, and an unsigned reply is one an off-path attacker could forge, which
// would make it a way to push a peer's clock around rather than a way to fix
// it. Echoing the client's Time Signed is what lets that peer verify the
// answer at all — its own clock is the one thing it can check against.
//
// The signing itself is WriteMsg's (see writeRefusal). For the unsigned pair
// it also zeroes Time Signed on the way out (tsig.go:191), which is why
// nothing here works to give those two a meaningful one.
func errorTSIG(q *dns.Msg, code uint16, now time.Time) *dns.TSIG {
	req := q.IsTsig()
	if req == nil {
		return nil
	}
	rr := &dns.TSIG{
		Hdr:        dns.RR_Header{Name: req.Hdr.Name, Rrtype: dns.TypeTSIG, Class: dns.ClassANY, Ttl: 0},
		Algorithm:  req.Algorithm,
		TimeSigned: uint64(now.Unix()),
		Fudge:      req.Fudge,
		MACSize:    0,
		MAC:        "",
		Error:      code,
		OrigId:     q.Id,
	}
	if code == dns.RcodeBadTime {
		// The client's own time goes back in Time Signed, so the peer can
		// verify a reply its clock disagrees with; ours goes in Other Data,
		// so it can see by how much.
		rr.TimeSigned = req.TimeSigned
		// The server's own time, as the 48-bit big-endian integer §5.2.3
		// asks for. miekg stores the field hex-encoded with OtherLen
		// counting the bytes rather than the characters (tsig.go:107,
		// `dns:"size-hex:OtherLen"`, packed by zmsg.go:1140-1147).
		var b [6]byte
		secs := uint64(now.Unix())
		for i := range b {
			b[len(b)-1-i] = byte(secs >> (8 * i))
		}
		rr.OtherLen = uint16(len(b))
		rr.OtherData = hex.EncodeToString(b[:])
	}
	return rr
}

// requestTSIG returns the TSIG a reply to q must be signed under, or nil when
// there is nothing to sign with: a request that carried no signature, or one
// whose signature did not verify.
//
// RFC 8945 §5.3: "The server MUST NOT generate a signed response to a request
// if either the key is invalid (e.g., key name or algorithm name are unknown)
// or the MAC fails validation."
func requestTSIG(q *dns.Msg, key string, tsigErr error) *dns.TSIG {
	if tsigErr != nil || key == "" {
		return nil
	}
	return q.IsTsig()
}

// signIfVerified attaches the stub TSIG that makes WriteMsg sign m under the
// key the request verified under, for the paths that write one message and so
// need no room reserved for the signature in advance.
//
// The stub is deliberately empty of a MAC: WriteMsg passes it to
// TsigGenerateWithProvider, which computes one (server.go:753-761). Building
// it is dnssrv.ReplyTSIG's job and not a second copy of it here — the fudge
// default and the algorithm canonicalisation were written out twice with
// nothing keeping the two in step, and the envelope loop would have made a
// third.
func signIfVerified(m *dns.Msg, q *dns.Msg, key string, tsigErr error) {
	if req := requestTSIG(q, key, tsigErr); req != nil {
		m.Extra = append(m.Extra, dnssrv.ReplyTSIG(m, key, req))
	}
}

// peerAddr is the address the request came from, unmapped the way
// dnssrv.Server.serve unmaps it: a dual-stack listener reports a v4 peer as
// ::ffff:10.0.0.5, which matches no IPv4 prefix, and the failure mode is an
// ACL that looks correct and denies everything. (ACLAllows unmaps too — this
// is also what gets logged.)
//
// An address that will not parse leaves the zero Addr, which matches no
// prefix, so an unrecognised transport fails closed.
func peerAddr(w dns.ResponseWriter) netip.Addr {
	var ip netip.Addr
	switch a := w.RemoteAddr().(type) {
	case *net.TCPAddr:
		ip, _ = netip.AddrFromSlice(a.IP)
	case *net.UDPAddr:
		ip, _ = netip.AddrFromSlice(a.IP)
	}
	return ip.Unmap()
}

// clientSerial reads the serial an IXFR query says the client already holds.
//
// RFC 1995 §3: "the authority section carries the SOA record of client's
// version of the zone." The owner name is checked because an SOA naming some
// other zone says nothing about this one, and believing it would answer a
// peer that is behind with a single SOA — a secondary told it is current when
// it is not, which is the one mistake here that does not heal on the next
// timer.
func clientSerial(q *dns.Msg) (uint32, bool) {
	want := dns.CanonicalName(q.Question[0].Name)
	for _, rr := range q.Ns {
		soa, ok := rr.(*dns.SOA)
		if !ok || !strings.EqualFold(dns.CanonicalName(soa.Hdr.Name), want) {
			continue
		}
		return soa.Serial, true
	}
	return 0, false
}

// watchStalledWrite closes w when ctx ends, and returns the function that
// stands the watch down. See its call site for why closing is the only lever
// there is.
//
// Standing the watch down *joins* the close rather than merely signalling it,
// which is the whole point of the shape. serve calls it and then returns, and
// the moment ServeTransfer returns miekg reads the response writer again
// (server.go's serveTCPConn, after the handler) — so a Close still in flight
// at that point is a data race on the writer, which is exactly what CI
// caught. Waiting here means the caller cannot return while a Close is
// pending, and the connection is either already closed or never will be.
//
// context.AfterFunc rather than a goroutine parked in a select, because its
// stop reports which of the two happened: true means the close was cancelled
// before it started and there is nothing to wait for, false means it is
// already running and `stopped` is what says when it has finished. A select
// over the two channels cannot tell those apart when both are ready — it
// picks one at random, and picking ctx.Done() after the stand-down had
// already returned is how the race got out.
//
// The returned function is safe to call once, on the one path that calls it:
// the stream returning, however it returned.
func watchStalledWrite(ctx context.Context, w dns.ResponseWriter) func() {
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(stopped)
		// The peer receives whatever the kernel already accepted and then a
		// closed connection, which is what a transfer that ran out of time
		// looks like from the far end. It retries on its own SOA schedule.
		_ = w.Close()
	})
	return func() {
		if !stop() {
			<-stopped
		}
	}
}

// qnameOf is the queried name for a log line, for a message that may have no
// question at all.
func qnameOf(q *dns.Msg) string {
	if len(q.Question) == 0 {
		return ""
	}
	return q.Question[0].Name
}
