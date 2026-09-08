package dnssrv

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/certtest"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// startDoTServerForPadding brings up a DoT server with h as its handler, plus
// any extra opts (WithTSIGKeys, say) layered on top of WithTLS. Returns the
// bound address and the pool that trusts its certificate.
func startDoTServerForPadding(t *testing.T, h Handler, opts ...Option) (string, *x509.CertPool) {
	t.Helper()
	cert, pool := certtest.For(t, "pad.test")
	all := append([]Option{WithTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})}, opts...)
	s := NewServer("127.0.0.1:0", h, all...)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Logf("shutdown error: %v", err)
		}
	})
	return s.Addr(), pool
}

// dotClient dials the server startDoTServerForPadding started, trusting its
// certificate.
func dotClient(pool *x509.CertPool) *dns.Client {
	return &dns.Client{
		Net:       "tcp-tls",
		Timeout:   5 * time.Second,
		TLSConfig: &tls.Config{ServerName: "pad.test", RootCAs: pool, MinVersion: tls.VersionTLS12},
	}
}

// paddedQuery returns a query for name carrying an RFC 8467 §4.2 EDNS(0)
// Padding option, the same shape E1's client-side exchangers send.
func paddedQuery(t *testing.T, name string) *dns.Msg {
	t.Helper()
	m := query(name)
	if err := Pad(m, PaddingBlockQuery); err != nil {
		t.Fatalf("Pad: %v", err)
	}
	return m
}

// Test 1: a padded query over DoT gets a response whose wire length is a
// multiple of PaddingBlockResponse.
func TestServePaddingDoTPadsPaddedQuery(t *testing.T) {
	addr, pool := startDoTServerForPadding(t, echoHandler())
	c := dotClient(pool)
	m := paddedQuery(t, "padded.example.")

	r, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if !hasPadding(r) {
		t.Fatal("response carries no Padding option, even though the query did")
	}
	wire, err := r.Pack()
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if len(wire)%PaddingBlockResponse != 0 {
		t.Errorf("response wire length %d is not a multiple of %d", len(wire), PaddingBlockResponse)
	}
}

// Test 2: an unpadded query over DoT gets an unpadded response. What
// triggers padding is the client's own Padding option, not the transport by
// itself -- an EDNS query with no Padding option must not come back padded.
func TestServePaddingDoTLeavesUnpaddedQueryUnpadded(t *testing.T) {
	addr, pool := startDoTServerForPadding(t, echoHandler())
	c := dotClient(pool)
	m := query("unpadded.example.")
	m.SetEdns0(1232, false) // EDNS, but deliberately no Padding option

	r, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if hasPadding(r) {
		t.Error("response carries a Padding option even though the query never asked for one")
	}
}

// Test 3: a padded query over plain TCP gets an unpadded response. This is
// the test a refactor hoisting Pad above the encrypted-transport check would
// fail -- without it, nothing here distinguishes "pads correctly" from
// "pads everything".
func TestServePaddingPlainTCPNeverPads(t *testing.T) {
	s := NewServer("127.0.0.1:0", echoHandler())
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Logf("shutdown error: %v", err)
		}
	})

	c := &dns.Client{Net: "tcp", Timeout: 5 * time.Second}
	m := paddedQuery(t, "plaintext.example.")

	r, _, err := c.Exchange(m, s.Addr())
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if hasPadding(r) {
		t.Error("a plaintext reply came back padded -- an observer already sees the name on this transport, so padding it only wastes bytes, and this happening at all means the encrypted-only check was lost")
	}
}

// Test 4: a signed, padded query over DoT still verifies. This does not pin
// down where padding runs relative to ReplyTSIG -- ReplyTSIG only builds an
// unsigned stub, and the actual MAC is computed later, inside miekg's
// WriteMsg, over whatever the message holds at that point, so nothing about
// ordering relative to ReplyTSIG is load-bearing here. What this does prove
// is that padding and TSIG signing coexist correctly: the padding option
// lands in the OPT record without disturbing anything TsigGenerateWithProvider
// needs, so dns.Client's own MAC check inside Exchange (client.go's
// Conn.ReadMsg) still accepts the reply. Padding that corrupted the OPT
// record in a way that broke signing would fail here.
func TestServePaddingSignedDoTStillVerifies(t *testing.T) {
	const keyName = "pad.e412.in."
	keys := newFakeKeys(store.TSIGKey{
		Name: keyName, Algorithm: dns.HmacSHA256, Secret: goodSecret,
	})
	addr, pool := startDoTServerForPadding(t, echoHandler(), WithTSIGKeys(keys))
	c := dotClient(pool)
	c.TsigSecret = map[string]string{keyName: goodSecret}

	m := paddedQuery(t, "signed.example.")
	m.SetTsig(keyName, dns.HmacSHA256, 300, time.Now().Unix())

	r, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("Exchange: %v (a verification failure here means padding disturbed something TSIG signing needed)", err)
	}
	if r.IsTsig() == nil {
		t.Fatal("reply carried no TSIG; a signed, verified query must get a signed answer")
	}
	if !hasPadding(r) {
		t.Error("reply carries no Padding option, even though the query did")
	}
}

// Test 5: a padded query over DoH gets a padded response.
//
// doh.go's Pad call is a second, independent call site from Server.serve's:
// the DoH handler is its own type with its own response path, and nothing
// it does is reached by any of the four tests above. Until this existed the
// whole of DoH's padding was uncovered — a change that dropped the call, or
// inverted the hasPadding(m) test, would have gone out green.
//
// The length is measured on the response body itself rather than on a
// repacked dns.Msg: on this transport the body *is* the wire message, so
// this is the exact number of bytes an observer counts.
func TestServePaddingDoHPadsPaddedQuery(t *testing.T) {
	cert, pool := certtest.For(t, "doh.test")
	srv := startDoHServer(t, echoHandler(), cert)
	client := dohHTTPClient(pool, "doh.test", nil)

	m := paddedQuery(t, "padded.example.")
	status, _, body := dohPost(t, client, srv.Addr(), packMsg(t, m))
	if status != 200 {
		t.Fatalf("POST returned %d, want 200", status)
	}
	if len(body)%PaddingBlockResponse != 0 {
		t.Errorf("response is %d bytes, not a multiple of %d", len(body), PaddingBlockResponse)
	}

	r := new(dns.Msg)
	if err := r.Unpack(body); err != nil {
		t.Fatalf("unpacking the response: %v", err)
	}
	if !hasPadding(r) {
		t.Error("response carries no Padding option, even though the query did")
	}
}

// Test 6: an unpadded query over DoH gets an unpadded response. Without
// this, Test 5 alone cannot tell "pads when asked" from "pads everything" —
// and padding every DoH reply to 468 bytes would inflate every answer on
// the transport for no benefit.
func TestServePaddingDoHLeavesUnpaddedQueryUnpadded(t *testing.T) {
	cert, pool := certtest.For(t, "doh.test")
	srv := startDoHServer(t, echoHandler(), cert)
	client := dohHTTPClient(pool, "doh.test", nil)

	m := query("unpadded.example.")
	m.SetEdns0(1232, false) // EDNS, but deliberately no Padding option
	status, _, body := dohPost(t, client, srv.Addr(), packMsg(t, m))
	if status != 200 {
		t.Fatalf("POST returned %d, want 200", status)
	}

	r := new(dns.Msg)
	if err := r.Unpack(body); err != nil {
		t.Fatalf("unpacking the response: %v", err)
	}
	if hasPadding(r) {
		t.Error("response carries a Padding option even though the query never asked for one")
	}
}
