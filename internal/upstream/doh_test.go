package upstream

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/certtest"
	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
)

const dohName = "doh.test"

func dohUpstream(addr string) Upstream {
	return Upstream{Scheme: SchemeDoH, Addr: addr, VerifyName: dohName, Path: "/dns-query",
		Canonical: "https://" + addr + "/dns-query#" + dohName}
}

func echoReply(ip string) func(*dns.Msg) *dns.Msg {
	return func(m *dns.Msg) *dns.Msg {
		r := new(dns.Msg)
		r.SetReply(m)
		if rr, err := dns.NewRR(m.Question[0].Name + " 60 IN A " + ip); err == nil {
			r.Answer = append(r.Answer, rr)
		}
		return r
	}
}

func TestDoHExchange(t *testing.T) {
	cert, pool := certtest.For(t, dohName)
	addr, rec := startDoH(t, cert, echoReply("10.0.0.2"))
	e := newDoHExchanger(dohUpstream(addr), 5*time.Second, pool)
	t.Cleanup(func() { _ = e.Close() })

	r, err := e.Exchange(context.Background(), query("example.com"))
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if len(r.Answer) != 1 {
		t.Fatalf("got %d answers, want 1", len(r.Answer))
	}
	if got := rec.path.Load(); got != "/dns-query" {
		t.Errorf("server saw path %v, want /dns-query", got)
	}
	if got := rec.ctype.Load(); got != "application/dns-message" {
		t.Errorf("server saw Content-Type %v, want application/dns-message", got)
	}
	if got := rec.proto.Load(); got != "HTTP/2.0" {
		t.Errorf("server saw %v, want HTTP/2.0 — ForceAttemptHTTP2 is not taking effect", got)
	}
}

// RFC 8484 §4.1: the ID on the wire is 0, and the caller gets its own ID
// back. Losing the restore breaks nothing visible until something matches
// on it, which is exactly why it is asserted here.
func TestDoHZeroesTheWireIDAndRestoresIt(t *testing.T) {
	cert, pool := certtest.For(t, dohName)
	addr, rec := startDoH(t, cert, echoReply("10.0.0.2"))
	e := newDoHExchanger(dohUpstream(addr), 5*time.Second, pool)
	t.Cleanup(func() { _ = e.Close() })

	m := query("example.com")
	m.Id = 0x1234
	r, err := e.Exchange(context.Background(), m)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if got := rec.lastID.Load(); got != 0 {
		t.Errorf("the wire carried ID %d, want 0", got)
	}
	if r.Id != 0x1234 {
		t.Errorf("the reply carries ID %#x, want %#x", r.Id, 0x1234)
	}
}

func TestDoHPadsWhatItSendsAndStripsWhatItGets(t *testing.T) {
	cert, pool := certtest.For(t, dohName)
	addr, rec := startDoH(t, cert, func(m *dns.Msg) *dns.Msg {
		r := echoReply("10.0.0.2")(m)
		_ = dnssrv.Pad(r, dnssrv.PaddingBlockQuery) // a compliant server pads its replies
		return r
	})
	e := newDoHExchanger(dohUpstream(addr), 5*time.Second, pool)
	t.Cleanup(func() { _ = e.Close() })

	r, err := e.Exchange(context.Background(), query("a.com"))
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if n := rec.lastSize.Load(); n%dnssrv.PaddingBlockQuery != 0 {
		t.Errorf("the server received %d bytes, not a multiple of %d", n, dnssrv.PaddingBlockQuery)
	}
	if opt := r.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if _, isPad := o.(*dns.EDNS0_PADDING); isPad {
				t.Error("the reply's padding was passed on; it would be cached and then sent to the client")
			}
		}
	}
}

func TestDoHTreatsNon200AsFailure(t *testing.T) {
	cert, pool := certtest.For(t, dohName)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "over quota", http.StatusTooManyRequests)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	e := newDoHExchanger(dohUpstream(srv.Listener.Addr().String()), 5*time.Second, pool)
	t.Cleanup(func() { _ = e.Close() })

	if _, err := e.Exchange(context.Background(), query("example.com")); err == nil {
		t.Fatal("Exchange succeeded on a 429")
	}
}

func TestDoHRejectsAMismatchedCertificate(t *testing.T) {
	cert, pool := certtest.For(t, "somewhere.else")
	addr, _ := startDoH(t, cert, echoReply("10.0.0.2"))
	e := newDoHExchanger(dohUpstream(addr), 5*time.Second, pool) // expects doh.test
	t.Cleanup(func() { _ = e.Close() })

	if _, err := e.Exchange(context.Background(), query("example.com")); err == nil {
		t.Fatal("Exchange succeeded against a certificate for the wrong name")
	}
}

// A percent-escape the operator wrote in the path survives to the server as
// an escape, rather than being decoded into a different resource.
//
// The failure this pins is silent: addr.go used to read url.URL.Path, which
// is *decoded*, so "/a%2Fb" arrived here as "/a/b" and the exchanger asked
// for a two-segment path the operator never configured. Nothing errors —
// the server simply answers about a different URL, or 404s.
func TestDoHKeepsAPercentEncodedPath(t *testing.T) {
	ups, err := ParseUpstreams("https://127.0.0.1/a%2Fb#" + dohName)
	if err != nil {
		t.Fatalf("ParseUpstreams: %v", err)
	}
	if ups[0].Path != "/a%2Fb" {
		t.Fatalf("parsed path = %q, want the escape kept as %q", ups[0].Path, "/a%2Fb")
	}

	cert, pool := certtest.For(t, dohName)
	addr, rec := startDoH(t, cert, echoReply("10.0.0.7"))
	u := ups[0]
	u.Addr = addr
	e := newDoHExchanger(u, 5*time.Second, pool)
	t.Cleanup(func() { _ = e.Close() })

	if _, err := e.Exchange(context.Background(), query("example.com")); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if got := rec.rawPath.Load(); got != "/a%2Fb" {
		t.Errorf("the server was asked for %v, want %q", got, "/a%2Fb")
	}
}
