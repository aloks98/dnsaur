package dnssrv

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/certtest"
	"github.com/miekg/dns"
)

// A DoT client's real address reaches the handler.
//
// Everything about per-client filtering hangs off Request.ClientIP:
// clients.Registry.Lookup maps it to a group, and the group decides which
// blocklists and rules apply. A transport that lost it would still answer
// every query correctly-looking — with the default group's rules and the
// wrong attribution in the query log — so nothing else in the suite would
// fail. Hence this test.
//
// The server is bound on 127.0.0.1 and the client dials from 127.0.0.2 —
// deliberately different loopback addresses. A test where both sides sit on
// the same address cannot tell a correct ClientIP from a bug that reports
// the server's own address instead: both are loopback, so an assertion of
// merely "is loopback" passes either way. Asserting the exact source address
// the client dialled from is the only assertion a wrong-but-plausible
// attribution cannot slip past.
func TestDoTPreservesClientIdentity(t *testing.T) {
	cert, pool := certtest.For(t, "dot.test")

	// Not all of 127.0.0.0/8 is guaranteed routable to loopback on every
	// platform, even though it is on Linux. Prove 127.0.0.2 is assignable
	// here before building the assertion on it, and skip honestly rather
	// than fall back to a weaker check that cannot discriminate.
	const altAddr = "127.0.0.2"
	probeLn, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP(altAddr)})
	if err != nil {
		t.Skipf("cannot bind a local address of %s on this host (%v); this test needs a second loopback address distinct from the server's to tell a correct ClientIP from a wrong-but-plausible one", altAddr, err)
	}
	_ = probeLn.Close()

	got := make(chan netip.Addr, 1)
	h := HandlerFunc(func(_ context.Context, req *Request) (*Response, error) {
		got <- req.ClientIP
		r := new(dns.Msg)
		r.SetReply(req.Msg)
		return &Response{Msg: r}, nil
	})

	srv := NewServer("127.0.0.1:0", h, WithTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}))
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	c := &dns.Client{
		Net:       "tcp-tls",
		Timeout:   5 * time.Second,
		TLSConfig: &tls.Config{ServerName: "dot.test", RootCAs: pool, MinVersion: tls.VersionTLS12},
		// The distinct source address the assertion below keys on.
		Dialer: &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(altAddr)}},
	}
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeA)
	if _, _, err := c.Exchange(m, srv.Addr()); err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	want := netip.MustParseAddr(altAddr)
	select {
	case ip := <-got:
		if !ip.IsValid() {
			t.Fatal("the handler saw no client address at all")
		}
		if ip != want {
			t.Errorf("handler saw ClientIP %v, want %v (the address the client actually dialled from — the server is on 127.0.0.1, so this cannot be confused with the server's own address)", ip, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the handler never ran")
	}
}

// A TLS server binds TCP only. Binding UDP as well would occupy 853/udp for
// nothing, and DNS-over-TLS has no datagram form.
func TestDoTBindsNoUDPSocket(t *testing.T) {
	cert, _ := certtest.For(t, "dot.test")
	srv := NewServer("127.0.0.1:0", HandlerFunc(func(context.Context, *Request) (*Response, error) {
		return nil, nil
	}), WithTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}))
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	// The same port must still be free on UDP.
	pc, err := net.ListenPacket("udp", srv.Addr())
	if err != nil {
		t.Fatalf("UDP port %s is occupied, so the TLS server bound a datagram socket it should not have: %v", srv.Addr(), err)
	}
	_ = pc.Close()
}

// A DoT listener shut down immediately after Start really is gone: the
// address rebinds, and nothing answers on it.
//
// The defect this pins is a race with miekg, not with this package.
// ShutdownContext returns "dns: server not started" *before* it closes
// anything while the serve goroutine has not yet run (server.go:411-416),
// so a Shutdown that wins the race against `go ActivateAndServe` closed
// nothing at all -- and the goroutine then went on to serve the listener
// for the rest of the process's life, holding the port and answering a
// configuration the settings no longer named. Toggling DoT off is exactly
// that Start/Shutdown pair, and it is operator-driven, so it can happen
// again and again.
//
// Repeated rather than run once: the window is small, and a single
// iteration that happened to lose the race would pass against the very bug
// it exists to catch.
func TestDoTShutdownRightAfterStartReleasesThePort(t *testing.T) {
	cert, _ := certtest.For(t, "dot.test")
	addr := freeLoopbackAddr(t)

	for i := range 50 {
		srv := NewServer(addr, HandlerFunc(func(context.Context, *Request) (*Response, error) {
			return nil, nil
		}), WithTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}))
		if err := srv.Start(); err != nil {
			t.Fatalf("iteration %d: Start: %v", i, err)
		}
		// No pause: shutting down the instant Start returns is the whole
		// point.
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Fatalf("iteration %d: Shutdown reported %v -- a listener that would not close must not be silent", i, err)
		}

		// Two independent assertions, because the failure has two faces.
		// The port must be free again...
		probe, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("iteration %d: %s is still bound after Shutdown: %v", i, addr, err)
		}
		if err := probe.Close(); err != nil {
			t.Fatalf("iteration %d: closing the probe listener: %v", i, err)
		}
		// ...and nothing must still be accepting on it. A leaked listener
		// answers a connection even when a rebind would have failed for
		// some other reason.
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("iteration %d: a connection to %s was accepted after Shutdown -- the listener is still serving", i, addr)
		}
	}
}

// freeLoopbackAddr names a loopback address that is free right now, by
// binding port 0 and releasing it. A test that has to rebind the same
// address cannot use ":0" -- it needs a real, stable address to come back
// to.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing the probe listener: %v", err)
	}
	return addr
}
