package zones

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// The AXFR client: dnsaur as a secondary.
//
// This is the first code in this project that acts as a DNS *client* against
// another server, and the three things that makes different are worth
// stating before the code:
//
//   - The framing is the library's. AXFR is TCP-only, the answer is a
//     sequence of messages rather than one, and it is delimited by the
//     zone's SOA appearing first and last (RFC 5936 §2.2). dns.Transfer.In
//     implements all of that, including reading the stream to its end, and
//     it is not reimplemented here.
//   - The contents are not trusted. A primary is configured by the operator,
//     not chosen by an attacker, but it is still a remote peer whose bytes
//     end up in this server's authoritative answers. Every RR goes through
//     BuildRecord — the validator POST /zones/{id}/records enforces — and
//     anything outside the zone that was asked for is refused outright.
//   - The install is one transaction. ReplaceRecords writes the deletes, the
//     updates, the adds and the zone row together, so a transfer either
//     lands whole or does not land. Its doc comment already names this
//     caller.
//
// What makes a transferred zone *serve* is the other half of the install and
// is not an afterthought: a secondary answers nothing until refreshed_at is
// set, and stops answering again once expires_at passes (Zone.Serving). Both
// are written here, in the same transaction as the records they describe.

// TSIGKeys is what a transfer needs from the key store: the key its zone
// names by id, so the request can be signed under that key's own name, and
// the same key by name, so the signature can be generated and the primary's
// reply verified. store.TSIGKeyStore satisfies it. Narrowed to two reads so
// a transfer cannot reach the write side.
type TSIGKeys interface {
	Get(ctx context.Context, id int64) (store.TSIGKey, bool, error)
	ByName(ctx context.Context, name string) (store.TSIGKey, bool, error)
}

const (
	// transferDialTimeout bounds reaching a primary. It is short because it
	// is the cost of moving on: a list of primaries exists so one being down
	// is survivable, and a secondary that spends thirty seconds on the first
	// of three has not really got a list.
	transferDialTimeout = 5 * time.Second
	// transferReadTimeout bounds each individual read, not the transfer. A
	// primary that has stopped mid-zone is the case it catches; a large but
	// healthy zone resets it on every envelope.
	transferReadTimeout = 30 * time.Second
	// tsigFudge is the time window, in seconds, the request allows between
	// the two clocks (RFC 8945 §5.2.3). 300 is the usual default and miekg's.
	tsigFudge = 300
	// DefaultMaxTransferRecords bounds how much a primary can make this
	// process allocate before the transfer is abandoned. Set far above any
	// zone this server is likely to hold and well below anything that would
	// threaten it, because its job is to have a limit at all rather than to
	// be the right limit: without one, the size of a transfer is decided
	// entirely by the peer sending it.
	DefaultMaxTransferRecords = 100_000
)

// Transferrer pulls secondary zones from their primaries and installs them.
type Transferrer struct {
	zs   store.ZoneStore
	keys TSIGKeys
	tsig dns.TsigProvider
	// res resolves primaries named by hostname. nil means
	// net.DefaultResolver — see Lookup and ParsePrimaries.
	res Lookup
	// now is the clock the refreshed_at and expires_at stamps come from,
	// injected for the same reason Resolver's is: crossing a zone's expiry
	// is a decision a test has to be able to drive rather than wait for.
	//
	// It is deliberately NOT the clock a TSIG request is signed with. That
	// timestamp is checked against the *primary's* clock inside a fudge
	// window (RFC 8945 §5.2.3), so it has to be real wall time; signing with
	// a test clock would produce a signature a real peer rejects, and a test
	// that moved time would start failing for a reason that has nothing to
	// do with what it is testing.
	now func() time.Time
	// reload republishes the resolver's snapshot after an install. Optional:
	// without it a transfer still lands correctly in the store, it is simply
	// not being answered from yet.
	reload     func(context.Context) error
	maxRecords int
	// notifyWake, when set, is called after every zone this Transferrer
	// installs. See WithNotifyWake.
	notifyWake func()
}

// TransferOption configures a Transferrer at construction.
type TransferOption func(*Transferrer)

// WithTransferNow replaces the clock refreshed_at and expires_at are stamped
// from. See Transferrer.now for why this does not reach TSIG.
func WithTransferNow(now func() time.Time) TransferOption {
	return func(t *Transferrer) { t.now = now }
}

// WithTransferResolver sets the resolver primaries named by hostname are
// looked up through. nil (the default) means net.DefaultResolver; production
// passes dnsaur's own forwarder — see Lookup.
func WithTransferResolver(res Lookup) TransferOption {
	return func(t *Transferrer) { t.res = res }
}

// WithReload gives the Transferrer the callback that republishes the served
// snapshot — Resolver.Reload in production. A transfer that installs a zone
// nothing has reloaded has written the right rows and is still answering
// from the previous ones, so wiring this is what completes the install from
// a client's point of view.
func WithReload(reload func(context.Context) error) TransferOption {
	return func(t *Transferrer) { t.reload = reload }
}

// WithMaxRecords bounds how many RRs a single transfer may carry. Zero or
// negative restores DefaultMaxTransferRecords.
func WithMaxRecords(n int) TransferOption {
	return func(t *Transferrer) {
		if n <= 0 {
			n = DefaultMaxTransferRecords
		}
		t.maxRecords = n
	}
}

// WithNotifyWake gives the Transferrer a callback to invoke after every zone
// it installs — Notifier.Wake in production. It is the cascade: a secondary
// that just pulled a zone may itself be a primary to further secondaries via
// notify_to, and install writes the primary's serial verbatim, so the pass
// that decides who to tell needs nothing cascade-specific — it compares
// serials exactly as it does anywhere else, once woken.
//
// Optional, and deliberately so: without it, nothing calls Wake and the next
// scheduled pass picks up the change instead. See Notifier.Wake — this is
// promptness, never correctness.
func WithNotifyWake(wake func()) TransferOption {
	return func(t *Transferrer) { t.notifyWake = wake }
}

// NewTransferrer returns a Transferrer that installs into zs and signs with
// the keys in keys. keys may be nil, in which case a zone naming a TSIG key
// fails rather than transferring unsigned.
func NewTransferrer(zs store.ZoneStore, keys TSIGKeys, opts ...TransferOption) *Transferrer {
	if !usableKeys(keys) {
		keys = nil
	}
	t := &Transferrer{
		zs:         zs,
		keys:       keys,
		now:        time.Now,
		maxRecords: DefaultMaxTransferRecords,
	}
	// The provider that verifies inbound signatures is the one that generates
	// outbound ones — same keys, same algorithm check, same store read on
	// every message. A second HMAC implementation here could produce
	// signatures this very server would refuse, and neither half's tests
	// would notice.
	if keys != nil {
		t.tsig = dnssrv.NewTSIGProvider(keys)
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// usableKeys reports whether keys is a lookup that can actually be called.
//
// A plain nil is easy. A *typed* nil — an interface holding a nil pointer,
// which is what `var ks *someKeyStore` assigned into a TSIGKeys produces —
// is not: `keys != nil` is true for it, so it would be accepted here and
// then panic inside the first transfer that needed a key, which is a long
// way in both time and stack from the wiring mistake that caused it.
// store.TSIGKeyStore's own implementation is a pointer whose methods
// dereference it, so this is the exact shape at risk.
//
// Reflection rather than a type switch because TSIGKeys is an interface any
// caller may implement, and the set of concrete types behind it is not
// knowable here. Nil-able kinds only; a struct value implementing the
// interface is always callable.
func usableKeys(keys TSIGKeys) bool {
	if keys == nil {
		return false
	}
	switch v := reflect.ValueOf(keys); v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func:
		return !v.IsNil()
	default:
		return true
	}
}

// TransferResult describes one completed transfer.
type TransferResult struct {
	// Primary is the address that answered — not the whole list, the one.
	// A zone that has been failing over to its second primary for a month is
	// invisible unless this is recorded somewhere.
	Primary netip.AddrPort
	// Serial is the primary's, adopted verbatim.
	Serial uint32
	// Records is how many rows the zone holds after the install.
	Records int
	// RefreshedAt and ExpiresAt are the stamps written to the zone row, in
	// unix milliseconds — the two values Zone.Serving reads.
	RefreshedAt int64
	ExpiresAt   int64
	// Unchanged is true when the primary's serial said there was nothing to
	// fetch, so the two stamps above moved and nothing else did. Records is 0
	// for such a result rather than the zone's size: nothing was installed to
	// count. See Sync.
	Unchanged bool
}

// Sync is the attempt the schedule makes: ask the primary what serial it is
// at, and transfer only if that serial has moved.
//
// It is RFC 1034 §4.3.5's refresh timer as the RFC describes it — "check to
// see if the zone has been updated", an SOA query first and a transfer only if
// the answer says so. Transferring unconditionally, which is what the schedule
// used to do, spends a whole zone on the wire, a record diff, a whole-store
// snapshot rebuild and a NOTIFY pass every refresh interval, per secondary,
// for a zone nobody has touched.
//
// A check that succeeded still stamps refreshed_at and expires_at, because
// that is what the check *is*: §4.3.5 restarts the expire timer when the
// primary answers, not only when it answers with something new. A secondary
// that probed successfully and stamped nothing would go dark on schedule with
// its primary reachable the whole time.
//
// **A probe that fails falls through to the transfer rather than failing the
// attempt.** The probe is one UDP exchange and the transfer is TCP, so a
// primary — or a middlebox — that answers one and not the other is an ordinary
// misconfiguration; and of the two ways to be wrong about it, an AXFR nobody
// needed costs bandwidth, while an attempt abandoned over a probe leaves a
// secondary stale and then expired. A primary that is genuinely down fails the
// transfer too, a moment later, and is recorded as the one failure it is.
//
// A zone that has never transferred does not probe at all: there is no
// baseline to compare against, since a secondary created through the API
// starts at the placeholder soa_serial 1 and a primary that happens to be at 1
// would make every comparison say "not newer" and leave the zone permanently
// empty while reporting nothing wrong. NotifyServer.act declines the same
// comparison for the same reason.
//
// The manual path (Refresher.Refresh, behind POST /zones/{id}/refresh) does
// not come through here: a person pressing a button means "fetch it", and a
// button that answered "your serial has not moved" would be indistinguishable
// from one that did nothing.
func (t *Transferrer) Sync(ctx context.Context, z store.Zone) (TransferResult, error) {
	if !strings.EqualFold(z.Type, "secondary") || z.RefreshedAt == 0 {
		return t.Transfer(ctx, z)
	}
	remote, from, err := t.ProbeSerial(ctx, z)
	if err != nil {
		slog.Debug("the SOA probe before a scheduled transfer failed, transferring anyway",
			"zone", z.Name, "err", err)
		return t.Transfer(ctx, z)
	}
	if SerialNewer(remote, z.SOASerial) {
		slog.Debug("the primary is ahead, transferring",
			"zone", z.Name, "ours", z.SOASerial, "theirs", remote, "primary", from)
		return t.Transfer(ctx, z)
	}
	return t.markChecked(ctx, z, from)
}

// markChecked writes the two stamps a successful check earns, for a zone whose
// primary confirmed it is already current.
func (t *Transferrer) markChecked(ctx context.Context, z store.Zone, ap netip.AddrPort) (TransferResult, error) {
	// The write must not be abandoned because whoever asked for the attempt
	// went away: everything cancellable has already happened. install's own
	// context is stripped for the same reason.
	ctx = context.WithoutCancel(ctx)
	// Re-read for the reason install re-reads. This binds every column of the
	// zone row, and a probe takes as long as a primary takes to answer, so
	// writing back the copy the attempt started from would silently undo an
	// edit made while it was in flight.
	current, err := t.zs.Zone(ctx, z.ID)
	if err != nil {
		return TransferResult{}, fmt.Errorf("re-reading zone %q after its SOA probe: %w", z.Name, err)
	}
	if !strings.EqualFold(current.Type, "secondary") {
		return TransferResult{}, fmt.Errorf("zone %q became type %q while its SOA was being probed", current.Name, current.Type)
	}
	nowMs := t.now().UnixMilli()
	current.RefreshedAt = nowMs
	// The same arithmetic install uses, from the same field, so "checked" and
	// "transferred" cannot drift into two different expiries.
	current.ExpiresAt = nowMs + int64(current.SOAExpire)*1000
	// modified_at is deliberately untouched: nothing about the zone's contents
	// changed, and moving it would make every secondary look edited on every
	// refresh — the same rule install applies through contentChanged.
	if err := t.zs.UpdateZone(ctx, current); err != nil {
		return TransferResult{}, fmt.Errorf("stamping zone %q after its SOA probe: %w", current.Name, err)
	}
	// The stamps are what let the zone answer (Zone.Serving), and the
	// answering path reads a snapshot rather than the row. A zone whose expiry
	// moved in the store and not in the snapshot expires anyway, on time, with
	// its primary answering — which is the failure this whole path exists to
	// prevent, reached through the back door.
	if t.reload != nil {
		if err := t.reload(ctx); err != nil {
			slog.Error("zone reload after an SOA probe failed",
				"zone", current.Name, "primary", ap.String(), "err", err)
		}
	}
	return TransferResult{
		Primary:     ap,
		Serial:      current.SOASerial,
		RefreshedAt: current.RefreshedAt,
		ExpiresAt:   current.ExpiresAt,
		Unchanged:   true,
	}, nil
}

// Transfer fetches z from its primaries and installs what arrives.
//
// The primaries are tried in the order they were written, and *any* failure
// moves to the next one — a connection refused, an rcode, a malformed
// stream, or a zone this server would not accept from a human. That last one
// is deliberate: primaries are meant to be replicas of each other, so one of
// them serving something invalid is a reason to ask another rather than to
// give up, and if they all serve it the error names every attempt. The only
// failure that stops the loop is the install itself, which is this server's
// storage rather than any primary's fault.
//
// A failed transfer changes nothing at all. The zone keeps the records and
// the refreshed_at it already had, which for a zone that has never
// transferred means it keeps answering nothing.
func (t *Transferrer) Transfer(ctx context.Context, z store.Zone) (TransferResult, error) {
	// Only a secondary holds someone else's data. Installing a transfer into
	// a primary would destroy a zone this server owns and is the author of,
	// so this is a hard refusal rather than a no-op.
	if !strings.EqualFold(z.Type, "secondary") {
		return TransferResult{}, fmt.Errorf("zone %q is type %q: only a secondary is transferred from anywhere", z.Name, z.Type)
	}

	// Resolved now rather than at write time, so a primary named by hostname
	// follows its address (see ParsePrimaries).
	primaries, err := ParsePrimaries(ctx, t.res, z.Primaries)
	if err != nil {
		return TransferResult{}, fmt.Errorf("zone %q: %w", z.Name, err)
	}

	key, err := t.zoneKey(ctx, z)
	if err != nil {
		return TransferResult{}, err
	}

	var failures []error
	for _, ap := range primaries {
		// Checked before each attempt rather than only inside the dial, so a
		// cancellation reports itself as one instead of as "every primary
		// failed" with a list of confusing i/o errors.
		if err := ctx.Err(); err != nil {
			return TransferResult{}, fmt.Errorf("zone %q: %w", z.Name, err)
		}
		rrs, err := t.fetch(ctx, z.Name, ap, key)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", ap, err))
			continue
		}
		recs, soa, err := t.build(z, rrs)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", ap, err))
			continue
		}
		res, err := t.install(ctx, z, ap, recs, soa)
		if err != nil {
			return TransferResult{}, fmt.Errorf("zone %q from %s: %w", z.Name, ap, err)
		}
		return res, nil
	}
	if err := ctx.Err(); err != nil {
		return TransferResult{}, fmt.Errorf("zone %q: %w", z.Name, err)
	}
	return TransferResult{}, fmt.Errorf("zone %q: every primary failed: %w", z.Name, errors.Join(failures...))
}

// zoneKey resolves the zone's tsig_key_id to the key to sign with, or nil
// when the zone names none.
func (t *Transferrer) zoneKey(ctx context.Context, z store.Zone) (*store.TSIGKey, error) {
	return zoneTSIGKey(ctx, t.keys, z)
}

// zoneTSIGKey resolves z's tsig_key_id against keys, or returns nil when the
// zone names none. keys may be nil, which a zone naming a key treats as a
// failure rather than as permission to send unsigned.
//
// A zone naming a key that is not there fails before any peer is contacted.
// That state is reachable — the D1 delete guard documents the window that
// produces it — and the two alternatives are both worse: sending unsigned
// would send an unauthenticated request the operator believes is
// authenticated, and a peer that requires TSIG would refuse it anyway,
// reporting a refusal instead of the missing key that caused it.
//
// Package-level rather than a Transferrer method because the stub fetcher
// (stub.go) has the identical rule and reaching the same code is what keeps
// it identical: a second copy would drift on exactly the branch — keys ==
// nil, or a key deleted out from under a zone — that nothing exercises until
// it matters.
func zoneTSIGKey(ctx context.Context, keys TSIGKeys, z store.Zone) (*store.TSIGKey, error) {
	if z.TSIGKeyID == 0 {
		return nil, nil
	}
	if keys == nil {
		return nil, fmt.Errorf("zone %q names tsig key %d but no key store is attached", z.Name, z.TSIGKeyID)
	}
	k, ok, err := keys.Get(ctx, z.TSIGKeyID)
	if err != nil {
		return nil, fmt.Errorf("zone %q: reading tsig key %d: %w", z.Name, z.TSIGKeyID, err)
	}
	if !ok {
		// "requests" rather than "the transfer": this is reached by a stub
		// fetch as well, and a stub makes ordinary queries and never
		// transfers anything. Naming the wrong operation in the one error an
		// operator reads to find a deleted key is a small cost with no
		// upside.
		return nil, fmt.Errorf("zone %q names tsig key %d, which no longer exists: its requests cannot be signed", z.Name, z.TSIGKeyID)
	}
	return &k, nil
}

// signedExchange sends m to ap and returns the reply, signed under key when
// there is one.
//
// It is the one implementation of an *ordinary* (non-AXFR) signed DNS query
// in this package: the SOA probe's and the stub fetcher's two queries. A
// second copy is the bug that only shows against a peer requiring TSIG, and
// the RFC 8945 §5.4 check below is the half a copy is most likely to omit,
// because nothing fails without it until someone is spoofing.
//
// network is dns.Client.Net: "" is miekg's default (UDP), "tcp" is what a
// truncated answer is re-asked over.
//
// The signing timestamp is real wall time, never an injected clock — it is
// checked against the *peer's* clock inside a fudge window (RFC 8945
// §5.2.3), so a test clock would produce a signature a real peer rejects.
// See Transferrer.now.
func signedExchange(ctx context.Context, network string, tsig dns.TsigProvider, ap netip.AddrPort, m *dns.Msg, key *store.TSIGKey) (*dns.Msg, error) {
	c := &dns.Client{Net: network}
	if key != nil {
		// Signed under the key's own name and algorithm, which is what the
		// peer looks the secret up by (RFC 8945 §4.2).
		c.TsigProvider = tsig
		m.SetTsig(dns.CanonicalName(key.Name), dns.CanonicalName(key.Algorithm), tsigFudge, time.Now().Unix())
	}
	reply, _, err := c.ExchangeContext(ctx, m, ap.String())
	if err != nil {
		return nil, err
	}
	// RFC 8945 §5.4: a signed request's response must be signed too.
	// miekg's ExchangeContext verifies a TSIG only when the reply carries
	// one ("if t := m.IsTsig(); t != nil" — client.go:267) — a reply with
	// none skips verification entirely. Without this check, an off-path
	// attacker who spoofs the peer's address and guesses the query ID and
	// ephemeral port could hand back an unsigned answer for the right owner
	// name and have it believed. Mirrors notifysend.go's identical check on
	// its read loop.
	if key != nil && reply.IsTsig() == nil {
		return nil, errors.New("signed request got an unsigned reply")
	}
	return reply, nil
}

// fetch runs one AXFR against one primary and returns the RRs it sent, in
// order, the delimiting SOAs included.
func (t *Transferrer) fetch(ctx context.Context, zoneName string, ap netip.AddrPort, key *store.TSIGKey) ([]dns.RR, error) {
	q := new(dns.Msg)
	q.SetAxfr(dns.Fqdn(zoneName))

	tr := &dns.Transfer{ReadTimeout: transferReadTimeout, WriteTimeout: transferReadTimeout}
	if key != nil {
		tr.TsigProvider = t.tsig
		// Signed under the key's own name and algorithm, which is what the
		// primary looks the secret up by (RFC 8945 §4.2). Real wall time, not
		// t.now — see Transferrer.now.
		q.SetTsig(dns.CanonicalName(key.Name), dns.CanonicalName(key.Algorithm), tsigFudge, time.Now().Unix())
	}

	// The connection is dialled here rather than left to Transfer.In so the
	// context reaches it: In takes none, and a transfer that cannot be
	// cancelled while it is connecting is one a shutdown has to wait out.
	// AXFR is TCP-only (RFC 5936 §2.2), so there is no UDP attempt to fall
	// back from.
	d := net.Dialer{Timeout: transferDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", ap.String())
	if err != nil {
		return nil, err
	}
	tr.Conn = &dns.Conn{Conn: conn}

	env, err := tr.In(q, ap.String())
	if err != nil {
		_ = tr.Close()
		return nil, err
	}

	// In hands the read loop to a goroutine that writes envelopes to an
	// unbuffered channel and only finishes once it has closed that channel.
	// Walking away from the channel therefore strands that goroutine and the
	// connection with it, for the lifetime of the process. So every exit
	// below goes through here: close the connection, which makes the pending
	// read fail, then drain whatever is still queued so the goroutine can
	// run to completion. On the ordinary path the channel is already closed
	// and both are no-ops.
	stop := make(chan struct{})
	defer func() {
		close(stop)
		_ = tr.Close()
		for range env { //nolint:revive // draining, deliberately
		}
	}()
	// The context's reach into the read loop. Closing the connection is the
	// only lever there is — In exposes no cancellation of its own.
	go func() {
		select {
		case <-ctx.Done():
			_ = tr.Close()
		case <-stop:
		}
	}()

	var rrs []dns.RR
	for e := range env {
		if e.Error != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				// The read failed because we closed the socket out from under
				// it. Reporting "use of closed network connection" would
				// describe the mechanism and hide the reason.
				return nil, ctxErr
			}
			return nil, e.Error
		}
		if len(rrs)+len(e.RR) > t.maxRecords {
			return nil, fmt.Errorf("the zone exceeds the %d record limit a transfer will accept", t.maxRecords)
		}
		rrs = append(rrs, e.RR...)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(rrs) == 0 {
		return nil, errors.New("the primary sent no records at all")
	}
	return rrs, nil
}

// ProbeSerial asks z's primaries for the zone's current serial, and returns
// the first answer with the address that gave it.
//
// It is the mechanism RFC 1996 §4.7 leaves when it says a notified secondary
// "should enter the state it would if the zone's refresh timer had expired":
// under RFC 1034 that means asking for the SOA and comparing serials, not
// transferring outright. Without it a primary that edits ten records causes
// ten full zone transfers.
//
// The primaries are tried in the order written and any failure moves to the
// next, exactly as Transfer does and for the same reason — primaries are
// meant to be replicas of each other, so one refusing is a reason to ask
// another. The error names every attempt when they all fail.
//
// It is signed with the zone's key when it names one, through the same
// t.tsig provider fetch signs an AXFR with — not a second implementation. A
// primary that requires TSIG on ordinary queries would otherwise refuse the
// probe and make a perfectly healthy zone look unreachable.
func (t *Transferrer) ProbeSerial(ctx context.Context, z store.Zone) (uint32, netip.AddrPort, error) {
	primaries, err := ParsePrimaries(ctx, t.res, z.Primaries)
	if err != nil {
		return 0, netip.AddrPort{}, fmt.Errorf("zone %q: %w", z.Name, err)
	}
	key, err := t.zoneKey(ctx, z)
	if err != nil {
		return 0, netip.AddrPort{}, err
	}

	var failures []error
	for _, ap := range primaries {
		if err := ctx.Err(); err != nil {
			return 0, netip.AddrPort{}, fmt.Errorf("zone %q: %w", z.Name, err)
		}
		serial, err := t.probeOne(ctx, z.Name, ap, key)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", ap, err))
			continue
		}
		return serial, ap, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, netip.AddrPort{}, fmt.Errorf("zone %q: %w", z.Name, err)
	}
	return 0, netip.AddrPort{}, fmt.Errorf("zone %q: every primary failed: %w",
		z.Name, errors.Join(failures...))
}

// probeOne runs one SOA query against one primary.
func (t *Transferrer) probeOne(ctx context.Context, zoneName string, ap netip.AddrPort, key *store.TSIGKey) (uint32, error) {
	m := new(dns.Msg).SetQuestion(dns.Fqdn(zoneName), dns.TypeSOA)
	// The same provider and the same canonicalisation fetch signs an AXFR with
	// (t.tsig, set from dnssrv.NewTSIGProvider(keys) at construction), and the
	// same §5.4 check on the reply — see signedExchange for why a second
	// implementation here would be a bug that only shows up against a
	// TSIG-requiring primary.
	reply, err := signedExchange(ctx, "", t.tsig, ap, m, key)
	if err != nil {
		return 0, err
	}
	if reply.Rcode != dns.RcodeSuccess {
		return 0, fmt.Errorf("SOA probe answered %s", dns.RcodeToString[reply.Rcode])
	}
	for _, rr := range reply.Answer {
		soa, ok := rr.(*dns.SOA)
		if !ok {
			continue
		}
		// The SOA must name the zone we asked about. build() applies the
		// identical check to a transferred SOA (transfer.go, "the transfer
		// carries the SOA of %q, not of %q") and the reason is stronger
		// here: a probe is a single unsigned UDP exchange for a keyless
		// zone, so a misconfigured multi-tenant primary or an off-path
		// forgery could hand back an unrelated zone's serial. Accepting it
		// would make SerialNewer compare against the wrong number and, when
		// that number happens to be higher, silently decline a transfer
		// that should have happened — leaving a stale zone reporting
		// nothing wrong, which is the exact failure the probe exists to
		// prevent.
		if !strings.EqualFold(dns.CanonicalName(soa.Hdr.Name), dns.CanonicalName(dns.Fqdn(zoneName))) {
			return 0, fmt.Errorf("SOA probe answered with the SOA of %q, not of %q", soa.Hdr.Name, zoneName)
		}
		return soa.Serial, nil
	}
	// A NOERROR with no SOA is a primary that answered the wrong question,
	// which is a failure of this probe rather than of the zone — reported so
	// the next primary is tried instead of the zone being called current.
	return 0, errors.New("SOA probe answered NOERROR with no SOA record")
}

// maxNamedProblems bounds how many per-record refusals one error repeats.
// A primary whose whole zone is unacceptable would otherwise produce an
// error the length of the zone, in a log line. Ten is enough to see the
// pattern; the count that follows says how much was not shown.
const maxNamedProblems = 10

// build converts the RRs received into the rows to store and returns the
// zone's SOA alongside them, or an error naming everything wrong with them.
//
// Every RR is validated by BuildRecord — the same function POST
// /zones/{id}/records runs — against the records accepted before it rather
// than against the zone's current contents, because what arrived is what the
// zone is about to be, and that is what an RRSet TTL or a CNAME sibling has
// to be judged against. Any failure rejects the whole transfer: a zone
// installed minus the records that did not pass is neither what the primary
// holds nor what was there before, and it would be served authoritatively.
func (t *Transferrer) build(z store.Zone, rrs []dns.RR) ([]store.ZoneRecord, *dns.SOA, error) {
	// dns.Transfer.In enforces that the first RR is an SOA before it yields
	// anything (isSOAFirst) and stops at the closing one, so both of these
	// are re-checks rather than the only check. They are here because the
	// contract they express — the SOA delimits the zone, and it is *this*
	// zone's — is what the rest of this function assumes, and a delimiter
	// taken on trust is how a transfer for one zone installs another.
	soa, ok := rrs[0].(*dns.SOA)
	if !ok {
		return nil, nil, fmt.Errorf("the transfer does not begin with an SOA but with %s", dns.Type(rrs[0].Header().Rrtype))
	}
	if !strings.EqualFold(dns.CanonicalName(soa.Hdr.Name), dns.CanonicalName(dns.Fqdn(z.Name))) {
		return nil, nil, fmt.Errorf("the transfer carries the SOA of %q, not of %q", soa.Hdr.Name, z.Name)
	}
	body := rrs[1:]
	if len(rrs) > 1 {
		last, ok := rrs[len(rrs)-1].(*dns.SOA)
		if !ok {
			return nil, nil, errors.New("the transfer does not end with the zone's SOA")
		}
		// RFC 5936 §2.2 asks for "the same SOA resource record" at both ends,
		// and a serial that moved between them is a primary that edited the
		// zone mid-stream — BIND aborts such a transfer rather than serving
		// it. What arrived is then neither version: part of it predates the
		// edit and part of it does not. Installed under the opening serial it
		// would be a zone nobody else holds, and every later comparison —
		// this server's own next probe, and every secondary below it — would
		// read that serial as one it has already seen and never ask again.
		if last.Serial != soa.Serial {
			return nil, nil, fmt.Errorf(
				"the transfer opens at serial %d and closes at serial %d: the zone changed while it was being sent",
				soa.Serial, last.Serial)
		}
		body = rrs[1 : len(rrs)-1]
	}

	// RFC 2308 §5 makes a negative answer's TTL min(SOAMinimum, SOATTL), so a
	// zero here would make every NXDOMAIN this zone hands out uncacheable.
	// The zone-file import refuses the same value for the same reason, and a
	// transfer must not be the door that lets it back in.
	if msg := soaTTLProblem(soa); msg != "" {
		return nil, nil, errors.New(msg)
	}
	// An SOA that expires the instant it lands installs a zone that answers
	// nothing (Zone.Serving) — a transfer that reports success and leaves the
	// secondary silent. Refusing it keeps whatever the zone was already
	// serving, which is strictly more than nothing.
	if soa.Expire == 0 {
		return nil, nil, errors.New("the zone's SOA has an expire of 0: the transfer would expire the moment it landed and the zone would answer nothing (RFC 1034 §4.3.5)")
	}

	apex := dns.CanonicalName(dns.Fqdn(z.Name))
	recs := make([]store.ZoneRecord, 0, len(body))
	// The same records, keyed by the relative name BuildRecord stores them
	// under. BuildRecord judges each arriving record against its *siblings* —
	// RFC 2181 §5.2's one TTL per RRSet, RFC 1034 §3.6.2's lone CNAME — and
	// skips every entry whose name differs, so handing it the whole
	// accumulating slice makes each record cost a scan of the zone so far:
	// quadratic, holding the per-zone transfer lock, with the refresh pass
	// queued behind it. At the 100,000 records a transfer will accept that is
	// 5×10⁹ comparisons to answer a question about, typically, one or two.
	//
	// Indexed here rather than inside BuildRecord because the same function is
	// what validates a single record arriving through the API, where the
	// caller has the zone's rows and no index — and one validator for both
	// paths is what keeps a transfer unable to install what a human could not
	// write (see this file's own header).
	byName := make(map[string][]store.ZoneRecord, len(body))
	// named holds the first maxNamedProblems messages; problems counts all of
	// them. Kept apart so a wholly unacceptable zone costs a counter rather
	// than one string per record.
	var named []string
	problems := 0
	note := func(rr dns.RR, format string, args ...any) {
		problems++
		if len(named) < maxNamedProblems {
			named = append(named, fmt.Sprintf("%s %s: %s",
				rr.Header().Name, dns.Type(rr.Header().Rrtype), fmt.Sprintf(format, args...)))
		}
	}

	for _, rr := range body {
		switch rr.(type) {
		case *dns.OPT, *dns.TSIG:
			// Neither belongs in an answer section, and miekg puts both in
			// Extra, so this is unreachable through Transfer.In. Skipped
			// rather than refused because they are metadata about the message
			// and not content of the zone — a peer that put one here has sent
			// something meaningless, not something dangerous.
			continue
		}
		// RFC 5936 §2.2: an AXFR carries the zone, and a zone is one class.
		// ToRR rebuilds every row as IN, so a CH or HS record accepted here
		// would be silently reclassified into the zone rather than rejected.
		if rr.Header().Class != dns.ClassINET {
			note(rr, "class %s is not IN", dns.Class(rr.Header().Class))
			continue
		}
		// The zone has exactly one SOA and it is the delimiter. Another one
		// inside the stream is either a second zone's or a duplicate, and
		// either way it must not become a record row — the zone's SOA lives
		// on the zones row, and a copy in zone_records would be served beside
		// it with whatever the two disagreed about.
		if _, isSOA := rr.(*dns.SOA); isSOA {
			note(rr, "a second SOA inside the transfer")
			continue
		}
		// Out of bailiwick. Left in, the owner would be normalised to a name
		// relative to *our* apex (RelRecordName leaves a name that is not
		// inside the zone alone), and login.bank.example would be stored as
		// the record "login.bank.example" of this zone and served at
		// login.bank.example.<apex>. That is not what the primary said, and a
		// primary saying it in the first place is a reason to distrust the
		// whole answer.
		if !dns.IsSubDomain(apex, dns.CanonicalName(rr.Header().Name)) {
			note(rr, "the name is outside zone %q", z.Name)
			continue
		}

		name := RelRecordName(rr.Header().Name, z.Name)
		rec, err := BuildRecord(z, RecordWrite{
			Name:  rr.Header().Name,
			Type:  dns.Type(rr.Header().Rrtype).String(),
			TTL:   rr.Header().Ttl,
			RData: RDataOf(rr),
		}, byName[name], 0, false)
		if err != nil {
			note(rr, "%s", err.Error())
			continue
		}
		// Only records that passed accumulate, so a later record is never
		// judged against one that is not going to exist.
		recs = append(recs, rec)
		byName[name] = append(byName[name], rec)
	}

	if problems > 0 {
		suffix := ""
		if problems > len(named) {
			suffix = fmt.Sprintf(" (and %d more)", problems-len(named))
		}
		return nil, nil, fmt.Errorf("the primary sent %d record(s) this server would not accept from anyone: %s%s",
			problems, strings.Join(named, "; "), suffix)
	}

	// A zone's apex has NS records by definition (RFC 1034 §4.2.1), so a
	// transfer carrying none did not carry a zone — BIND and NSD both refuse
	// to load one. That is the conformance reason, and it is the smaller one.
	//
	// The reason this check is load-bearing: an SOA-first-and-last stream
	// with nothing between them is *well-formed on the wire*, so nothing in
	// dns.Transfer.In objects to it, and installing it stamps refreshed_at
	// onto a zone holding no records. Zone.Serving then says that zone is
	// entitled to answer, and a zone entitled to answer and holding nothing
	// gives an AUTHORITATIVE NXDOMAIN carrying its SOA for every name beneath
	// the apex. RFC 8020 makes that a denial of the entire subtree and the
	// SOA tells every resolver how long to cache it. It is the same
	// black hole Zone.Serving exists to prevent, reached through the front
	// door: a misconfigured or hostile primary erases a zone and this server
	// asserts the erasure as fact.
	//
	// Refusing is strictly better than installing, in both starting states. A
	// zone that has never transferred goes on answering nothing, which
	// asserts nothing. A zone that is already serving goes on serving what it
	// has, which is at worst stale — and staleness is what expires_at is for.
	//
	// This is deliberately "an apex NS" rather than "any record at all". A
	// stream of A records with no NS is just as much not-a-zone, and the
	// weaker check would admit it.
	hasApexNS := false
	for _, rec := range recs {
		if rec.Name == apexName && rec.Type == "NS" {
			hasApexNS = true
			break
		}
	}
	if !hasApexNS {
		return nil, nil, fmt.Errorf("the transfer carries no NS record at the apex of %q, so it is not a zone (RFC 1034 §4.2.1); installing it would leave this server denying every name beneath that apex", z.Name)
	}

	return recs, soa, nil
}

// install writes the received zone as one transaction and republishes it.
func (t *Transferrer) install(ctx context.Context, z store.Zone, ap netip.AddrPort, recs []store.ZoneRecord, soa *dns.SOA) (TransferResult, error) {
	// The write half must not be abandoned because whoever asked for the
	// transfer went away — same reasoning as the import's applyZoneFile, and
	// the same transaction underneath. Everything cancellable has already
	// happened; what is left is local.
	ctx = context.WithoutCancel(ctx)

	existing, err := t.zs.Records(ctx, z.ID)
	if err != nil {
		return TransferResult{}, fmt.Errorf("reading the zone's current records: %w", err)
	}
	// Diffed rather than deleted-and-re-added wholesale, through the same
	// function the import diff uses. A refresh that found no change then
	// issues no record statements at all, instead of churning every row id in
	// the zone on a schedule.
	diff := DiffRecords(z.Name, existing, recs)

	// The row this transfer writes back is the operator's *current* row with
	// the new SOA and stamps applied on top, not the copy the transfer
	// started from.
	//
	// ReplaceRecords binds every column (updateZoneArgs), and a transfer takes
	// as long as the zone takes to arrive. Writing back the copy it started
	// from would therefore silently undo any edit made while it was in
	// flight: a secondary disabled, or repointed at a different primary, would
	// come back on its own. Re-reading here narrows that window from the
	// duration of the transfer to the gap between this read and the commit
	// below — the same window the record diff above already has.
	current, err := t.zs.Zone(ctx, z.ID)
	if err != nil {
		return TransferResult{}, fmt.Errorf("re-reading the zone before installing it: %w", err)
	}
	// Deleted or retyped while this transfer was running. Installing anyway
	// would either write rows for a zone that no longer exists or, worse,
	// overwrite a zone this server has since been made the author of — the
	// destruction Transfer's own type check exists to prevent, reached
	// through a window that check was too early to see.
	if !strings.EqualFold(current.Type, "secondary") {
		return TransferResult{}, fmt.Errorf("zone %q became type %q while the transfer was running", current.Name, current.Type)
	}
	z = current

	// The primary's SOA becomes the zone's, serial included and unbumped. A
	// secondary does not author this zone: the serial is the primary's name
	// for a particular version of its contents, and a secondary that
	// invented its own would advertise a version no one else has and would
	// compare its next transfer against a number of its own making. This is
	// the one place the import path and the transfer path deliberately
	// differ — an import takes max(file, current) + 1 because the file's
	// serial is a human's and can go backwards.
	//
	// ReplaceRecords writes the zone row from the struct it is given
	// (updateZoneArgs binds soa_serial directly), so "verbatim" is what
	// happens, not what is hoped for.
	now := t.now()
	nowMs := now.UnixMilli()
	// Kept so the SOA fields can be compared before and after — see
	// contentChanged below.
	before := z
	z.SOANS = strings.TrimSuffix(soa.Ns, ".")
	z.SOAMbox = strings.TrimSuffix(soa.Mbox, ".")
	z.SOASerial = soa.Serial
	z.SOARefresh = soa.Refresh
	z.SOARetry = soa.Retry
	z.SOAExpire = soa.Expire
	z.SOAMinimum = soa.Minttl
	z.SOATTL = soa.Hdr.Ttl
	// The two stamps that let the zone answer. They are written here, in the
	// same transaction as the records, because they are a statement about
	// exactly these records: a refreshed_at that committed without them, or
	// records that committed without it, is a zone either serving data it
	// cannot vouch for or holding data it will not serve.
	z.RefreshedAt = nowMs
	z.ExpiresAt = nowMs + int64(soa.Expire)*1000
	// modified_at only when something actually changed. refreshed_at is the
	// "we checked" stamp and moves every cycle by design; modified_at is the
	// "contents changed" one, and moving it in step would make every
	// secondary look edited on every refresh — undoing, in the one column an
	// operator reads to find out what happened, the no-op the diff above
	// works to achieve.
	if contentChanged(before, z, diff) {
		z.ModifiedAt = nowMs
	}

	if err := t.zs.ReplaceRecords(ctx, z, diff.DeleteIDs(), diff.Updates(), diff.Add); err != nil {
		return TransferResult{}, err
	}

	// The install committed, so a reload failure does not undo it and must
	// not be reported as a failed transfer — same reasoning as the API's
	// reloadZones. What it costs is that the zone keeps answering from the
	// previous snapshot until something reloads it.
	if t.reload != nil {
		if err := t.reload(ctx); err != nil {
			slog.Error("zone reload after transfer failed", "zone", z.Name, "primary", ap.String(), "err", err)
		}
	}

	// The cascade: this zone's serial just moved, verbatim from its primary,
	// so anything this server in turn notifies may now be behind. See
	// WithNotifyWake.
	if t.notifyWake != nil {
		t.notifyWake()
	}

	return TransferResult{
		Primary:     ap,
		Serial:      soa.Serial,
		Records:     len(recs),
		RefreshedAt: z.RefreshedAt,
		ExpiresAt:   z.ExpiresAt,
	}, nil
}

// contentChanged reports whether this transfer changed anything about the
// zone beyond the fact that it happened — any record added, altered or
// removed, or any SOA field the primary now spells differently.
//
// refreshed_at and expires_at are deliberately not compared: they move on
// every successful transfer, so including them would make this always true
// and the check pointless.
func contentChanged(before, after store.Zone, diff RecordDiff) bool {
	if len(diff.Add) > 0 || len(diff.Change) > 0 || len(diff.Delete) > 0 {
		return true
	}
	return before.SOASerial != after.SOASerial ||
		before.SOANS != after.SOANS ||
		before.SOAMbox != after.SOAMbox ||
		before.SOARefresh != after.SOARefresh ||
		before.SOARetry != after.SOARetry ||
		before.SOAExpire != after.SOAExpire ||
		before.SOAMinimum != after.SOAMinimum ||
		before.SOATTL != after.SOATTL
}
