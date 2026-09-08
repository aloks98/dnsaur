package upstream

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/certtest"
	"github.com/miekg/dns"
)

// A tls:// entry in the settings reaches the DoT server, end to end through
// the Forwarder rather than through the exchanger alone.
func TestForwarderUsesDoT(t *testing.T) {
	cert, pool := certtest.For(t, dotName)
	addr, _ := startDoT(t, cert, answerA("10.0.0.9"))
	f, err := New(Config{Upstreams: []string{"tls://" + addr + "#" + dotName}, Strategy: "failover"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	// The self-signed certificate is not in the system roots, so the
	// exchanger New built has to be told about it. This reaches inside on
	// purpose: everything else about the path — parsing, newUp's dispatch,
	// the strategy, markResult — is the production one.
	f.def[0].ex = newDoTExchanger(dotUpstream(addr), encryptedTimeout, pool)

	resp, err := f.Handler().ServeDNS(context.Background(), req("example.com"))
	if err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	if len(resp.Msg.Answer) != 1 {
		t.Fatalf("got %d answers, want 1", len(resp.Msg.Answer))
	}
	if resp.Upstream != "tls://"+addr+"#"+dotName {
		t.Errorf("Response.Upstream = %q, want the canonical entry", resp.Upstream)
	}
}

// newUp builds the transport the scheme names, and nothing else.
func TestNewUpDispatchesOnScheme(t *testing.T) {
	for _, tc := range []struct {
		entry string
		want  string
	}{
		{"1.1.1.1:53", "*upstream.plainExchanger"},
		{"tls://1.1.1.1:853#cloudflare-dns.com", "*upstream.dotExchanger"},
		{"https://1.1.1.1/dns-query#cloudflare-dns.com", "*upstream.dohExchanger"},
	} {
		t.Run(tc.entry, func(t *testing.T) {
			ups, err := ParseUpstreams(tc.entry)
			if err != nil {
				t.Fatalf("ParseUpstreams: %v", err)
			}
			u := newUp(ups[0], 2*time.Second)
			t.Cleanup(func() { _ = u.ex.Close() })
			if got := fmt.Sprintf("%T", u.ex); got != tc.want {
				t.Errorf("newUp(%q) built %s, want %s", tc.entry, got, tc.want)
			}
		})
	}
}

// **The guarantee.** An encrypted upstream that cannot be verified fails.
// It does not quietly try again in plaintext — which would make the whole
// feature a lie at exactly the moment it matters, with nothing on screen
// saying so.
//
// The plain listener is bound to the same host and the same port number on
// UDP. UDP and TCP are separate namespaces, so it sits beside the DoT
// listener rather than fighting it, and plainExchanger tries UDP first — so
// this is precisely where a downgrade would land. Asserting only that the
// query failed would pass even if a plaintext attempt had succeeded first
// and then been discarded; the counter is what makes the test discriminate.
func TestEncryptedUpstreamNeverFallsBackToPlaintext(t *testing.T) {
	cert, _ := certtest.For(t, "right.test")
	addr, _ := startDoT(t, cert, answerA("10.0.0.1"))
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("splitting %q: %v", addr, err)
	}

	var plainHits atomic.Int64
	pc, err := net.ListenPacket("udp", net.JoinHostPort(host, port))
	if err != nil {
		t.Fatalf("binding the plaintext listener beside the DoT one: %v", err)
	}
	plain := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		plainHits.Add(1)
		r := new(dns.Msg)
		r.SetReply(m)
		_ = w.WriteMsg(r)
	})}
	go func() { _ = plain.ActivateAndServe() }()
	t.Cleanup(func() { _ = plain.Shutdown() })

	// #wrong.test against a certificate for right.test, and the certificate
	// is self-signed so it is not in the system roots either: verification
	// fails for two independent reasons.
	f, err := New(Config{
		Upstreams: []string{"tls://" + addr + "#wrong.test"},
		Strategy:  "failover",
		Timeout:   2 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	if _, err := f.Handler().ServeDNS(context.Background(), req("example.com")); err == nil {
		t.Fatal("the query succeeded against an unverifiable upstream")
	}
	if n := plainHits.Load(); n != 0 {
		t.Errorf("the plaintext listener received %d queries — the encrypted upstream downgraded", n)
	}
}
