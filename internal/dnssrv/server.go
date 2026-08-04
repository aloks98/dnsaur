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
	udp     *dns.Server
	tcp     *dns.Server
	bound   string
}

func NewServer(addr string, h Handler) *Server {
	return &Server{addr: addr, handler: h}
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
		s.udp = &dns.Server{PacketConn: pc, Handler: mux}
		s.tcp = &dns.Server{Listener: ln, Handler: mux}
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
		resp.Msg.Truncate(size)
	}
	if err := w.WriteMsg(resp.Msg); err != nil {
		slog.Error("write response error", "err", err)
	}
}
