package upstream

import (
	"context"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/certtest"
	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
)

const dotName = "dot.test"

func dotUpstream(addr string) Upstream {
	return Upstream{Scheme: SchemeDoT, Addr: addr, VerifyName: dotName,
		Canonical: "tls://" + addr + "#" + dotName}
}

func TestDoTExchange(t *testing.T) {
	cert, pool := certtest.For(t, dotName)
	addr, _ := startDoT(t, cert, answerA("10.0.0.1"))
	e := newDoTExchanger(dotUpstream(addr), 5*time.Second, pool)
	t.Cleanup(func() { _ = e.Close() })

	r, err := e.Exchange(context.Background(), query("example.com"))
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if len(r.Answer) != 1 {
		t.Fatalf("got %d answers, want 1", len(r.Answer))
	}
	if a, ok := r.Answer[0].(*dns.A); !ok || a.A.String() != "10.0.0.1" {
		t.Errorf("answer = %v, want 10.0.0.1", r.Answer[0])
	}
}

// Several queries share one connection. Asserting only that the queries
// succeeded would pass with no pool at all — the accept count is the whole
// test.
func TestDoTReusesConnections(t *testing.T) {
	cert, pool := certtest.For(t, dotName)
	addr, ln := startDoT(t, cert, answerA("10.0.0.1"))
	e := newDoTExchanger(dotUpstream(addr), 5*time.Second, pool)
	t.Cleanup(func() { _ = e.Close() })

	for i := range 5 {
		if _, err := e.Exchange(context.Background(), query("example.com")); err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
	}
	if got := ln.count(); got != 1 {
		t.Errorf("server accepted %d connections for 5 queries, want 1 — the pool is not being used", got)
	}
}

// The server closing an idle connection is routine (RFC 7858 §3.4), not a
// query failure. Without the one-shot retry on a fresh connection this is
// an intermittent SERVFAIL that also drives a healthy upstream toward being
// marked down.
func TestDoTRetriesOnceOnAStaleConnection(t *testing.T) {
	cert, pool := certtest.For(t, dotName)
	addr, ln := startDoT(t, cert, answerA("10.0.0.1"))
	e := newDoTExchanger(dotUpstream(addr), 5*time.Second, pool)
	t.Cleanup(func() { _ = e.Close() })

	if _, err := e.Exchange(context.Background(), query("example.com")); err != nil {
		t.Fatalf("first query: %v", err)
	}
	// The connection is now in the pool, and the server hangs up on it.
	ln.closeAll()

	r, err := e.Exchange(context.Background(), query("example.com"))
	if err != nil {
		t.Fatalf("query after the server closed the pooled connection: %v", err)
	}
	if len(r.Answer) != 1 {
		t.Errorf("got %d answers, want 1", len(r.Answer))
	}
	if got := ln.count(); got != 2 {
		t.Errorf("server accepted %d connections, want 2 (the first, and the redial)", got)
	}
}

// A connection idle longer than the pool's own deadline is discarded rather
// than handed out.
func TestDoTDiscardsIdleConnections(t *testing.T) {
	cert, pool := certtest.For(t, dotName)
	addr, ln := startDoT(t, cert, answerA("10.0.0.1"))
	e := newDoTExchanger(dotUpstream(addr), 5*time.Second, pool)
	e.idleTimeout = time.Millisecond
	t.Cleanup(func() { _ = e.Close() })

	if _, err := e.Exchange(context.Background(), query("example.com")); err != nil {
		t.Fatalf("first query: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := e.Exchange(context.Background(), query("example.com")); err != nil {
		t.Fatalf("second query: %v", err)
	}
	if got := ln.count(); got != 2 {
		t.Errorf("server accepted %d connections, want 2 — the expired connection was reused", got)
	}
}

// A certificate that does not match the configured name is a failure. There
// is no plaintext retry and no "try anyway" — see
// TestEncryptedUpstreamNeverFallsBackToPlaintext for the other half.
func TestDoTRejectsAMismatchedCertificate(t *testing.T) {
	cert, pool := certtest.For(t, "somewhere.else")
	addr, _ := startDoT(t, cert, answerA("10.0.0.1"))
	e := newDoTExchanger(dotUpstream(addr), 5*time.Second, pool) // expects dot.test
	t.Cleanup(func() { _ = e.Close() })

	if _, err := e.Exchange(context.Background(), query("example.com")); err == nil {
		t.Fatal("Exchange succeeded against a certificate for the wrong name")
	}
}

// The query reaches the server padded, so its length says nothing about the
// name inside it.
func TestDoTPadsWhatItSends(t *testing.T) {
	cert, pool := certtest.For(t, dotName)
	sizes := make(chan int, 4)
	addr, _ := startDoT(t, cert, func(w dns.ResponseWriter, m *dns.Msg) {
		wire, err := m.Pack()
		if err == nil {
			sizes <- len(wire)
		}
		// Reply padded too, so the strip below has something to remove.
		r := new(dns.Msg)
		r.SetReply(m)
		_ = dnssrv.Pad(r, dnssrv.PaddingBlockQuery)
		_ = w.WriteMsg(r)
	})
	e := newDoTExchanger(dotUpstream(addr), 5*time.Second, pool)
	t.Cleanup(func() { _ = e.Close() })

	r, err := e.Exchange(context.Background(), query("a.com"))
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	select {
	case n := <-sizes:
		if n%dnssrv.PaddingBlockQuery != 0 {
			t.Errorf("the server received %d bytes, not a multiple of %d", n, dnssrv.PaddingBlockQuery)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the server never recorded a query size")
	}
	if opt := r.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if _, isPad := o.(*dns.EDNS0_PADDING); isPad {
				t.Error("the reply's padding was passed on; it would be cached and then sent to the client")
			}
		}
	}
}

// Close releases the real socket, not just the bookkeeping.
//
// Everything else about the Close guarantee is asserted against
// countingExchanger (exchanger_test.go), which proves the Forwarder calls
// Close once per upstream and nothing about what Close then does. This is
// the one link that has to be observed from the far end of a real
// connection: after Close, the server's side of the pooled connection reads
// EOF. Empty the pool without closing what was in it and the descriptor
// leaks exactly as §6 describes, with every existing test still green.
func TestDoTCloseReleasesPooledConnections(t *testing.T) {
	cert, pool := certtest.For(t, dotName)
	addr, ln := startDoT(t, cert, answerA("10.0.0.4"))
	e := newDoTExchanger(dotUpstream(addr), 5*time.Second, pool)

	if _, err := e.Exchange(context.Background(), query("example.com")); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if got := ln.count(); got != 1 {
		t.Fatalf("server accepted %d connections, want 1", got)
	}
	// The connection is now idle in the pool, and nothing has gone away.
	select {
	case <-ln.peerGone:
		t.Fatal("the connection was already gone before Close; it was never pooled")
	case <-time.After(50 * time.Millisecond):
	}

	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-ln.peerGone:
	case <-time.After(2 * time.Second):
		t.Fatal("the server still holds the pooled connection: Close emptied the pool without closing it")
	}
}
