package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"sync"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
)

const (
	// idleConnTimeout is how long a pooled connection may sit unused before
	// it is discarded rather than handed out. Checked when a connection is
	// taken, so there is no reaper goroutine: with maxIdleConns connections
	// per upstream and a handful of upstreams, the worst case is a few
	// descriptors held until the next query.
	idleConnTimeout = 30 * time.Second
	// maxIdleConns bounds the pool. Each connection carries one query at a
	// time, so this is also the concurrency ceiling per upstream — ample
	// behind a cache, and the alternative (pipelining) needs message-ID
	// bookkeeping this does not.
	//
	// Both constants are also used by doh.go's http.Transport, which is why
	// neither is named for a transport.
	maxIdleConns = 4
)

// dotExchanger is DNS over TLS (RFC 7858) with a small connection pool.
type dotExchanger struct {
	addr        string
	client      *dns.Client
	idleTimeout time.Duration
	maxIdle     int

	mu     sync.Mutex
	idle   []idleConn
	closed bool
}

type idleConn struct {
	c    *dns.Conn
	last time.Time
}

// newDoTExchanger builds the transport for u.
//
// roots is the certificate pool to verify against; nil means the system
// roots, which is what production passes. It takes a pool rather than a
// *tls.Config on purpose: a test needs to trust a self-signed certificate,
// and nothing needs to skip verification — so there is no code path that
// can turn it off.
func newDoTExchanger(u Upstream, timeout time.Duration, roots *x509.CertPool) *dotExchanger {
	return &dotExchanger{
		addr: u.Addr,
		client: &dns.Client{
			Net:     "tcp-tls",
			Timeout: timeout,
			TLSConfig: &tls.Config{
				// The certificate is checked against the name the operator
				// configured, while the connection goes to the address they
				// configured. That split is the whole reason an entry
				// carries both (spec §3).
				ServerName: u.VerifyName,
				RootCAs:    roots,
				MinVersion: tls.VersionTLS12,
			},
		},
		idleTimeout: idleConnTimeout,
		maxIdle:     maxIdleConns,
	}
}

func (e *dotExchanger) Exchange(ctx context.Context, m *dns.Msg) (*dns.Msg, error) {
	if err := dnssrv.Pad(m, dnssrv.PaddingBlockQuery); err != nil {
		return nil, err
	}
	if c := e.get(); c != nil {
		r, _, err := e.client.ExchangeWithConnContext(ctx, m, c)
		if err == nil {
			e.put(c)
			dnssrv.StripPadding(r)
			return r, nil
		}
		_ = c.Close()
		// A pooled connection failing is expected: a DoT server may close an
		// idle connection at any time (RFC 7858 §3.4). Reporting that as a
		// query failure would turn the server's routine housekeeping into
		// intermittent SERVFAILs, and into markResult failures that
		// eventually mark a healthy upstream down. One fresh attempt below.
		if ctx.Err() != nil {
			// The caller gave up — under the race strategy the losers are
			// cancelled — so this was not the connection's fault and there
			// is nothing to retry for.
			return nil, err
		}
	}
	c, err := e.client.DialContext(ctx, e.addr)
	if err != nil {
		return nil, err
	}
	r, _, err := e.client.ExchangeWithConnContext(ctx, m, c)
	if err != nil {
		// Already fresh: this is a real failure, not a stale connection.
		_ = c.Close()
		return nil, err
	}
	e.put(c)
	dnssrv.StripPadding(r)
	return r, nil
}

// get takes a usable connection from the pool, or nil.
//
// Expired connections are collected under the lock and closed after it is
// released: Close on a *dns.Conn writes a TLS close_notify and can block
// for seconds against an unresponsive peer, and put and Close both already
// release e.mu before doing any I/O — get is the one place that must match
// that discipline, or one hung upstream stalls every other query
// contending for its pool.
func (e *dotExchanger) get() *dns.Conn {
	e.mu.Lock()
	now := time.Now()
	var found *dns.Conn
	var expired []*dns.Conn
	for len(e.idle) > 0 {
		last := e.idle[len(e.idle)-1]
		e.idle = e.idle[:len(e.idle)-1]
		if now.Sub(last.last) > e.idleTimeout {
			expired = append(expired, last.c)
			continue
		}
		found = last.c
		break
	}
	e.mu.Unlock()
	for _, c := range expired {
		_ = c.Close()
	}
	return found
}

// put returns a connection to the pool, or closes it if the pool is full or
// the exchanger has been closed.
func (e *dotExchanger) put(c *dns.Conn) {
	e.mu.Lock()
	if e.closed || len(e.idle) >= e.maxIdle {
		e.mu.Unlock()
		_ = c.Close()
		return
	}
	e.idle = append(e.idle, idleConn{c: c, last: time.Now()})
	e.mu.Unlock()
}

// Close releases every pooled connection. Safe to call more than once, and
// safe to call while a query is in flight: that query's connection is not
// in the pool, and put closes it rather than pooling it afterwards.
func (e *dotExchanger) Close() error {
	e.mu.Lock()
	conns := e.idle
	e.idle, e.closed = nil, true
	e.mu.Unlock()
	for _, ic := range conns {
		_ = ic.c.Close()
	}
	return nil
}
