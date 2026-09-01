package dnssrv

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/miekg/dns"
)

type Server struct {
	addr      string
	handler   Handler
	tsig      dns.TsigProvider
	transfers Transfers
	udp       *dns.Server
	tcp       *dns.Server
	bound     string
}

// Option configures a Server before Start. Nothing here can be changed
// afterwards: Start is where both dns.Servers are built from these values.
type Option func(*Server)

// WithTSIGKeys attaches a TSIG provider backed by keys, so signed messages are
// verified against what the key store holds at the moment they arrive.
//
// A nil keys is the same as not passing the option at all: no provider, no
// verification, and RequireTSIG reporting ErrTSIGUnavailable rather than
// letting a signed message through unchecked.
func WithTSIGKeys(keys TSIGKeys) Option {
	return func(s *Server) {
		if p := NewTSIGProvider(keys); p != nil {
			s.tsig = p
		}
	}
}

func NewServer(addr string, h Handler, opts ...Option) *Server {
	s := &Server{addr: addr, handler: h}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *Server) Start() error {
	// Check if port is fixed (not ephemeral)
	_, port, _ := net.SplitHostPort(s.addr)
	isEphemeral := port == "0" || port == ""

	const maxRetries = 5
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		pc, err := net.ListenPacket("udp", s.addr)
		if err != nil {
			return err
		}
		s.bound = pc.LocalAddr().String()

		ln, err := net.Listen("tcp", s.bound)
		if err != nil {
			_ = pc.Close()
			lastErr = err
			// Only retry for ephemeral ports; fixed ports fail the same way each time
			if !isEphemeral {
				return err
			}
			continue
		}

		// Successfully bound both UDP and TCP
		mux := dns.HandlerFunc(s.serve)
		// Both listeners get the provider. A zone transfer only ever arrives
		// over TCP, but RFC 8945 puts TSIG on any message, and a provider on
		// one socket and not the other would make verification depend on which
		// one a peer happened to use.
		s.udp = &dns.Server{PacketConn: pc, Handler: mux, TsigProvider: s.tsig}
		s.tcp = &dns.Server{Listener: ln, Handler: mux, TsigProvider: s.tsig}
		go func() {
			if err := s.udp.ActivateAndServe(); err != nil {
				slog.Error("udp server error", "err", err)
			}
		}()
		go func() {
			if err := s.tcp.ActivateAndServe(); err != nil {
				slog.Error("tcp server error", "err", err)
			}
		}()
		return nil
	}

	return lastErr
}

func (s *Server) Addr() string { return s.bound }

func (s *Server) Shutdown(ctx context.Context) error {
	var first error
	for _, srv := range []*dns.Server{s.udp, s.tcp} {
		if srv != nil {
			if err := srv.ShutdownContext(ctx); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func (s *Server) serve(w dns.ResponseWriter, m *dns.Msg) {
	// Verification has already happened by the time this runs: miekg checked
	// the message against the provider attached in Start and recorded the
	// outcome on w (server.go:673). Ask once, here, before either branch
	// below, and carry the answer into whichever one runs -- a handler on
	// either side can then require a valid signature without asking again,
	// which would be two chances for the two calls to disagree.
	key, tsigErr := s.RequireTSIG(w, m)

	if s.transfers != nil && isTransferQuery(m) {
		// Its own deadline, its own writer, and none of the pipeline: qlog's
		// per-query accounting, the filter, the cache and the forwarder have
		// no meaning for a transfer, and the OPT force-add below would stamp
		// OPT onto every envelope.
		ctx, cancel := context.WithTimeout(context.Background(), TransferTimeout)
		defer cancel()
		s.transfers.ServeTransfer(ctx, w, m, key, tsigErr)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ip netip.Addr
	switch a := w.RemoteAddr().(type) {
	case *net.UDPAddr:
		ip, _ = netip.AddrFromSlice(a.IP)
	case *net.TCPAddr:
		ip, _ = netip.AddrFromSlice(a.IP)
	}
	req := &Request{Msg: m, ClientIP: ip.Unmap()}
	// Nothing is refused here -- verification is non-enforcing by design, and
	// no path in the pipeline is transfer-only. key and tsigErr are what was
	// computed above, before the branch; see req.RequireTSIG().
	req.tsig = &tsigState{key: key, err: tsigErr}

	resp, err := s.handler.ServeDNS(ctx, req)
	if err != nil || resp == nil || resp.Msg == nil {
		resp = Servfail(req)
	}

	// RFC 6891: echo EDNS on any OPT-less response to an EDNS query.
	// Covers locally synthesized responses (SetReply/SetRcode) and upstream responses
	// that dropped OPT. Spec-correct behavior per RFC 6891.
	if m.IsEdns0() != nil && resp.Msg.IsEdns0() == nil {
		resp.Msg.SetEdns0(1232, false)
	}

	// RFC 8945 §5.3: a request that verified gets an answer signed under the
	// same key. miekg only signs a reply that already carries a TSIG RR
	// (server.go:753-761), so the stub goes on below. A request that did not
	// verify gets an unsigned reply — signing it would assert an authenticity
	// the server was unable to establish.
	var sig *dns.TSIG
	if tsigErr == nil && key != "" {
		sig = ReplyTSIG(resp.Msg, key, m.IsTsig())
	}

	// TCP is never truncated: it has no datagram limit to fit inside. A
	// transfer does not reach this line on either transport — the intercept
	// above owns both, and answers the UDP one itself rather than by
	// streaming a zone into a datagram.
	if _, isUDP := w.RemoteAddr().(*net.UDPAddr); isUDP {
		FitUDPReply(resp.Msg, m, sig)
	}
	if sig != nil {
		resp.Msg.Extra = append(resp.Msg.Extra, sig)
	}
	if err := w.WriteMsg(resp.Msg); err != nil {
		slog.Error("write response error", "err", err)
	}
}

// FitUDPReply trims reply so that what finally goes on the wire fits the
// datagram budget req advertised, leaving room for the signature sig will
// grow into. sig is nil for an unsigned reply, and is attached by the caller
// *after* this returns — see fitUDP for why that order is the whole point.
//
// It is exported for the zone-transfer handler, whose one UDP case is RFC 1995
// §2's single-SOA answer to an IXFR: that reply is signed like any other, and
// a signed reply that would overshoot 512 has to become RFC 8945 §5.3's TC
// reply rather than an over-size packet. The budget is not the caller's to
// pass, which is why req is: it comes from the request's own OPT, floored at
// 512, and getting that wrong is how a reservation comes out of a number that
// was never real.
func FitUDPReply(reply, req *dns.Msg, sig *dns.TSIG) {
	fitUDP(reply, udpBudget(req), sig)
}

// udpBudget reports how many bytes a UDP reply to req may occupy on the wire.
func udpBudget(req *dns.Msg) int {
	size := dns.MinMsgSize
	if opt := req.IsEdns0(); opt != nil {
		size = int(opt.UDPSize())
	}
	// RFC 6891 §6.2.3: "Values lower than 512 MUST be treated as equal to 512."
	// Msg.Truncate floors there too, but fitUDP subtracts the signature's bytes
	// from this number before Truncate ever sees it, so the floor has to be
	// applied here as well or the reservation would come out of a budget that
	// was never real — and a client advertising 256 would get a TC for an
	// answer that fits.
	if size < dns.MinMsgSize {
		size = dns.MinMsgSize
	}
	return size
}

// fitUDP trims reply so that what finally goes on the wire fits budget,
// including the TSIG that sig will grow into if there is one.
//
// The order is the fix. Msg.Truncate silently does nothing at all to a message
// that already carries a TSIG (msg_truncate.go:30, "to simplify this
// implementation"), so the signature cannot be attached first; and its bytes
// have to leave the budget before truncating, or the signature puts the reply
// straight back over the size the client can receive. Truncating to 512 and
// then attaching the stub is what answered a 512-byte client with 564 bytes and
// no TC bit — an over-size packet with nothing telling the client to retry.
func fitUDP(reply *dns.Msg, budget int, sig *dns.TSIG) {
	if sig == nil {
		reply.Truncate(budget)
		return
	}
	// The stub carries no MAC yet; WriteMsg fills one in, at most 64 bytes. The
	// signed RR is appended to the packed message whole and uncompressed
	// (tsig.go:207-211), so dns.Len is its exact contribution.
	budget -= dns.Len(sig) + maxTSIGMACLen
	if budget >= dns.MinMsgSize {
		reply.Truncate(budget)
		return
	}

	// Below 512 Truncate is no help: it raises any smaller size back up to 512
	// (msg_truncate.go:40), the number already overshot. A client that
	// advertised no EDNS size has a bare 512-byte budget, so for a signed reply
	// this is the ordinary case rather than a corner.
	if fitsIn(reply, budget) {
		return
	}
	// It does not fit, and RFC 8945 §5.3 mandates exactly what to send instead:
	//
	//	"If addition of the TSIG record will cause the message to be
	//	truncated, the server MUST alter the response so that a TSIG can be
	//	included. This response contains only the question and a TSIG record,
	//	has the TC bit set, and has an RCODE of 0 (NOERROR). At this point,
	//	the client SHOULD retry the request using TCP (as per Section 4.2.2
	//	of [RFC1035])."
	//
	// So: no records, TC set, NOERROR, still signed. Nothing is salvaged from
	// the sections, and RFC 2181 §9 is why that costs the client nothing — on
	// a TC reply it "should ignore that response, and query again" over a
	// transport that permits larger replies, so any records left behind are
	// bytes it discards. That also settles the alternative of re-implementing
	// the library's compression-aware record fitting to keep a partial answer:
	// the spec does not permit it here, and the client would not use it.
	//
	// The rcode reset is the part that looks wrong and is not. An NXDOMAIN
	// whose authority section overflows goes out as NOERROR+TC; the client
	// learns the real rcode when it asks again over TCP, and §5.3 requires the
	// reply that fits to make no claim it cannot carry the records for.
	//
	// The signature still goes on below, which §5.3 is the whole point of: an
	// unsigned instruction to switch transports is one an off-path attacker can
	// forge, and a peer that required authentication would be right to reject
	// it.
	reply.Rcode = dns.RcodeSuccess
	reply.Truncated = true
	reply.Answer = nil
	reply.Ns = nil
	reply.Extra = ednsOnly(reply.Extra)
	// Setting Rcode is enough to cover an extended rcode too, including one a
	// kept OPT is already carrying in its TTL: Msg.Pack rewrites that field
	// from Rcode whenever an OPT is present rather than only for rcodes above
	// 15, "to allow resetting the extended rcode bits if they need to"
	// (msg.go:744-747). Pinned end to end, because it is a property of the
	// library rather than of anything here.
	//
	// A question and a key name long enough that even this overshoots would
	// still go out over-size. It is unreachable while dns.Server.UDPSize keeps
	// miekg's default: unset, it becomes MinMsgSize (server.go:287-288), so no
	// more than 512 bytes of query are ever read, and an empty reply is the
	// question and the TSIG that arrived with it plus the MAC. Raising UDPSize
	// would make it reachable — and it stays accepted rather than guarded even
	// then, because there is no smaller correct reply: dropping the signature
	// would answer a peer that required authentication with something it must
	// refuse, and refusing outright would lose the retry-over-TCP signal, which
	// by that point is the only useful thing left to say.
}

// fitsIn reports whether reply packs into n bytes, leaving reply.Compress set
// to whatever made that true. It mirrors the order Msg.Truncate uses:
// uncompressed first, because compressing a reply that already fits is wasted
// work, then compressed.
//
// The size is measured by packing rather than estimated, because dns.Len is the
// uncompressed length only. Guessing high would send a client to TCP for an
// answer that would have fit — the 479-byte reply that started this is 269
// bytes compressed, and comfortably inside a signed 512.
func fitsIn(reply *dns.Msg, n int) bool {
	reply.Compress = false
	if reply.Len() <= n {
		return true
	}
	reply.Compress = true
	b, err := reply.Pack()
	return err == nil && len(b) <= n
}

// ednsOnly reduces an additional section to its OPT record, if it has one.
//
// This is a real conflict between two MUSTs, resolved rather than overlooked.
// RFC 8945 §5.3 says the truncated response "contains only the question and a
// TSIG record". RFC 6891 §6.1.1 says "if an OPT record is present in a received
// request, compliant responders MUST include an OPT record in their respective
// responses". Both cannot hold for a signed, truncated answer to an EDNS query.
//
// The OPT is kept. It is 11 bytes that cannot push any reply over any budget;
// §5.3's sentence is describing the minimal response, in a document about TSIG,
// rather than legislating about a pseudo-RR that carries transport parameters
// and no answer data; and an EDNS query answered without EDNS is a result some
// resolvers read as a broken server and downgrade against, which would make the
// TC unusable for the retry it exists to prompt. A reader who weighs it the
// other way should know this is a deliberate reading of §5.3, not an omission.
func ednsOnly(extra []dns.RR) []dns.RR {
	for _, rr := range extra {
		if opt, ok := rr.(*dns.OPT); ok {
			return []dns.RR{opt}
		}
	}
	return nil
}
