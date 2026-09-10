package zones

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// The real Sender: a NOTIFY put on the wire over UDP, and RFC 1996 §3.6's
// three stop conditions read off what comes back.
//
// A round is retried "until either too many copies have been sent (a
// 'timeout'), an ICMP message indicating that the port is unreachable, or
// until a NOTIFY response is received from the slave with a matching query
// ID, QNAME, IP source address, and UDP source port number." §4.8: "When a
// master server receives a NOTIFY response, it deletes this query from the
// retry queue" — no mention of the rcode.
//
// The three conditions map onto what one call to Send can report:
//
//   - A response, of any rcode, is (nil, ErrNotifyDelivered wrapping the
//     rcode) or nil outright for NOERROR. Either way the round is over —
//     Notifier.maybeSend is what decides that a REFUSED is still worth
//     showing the operator.
//   - ICMP port unreachable is Go's ECONNREFUSED on the read from a
//     connected UDP socket — positive evidence nothing is listening, wrapped
//     as ErrNotifyUnreachable so the pass stops the round on it too.
//   - Anything else — no answer before the deadline, a dial failure, a
//     malformed reply, an ID or QNAME that does not match what was sent — is
//     an ordinary error the pass treats as one attempt of the timeout budget.

// ErrNotifyDelivered marks a response that arrived and ended the round, but
// was not NOERROR. RFC 1996 §3.6 stops retransmitting on *any* response and
// §4.8 deletes the query from the retry queue without inspecting the rcode —
// a REFUSED will not become a NOERROR on retransmission. It is still an error
// so the rcode reaches last_error and the screen: ending the round and being
// satisfied with the outcome are different things.
var ErrNotifyDelivered = errors.New("notify answered")

// ErrNotifyUnreachable marks RFC 1996 §3.6's second stop condition, "an ICMP
// message indicating that the port is unreachable", which Go surfaces as
// ECONNREFUSED on the read from a connected UDP socket. It is positive
// evidence that nothing is listening rather than an absence of evidence, so
// the pass stops the round on it instead of spending the rest of the budget
// re-establishing the same fact.
var ErrNotifyUnreachable = errors.New("notify target unreachable")

// defaultNotifySendTimeout bounds how long one Send waits for a response
// before it is reported as an ordinary (retryable) error. It is short on
// purpose: silence here is not a verdict on the target, it is one attempt
// inside the round Notifier already backs off and budgets (backoffFor,
// MaxNotifyAttempts) — and the secondary's own refresh timer is the
// correctness backstop regardless of how this round ends (notifier.go).
const defaultNotifySendTimeout = 2 * time.Second

// testNotifySendTimeout is NewUDPSenderForTest's timeout — short enough that
// the silent-target tests don't sit for the production default, long enough
// that a responder doing real work inside the window (TestSendSignsWithTSIGWhenAKeyIsGiven's
// SQLite lookup plus HMAC verify, under -race) reliably finishes before the
// deadline rather than racing it. Only the tests against a target that never
// answers pay this in full, and they pay it once each.
const testNotifySendTimeout = time.Second

// udpSender is the production Sender: a NOTIFY dialed, written, and answered
// over UDP.
type udpSender struct {
	// res resolves a target named by hostname. nil means net.DefaultResolver
	// — see Lookup, and see NotifyTarget.Host and ParseNotifyTo's package
	// comment for why resolution happens here, at send time, rather than at
	// parse time.
	res Lookup
	// timeout bounds dialing and one write-then-read round trip.
	timeout time.Duration
}

// newUDPSender is the production constructor.
func newUDPSender(res Lookup) *udpSender {
	return &udpSender{res: res, timeout: defaultNotifySendTimeout}
}

// NewUDPSenderForTest exists for internal/zones_test, the external test
// package this package's own tests live in: an unexported type's
// constructor is unreachable from there, so this is the exported seam a test
// calls instead. Same body as newUDPSender, with a timeout short enough that
// a test asserting a timeout does not have to wait for the production one.
func NewUDPSenderForTest() *udpSender {
	return &udpSender{timeout: testNotifySendTimeout}
}

// NewUDPSenderForTestWithResolver is NewUDPSenderForTest with a caller-
// supplied resolver, for the same reason WithTransferResolver exists on
// Transferrer: a test that needs to control what a hostname target resolves
// to — e.g. proving Send's fan-out tries every address a name resolves to,
// not just the first — cannot do that against net.DefaultResolver, which
// depends on whatever the machine running the test happens to answer.
func NewUDPSenderForTestWithResolver(res Lookup) *udpSender {
	return &udpSender{res: res, timeout: testNotifySendTimeout}
}

// Send dials target, sends a NOTIFY for zone, and applies RFC 1996 §3.6's
// three stop conditions to whatever comes back — see the package comment
// above for the mapping. key signs the request when it names one, through
// the same dnssrv.NewTSIGProvider mechanism Transferrer.fetch and
// Transferrer.probeOne sign with (transfer.go): a NOTIFY signed by a second
// HMAC implementation is a bug that only shows against a TSIG-requiring
// peer.
func (s *udpSender) Send(ctx context.Context, target NotifyTarget, zone string, key *store.TSIGKey) error {
	resolveCtx, cancel := context.WithTimeout(ctx, s.timeout)
	addrs, err := s.resolve(resolveCtx, target)
	cancel()
	if err != nil {
		return err
	}
	if len(addrs) == 0 {
		// Defensive, and knowingly untested: resolve's own contract (see its
		// doc comment) is to never return an empty slice without an error.
		// This is here so that, if that invariant ever slips, Send reports a
		// failure rather than implicitly reading "sent to nothing" as
		// "delivered".
		return fmt.Errorf("notify target %q resolved to no addresses to send to", target.Host)
	}

	// A hostname target may resolve to more than one address — Host:Port as
	// written is the row identity (NotifyTarget.Addr), not any one resolved
	// address, so a multi-homed secondary is still one target and one round.
	// Tried in the order the resolver returned them, the same shape
	// Transferrer.Transfer tries a primary list in and for the same reason:
	// any failure moves to the next address. Only a genuine response — nil or
	// ErrNotifyDelivered, whatever its rcode — stops early, because that is
	// the positive evidence RFC 1996 §3.6 ends a round on; a timeout or
	// ECONNREFUSED on one address says nothing about whether another address
	// of the same target is listening.
	//
	// Each address gets its own timeout derived from s.timeout, not a share
	// of one deadline installed before the loop — mirroring Transferrer.fetch,
	// which gives each primary its own dial and read timeouts. A single
	// shared deadline would let a first address that silently drops packets
	// consume the whole budget, leaving the second address's attempt an
	// already-expired deadline instead of a real attempt.
	var failures []error
	unreachable := 0
	for _, addr := range addrs {
		// Checked before each attempt, exactly as Transfer's own primary loop
		// does, so a cancellation mid-fan-out reports as one instead of as a
		// confusing per-address I/O error.
		if err := ctx.Err(); err != nil {
			return err
		}
		addrCtx, cancel := context.WithTimeout(ctx, s.timeout)
		err := s.sendTo(addrCtx, addr, zone, key)
		cancel()
		if err == nil || errors.Is(err, ErrNotifyDelivered) {
			return err
		}
		if errors.Is(err, ErrNotifyUnreachable) {
			unreachable++
		}
		failures = append(failures, fmt.Errorf("%s: %w", addr, err))
	}

	// Every address's failure is kept for the message, mirroring how Transfer
	// reports "every primary failed". Whether the *whole* Send call
	// classifies as ErrNotifyUnreachable is decided here, explicitly, from
	// the complete set — not by wrapping every address's own classification
	// and letting a caller's errors.Is traverse into whichever one happens to
	// match. errors.Join's Unwrap() []error would do exactly that: address 1
	// timing out while address 2 answers ECONNREFUSED would make
	// errors.Is(joined, ErrNotifyUnreachable) true even though the address
	// that mattered was merely slow, ending the round after a single attempt.
	// notifySendErrors carries the joined text without that traversal, so
	// only the explicit wrap below can make the sentinel match.
	detail := errors.Join(failures...)
	if unreachable == len(failures) {
		// Every address gave positive evidence nothing is listening there —
		// the round really is over, not merely one more timeout.
		return fmt.Errorf("%w: %s", ErrNotifyUnreachable, detail)
	}
	return &notifySendErrors{detail: detail}
}

// notifySendErrors is every address's failure from one Send call, joined for
// the error text but deliberately opaque to errors.Is/As beyond that: Send
// itself decides, once, whether the whole attempt classifies as
// ErrNotifyUnreachable (see the comment above), and this type carrying no
// Unwrap is what stops a caller's errors.Is re-deciding that by traversing
// into one address's individual sentinel while a different address's outcome
// was merely a timeout.
type notifySendErrors struct{ detail error }

func (e *notifySendErrors) Error() string { return e.detail.Error() }

// sendTo runs one NOTIFY exchange against one already-resolved address,
// reading until ctx's deadline rather than stopping at the first packet.
func (s *udpSender) sendTo(ctx context.Context, addr string, zone string, key *store.TSIGKey) error {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	co := &dns.Conn{Conn: conn}
	m := new(dns.Msg).SetNotify(dns.Fqdn(zone))
	if key != nil {
		// The same provider fetch and probeOne sign under, over a lookup
		// scoped to this one key — see singleTSIGKey.
		co.TsigProvider = dnssrv.NewTSIGProvider(singleTSIGKey{*key})
		m.SetTsig(dns.CanonicalName(key.Name), dns.CanonicalName(key.Algorithm), tsigFudge, time.Now().Unix())
	}

	if err := co.WriteMsg(m); err != nil {
		return classifyNotifyErr(err)
	}

	// Reading exactly once would let a single non-matching datagram end the
	// attempt before the real response has a chance to arrive — an off-path
	// sender who reaches the ephemeral port with one guessed ID would then
	// burn one of the round's five attempts, repeatably. So this loops,
	// discarding whatever doesn't match, until either a genuine match is
	// found or the deadline set above ends it.
	for {
		reply, err := co.ReadMsg()
		if err != nil {
			if reply == nil {
				// A socket-level failure — the deadline elapsed, the kernel
				// returned ECONNREFUSED, or the connection is otherwise
				// gone. co.ReadMsgHeader only returns a nil message when the
				// underlying read itself failed (dns.Conn.ReadMsg), so
				// nothing more is coming; end the attempt.
				return classifyNotifyErr(err)
			}
			// A message-level failure: the packet didn't even unpack, or it
			// carried a TSIG that failed to verify. Not an authentic
			// response — discard it and keep waiting.
			continue
		}

		// RFC 1996 §3.6: the response must match the request's query ID and
		// QNAME. The connected socket already covers the other two — IP
		// source address and UDP source port — because the kernel delivers
		// to it only datagrams from the address it is connected to; these
		// two do not come for free, and without them an off-path response
		// with a guessed ID would end a round that never landed.
		if reply.Id != m.Id {
			continue
		}
		if len(reply.Question) != 1 || !strings.EqualFold(reply.Question[0].Name, m.Question[0].Name) {
			continue
		}
		// RFC 8945 §5.4: a signed request's response must be signed too.
		// miekg's ReadMsg verifies a TSIG only when the reply carries one
		// ("if t := m.IsTsig(); t != nil" — client.go:267) — a reply with
		// none skips verification entirely. Without this check, a spoofed
		// *unsigned* reply that merely guesses the ID and QNAME would end
		// the round exactly as an authentic signed one would, and TSIG would
		// contribute nothing at all to the response path.
		if key != nil && reply.IsTsig() == nil {
			continue
		}

		if reply.Rcode != dns.RcodeSuccess {
			return fmt.Errorf("%w: %s", ErrNotifyDelivered, dns.RcodeToString[reply.Rcode])
		}
		return nil
	}
}

// resolve turns target into the dial addresses to try, resolving Host
// through s.res when it is not already an IP literal — at send time, so a
// target that moves is followed rather than pinned to the address it had
// when notify_to was parsed (see NotifyTarget.Host). A hostname resolving to
// several addresses (as "localhost" ordinarily does — Go's resolver returns
// both loopback addresses, IPv6 first) contributes all of them, mirroring
// primary.resolve.
func (s *udpSender) resolve(ctx context.Context, target NotifyTarget) ([]string, error) {
	if addr, err := netip.ParseAddr(target.Host); err == nil {
		return []string{netip.AddrPortFrom(addr.Unmap(), target.Port).String()}, nil
	}
	res := s.res
	if res == nil {
		res = net.DefaultResolver
	}
	ips, err := res.LookupNetIP(ctx, "ip", target.Host)
	if err != nil {
		return nil, fmt.Errorf("resolving notify target %q: %w", target.Host, err)
	}
	if len(ips) == 0 {
		// Defensive, and knowingly untested — mirrors primary.resolve's own
		// note: the standard resolver reports "no addresses" as an error
		// rather than an empty slice, so nothing reachable through
		// net.Resolver produces this.
		return nil, fmt.Errorf("notify target %q resolved to no addresses", target.Host)
	}
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = netip.AddrPortFrom(ip.Unmap(), target.Port).String()
	}
	return out, nil
}

// classifyNotifyErr wraps err as ErrNotifyUnreachable when it is RFC 1996
// §3.6's ICMP port-unreachable case — ECONNREFUSED on the read from a
// connected UDP socket — and returns it unchanged otherwise, which covers
// silence past the deadline (an ordinary timeout, retried) and any other
// dial or I/O failure.
func classifyNotifyErr(err error) error {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("%w: %w", ErrNotifyUnreachable, err)
	}
	return err
}

// singleTSIGKey adapts one already-resolved TSIG key into the
// dnssrv.TSIGKeys lookup dnssrv.NewTSIGProvider expects, so Send signs
// through the exact mechanism Transferrer.fetch and Transferrer.probeOne do
// rather than a second HMAC implementation (see dnssrv.NewTSIGProvider's own
// comment on why that would be a bug that only shows up against a
// TSIG-requiring peer). Unlike Transferrer, Send is never handed a whole key
// store — Notifier resolves the one key a target names before calling
// Send — so the lookup this adapts is "the one key I already have", matched
// by name the same way a real store lookup is.
type singleTSIGKey struct{ key store.TSIGKey }

func (k singleTSIGKey) ByName(_ context.Context, name string) (store.TSIGKey, bool, error) {
	if !strings.EqualFold(dns.CanonicalName(name), dns.CanonicalName(k.key.Name)) {
		return store.TSIGKey{}, false, nil
	}
	return k.key, true, nil
}
