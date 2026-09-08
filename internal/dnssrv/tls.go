package dnssrv

import "crypto/tls"

// WithTLS makes Start bind a single TCP listener wrapped in TLS, and no UDP
// socket.
//
// DNS-over-TLS (RFC 7858) is a stream protocol with no datagram sibling, so
// the paired UDP/TCP bind Start does for plaintext would occupy a datagram
// port for nothing. Everything above the socket is identical — the same
// handler, the same TSIG provider, the same transfer and NOTIFY intercepts:
// none of them ever knew what carried the bytes.
func WithTLS(cfg *tls.Config) Option {
	return func(s *Server) {
		s.tlsConfig = cfg
		s.encrypted = true
	}
}
