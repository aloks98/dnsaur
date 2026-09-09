package dnssrv

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/miekg/dns"
)

type Server struct {
	addr      string
	handler   Handler
	tsig      dns.TsigProvider
	transfers Transfers
	notifies  Notifies
	udp       *dns.Server
	tcp       *dns.Server
	// The sockets the two dns.Servers above were activated on, kept so
	// Shutdown can close them itself when miekg declines to -- see
	// Shutdown.
	pc    net.PacketConn
	ln    net.Listener
	bound string

	tlsConfig *tls.Config
	encrypted bool
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
	if s.tlsConfig != nil {
		ln, err := tls.Listen("tcp", s.addr, s.tlsConfig)
		if err != nil {
			return err
		}
		s.bound = ln.Addr().String()
		s.ln = ln
		// Net is deliberately unset: ActivateAndServe uses the Listener it
		// is given, and this one already speaks TLS.
		//
		// The two overrides are what RFC 7858 §3.4 asks for and miekg's
		// defaults do not give. A DoT client is expected to hold one
		// connection open and send everything down it -- Android's Private
		// DNS keeps one for the life of the network -- and miekg would
		// close that connection after 128 queries and after eight idle
		// seconds, charging a full TLS handshake for each. -1 is miekg's
		// own spelling of "no query limit" (server.go:581-586); 0, the zero
		// value, is what selects the default of 128.
		s.tcp = &dns.Server{
			Listener:      ln,
			Handler:       dns.HandlerFunc(s.serve),
			TsigProvider:  s.tsig,
			IdleTimeout:   func() time.Duration { return encryptedIdleTimeout },
			MaxTCPQueries: -1,
		}
		serveInBackground(s.tcp, "dot")
		return nil
	}

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
		s.pc, s.ln = pc, ln
		// Both listeners get the provider. A zone transfer only ever arrives
		// over TCP, but RFC 8945 puts TSIG on any message, and a provider on
		// one socket and not the other would make verification depend on which
		// one a peer happened to use.
		s.udp = &dns.Server{PacketConn: pc, Handler: mux, TsigProvider: s.tsig}
		s.tcp = &dns.Server{Listener: ln, Handler: mux, TsigProvider: s.tsig}
		serveInBackground(s.udp, "udp")
		serveInBackground(s.tcp, "tcp")
		return nil
	}

	return lastErr
}

func (s *Server) Addr() string { return s.bound }

// serveInBackground starts srv and returns once it is actually serving --
// or once it has failed, whichever happens first.
//
// The wait is the whole point, and it is not cosmetic. miekg's
// ShutdownContext returns "dns: server not started" *before* it closes
// anything (server.go:411-416) while srv.started is still false, which it
// is for the whole window between ActivateAndServe being scheduled and the
// goroutine actually reaching it. A Shutdown landing in that window
// therefore closes nothing, and the goroutine then runs, sets started, and
// serves the socket for the rest of the process's life: the address cannot
// be rebound, and the listener keeps answering a configuration the settings
// no longer name. For :53, started once and stopped at exit, that window is
// unreachable in practice; for DoT it is an operator-driven cycle, reached
// every time a protocol is toggled or a certificate path changes.
//
// NotifyStartedFunc is called from serveTCP/serveUDP after started is set
// under srv.lock (server.go:388-394, 463-467), so a fired notification is
// proof that a later ShutdownContext will take the closing path rather than
// the early return. The done channel covers the other outcome: an
// ActivateAndServe that fails before it ever starts serving would otherwise
// leave this waiting forever.
func serveInBackground(srv *dns.Server, what string) {
	started := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(started) }
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.ActivateAndServe(); err != nil {
			slog.Error(what+" server error", "err", err)
		}
	}()
	select {
	case <-started:
	case <-done:
	}
}

// Shutdown stops every listener this Server started, and reports every
// failure rather than the first: a socket that would not close is exactly
// the failure a caller must not discard, and with two of them the second
// one's reason is as useful as the first's.
//
// The socket closes here belong to the "ShutdownContext refused" path only.
// A refusal means miekg closed nothing at all -- it returns before touching
// the Listener -- so the socket is this Server's to close, or it stays bound
// and, worse, keeps serving. On the ordinary path miekg has already closed
// both, and Close on a closed socket answers net.ErrClosed, which is not a
// failure to report.
func (s *Server) Shutdown(ctx context.Context) error {
	var errs []error
	for _, srv := range []*dns.Server{s.udp, s.tcp} {
		if srv == nil {
			continue
		}
		if err := srv.ShutdownContext(ctx); err != nil {
			errs = append(errs, err)
			errs = append(errs, closeSocket(s.socketFor(srv))...)
		}
	}
	return errors.Join(errs...)
}

// socketFor is the socket srv was activated on, so a refused shutdown
// closes the right one rather than both.
func (s *Server) socketFor(srv *dns.Server) io.Closer {
	switch srv {
	case s.udp:
		if s.pc == nil {
			return nil
		}
		return s.pc
	case s.tcp:
		if s.ln == nil {
			return nil
		}
		return s.ln
	default:
		return nil
	}
}

// closeSocket closes c, reporting anything other than "it was already
// closed" -- the answer on every path where miekg got there first.
func closeSocket(c io.Closer) []error {
	if c == nil {
		return nil
	}
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return []error{err}
	}
	return nil
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

	if s.notifies != nil && isNotify(m) {
		// Its own deadline and its own writer, and none of the pipeline —
		// which for a NOTIFY is not merely inapplicable but actively wrong:
		// before this branch existed, a NOTIFY for an apex this server does
		// not hold reached the terminal forwarder and was sent upstream.
		ctx, cancel := context.WithTimeout(context.Background(), NotifyTimeout)
		defer cancel()
		s.notifies.ServeNotify(ctx, w, m, key, tsigErr)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), PipelineTimeout)
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

	// Everything shapeReply does has to happen before w.WriteMsg, and its
	// padding step is why: that is where miekg computes the TSIG MAC,
	// lazily, over whatever resp.Msg holds at that point (ReplyTSIG below
	// only builds an unsigned stub). Where it sits among the steps that
	// stay here — ahead of ReplyTSIG, ahead of FitUDPReply — is conventional
	// rather than forced. Ordering against FitUDPReply in particular has no
	// effect today: FitUDPReply only runs for a UDP reply, and s.encrypted
	// is only ever true when WithTLS bound a TLS-wrapped, TCP-only listener
	// with no UDP socket at all (tls.go, Start above). So s.encrypted and
	// the isUDP branch below are mutually exclusive for every request this
	// server handles; padding and a UDP-budget trim never compete for the
	// same reply.
	resp, err := s.handler.ServeDNS(ctx, req)
	resp = shapeReply(req, resp, err, s.encrypted)

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

// shapeReply turns what the pipeline returned into the message that goes
// out, applying the rules that are the same whatever carried the query: the
// SERVFAIL fallback for a handler that failed or returned nothing, the
// EDNS(0) rules, and RFC 8467 padding.
//
// It is one function rather than one per transport because it is one rule
// set. It was written twice — once in Server.serve, once in
// DoHServer.handle — and the second copy is precisely where a rule goes
// missing: the RFC 6891 §7 OPT strip and the RFC 3225 §3 DO copy each had
// to be added in both places, and a third transport would have made it
// three.
//
// What stays with the callers is what genuinely belongs to them: TSIG
// signing and the UDP datagram fit in Server.serve, neither of which has
// any meaning on DoH, and the HTTP framing in DoHServer.handle. encrypted
// says whether padding hides anything on this transport — false for :53,
// true for DoT, and always true for DoH, which RFC 8484 runs over HTTPS by
// construction.
func shapeReply(req *Request, resp *Response, err error, encrypted bool) *Response {
	if err != nil || resp == nil || resp.Msg == nil {
		resp = Servfail(req)
	}

	// RFC 6891: echo EDNS on any OPT-less reply to an EDNS query, which
	// covers both a locally synthesized response (SetReply/SetRcode) and an
	// upstream one that dropped its OPT. The DO bit is the query's own, per
	// RFC 3225 §3: "The DO bit of the query MUST be copied in the
	// response." A synthesised OPT that always cleared it told a validating
	// client the server was not DNSSEC-aware for the answer it had just
	// asked to be able to validate.
	//
	// A query that carried no OPT gets any OPT the reply arrived with taken
	// off instead, which §7 makes a MUST NOT rather than a courtesy; see
	// dropOPT.
	if opt := req.Msg.IsEdns0(); opt != nil {
		if resp.Msg.IsEdns0() == nil {
			resp.Msg.SetEdns0(1232, opt.Do())
		}
	} else {
		dropOPT(resp.Msg)
	}

	// RFC 8467 §4.2. Only when the client padded, and only on an encrypted
	// transport: a plaintext reply gains nothing from padding and costs
	// bytes.
	if encrypted && hasPadding(req.Msg) {
		if err := Pad(resp.Msg, PaddingBlockResponse); err != nil {
			slog.Error("padding the reply", "err", err)
		}
	}

	return resp
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
// dropOPT removes the OPT record from a reply going to a client that sent
// none. RFC 6891 §7: "If a query message with more than one OPT RR is
// received, a FORMERR (RCODE=1) MUST be returned" — and, the sentence that
// matters here, a responder answering a query that carried no OPT "MUST
// NOT" put one in the response.
//
// Adding an OPT only when the reply lacked one was not enough on its own.
// The cache stores an upstream reply with the OPT it arrived with, so the
// first EDNS client to populate an entry decided what every later non-EDNS
// client received for the same name — including whatever options that OPT
// carried, a cookie minted for someone else among them.
//
// An extended rcode goes with the OPT, of necessity: its upper eight bits
// live in the OPT's TTL and nowhere else, so a reply that keeps one after
// losing the OPT is not merely lossy — Msg.Pack refuses it outright with
// ErrExtendedRcode, and the client would get no answer at all. SERVFAIL is
// the honest downgrade, because the real code is one this client has no way
// to be told.
func dropOPT(m *dns.Msg) {
	m.Extra = slices.DeleteFunc(m.Extra, func(rr dns.RR) bool {
		_, isOPT := rr.(*dns.OPT)
		return isOPT
	})
	if m.Rcode > 0xF {
		m.Rcode = dns.RcodeServerFailure
	}
}

func ednsOnly(extra []dns.RR) []dns.RR {
	for _, rr := range extra {
		if opt, ok := rr.(*dns.OPT); ok {
			return []dns.RR{opt}
		}
	}
	return nil
}
