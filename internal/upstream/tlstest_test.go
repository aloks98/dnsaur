package upstream

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/miekg/dns"
)

// recordingListener counts accepted connections and keeps them, which is
// how the pooling tests tell connection reuse from re-dialing, and how the
// stale-connection test closes the server's side deterministically instead
// of waiting for an idle timeout.
//
// It also reports, on peerGone, every connection whose client end went away:
// that is the only way to observe from outside the process that Close really
// released a pooled socket, rather than merely forgetting about it.
type recordingListener struct {
	net.Listener
	mu       sync.Mutex
	accepts  int
	conns    []net.Conn
	peerGone chan struct{}
}

func (l *recordingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	w := &watchedConn{Conn: c, gone: l.peerGone}
	l.mu.Lock()
	l.accepts++
	l.conns = append(l.conns, w)
	l.mu.Unlock()
	return w, nil
}

// watchedConn is the server's end of one accepted connection. It signals
// once, on peerGone, the first time a read ends -- which for a healthy DoT
// connection means the client closed it.
type watchedConn struct {
	net.Conn
	once sync.Once
	gone chan struct{}
}

func (c *watchedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err != nil {
		c.once.Do(func() {
			select {
			case c.gone <- struct{}{}:
			default: // nobody is watching; do not block the server's read loop
			}
		})
	}
	return n, err
}

func (l *recordingListener) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accepts
}

// closeAll shuts the server's end of every connection accepted so far —
// what a real DoT server does to an idle connection, on demand.
func (l *recordingListener) closeAll() {
	l.mu.Lock()
	conns := l.conns
	l.conns = nil
	l.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// startDoT runs a DNS-over-TLS server on 127.0.0.1 and returns its address
// and the listener, so a test can count accepts and close connections.
func startDoT(t *testing.T, cert tls.Certificate, h dns.HandlerFunc) (string, *recordingListener) {
	t.Helper()
	base, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	ln := &recordingListener{Listener: base, peerGone: make(chan struct{}, 8)}
	// Net is not set: ActivateAndServe uses the Listener it is given, and
	// this one already speaks TLS.
	srv := &dns.Server{Listener: ln, Handler: h}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return base.Addr().String(), ln
}

// answerA is not defined here: internal/upstream/forwarder_test.go already
// has one, with roughly twenty call sites in the same package. The DoT
// tests below use that existing helper as-is.

// dohRecord is what the test DoH server saw, so a test can assert on the
// wire rather than on the exchanger's own account of itself.
type dohRecord struct {
	requests atomic.Int64
	lastSize atomic.Int64
	lastID   atomic.Int64
	path     atomic.Value // string, r.URL.Path — decoded
	rawPath  atomic.Value // string, r.URL.EscapedPath() — as it arrived on the wire
	proto    atomic.Value // string, e.g. "HTTP/2.0"
	ctype    atomic.Value // string
}

// startDoH runs an RFC 8484 server over TLS on 127.0.0.1 and returns its
// address and a record of what it received.
func startDoH(t *testing.T, cert tls.Certificate, reply func(*dns.Msg) *dns.Msg) (string, *dohRecord) {
	t.Helper()
	rec := &dohRecord{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, dns.MaxMsgSize))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rec.requests.Add(1)
		rec.lastSize.Store(int64(len(body)))
		rec.path.Store(r.URL.Path)
		rec.rawPath.Store(r.URL.EscapedPath())
		rec.proto.Store(r.Proto)
		rec.ctype.Store(r.Header.Get("Content-Type"))
		m := new(dns.Msg)
		if err := m.Unpack(body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.lastID.Store(int64(m.Id))
		out, err := reply(m).Pack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(out)
	})
	srv := httptest.NewUnstartedServer(h)
	// Set before StartTLS: httptest only mints its own certificate when
	// TLS.Certificates is empty.
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), rec
}
