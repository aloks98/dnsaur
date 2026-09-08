package dnssrv

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// dohPath is the one endpoint RFC 8484 defines a request URI template for
// (§3); dnsaur does not make it configurable.
const dohPath = "/dns-query"

// dohContentType is RFC 8484 §6's media type for both the request and the
// response body. Also declared, independently, as internal/upstream's own
// unexported constant of the same name — that package is the client side of
// this same protocol and has no way to import a private identifier from
// here, so the two are kept in step by the RFC rather than by sharing code.
const dohContentType = "application/dns-message"

// DoHServer serves RFC 8484 DNS-over-HTTPS at /dns-query.
//
// It exists as its own type, in this package, rather than as another Option
// on Server the way DoT is: RFC 8484 carries a query and its answer inside
// an HTTP request/response instead of length-prefixed wire frames on a raw
// stream, so the transport is an *http.Server, not another *dns.Server —
// there is no dns.ResponseWriter for miekg to hand a TSIG verdict to, and no
// miekg parse for it to verify a signature during. Start/Addr/Shutdown
// deliberately mirror Server's three methods regardless, so a later
// reconciler can hold both kinds of listener behind one small interface.
type DoHServer struct {
	addr      string
	handler   Handler
	tlsConfig *tls.Config

	httpSrv *http.Server
	bound   string
}

// dohReadHeaderTimeout bounds how long a client may take to finish sending
// request headers, the same field internal/app's admin API server sets for
// the same reason (app.go:682): a client that opens a connection and
// dribbles bytes forever otherwise holds a goroutine and a file descriptor
// open indefinitely.
//
// That defence is real but narrower here than the comment used to claim.
// This listener negotiates HTTP/2 by default (see NewDoHServer), and
// net/http's bundled HTTP/2 server never reads srv.ReadHeaderTimeout at all
// -- checked directly against h2_bundle.go, which consults only ReadTimeout
// and WriteTimeout (per stream) and IdleTimeout (per connection). On the
// primary path, the actual backstop against a slow or silent client is
// dohIdleTimeout below plus HTTP/2's own fixed ten-second client-preface
// timeout, neither of which this field touches. What ReadHeaderTimeout does
// still cover is a connection that falls back to HTTP/1.1 -- a client that
// never negotiates h2 at all -- so it earns its place, just not for the
// reason a reader would assume from the app.go parallel alone.
const dohReadHeaderTimeout = 5 * time.Second

// dohReadTimeout bounds the rest of the request once headers are in: the
// body, which readQuery never lets exceed dns.MaxMsgSize (65535 bytes)
// regardless of what the client sends. A little more than
// dohReadHeaderTimeout, since a client that finished its headers on time can
// still trickle a small body in slowly. Unlike dohReadHeaderTimeout, this one
// is honored on the HTTP/2 path too (h2_bundle.go applies it per stream).
const dohReadTimeout = 10 * time.Second

// dohWriteTimeout bounds writing the response -- one DNS answer, same size
// ceiling as the request. Honored per stream on HTTP/2, the same as
// dohReadTimeout.
const dohWriteTimeout = 10 * time.Second

// dohIdleTimeout bounds how long a connection with no request in flight may
// sit open, and on the HTTP/2 path this -- not dohReadHeaderTimeout -- is
// the real bound on a client that goes quiet. It is deliberately generous:
// RFC 8484 connections are meant to be reused for many queries over the
// lifetime of a resolver's upstream configuration, and a client with query
// gaps longer than a minute -- a forwarding resolver with bursty traffic, an
// intermittent stub -- would otherwise pay a fresh TLS handshake per burst,
// which is exactly the cost negotiating HTTP/2 exists to amortise away.
// Public DoH resolvers commonly sit in the 180-300s range; three minutes
// here is still finite, which is all the security rationale (a client that
// never sends a second query must not hold a connection open forever)
// actually requires.
const dohIdleTimeout = 180 * time.Second

// NewDoHServer builds a DoH server that dispatches every query to h. tlsCfg
// must already carry a certificate — either Certificates or GetCertificate —
// the same as any other TLS listener in this package.
//
// tlsCfg is cloned, and its NextProtos slice is cloned again before being
// amended to include "h2": tls.Config.Clone copies the NextProtos slice
// header only, so the clone and tlsCfg start out sharing one backing array,
// and appending straight to that shared array risks writing into spare
// capacity that belongs to the caller rather than allocating a new one.
// Nothing today keeps a reference to tlsCfg.NextProtos after handing it
// here, but a later caller deriving both a DoT and a DoH config from one
// shared tls.Config would make that hazard live, so it is closed regardless
// of whether anything currently reaches it.
//
// The "h2" amendment itself is not cosmetic: Start hands the resulting
// config to a hand-built tls.Listener and then to Server, and net/http wires
// HTTP/2 onto that combination only through shouldConfigureHTTP2ForServe's
// "conservative policy" for Serve (as opposed to ListenAndServeTLS/
// ServeTLS), which requires the config to name "h2" itself before it will
// touch a listener it did not construct. Skip this and the failure is
// silent: TLS still completes, every query still answers, and the
// connection negotiates HTTP/1.1 — one request per connection instead of
// one connection serving many, with nothing in a casual test run pointing
// at why.
func NewDoHServer(addr string, h Handler, tlsCfg *tls.Config) *DoHServer {
	cfg := tlsCfg.Clone()
	cfg.NextProtos = slices.Clone(cfg.NextProtos)
	for _, proto := range []string{"h2", "http/1.1"} {
		if !slices.Contains(cfg.NextProtos, proto) {
			cfg.NextProtos = append(cfg.NextProtos, proto)
		}
	}

	s := &DoHServer{addr: addr, handler: h, tlsConfig: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc(dohPath, s.handle)
	s.httpSrv = &http.Server{
		Handler:           mux,
		TLSConfig:         cfg,
		ReadHeaderTimeout: dohReadHeaderTimeout,
		ReadTimeout:       dohReadTimeout,
		WriteTimeout:      dohWriteTimeout,
		IdleTimeout:       dohIdleTimeout,
	}
	return s
}

// Start binds a TLS listener and begins serving in the background, the same
// contract as Server.Start: a nil return means the socket is bound and
// Addr() is valid, not that serving has necessarily begun yet.
func (s *DoHServer) Start() error {
	ln, err := tls.Listen("tcp", s.addr, s.tlsConfig)
	if err != nil {
		return err
	}
	s.bound = ln.Addr().String()
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("doh server error", "err", err)
		}
	}()
	return nil
}

func (s *DoHServer) Addr() string { return s.bound }

func (s *DoHServer) Shutdown(ctx context.Context) error {
	return s.httpSrv.Shutdown(ctx)
}

// handle answers one HTTP request as RFC 8484 §4.1's query/response
// exchange.
func (s *DoHServer) handle(w http.ResponseWriter, r *http.Request) {
	wire, code, err := readQuery(r)
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	m := new(dns.Msg)
	if err := m.Unpack(wire); err != nil {
		http.Error(w, "malformed DNS message", http.StatusBadRequest)
		return
	}

	// A transfer or a NOTIFY has no meaning here: both are answered by
	// Server.serve through a dns.ResponseWriter that streams, which does not
	// exist on this path, and RFC 8484 does not contemplate AXFR at all.
	// Refusing is honest; letting either into the ordinary pipeline is how a
	// NOTIFY once reached the forwarder and was sent upstream.
	if isTransferQuery(m) || isNotify(m) {
		// There is no `refused` helper in this package to reach for — only
		// Servfail exists (pipeline.go:82) — so the reply is built by hand.
		refused := new(dns.Msg)
		refused.SetRcode(m, dns.RcodeRefused)
		s.write(w, refused)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	req := &Request{Msg: m, ClientIP: clientAddr(r)}
	// Nobody verified a signature on this request: there is no miekg parse
	// on the DoH path for a TsigProvider to run during, so RequireTSIG must
	// report ErrTSIGUnavailable rather than let a handler read an
	// unexamined message as "checked and fine". See tsig.go's tsigState.
	req.tsig = unverifiedTSIG()

	resp, err := s.handler.ServeDNS(ctx, req)
	if err != nil || resp == nil || resp.Msg == nil {
		resp = Servfail(req)
	}

	// RFC 6891: echo EDNS on any OPT-less response to an EDNS query, the same
	// rule Server.serve applies to plain, DoT and (implicitly, since it
	// shares this handler) transfer/notify-free traffic.
	if m.IsEdns0() != nil && resp.Msg.IsEdns0() == nil {
		resp.Msg.SetEdns0(1232, false)
	}

	// RFC 8467 §4.2. Only when the client padded: DoH is encrypted by
	// construction (RFC 8484 runs over HTTPS), so there is no plaintext
	// case to guard against here the way Server.serve must. There is also
	// no TSIG on this path (see unverifiedTSIG) to worry about ordering
	// against.
	if hasPadding(m) {
		if err := Pad(resp.Msg, PaddingBlockResponse); err != nil {
			slog.Error("padding the reply", "err", err)
		}
	}

	// RFC 8484 §4.1 unlike E1's client-side exchanger: a server does not
	// rewrite the message ID here. SetReply/SetRcode already copied it from
	// the request, and it is echoed back exactly as it arrived.
	s.write(w, resp.Msg)
}

// write packs m and sends it with the content type RFC 8484 §6 requires.
func (s *DoHServer) write(w http.ResponseWriter, m *dns.Msg) {
	wire, err := m.Pack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", dohContentType)
	if _, err := w.Write(wire); err != nil {
		slog.Error("doh write response error", "err", err)
	}
}

// readQuery extracts the wire-format message from r under RFC 8484 §4.1's
// two methods, and reports the HTTP status a caller should answer with if it
// cannot.
//
// The read is bounded at dns.MaxMsgSize either way. The body comes from a
// client nothing here has authenticated yet, and reading it without limit
// would let one request hold an arbitrary amount of memory open — a trivial
// denial of service against a resolver that, on its own network, nothing
// else gates.
func readQuery(r *http.Request) ([]byte, int, error) {
	switch r.Method {
	case http.MethodPost:
		// RFC 8484 §4.1: a POST body is "the DNS message ... with the
		// Content-Type header field set to application/dns-message". A
		// client that sends something else is misconfigured, and answering
		// it 200 hides that: it works here and then breaks against the
		// first strict resolver it meets, with dnsaur's tolerance having
		// concealed the bug for however long it took to get there.
		//
		// Parsed rather than compared, because the header may legitimately
		// carry parameters ("application/dns-message; charset=utf-8" is
		// odd but not malformed), and only the media type is the contract.
		if err := checkDoHContentType(r.Header.Get("Content-Type")); err != nil {
			return nil, http.StatusUnsupportedMediaType, err
		}
		// A Content-Length that already announces more than the limit is
		// rejected before a single byte of the body is read — "rejected
		// rather than read", not merely truncated after the fact. A missing
		// or lying Content-Length (chunked encoding, or -1) still can't get
		// past the LimitReader below: at most one byte over the limit is
		// ever pulled off the wire, regardless of how much the client sends
		// or claims to.
		if r.ContentLength > dns.MaxMsgSize {
			return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("body of %d bytes exceeds the %d-byte limit", r.ContentLength, dns.MaxMsgSize)
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, dns.MaxMsgSize+1))
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("reading body: %w", err)
		}
		if len(body) > dns.MaxMsgSize {
			return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("body exceeds the %d-byte limit", dns.MaxMsgSize)
		}
		return body, 0, nil
	case http.MethodGet:
		enc := r.URL.Query().Get("dns")
		if enc == "" {
			return nil, http.StatusBadRequest, errors.New("missing dns query parameter")
		}
		// base64.RawURLEncoding never decodes to more bytes than it was
		// given, so bounding the encoded string is enough to keep an
		// oversized parameter from being decoded — and held in memory
		// decoded — at all.
		if base64.RawURLEncoding.DecodedLen(len(enc)) > dns.MaxMsgSize {
			return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("dns parameter decodes to more than the %d-byte limit", dns.MaxMsgSize)
		}
		wire, err := base64.RawURLEncoding.DecodeString(enc)
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("invalid base64url dns parameter: %w", err)
		}
		return wire, 0, nil
	default:
		return nil, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method)
	}
}

// checkDoHContentType reports why a POST's Content-Type is not the one RFC
// 8484 §6 requires, or nil when it is.
//
// A missing header is refused along with a wrong one. net/http does not
// default it, and GET carries no body to type — so on the one method where
// it applies, "absent" is as much a misconfiguration as "application/json".
func checkDoHContentType(header string) error {
	if header == "" {
		return fmt.Errorf("missing Content-Type: a POST body must be typed %s", dohContentType)
	}
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return fmt.Errorf("unparseable Content-Type %q: %w", header, err)
	}
	if !strings.EqualFold(mediaType, dohContentType) {
		return fmt.Errorf("Content-Type %s is not %s", mediaType, dohContentType)
	}
	return nil
}

// clientAddr reports the address net/http recorded for the connection r
// arrived on. r.RemoteAddr comes from the accepted net.Conn, the same
// non-spoofable source Server.serve reads through dns.ResponseWriter's
// RemoteAddr — never from a header a client could set.
//
// The zone is dropped, which is what makes this address the same address
// every other transport produces for the same client. Server.serve builds
// its netip.Addr from net.TCPAddr.IP / net.UDPAddr.IP, and those fields
// carry no zone at all; here the address arrives as text
// ("[fe80::1%eth0]:9000") and netip.ParseAddr keeps it. A zoned Addr misses
// in clients.Registry.Lookup twice over — netip.Prefix.Contains returns
// false outright for one, and the exact-match map is keyed by a zoneless
// Addr — so a link-local client reaching dnsaur over DoH would land in the
// default group and get the wrong blocklists, while the same client over
// DoT or plain matched its own group.
func clientAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return ip.Unmap().WithZone("")
}

// unverifiedTSIG is the tsigState every DoH request carries. Leaving
// req.tsig nil would already fail closed — RequireTSIG treats a nil tsig as
// "nobody asked" and returns ErrTSIGUnavailable on its own — but setting it
// explicitly here says the same thing on purpose rather than by omission,
// and rules out a future edit near this call site quietly swapping in
// &tsigState{} with a zero err, which would read as "checked and fine" for
// a message nothing on this path ever parsed with miekg.
func unverifiedTSIG() *tsigState {
	return &tsigState{err: ErrTSIGUnavailable}
}
