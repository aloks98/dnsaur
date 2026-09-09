package dnssrv

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/certtest"
	"github.com/miekg/dns"
)

// echoHandler answers every query NOERROR with a single fixed A record, so a
// test can tell "the pipeline answered this" from "something else answered
// it" by looking at the record it got back.
func echoHandler() Handler {
	return HandlerFunc(func(_ context.Context, req *Request) (*Response, error) {
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		rr, _ := dns.NewRR(req.Msg.Question[0].Name + " 300 IN A 1.2.3.4")
		m.Answer = append(m.Answer, rr)
		return &Response{Msg: m}, nil
	})
}

// startDoHServer brings up a DoHServer on an ephemeral loopback port with h
// as its pipeline handler, and shuts it down when the test ends.
func startDoHServer(t *testing.T, h Handler, cert tls.Certificate) *DoHServer {
	t.Helper()
	srv := NewDoHServer("127.0.0.1:0", h, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Logf("shutdown error: %v", err)
		}
	})
	return srv
}

// dohHTTPClient builds the client side of the same protocol
// internal/upstream's exchanger speaks: TLS pinned to pool, and HTTP/2
// forced on because setting TLSClientConfig on a Transport disables the
// library's own automatic attempt otherwise. dialer, if non-nil, controls
// the connection's local address -- the seam TestDoHPreservesClientIdentity
// needs.
func dohHTTPClient(pool *x509.CertPool, serverName string, dialer *net.Dialer) *http.Client {
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			ForceAttemptHTTP2: true,
			TLSClientConfig: &tls.Config{
				ServerName: serverName,
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			},
			DialContext: dialer.DialContext,
		},
	}
}

func packMsg(t *testing.T, m *dns.Msg) []byte {
	t.Helper()
	wire, err := m.Pack()
	if err != nil {
		t.Fatalf("packing message: %v", err)
	}
	return wire
}

// dohPost POSTs wire to srv and returns the response's status code, its
// Content-Type header, and its body.
func dohPost(t *testing.T, client *http.Client, addr string, wire []byte) (int, string, []byte) {
	t.Helper()
	resp, err := client.Post("https://"+addr+dohPath, dohContentType, bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), body
}

// dohGet issues the RFC 8484 §4.1.1 GET form of the same exchange:
// unpadded, URL-safe base64 of wire in the "dns" query parameter.
func dohGet(t *testing.T, client *http.Client, addr string, wire []byte) (int, string, []byte) {
	t.Helper()
	u := "https://" + addr + dohPath + "?" + url.Values{
		"dns": {base64.RawURLEncoding.EncodeToString(wire)},
	}.Encode()
	resp, err := client.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), body
}

// A POST carrying a wire-format query gets a wire-format answer back, typed
// as RFC 8484 §6 requires.
func TestDoHPostReturnsWireAnswer(t *testing.T) {
	cert, pool := certtest.For(t, "doh.test")
	srv := startDoHServer(t, echoHandler(), cert)
	client := dohHTTPClient(pool, "doh.test", nil)

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	wire := packMsg(t, q)

	code, ctype, body := dohPost(t, client, srv.Addr(), wire)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want %d", code, http.StatusOK)
	}
	if ctype != dohContentType {
		t.Errorf("Content-Type = %q, want %q", ctype, dohContentType)
	}

	m := new(dns.Msg)
	if err := m.Unpack(body); err != nil {
		t.Fatalf("unpacking answer: %v", err)
	}
	if m.Rcode != dns.RcodeSuccess {
		t.Errorf("rcode = %v, want NOERROR", dns.RcodeToString[m.Rcode])
	}
	if len(m.Answer) != 1 {
		t.Fatalf("answer count = %d, want 1", len(m.Answer))
	}
	a, ok := m.Answer[0].(*dns.A)
	if !ok || a.A.String() != "1.2.3.4" {
		t.Errorf("answer = %v, want an A record for 1.2.3.4", m.Answer[0])
	}
}

// GET with the base64url-encoded query gives byte-identical results to the
// equivalent POST: the same wire bytes, sent both ways, must produce the
// same wire bytes back. Reusing one packed query for both requests is what
// makes the comparison exact -- two separately built dns.Msg values would
// carry two different random IDs, and the comparison would be comparing
// that difference rather than anything meaningful.
func TestDoHGetMatchesPost(t *testing.T) {
	cert, pool := certtest.For(t, "doh.test")
	srv := startDoHServer(t, echoHandler(), cert)
	client := dohHTTPClient(pool, "doh.test", nil)

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	wire := packMsg(t, q)

	postCode, postCType, postBody := dohPost(t, client, srv.Addr(), wire)
	getCode, getCType, getBody := dohGet(t, client, srv.Addr(), wire)

	if postCode != http.StatusOK || getCode != http.StatusOK {
		t.Fatalf("status codes = POST %d, GET %d, want both %d", postCode, getCode, http.StatusOK)
	}
	if postCType != getCType {
		t.Errorf("Content-Type differs: POST %q, GET %q", postCType, getCType)
	}
	if !bytes.Equal(postBody, getBody) {
		t.Errorf("GET answer differs from POST answer:\nPOST: %x\nGET:  %x", postBody, getBody)
	}
}

// A DoH client's real address reaches the handler, mirroring
// TestDoTPreservesClientIdentity (tls_test.go) for the same reason: everything
// about per-client filtering hangs off Request.ClientIP, and a transport that
// silently lost it would still answer every query correctly-looking.
func TestDoHPreservesClientIdentity(t *testing.T) {
	cert, pool := certtest.For(t, "doh.test")

	// Not all of 127.0.0.0/8 is guaranteed routable to loopback on every
	// platform. Prove 127.0.0.2 is assignable here before building the
	// assertion on it, and skip honestly rather than fall back to a weaker
	// check that cannot discriminate a correct ClientIP from a wrong-but
	// -plausible one.
	const altAddr = "127.0.0.2"
	probeLn, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP(altAddr)})
	if err != nil {
		t.Skipf("cannot bind a local address of %s on this host (%v); this test needs a second loopback address distinct from the server's to tell a correct ClientIP from a wrong-but-plausible one", altAddr, err)
	}
	_ = probeLn.Close()

	got := make(chan netip.Addr, 1)
	h := HandlerFunc(func(_ context.Context, req *Request) (*Response, error) {
		got <- req.ClientIP
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		return &Response{Msg: m}, nil
	})
	srv := startDoHServer(t, h, cert)

	client := dohHTTPClient(pool, "doh.test", &net.Dialer{
		// The distinct source address the assertion below keys on.
		LocalAddr: &net.TCPAddr{IP: net.ParseIP(altAddr)},
	})

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	code, _, _ := dohPost(t, client, srv.Addr(), packMsg(t, q))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want %d", code, http.StatusOK)
	}

	want := netip.MustParseAddr(altAddr)
	select {
	case ip := <-got:
		if !ip.IsValid() {
			t.Fatal("the handler saw no client address at all")
		}
		if ip != want {
			t.Errorf("handler saw ClientIP %v, want %v (the address the client actually dialled from -- the server is on 127.0.0.1, so this cannot be confused with the server's own address)", ip, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the handler never ran")
	}
}

// HTTP/2 is what makes a resolver capable of many outstanding queries per
// connection instead of one at a time; losing it is silent, because a
// server still answers correctly over plain HTTP/1.1 -- one request per
// connection, nothing failing, nothing logging. Request carries no HTTP
// details for a pipeline handler to inspect (nor should it: the plain and
// DoT paths have no *http.Request to describe), so the negotiated protocol
// is read off the client's own response -- it can only read "HTTP/2.0" if
// the TLS handshake with the real DoHServer negotiated h2 via ALPN and the
// exchange was actually framed as HTTP/2 end to end.
func TestDoHNegotiatesHTTP2(t *testing.T) {
	cert, pool := certtest.For(t, "doh.test")
	srv := startDoHServer(t, echoHandler(), cert)
	client := dohHTTPClient(pool, "doh.test", nil)

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	wire := packMsg(t, q)

	resp, err := client.Post("https://"+srv.Addr()+dohPath, dohContentType, bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.ProtoMajor != 2 || resp.Proto != "HTTP/2.0" {
		t.Errorf("response negotiated %s, want HTTP/2.0", resp.Proto)
	}
}

// NewDoHServer must not write into the caller's own NextProtos backing
// array while amending it to include "h2". tls.Config.Clone copies only the
// slice header, so a naive append(cfg.NextProtos, ...) after Clone can grow
// into spare capacity that the caller's own slice still points at --
// invisible today, but live the moment a caller derives more than one
// TLS config from a NextProtos slice with room to spare, which is exactly
// what a listener reconciler sharing one base config between DoT and DoH
// would do.
func TestDoHServerDoesNotAliasCallerNextProtos(t *testing.T) {
	cert, _ := certtest.For(t, "doh.test")

	// Built with spare capacity on purpose: cap 4 against a length-1 slice
	// leaves three unused slots in the backing array. That spare capacity is
	// exactly where an in-place append would land without reallocating --
	// the caller's own slice header (len 1) would not show it, but the
	// array underneath would carry it, and a second slice sharing that array
	// would see it appear.
	backing := make([]string, 1, 4)
	backing[0] = "http/1.1"
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   backing,
	}

	_ = NewDoHServer("127.0.0.1:0", echoHandler(), cfg)

	if !slices.Equal(cfg.NextProtos, []string{"http/1.1"}) {
		t.Errorf("NewDoHServer changed the caller's own NextProtos slice: got %v, want [http/1.1]", cfg.NextProtos)
	}
	// Reslice into the spare capacity behind the caller's slice -- legal in
	// Go up to cap, and the only way to see what an in-place append would
	// have written there without touching len or cap as seen through the
	// caller's own header.
	spare := backing[len(backing):cap(backing)]
	for i, s := range spare {
		if s != "" {
			t.Errorf("NewDoHServer wrote %q into the caller's spare capacity at index %d (backing array = %v): the append is aliasing tlsCfg.NextProtos instead of a clone of it", s, i, backing[:cap(backing)])
		}
	}
}

// rejectedBeforeThePipeline is a handler that fails the test if it ever
// runs. Every case in TestDoHRejectsMalformedRequests is refused ahead of
// the pipeline, so "the handler was never called" is half of what each of
// them asserts; the status code or rcode the client sees is the other half.
// Without it a subtest cannot tell a request the gate refused from one the
// pipeline forwarded upstream and then happened to answer the same way.
func rejectedBeforeThePipeline(t *testing.T) Handler {
	t.Helper()
	return HandlerFunc(func(_ context.Context, req *Request) (*Response, error) {
		t.Errorf("the pipeline ran for a request that should have been refused ahead of it: %v", req.Msg)
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		return &Response{Msg: m}, nil
	})
}

// dohPostReply POSTs m and returns the DNS message that came back, failing
// the test unless the exchange was the HTTP 200 a DNS-level refusal is
// carried inside.
func dohPostReply(t *testing.T, client *http.Client, addr string, m *dns.Msg) *dns.Msg {
	t.Helper()
	code, _, body := dohPost(t, client, addr, packMsg(t, m))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want %d (a DNS-level refusal is carried in the message, not the HTTP status)", code, http.StatusOK)
	}
	r := new(dns.Msg)
	if err := r.Unpack(body); err != nil {
		t.Fatalf("unpacking answer: %v", err)
	}
	return r
}

func TestDoHRejectsMalformedRequests(t *testing.T) {
	cert, pool := certtest.For(t, "doh.test")
	srv := startDoHServer(t, rejectedBeforeThePipeline(t), cert)
	client := dohHTTPClient(pool, "doh.test", nil)

	t.Run("wrong method", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodDelete, "https://"+srv.Addr()+dohPath, nil)
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
		}
	})

	t.Run("body is not a DNS message", func(t *testing.T) {
		code, _, _ := dohPost(t, client, srv.Addr(), []byte("this is not a dns message"))
		if code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", code, http.StatusBadRequest)
		}
	})

	t.Run("dns parameter is not valid base64url", func(t *testing.T) {
		u := "https://" + srv.Addr() + dohPath + "?dns=not-valid-base64url!!!"
		resp, err := client.Get(u)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
		}
	})

	// The rest are the rules miekg's DefaultMsgAcceptFunc applies to every
	// message arriving on :53, restated on the one transport with no miekg
	// dns.Server in front of it to apply them. A body that unpacks is not
	// yet a query: before this gate existed, an UPDATE opcode, a message
	// with the QR bit set, or a two-question query all entered the pipeline
	// and were forwarded upstream verbatim.
	t.Run("message is a response, not a query", func(t *testing.T) {
		m := new(dns.Msg)
		m.SetQuestion("example.com.", dns.TypeA)
		m.Response = true
		// miekg answers this on :53 with MsgIgnore -- send nothing at all,
		// so a forged response cannot be bounced off the server as an
		// amplifier. HTTP has no "say nothing" and needs none: whatever
		// goes back travels down the client's own connection. 400 says the
		// same thing in the only vocabulary this transport has.
		code, _, _ := dohPost(t, client, srv.Addr(), packMsg(t, m))
		if code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", code, http.StatusBadRequest)
		}
	})

	t.Run("opcode is not QUERY", func(t *testing.T) {
		m := new(dns.Msg)
		m.SetUpdate("example.com.")
		r := dohPostReply(t, client, srv.Addr(), m)
		if r.Rcode != dns.RcodeNotImplemented {
			t.Errorf("rcode = %v, want NOTIMP", dns.RcodeToString[r.Rcode])
		}
		if r.Opcode != dns.OpcodeUpdate {
			t.Errorf("opcode = %v, want the request's own UPDATE echoed back", dns.OpcodeToString[r.Opcode])
		}
	})

	t.Run("no question", func(t *testing.T) {
		m := new(dns.Msg)
		m.Id = dns.Id()
		m.RecursionDesired = true
		r := dohPostReply(t, client, srv.Addr(), m)
		if r.Rcode != dns.RcodeFormatError {
			t.Errorf("rcode = %v, want FORMERR", dns.RcodeToString[r.Rcode])
		}
	})

	t.Run("two questions", func(t *testing.T) {
		m := new(dns.Msg)
		m.Id = dns.Id()
		m.RecursionDesired = true
		m.Question = []dns.Question{
			{Name: "example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
			{Name: "example.org.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
		}
		r := dohPostReply(t, client, srv.Addr(), m)
		if r.Rcode != dns.RcodeFormatError {
			t.Errorf("rcode = %v, want FORMERR", dns.RcodeToString[r.Rcode])
		}
	})

	// The section caps are what keep miekg's "don't allow dynamic updates,
	// because then the sections can contain a whole bunch of RRs" true for
	// a message whose opcode claims QUERY.
	t.Run("oversized sections", func(t *testing.T) {
		rr := func(s string) dns.RR {
			t.Helper()
			r, err := dns.NewRR(s)
			if err != nil {
				t.Fatalf("building %q: %v", s, err)
			}
			return r
		}
		for _, tc := range []struct {
			name  string
			shape func(m *dns.Msg)
		}{
			{"two answers", func(m *dns.Msg) {
				m.Answer = []dns.RR{rr("a.example.com. 300 IN A 1.2.3.4"), rr("b.example.com. 300 IN A 1.2.3.5")}
			}},
			{"two authority records", func(m *dns.Msg) {
				m.Ns = []dns.RR{rr("example.com. 300 IN NS a.example.com."), rr("example.com. 300 IN NS b.example.com.")}
			}},
			{"three additional records", func(m *dns.Msg) {
				m.Extra = []dns.RR{
					rr("a.example.com. 300 IN A 1.2.3.4"),
					rr("b.example.com. 300 IN A 1.2.3.5"),
					rr("c.example.com. 300 IN A 1.2.3.6"),
				}
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				m := new(dns.Msg)
				m.SetQuestion("example.com.", dns.TypeA)
				tc.shape(m)
				r := dohPostReply(t, client, srv.Addr(), m)
				if r.Rcode != dns.RcodeFormatError {
					t.Errorf("rcode = %v, want FORMERR", dns.RcodeToString[r.Rcode])
				}
			})
		}
	})
}

// A NOTIFY still reaches the REFUSED intercept rather than being turned
// away by the accept gate ahead of it. miekg's DefaultMsgAcceptFunc lets
// OpcodeNotify through on :53 -- the gate's job is to reject what the
// server has no answer for at all, and dnsaur has an answer for a NOTIFY on
// every transport. Narrowing the gate to QUERY alone would answer NOTIMP
// here and silently change what a secondary sees depending on which
// transport it used.
func TestDoHNotifyPassesTheAcceptGate(t *testing.T) {
	cert, pool := certtest.For(t, "doh.test")
	srv := startDoHServer(t, rejectedBeforeThePipeline(t), cert)
	client := dohHTTPClient(pool, "doh.test", nil)

	// RFC 1996 §3.7: a NOTIFY may carry the SOA in its answer section, which
	// is also why the gate's answer-section cap is one RR and not zero.
	m := new(dns.Msg).SetNotify("example.com.")
	soa, err := dns.NewRR("example.com. 300 IN SOA ns.example.com. hostmaster.example.com. 7 3600 600 86400 300")
	if err != nil {
		t.Fatalf("building the SOA: %v", err)
	}
	m.Answer = []dns.RR{soa}

	r := dohPostReply(t, client, srv.Addr(), m)
	if r.Rcode != dns.RcodeRefused {
		t.Errorf("rcode = %v, want REFUSED (the NOTIFY intercept's answer, not the accept gate's NOTIMP)", dns.RcodeToString[r.Rcode])
	}
}

// An AXFR arriving over DoH is REFUSED rather than reaching the pipeline.
// Server.serve intercepts a transfer through its own dns.ResponseWriter,
// which this transport has no equivalent of, and RFC 8484 does not
// contemplate AXFR at all. The handler below fails the test if it is ever
// invoked, which is the only way to tell "refused before the pipeline" from
// "the pipeline itself decided to refuse it".
func TestDoHRefusesAXFR(t *testing.T) {
	cert, pool := certtest.For(t, "doh.test")
	h := HandlerFunc(func(_ context.Context, req *Request) (*Response, error) {
		t.Fatal("the pipeline ran for an AXFR query; it should have been refused before reaching here")
		return nil, nil
	})
	srv := startDoHServer(t, h, cert)
	client := dohHTTPClient(pool, "doh.test", nil)

	m := new(dns.Msg)
	m.SetAxfr("example.com.")
	code, _, body := dohPost(t, client, srv.Addr(), packMsg(t, m))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want %d (REFUSED is carried in the DNS message, not the HTTP status)", code, http.StatusOK)
	}
	r := new(dns.Msg)
	if err := r.Unpack(body); err != nil {
		t.Fatalf("unpacking answer: %v", err)
	}
	if r.Rcode != dns.RcodeRefused {
		t.Errorf("rcode = %v, want REFUSED", dns.RcodeToString[r.Rcode])
	}
}

// A NOTIFY arriving over DoH is refused for the same reason an AXFR is: see
// TestDoHRefusesAXFR. This is the specific bug rule 2 exists to prevent --
// E1's internal/app once let a NOTIFY reach the forwarder and forwarded it
// upstream.
func TestDoHRefusesNotify(t *testing.T) {
	cert, pool := certtest.For(t, "doh.test")
	h := HandlerFunc(func(_ context.Context, req *Request) (*Response, error) {
		t.Fatal("the pipeline ran for a NOTIFY; it should have been refused before reaching here")
		return nil, nil
	})
	srv := startDoHServer(t, h, cert)
	client := dohHTTPClient(pool, "doh.test", nil)

	m := new(dns.Msg).SetNotify("example.com.")
	code, _, body := dohPost(t, client, srv.Addr(), packMsg(t, m))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want %d", code, http.StatusOK)
	}
	r := new(dns.Msg)
	if err := r.Unpack(body); err != nil {
		t.Fatalf("unpacking answer: %v", err)
	}
	if r.Rcode != dns.RcodeRefused {
		t.Errorf("rcode = %v, want REFUSED", dns.RcodeToString[r.Rcode])
	}
}

// dohPostHeaders POSTs m and returns the HTTP response headers alongside
// the DNS message, for assertions about the HTTP envelope rather than the
// answer inside it.
func dohPostHeaders(t *testing.T, client *http.Client, addr string, m *dns.Msg) (http.Header, *dns.Msg) {
	t.Helper()
	resp, err := client.Post("https://"+addr+dohPath, dohContentType, bytes.NewReader(packMsg(t, m)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	r := new(dns.Msg)
	if err := r.Unpack(body); err != nil {
		t.Fatalf("unpacking answer: %v", err)
	}
	return resp.Header, r
}

// RFC 8484 §5.1: a DoH server assigns the response an explicit freshness
// lifetime, and it may not exceed the smallest TTL the response carries.
// Without one, every HTTP cache between the client and here is left to
// guess -- and an answer cached past its TTL is the one failure mode DNS
// has no way to correct.
func TestDoHCacheControlFollowsTheSmallestTTL(t *testing.T) {
	cert, pool := certtest.For(t, "doh.test")
	client := dohHTTPClient(pool, "doh.test", nil)

	newRR := func(t *testing.T, s string) dns.RR {
		t.Helper()
		rr, err := dns.NewRR(s)
		if err != nil {
			t.Fatalf("building %q: %v", s, err)
		}
		return rr
	}

	for _, tc := range []struct {
		name  string
		edns  bool
		reply func(t *testing.T, req *dns.Msg) *dns.Msg
		want  string
	}{
		{
			name: "the smallest TTL in the answer, not the first",
			reply: func(t *testing.T, req *dns.Msg) *dns.Msg {
				m := new(dns.Msg)
				m.SetReply(req)
				m.Answer = []dns.RR{
					newRR(t, "example.com. 300 IN A 1.2.3.4"),
					newRR(t, "example.com. 60 IN A 1.2.3.5"),
				}
				return m
			},
			want: "max-age=60",
		},
		{
			// RFC 2308: an NXDOMAIN's lifetime is the SOA's, and it rides
			// in the authority section, so a max-age read from the answer
			// section alone would be zero for every negative answer.
			name: "the SOA's negative TTL for an NXDOMAIN",
			reply: func(t *testing.T, req *dns.Msg) *dns.Msg {
				m := new(dns.Msg)
				m.SetRcode(req, dns.RcodeNameError)
				m.Ns = []dns.RR{newRR(t, "example.com. 120 IN SOA ns.example.com. hostmaster.example.com. 7 3600 600 86400 120")}
				return m
			},
			want: "max-age=120",
		},
		{
			name: "an answer with no records at all holds nothing",
			reply: func(_ *testing.T, req *dns.Msg) *dns.Msg {
				m := new(dns.Msg)
				m.SetRcode(req, dns.RcodeServerFailure)
				return m
			},
			want: "max-age=0",
		},
		{
			// An OPT's TTL field is the extended rcode and the DO bit, not
			// a lifetime (RFC 6891 §6.1.3). Counting it would answer
			// max-age=0 for every EDNS query, since those bits are
			// ordinarily all zero.
			name: "the OPT's flags are not a TTL",
			edns: true,
			reply: func(t *testing.T, req *dns.Msg) *dns.Msg {
				m := new(dns.Msg)
				m.SetReply(req)
				m.Answer = []dns.RR{newRR(t, "example.com. 300 IN A 1.2.3.4")}
				m.SetEdns0(1232, false)
				return m
			},
			want: "max-age=300",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startDoHServer(t, HandlerFunc(func(_ context.Context, req *Request) (*Response, error) {
				return &Response{Msg: tc.reply(t, req.Msg)}, nil
			}), cert)

			q := new(dns.Msg)
			q.SetQuestion("example.com.", dns.TypeA)
			if tc.edns {
				q.SetEdns0(1232, false)
			}
			header, _ := dohPostHeaders(t, client, srv.Addr(), q)
			if got := header.Get("Cache-Control"); got != tc.want {
				t.Errorf("Cache-Control = %q, want %q", got, tc.want)
			}
		})
	}
}

// The RFC 3225 §3 rule server_test.go's TestSynthesizedOPTCopiesTheDOBit
// pins on the plain and DoT path, restated on this one for the same reason
// as TestDoHNonEDNSGetsNoOPTFromACachedReply below.
func TestDoHSynthesizedOPTCopiesTheDOBit(t *testing.T) {
	cert, pool := certtest.For(t, "doh.test")
	srv := startDoHServer(t, noOPTHandler(), cert)
	client := dohHTTPClient(pool, "doh.test", nil)

	for _, do := range []bool{true, false} {
		t.Run(fmt.Sprintf("do=%t", do), func(t *testing.T) {
			q := new(dns.Msg)
			q.SetQuestion("example.com.", dns.TypeA)
			q.SetEdns0(4096, do)

			r := dohPostReply(t, client, srv.Addr(), q)
			opt := r.IsEdns0()
			if opt == nil {
				t.Fatal("an EDNS query got no OPT back")
			}
			if opt.Do() != do {
				t.Errorf("reply DO = %t, want the query's own %t", opt.Do(), do)
			}
		})
	}
}

// The RFC 6891 §7 rule server_test.go's TestNonEDNSGetsNoOPTFromACachedReply
// pins on the plain and DoT path, restated here because the two paths shape
// their replies through the same helper and a transport-specific regression
// would otherwise show up on only one of them.
func TestDoHNonEDNSGetsNoOPTFromACachedReply(t *testing.T) {
	cert, pool := certtest.For(t, "doh.test")
	srv := startDoHServer(t, optCarryingHandler(), cert)
	client := dohHTTPClient(pool, "doh.test", nil)

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	// No EDNS: this client never said it could read an OPT.

	r := dohPostReply(t, client, srv.Addr(), q)
	if opt := r.IsEdns0(); opt != nil {
		t.Errorf("a non-EDNS query got an OPT back carrying %v", opt.Option)
	}
	if len(r.Answer) != 1 {
		t.Errorf("answer records = %d, want the answer itself left alone", len(r.Answer))
	}
}

// TSIG is never verified over DoH: there is no miekg parse on this path for
// a TsigProvider to run during, so a handler asking RequireTSIG must be told
// "unavailable", never mistake a signature nobody checked for one that
// checked out fine.
func TestDoHRequireTSIGReportsUnavailable(t *testing.T) {
	cert, pool := certtest.For(t, "doh.test")
	errs := make(chan error, 1)
	h := HandlerFunc(func(_ context.Context, req *Request) (*Response, error) {
		_, err := req.RequireTSIG()
		errs <- err
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		return &Response{Msg: m}, nil
	})
	srv := startDoHServer(t, h, cert)
	client := dohHTTPClient(pool, "doh.test", nil)

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	if code, _, _ := dohPost(t, client, srv.Addr(), packMsg(t, q)); code != http.StatusOK {
		t.Fatalf("status = %d, want %d", code, http.StatusOK)
	}

	select {
	case err := <-errs:
		if !errIsTSIGUnavailable(err) {
			t.Errorf("RequireTSIG() error = %v, want ErrTSIGUnavailable", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the handler never ran")
	}
}

// errIsTSIGUnavailable exists only so the assertion above reads as a
// sentence; errors.Is would do exactly this and is used for parity with the
// rest of the package's style elsewhere, but a direct comparison is exact
// here since ErrTSIGUnavailable is never wrapped on this path.
func errIsTSIGUnavailable(err error) bool { return err == ErrTSIGUnavailable }

// infiniteReader never returns EOF and counts every byte it hands out, so a
// test can tell a bounded read from an unbounded one without needing an
// actually gigantic body on the wire.
type infiniteReader struct{ n atomic.Int64 }

func (r *infiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'A'
	}
	r.n.Add(int64(len(p)))
	return len(p), nil
}

// panicReader fails the test the instant anything reads from it, so a test
// can prove a body was never touched at all rather than merely truncated.
type panicReader struct{ t *testing.T }

func (r panicReader) Read([]byte) (int, error) {
	r.t.Fatal("the body was read from at all; a Content-Length already over the limit must be rejected before any read")
	return 0, nil
}

// A request whose Content-Length already announces more than dns.MaxMsgSize
// is rejected without a single byte of its body being read.
func TestDoHRejectsOversizedContentLengthWithoutReading(t *testing.T) {
	cert, _ := certtest.For(t, "doh.test")
	srv := startDoHServer(t, echoHandler(), cert)

	req := httptest.NewRequest(http.MethodPost, dohPath, panicReader{t: t})
	req.ContentLength = dns.MaxMsgSize + 1
	req.Header.Set("Content-Type", dohContentType)

	rec := httptest.NewRecorder()
	srv.handle(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

// A body with no announced (or a lying) Content-Length is still bounded at
// dns.MaxMsgSize: the handler stops reading at the limit rather than
// consuming whatever the client keeps sending. infiniteReader would hang
// io.ReadAll forever if the read here were not bounded, so this test failing
// to complete is itself evidence of the bug, not just the assertion below.
func TestDoHBoundsUnknownLengthBody(t *testing.T) {
	cert, _ := certtest.For(t, "doh.test")
	srv := startDoHServer(t, echoHandler(), cert)

	body := &infiniteReader{}
	req := httptest.NewRequest(http.MethodPost, dohPath, body)
	req.ContentLength = -1 // unknown/streamed, so the pre-check cannot short-circuit this
	req.Header.Set("Content-Type", dohContentType)

	done := make(chan struct{})
	rec := httptest.NewRecorder()
	go func() {
		srv.handle(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handle did not return -- the body read is not bounded")
	}

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if n := body.n.Load(); n > dns.MaxMsgSize+1 {
		t.Errorf("read %d bytes from an unbounded body, want at most %d", n, dns.MaxMsgSize+1)
	}
}

// DoH answers identically to plain DNS for the same question against the
// same handler. This is the test that would catch a DoH transport wired
// beside the pipeline instead of into it -- every other test in this file
// would still pass if DoHServer answered from somewhere else entirely, since
// none of them compares against a second, independent transport sharing the
// same Handler value.
func TestDoHAnswersMatchPlainDNS(t *testing.T) {
	cert, pool := certtest.For(t, "doh.test")
	h := echoHandler()

	plain := NewServer("127.0.0.1:0", h)
	if err := plain.Start(); err != nil {
		t.Fatalf("plain Start: %v", err)
	}
	t.Cleanup(func() { _ = plain.Shutdown(context.Background()) })

	doh := startDoHServer(t, h, cert)
	client := dohHTTPClient(pool, "doh.test", nil)

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)

	dc := &dns.Client{Net: "tcp", Timeout: 5 * time.Second}
	plainResp, _, err := dc.Exchange(q, plain.Addr())
	if err != nil {
		t.Fatalf("plain exchange: %v", err)
	}

	code, _, body := dohPost(t, client, doh.Addr(), packMsg(t, q))
	if code != http.StatusOK {
		t.Fatalf("doh status = %d, want %d", code, http.StatusOK)
	}
	dohResp := new(dns.Msg)
	if err := dohResp.Unpack(body); err != nil {
		t.Fatalf("unpacking doh answer: %v", err)
	}

	if plainResp.Rcode != dohResp.Rcode {
		t.Errorf("rcode differs: plain %v, doh %v", dns.RcodeToString[plainResp.Rcode], dns.RcodeToString[dohResp.Rcode])
	}
	if len(plainResp.Answer) != len(dohResp.Answer) {
		t.Fatalf("answer count differs: plain %d, doh %d", len(plainResp.Answer), len(dohResp.Answer))
	}
	for i := range plainResp.Answer {
		if plainResp.Answer[i].String() != dohResp.Answer[i].String() {
			t.Errorf("answer[%d] differs: plain %v, doh %v", i, plainResp.Answer[i], dohResp.Answer[i])
		}
	}
}

// dohPostTyped POSTs wire under an arbitrary Content-Type, including none
// at all (contentType == ""), which is what the media-type check below has
// to distinguish. dohPost always sends the correct one, which is exactly
// why nothing pinned this until now.
func dohPostTyped(t *testing.T, client *http.Client, addr, contentType string, wire []byte) int {
	t.Helper()
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, "https://"+addr+dohPath, bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// A POST is answered only when it is typed application/dns-message.
//
// RFC 8484 §4.1 words the POST form as carrying that media type, and §11 of
// the design claims RFC 8484 conformance. Answering application/json with a
// 200 is not a hole — the body still has to be a wire-format DNS message to
// get past Unpack — but it hides a client's misconfiguration until that
// client meets a resolver that does check, at which point the bug surfaces
// somewhere else entirely.
func TestDoHRequiresTheDNSMessageContentType(t *testing.T) {
	cert, pool := certtest.For(t, "doh.test")
	srv := startDoHServer(t, echoHandler(), cert)
	client := dohHTTPClient(pool, "doh.test", nil)
	wire := packMsg(t, query("typed.example."))

	for _, tc := range []struct {
		name        string
		contentType string
		want        int
	}{
		{"the RFC's media type", dohContentType, http.StatusOK},
		// A parameter is legal on any media type and changes nothing about
		// which type it is, so a strict string compare would wrongly refuse
		// this one.
		{"with a parameter", dohContentType + "; charset=utf-8", http.StatusOK},
		{"the wrong media type", "application/json", http.StatusUnsupportedMediaType},
		{"a near miss", "application/dns-json", http.StatusUnsupportedMediaType},
		{"no Content-Type at all", "", http.StatusUnsupportedMediaType},
		{"an unparseable Content-Type", "application/;;;", http.StatusUnsupportedMediaType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dohPostTyped(t, client, srv.Addr(), tc.contentType, wire); got != tc.want {
				t.Errorf("POST with Content-Type %q = %d, want %d", tc.contentType, got, tc.want)
			}
		})
	}
}

// A GET is not subject to the POST's media-type rule: it carries no body to
// type. Pinned so a later tightening of checkDoHContentType cannot quietly
// start refusing the whole GET form, which RFC 8484 §4.1 defines as the
// cache-friendly half of the protocol.
func TestDoHGetNeedsNoContentType(t *testing.T) {
	cert, pool := certtest.For(t, "doh.test")
	srv := startDoHServer(t, echoHandler(), cert)
	client := dohHTTPClient(pool, "doh.test", nil)

	status, _, _ := dohGet(t, client, srv.Addr(), packMsg(t, query("gettable.example.")))
	if status != http.StatusOK {
		t.Errorf("GET returned %d, want 200", status)
	}
}

// The address a DoH client is filed under carries no IPv6 zone, so it is
// the same address every other transport produces for the same client.
//
// Server.serve builds its netip.Addr from net.TCPAddr.IP / net.UDPAddr.IP,
// which have no zone field to lose; DoH reads r.RemoteAddr as text, where
// the zone survives. clients.Registry.Lookup misses on a zoned Addr twice
// over — netip.Prefix.Contains refuses one outright, and the exact-match
// map is keyed by a zoneless Addr — so the divergence is not cosmetic: the
// same client would get its own group over DoT and the default group's
// blocklists over DoH.
//
// A unit test on clientAddr rather than a served request, because a
// link-local peer needs a link-local destination, and no CI host can be
// relied on to have one. The seam under test is the parse, and this reaches
// it exactly.
func TestDoHClientAddrDropsTheIPv6Zone(t *testing.T) {
	for _, tc := range []struct {
		remote string
		want   string
	}{
		{"[fe80::1%eth0]:9000", "fe80::1"},
		{"[fe80::1]:9000", "fe80::1"},
		{"192.0.2.7:9000", "192.0.2.7"},
		{"[::ffff:192.0.2.7]:9000", "192.0.2.7"},
	} {
		t.Run(tc.remote, func(t *testing.T) {
			got := clientAddr(&http.Request{RemoteAddr: tc.remote})
			if got.String() != tc.want {
				t.Errorf("clientAddr(%q) = %q, want %q", tc.remote, got.String(), tc.want)
			}
			if got.Zone() != "" {
				t.Errorf("clientAddr(%q) kept the zone %q; clients.Registry.Lookup cannot match a zoned address", tc.remote, got.Zone())
			}
		})
	}
}
