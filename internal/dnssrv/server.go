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
	addr    string
	handler Handler
	tsig    dns.TsigProvider
	udp     *dns.Server
	tcp     *dns.Server
	bound   string
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
		if keys == nil {
			return
		}
		s.tsig = &tsigProvider{keys: keys}
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
	// Verification has already happened by the time this runs: miekg checked
	// the message against the provider attached in Start and recorded the
	// outcome on w (server.go:673). Ask once and carry the answer, so a
	// handler can require a valid signature via req.RequireTSIG(). Nothing is
	// refused here — verification is non-enforcing by design, and no path in
	// the pipeline is transfer-only yet.
	key, tsigErr := s.RequireTSIG(w, m)
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
		sig = replyTSIG(resp.Msg, key, m.IsTsig())
	}

	if _, isUDP := w.RemoteAddr().(*net.UDPAddr); isUDP {
		size := 512
		if opt := m.IsEdns0(); opt != nil {
			size = int(opt.UDPSize())
		}
		// RFC 6891 §6.2.5: clamp UDP size to minimum 512.
		// miekg's Truncate also floors at MinMsgSize (512); kept as defense-in-depth
		// against dependency behavior changes.
		if size < 512 {
			size = 512
		}
		// Truncate refuses outright to touch a message that already carries a
		// TSIG (msg_truncate.go:30), so the signature has to be attached
		// afterwards — and its bytes taken out of the budget first, or a
		// signed reply overshoots the size the client advertised. Truncate
		// floors at 512 itself, so there is nothing to reserve from a budget
		// that small; a client that wants signed answers over UDP without
		// risking a drop should advertise EDNS room for them.
		if sig != nil {
			if reserved := size - (dns.Len(sig) + maxTSIGMACLen); reserved >= 512 {
				size = reserved
			}
		}
		resp.Msg.Truncate(size)
	}
	if sig != nil {
		resp.Msg.Extra = append(resp.Msg.Extra, sig)
	}
	if err := w.WriteMsg(resp.Msg); err != nil {
		slog.Error("write response error", "err", err)
	}
}
