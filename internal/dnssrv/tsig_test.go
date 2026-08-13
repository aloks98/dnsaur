package dnssrv

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// The two secrets are both valid base64 and the same length, so the only thing
// separating them at verify time is the MAC.
const (
	goodSecret  = "c2VjcmV0LXNlY3JldC1zZWNyZXQ="
	wrongSecret = "d3Jvbmctd3Jvbmctd3JvbmctIQ=="
)

// fakeKeys is the slice of store.TSIGKeyStore the provider consumes. The real
// store is exercised in internal/store; what matters here is that the provider
// asks on every message, which a map with a mutex shows just as well and lets a
// test add a key while the server is already running.
type fakeKeys struct {
	mu   sync.Mutex
	keys map[string]store.TSIGKey
	err  error
	hits int
}

func newFakeKeys(ks ...store.TSIGKey) *fakeKeys {
	f := &fakeKeys{keys: map[string]store.TSIGKey{}}
	for _, k := range ks {
		f.keys[k.Name] = k
	}
	return f
}

// ByName returns whatever it found *alongside* any failure, rather than a zero
// key. A real store would not, but a fake that zeroes the key on error lets a
// message die at the algorithm check instead of at the error check, and then
// the store-failure test passes whether or not the provider handles the error
// at all. Handing back a usable key makes that test depend on the error check
// and nothing else.
func (f *fakeKeys) ByName(_ context.Context, name string) (store.TSIGKey, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	k, ok := f.keys[name]
	return k, ok, f.err
}

func (f *fakeKeys) add(k store.TSIGKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[k.Name] = k
}

func (f *fakeKeys) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeKeys) lookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// tsigResult is what the handler saw when it asked whether the message it was
// given had been authenticated.
type tsigResult struct {
	key string
	err error
}

// startTSIGServer brings up a server whose provider is backed by keys, and
// returns its address plus a channel carrying the handler's view of each
// request's TSIG state.
func startTSIGServer(t *testing.T, keys TSIGKeys) (string, <-chan tsigResult) {
	t.Helper()
	seen := make(chan tsigResult, 8)
	h := HandlerFunc(func(_ context.Context, req *Request) (*Response, error) {
		key, err := req.RequireTSIG()
		seen <- tsigResult{key: key, err: err}
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		rr, _ := dns.NewRR(req.Msg.Question[0].Name + " 300 IN A 1.2.3.4")
		m.Answer = append(m.Answer, rr)
		return &Response{Msg: m, Decision: DecisionAuthoritative}, nil
	})
	s := NewServer("127.0.0.1:0", h, WithTSIGKeys(keys))
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Logf("shutdown error: %v", err)
		}
	})
	return s.Addr(), seen
}

// signedExchange sends a query signed as keyName with secret and returns the
// reply. Any client-side error is returned rather than fatal: a reply the
// server refused to sign is a valid outcome for some of these tests.
func signedExchange(t *testing.T, addr, keyName, secret string) (*dns.Msg, error) {
	t.Helper()
	c := new(dns.Client)
	c.TsigSecret = map[string]string{keyName: secret}
	m := new(dns.Msg)
	m.SetQuestion("bifrost.e412.in.", dns.TypeA)
	m.SetTsig(keyName, dns.HmacSHA256, 300, time.Now().Unix())
	reply, _, err := c.Exchange(m, addr)
	return reply, err
}

func mustSee(t *testing.T, seen <-chan tsigResult) tsigResult {
	t.Helper()
	select {
	case r := <-seen:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("handler never ran")
		return tsigResult{}
	}
}

// A signed query verifies and the reply is signed back. RFC 8945 permits TSIG
// on any message, so this is exercisable before transfers exist. The client
// holds the same secret, so c.Exchange also verifies the reply's MAC -- if the
// provider's Generate disagreed with miekg's by a single byte the exchange
// would fail here rather than pass with a wrong signature.
func TestTsigSignedQueryVerifies(t *testing.T) {
	keys := newFakeKeys(store.TSIGKey{
		Name: "xfer.e412.in.", Algorithm: dns.HmacSHA256, Secret: goodSecret,
	})
	addr, seen := startTSIGServer(t, keys)

	reply, err := signedExchange(t, addr, "xfer.e412.in.", goodSecret)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if reply.IsTsig() == nil {
		t.Fatal("reply carried no TSIG; a signed request must get a signed answer")
	}
	got := mustSee(t, seen)
	if got.err != nil {
		t.Fatalf("handler saw TSIG error %v; want a verified message", got.err)
	}
	if got.key != "xfer.e412.in." {
		t.Fatalf("handler saw key %q; want xfer.e412.in.", got.key)
	}
}

// The half that matters: a wrong secret must not verify.
func TestTsigWrongSecretFailsVerification(t *testing.T) {
	keys := newFakeKeys(store.TSIGKey{
		Name: "xfer.e412.in.", Algorithm: dns.HmacSHA256, Secret: goodSecret,
	})
	addr, seen := startTSIGServer(t, keys)

	reply, err := signedExchange(t, addr, "xfer.e412.in.", wrongSecret)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	got := mustSee(t, seen)
	if got.err == nil {
		t.Fatalf("handler saw key %q with no error; a message signed with the wrong secret verified", got.key)
	}
	if reply.IsTsig() != nil {
		t.Fatal("reply was signed; the server must not vouch for a message it could not authenticate")
	}
}

// An unknown key name is not an error the server can sign its way out of.
func TestTsigUnknownKeyNameFailsVerification(t *testing.T) {
	keys := newFakeKeys(store.TSIGKey{
		Name: "xfer.e412.in.", Algorithm: dns.HmacSHA256, Secret: goodSecret,
	})
	addr, seen := startTSIGServer(t, keys)

	reply, err := signedExchange(t, addr, "nosuch.e412.in.", goodSecret)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	got := mustSee(t, seen)
	if got.err == nil {
		t.Fatalf("handler saw key %q with no error; an unknown key name verified", got.key)
	}
	if reply.IsTsig() != nil {
		t.Fatal("reply was signed under a key the server does not have")
	}
}

// The provider reads through to the store on every signed message rather than
// caching, which is what keeps a deleted key from outliving its deletion.
// Pinned as a test because a cache is an easy, quiet thing to add later.
func TestTsigStoreIsConsultedPerMessage(t *testing.T) {
	keys := newFakeKeys(store.TSIGKey{
		Name: "xfer.e412.in.", Algorithm: dns.HmacSHA256, Secret: goodSecret,
	})
	addr, seen := startTSIGServer(t, keys)

	if _, err := signedExchange(t, addr, "xfer.e412.in.", goodSecret); err != nil {
		t.Fatalf("first exchange: %v", err)
	}
	<-seen
	first := keys.lookups()
	if first == 0 {
		t.Fatal("a signed message did not reach the key store at all")
	}
	if _, err := signedExchange(t, addr, "xfer.e412.in.", goodSecret); err != nil {
		t.Fatalf("second exchange: %v", err)
	}
	<-seen
	if keys.lookups() <= first {
		t.Fatalf("lookups went %d -> %d; the second message was answered from a cache", first, keys.lookups())
	}
}

// The reason this is a provider and not dns.Server.TsigSecret: the store is
// consulted per message, so a key created through the API works on the next
// packet rather than the next restart.
func TestTsigKeyAddedAfterStartNeedsNoRestart(t *testing.T) {
	keys := newFakeKeys()
	addr, seen := startTSIGServer(t, keys)

	if _, err := signedExchange(t, addr, "late.e412.in.", goodSecret); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if got := mustSee(t, seen); got.err == nil {
		t.Fatal("a key that does not exist yet verified")
	}

	keys.add(store.TSIGKey{Name: "late.e412.in.", Algorithm: dns.HmacSHA256, Secret: goodSecret})

	reply, err := signedExchange(t, addr, "late.e412.in.", goodSecret)
	if err != nil {
		t.Fatalf("exchange after add: %v", err)
	}
	if got := mustSee(t, seen); got.err != nil {
		t.Fatalf("key added after Start did not verify: %v", got.err)
	}
	if reply.IsTsig() == nil {
		t.Fatal("reply carried no TSIG after the key was added")
	}
}

// A store that cannot answer must not be read as "no signature required".
// The key it fails on is the right key, signed with the right secret and the
// right algorithm, so the only thing standing between this message and a
// verification is the provider handling the error.
func TestTsigStoreFailureDoesNotVerify(t *testing.T) {
	keys := newFakeKeys(store.TSIGKey{
		Name: "xfer.e412.in.", Algorithm: dns.HmacSHA256, Secret: goodSecret,
	})
	keys.fail(errors.New("database is locked"))
	addr, seen := startTSIGServer(t, keys)

	reply, err := signedExchange(t, addr, "xfer.e412.in.", goodSecret)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if got := mustSee(t, seen); got.err == nil {
		t.Fatal("a message verified while the key store was failing")
	}
	if reply.IsTsig() != nil {
		t.Fatal("reply was signed while the key store was failing")
	}
}

// RFC 8945 §5.2.3: a correctly signed message outside the fudge window is
// still refused, which is what stops a captured signed message being replayed
// later. The window check is miekg's (tsig.go:243-252, run after the MAC
// deliberately, per CVE-2017-3142/3143) rather than code written here, but it
// only reaches a caller because the provider is attached and RequireTSIG
// surfaces the status -- both of which are this package's doing, and neither
// of which any other test covers for a message whose MAC is perfectly good.
// No clock injection is needed: the sender picks TimeSigned.
func TestTsigStaleTimestampFailsVerification(t *testing.T) {
	keys := newFakeKeys(store.TSIGKey{
		Name: "xfer.e412.in.", Algorithm: dns.HmacSHA256, Secret: goodSecret,
	})
	addr, seen := startTSIGServer(t, keys)

	c := new(dns.Client)
	c.TsigSecret = map[string]string{"xfer.e412.in.": goodSecret}
	m := new(dns.Msg)
	m.SetQuestion("bifrost.e412.in.", dns.TypeA)
	// Right key, right secret, right algorithm, valid MAC -- signed two hours
	// ago against a 300-second fudge.
	m.SetTsig("xfer.e412.in.", dns.HmacSHA256, 300, time.Now().Add(-2*time.Hour).Unix())
	reply, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	got := mustSee(t, seen)
	if !errors.Is(got.err, dns.ErrTime) {
		t.Fatalf("stale message gave key %q err %v; want dns.ErrTime", got.key, got.err)
	}
	if reply.IsTsig() != nil {
		t.Fatal("reply was signed for a message outside the time window")
	}
}

// The trap the helper exists for: miekg leaves TsigStatus() nil for a message
// that carried no TSIG at all, so a handler that only checks the status treats
// an unsigned message as authenticated.
func TestTsigUnsignedMessageIsNotVerified(t *testing.T) {
	keys := newFakeKeys(store.TSIGKey{
		Name: "xfer.e412.in.", Algorithm: dns.HmacSHA256, Secret: goodSecret,
	})
	addr, seen := startTSIGServer(t, keys)

	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion("bifrost.e412.in.", dns.TypeA)
	if _, _, err := c.Exchange(m, addr); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	got := mustSee(t, seen)
	if !errors.Is(got.err, ErrTSIGUnsigned) {
		t.Fatalf("unsigned message gave err %v; want ErrTSIGUnsigned", got.err)
	}
}

// A server with no provider verified nothing, so it must not report success
// either -- miekg leaves the status nil in that case too.
func TestTsigNoProviderReportsUnavailable(t *testing.T) {
	seen := make(chan tsigResult, 4)
	h := HandlerFunc(func(_ context.Context, req *Request) (*Response, error) {
		key, err := req.RequireTSIG()
		seen <- tsigResult{key: key, err: err}
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		return &Response{Msg: m, Decision: DecisionAuthoritative}, nil
	})
	s := NewServer("127.0.0.1:0", h)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Logf("shutdown error: %v", err)
		}
	}()

	if _, err := signedExchange(t, s.Addr(), "xfer.e412.in.", goodSecret); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if got := mustSee(t, seen); !errors.Is(got.err, ErrTSIGUnavailable) {
		t.Fatalf("no-provider server gave err %v; want ErrTSIGUnavailable", got.err)
	}
}

// The stored algorithm is part of the key, not a hint: a message that names a
// different one is a different key and must not verify.
func TestTsigAlgorithmMismatchFailsVerification(t *testing.T) {
	keys := newFakeKeys(store.TSIGKey{
		Name: "xfer.e412.in.", Algorithm: dns.HmacSHA512, Secret: goodSecret,
	})
	addr, seen := startTSIGServer(t, keys)

	// signedExchange always signs with hmac-sha256.
	if _, err := signedExchange(t, addr, "xfer.e412.in.", goodSecret); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if got := mustSee(t, seen); got.err == nil {
		t.Fatal("a message signed with an algorithm the key is not configured for verified")
	}
}

// ----------------------------------------------------------------------------
// UDP size: a signed reply must still fit what the client can receive.
// ----------------------------------------------------------------------------

// sizeQName is the question the size tests ask. Its wire form is 17 bytes, so
// the sizes named below are reproducible arithmetic rather than magic numbers.
const sizeQName = "bifrost.e412.in."

// answerOfLen builds an answer section for a reply to qname whose whole
// uncompressed message comes to exactly want bytes. Bulk is A records; a
// trailing TXT is padded to land on the byte. Sizing the reply rather than
// counting records is what lets a test say "479 bytes" -- the number the
// reviewer measured -- instead of "fifteen-ish records".
func answerOfLen(t *testing.T, qname string, want int) []dns.RR {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(qname, dns.TypeA)
	a, err := dns.NewRR(qname + " 300 IN A 1.2.3.4")
	if err != nil {
		t.Fatalf("building filler record: %v", err)
	}
	txt := &dns.TXT{
		Hdr: dns.RR_Header{Name: qname, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 300},
		Txt: []string{""},
	}
	for m.Len()+dns.Len(a)+dns.Len(txt) <= want {
		m.Answer = append(m.Answer, dns.Copy(a))
	}
	pad := want - m.Len() - dns.Len(txt)
	if pad < 0 {
		t.Fatalf("cannot build a %d-byte reply for %s: the smallest one is %d bytes",
			want, qname, m.Len()+dns.Len(txt))
	}
	txt.Txt = []string{strings.Repeat("x", pad)}
	return append(m.Answer, txt)
}

// tsigServerAnswering brings up a signing server that answers every question
// with the same records, so a test controls the size of the reply.
func tsigServerAnswering(t *testing.T, keys TSIGKeys, answer []dns.RR) string {
	t.Helper()
	return tsigServerShaping(t, keys, func(m *dns.Msg) {
		m.Answer = append(m.Answer, answer...)
	})
}

// tsigServerShaping is tsigServerAnswering for a reply that is not just an
// answer section -- a negative answer or a delegation, which put their records
// in the authority section instead.
func tsigServerShaping(t *testing.T, keys TSIGKeys, shape func(*dns.Msg)) string {
	t.Helper()
	h := HandlerFunc(func(_ context.Context, req *Request) (*Response, error) {
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		shape(m)
		return &Response{Msg: m, Decision: DecisionAuthoritative}, nil
	})
	s := NewServer("127.0.0.1:0", h, WithTSIGKeys(keys))
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Logf("shutdown error: %v", err)
		}
	})
	return s.Addr()
}

// rawUDPExchange sends q over a plain UDP socket and returns the reply exactly
// as it arrived, in bytes.
//
// dns.Client cannot measure this. It reads a UDP reply into a buffer of its own
// UDPSize -- 512 by default -- so an over-size answer comes back as an unpack
// error rather than as a number, which is precisely the "malformed response"
// the bug looked like from the outside. The read buffer here is deliberately
// large enough to catch a server that overshoots.
func rawUDPExchange(t *testing.T, addr string, q []byte) []byte {
	t.Helper()
	c, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	if _, err := c.Write(q); err != nil {
		t.Fatalf("write query: %v", err)
	}
	buf := make([]byte, dns.MaxMsgSize)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	return buf[:n]
}

// signedQueryBytes packs a query the way a client puts it on the wire and
// returns the request's MAC alongside, which a reply's MAC is computed over
// (RFC 8945 §5.3 lists the request MAC first among an answer's digest
// components) and which a test therefore needs to verify the answer.
//
// udpSize == 0 leaves the query non-EDNS. That is the case the bug lives in: a
// client that advertises nothing has a bare 512-byte budget.
func signedQueryBytes(t *testing.T, qname string, udpSize uint16) (query []byte, requestMAC string) {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(qname, dns.TypeA)
	if udpSize > 0 {
		m.SetEdns0(udpSize, false)
	}
	m.SetTsig("xfer.e412.in.", dns.HmacSHA256, 300, time.Now().Unix())
	buf, mac, err := dns.TsigGenerate(m, goodSecret, "", false)
	if err != nil {
		t.Fatalf("signing query: %v", err)
	}
	return buf, mac
}

// unpackReply turns raw wire bytes into a message, failing the test if they are
// not a message at all.
func unpackReply(t *testing.T, raw []byte) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	if err := m.Unpack(raw); err != nil {
		t.Fatalf("reply of %d bytes did not unpack: %v", len(raw), err)
	}
	return m
}

// verifyReplyMAC checks the signature on a reply as the requesting client
// would. dns.stripTsig decrements ARCOUNT in the buffer it is given, so this
// works on a copy and callers can keep using raw afterwards.
func verifyReplyMAC(t *testing.T, raw []byte, requestMAC string) {
	t.Helper()
	if err := dns.TsigVerify(append([]byte(nil), raw...), goodSecret, requestMAC, false); err != nil {
		t.Fatalf("reply MAC did not verify: %v", err)
	}
}

func sizeKeys(t *testing.T) *fakeKeys {
	t.Helper()
	return newFakeKeys(store.TSIGKey{
		Name: "xfer.e412.in.", Algorithm: dns.HmacSHA256, Secret: goodSecret,
	})
}

// The bug this task exists for.
//
// A non-EDNS client advertises nothing, so RFC 6891 §6.2.3 gives it a bare
// 512-byte budget. Msg.Truncate refuses outright to touch a message that
// already carries a TSIG (msg_truncate.go:30, "to simplify this
// implementation") and floors any size below 512 back up to 512
// (msg_truncate.go:40) -- so truncating to 512 and *then* attaching the
// signature put a 479-byte answer on the wire at 564 bytes with no TC bit: an
// over-size packet, and no instruction to retry over TCP.
//
// 479 + 53 (the TSIG RR for xfer.e412.in. under hmac-sha256, MAC-less) + 32
// (the MAC) = 564, which is the number the reviewer measured.
func TestTsigSignedReplyToNonEDNSClientFitsIn512(t *testing.T) {
	keys := sizeKeys(t)
	addr := tsigServerAnswering(t, keys, answerOfLen(t, sizeQName, 479))

	// The unsigned baseline, from the same server: 479 bytes, comfortably
	// inside 512, nothing truncated. This is the "479" half of the pair.
	unsignedQ := new(dns.Msg)
	unsignedQ.SetQuestion(sizeQName, dns.TypeA)
	packed, err := unsignedQ.Pack()
	if err != nil {
		t.Fatalf("packing unsigned query: %v", err)
	}
	if got := len(rawUDPExchange(t, addr, packed)); got != 479 {
		t.Fatalf("unsigned baseline was %d bytes, want 479; the test fixture no longer builds the size it claims", got)
	}

	answer := answerOfLen(t, sizeQName, 479)
	q, requestMAC := signedQueryBytes(t, sizeQName, 0)
	raw := rawUDPExchange(t, addr, q)

	if len(raw) > dns.MinMsgSize {
		t.Fatalf("signed reply to a non-EDNS client was %d bytes; a 512-byte client cannot receive it", len(raw))
	}
	reply := unpackReply(t, raw)
	// 479 bytes uncompressed is 269 compressed, which still fits the 395 left
	// after the signature is reserved -- so the right answer here is the whole
	// answer, not a trip to TCP. Reserving room must not cost records that fit.
	if reply.Truncated {
		t.Fatal("TC was set on an answer that still fits once compressed; the reservation is over-charging the reply")
	}
	if len(reply.Answer) != len(answer) {
		t.Fatalf("reply carried %d of %d records", len(reply.Answer), len(answer))
	}
	if reply.IsTsig() == nil {
		t.Fatal("reply carried no TSIG")
	}
	verifyReplyMAC(t, raw, requestMAC)
}

// The other half of the non-EDNS case: an answer that does not fit even
// compressed, once the signature's bytes are reserved out of a bare 512.
//
// RFC 8945 §5.3 mandates the shape -- "This response contains only the question
// and a TSIG record, has the TC bit set, and has an RCODE of 0 (NOERROR)" --
// and RFC 2181 §9 is why an empty body costs the client nothing: on a TC reply
// it "should ignore that response, and query again" over TCP for the whole
// thing, so salvaged records would be bytes it discards.
func TestTsigSignedReplyTooLargeForNonEDNSClientIsEmptyTC(t *testing.T) {
	keys := sizeKeys(t)
	addr := tsigServerAnswering(t, keys, answerOfLen(t, sizeQName, 1200))

	q, requestMAC := signedQueryBytes(t, sizeQName, 0)
	raw := rawUDPExchange(t, addr, q)

	if len(raw) > dns.MinMsgSize {
		t.Fatalf("signed reply to a non-EDNS client was %d bytes; a 512-byte client cannot receive it", len(raw))
	}
	reply := unpackReply(t, raw)
	if !reply.Truncated {
		t.Fatal("records were dropped but TC was not set; the client is given no reason to retry over TCP")
	}
	if len(reply.Answer) != 0 || len(reply.Ns) != 0 {
		t.Fatalf("reply carried %d answer and %d authority records; this case answers TC with an empty body",
			len(reply.Answer), len(reply.Ns))
	}
	if reply.IsTsig() == nil {
		t.Fatal("the TC reply was not signed; a peer that requires TSIG cannot trust an unsigned instruction to switch transports")
	}
	verifyReplyMAC(t, raw, requestMAC)
}

// An EDNS client that advertised room keeps miekg's ordinary partial
// truncation. The empty-body rule above is confined to budgets too small for
// Truncate to work in at all, and must not spread to clients that gave the
// server space.
func TestTsigSignedReplyToEDNSClientUsesAdvertisedBudget(t *testing.T) {
	keys := sizeKeys(t)
	addr := tsigServerAnswering(t, keys, answerOfLen(t, sizeQName, 6000))

	q, requestMAC := signedQueryBytes(t, sizeQName, 1232)
	raw := rawUDPExchange(t, addr, q)

	if len(raw) > 1232 {
		t.Fatalf("signed reply was %d bytes against an advertised 1232", len(raw))
	}
	reply := unpackReply(t, raw)
	if !reply.Truncated {
		t.Fatal("a 6000-byte answer was cut to fit 1232 but TC was not set")
	}
	if len(reply.Answer) == 0 {
		t.Fatal("an EDNS client with room to spare got no records at all; partial truncation is still miekg's job above 512")
	}
	if reply.IsTsig() == nil {
		t.Fatal("reply carried no TSIG")
	}
	verifyReplyMAC(t, raw, requestMAC)
}

// A signed reply that fits gets no TC and loses no records. The reservation is
// a ceiling, not a tax: reserving the signature's bytes must not cost a small
// answer any of its own.
func TestTsigSignedReplyThatFitsIsNotTruncated(t *testing.T) {
	keys := sizeKeys(t)
	answer := answerOfLen(t, sizeQName, 200)
	addr := tsigServerAnswering(t, keys, answer)

	q, requestMAC := signedQueryBytes(t, sizeQName, 0)
	raw := rawUDPExchange(t, addr, q)

	if len(raw) > dns.MinMsgSize {
		t.Fatalf("signed reply to a non-EDNS client was %d bytes", len(raw))
	}
	reply := unpackReply(t, raw)
	if reply.Truncated {
		t.Fatal("TC was set on a reply that fits; the client is sent to TCP for nothing")
	}
	if len(reply.Answer) != len(answer) {
		t.Fatalf("reply carried %d of %d records; nothing needed dropping", len(reply.Answer), len(answer))
	}
	verifyReplyMAC(t, raw, requestMAC)
}

// An EDNS client can advertise a budget that is still too small to truncate
// into once the signature is reserved. It gets the same empty-body TC -- but it
// keeps its OPT record.
//
// That is a resolved conflict, not a free choice: RFC 8945 §5.3 says the reply
// "contains only the question and a TSIG record", while RFC 6891 §6.1.1 says a
// compliant responder MUST include an OPT in its response to a request that
// carried one. See ednsOnly in server.go for why the OPT wins. The test exists
// so the resolution cannot be reversed by accident.
func TestTsigSignedReplyKeepsOPTWhenTruncatedToNothing(t *testing.T) {
	keys := sizeKeys(t)
	addr := tsigServerAnswering(t, keys, answerOfLen(t, sizeQName, 1200))

	q, requestMAC := signedQueryBytes(t, sizeQName, 512)
	raw := rawUDPExchange(t, addr, q)

	if len(raw) > dns.MinMsgSize {
		t.Fatalf("signed reply was %d bytes against an advertised 512", len(raw))
	}
	reply := unpackReply(t, raw)
	if !reply.Truncated {
		t.Fatal("TC was not set")
	}
	if len(reply.Answer) != 0 {
		t.Fatalf("reply carried %d records", len(reply.Answer))
	}
	if reply.IsEdns0() == nil {
		t.Fatal("the OPT record was dropped along with the answer; an EDNS query must get an EDNS reply")
	}
	verifyReplyMAC(t, raw, requestMAC)
}

// bigAuthority is the shape a negative answer or a delegation has: nothing in
// the answer section, a large authority section. It overshoots exactly as an
// answer section does, so clearing only reply.Answer is not enough -- with
// reply.Ns left in place this reply goes out at over 700 bytes.
func bigAuthority(t *testing.T, rcode int) func(*dns.Msg) {
	t.Helper()
	ns := answerOfLen(t, sizeQName, 1200)
	return func(m *dns.Msg) {
		m.Rcode = rcode
		m.Ns = append(m.Ns, ns...)
	}
}

// The authority section is dropped too. RFC 8945 §5.3 leaves "only the question
// and a TSIG record", and a large authority section breaks the 512-byte budget
// just as an answer section does.
func TestTsigSignedReplyDropsTheAuthoritySectionToo(t *testing.T) {
	keys := sizeKeys(t)
	addr := tsigServerShaping(t, keys, bigAuthority(t, dns.RcodeSuccess))

	q, requestMAC := signedQueryBytes(t, sizeQName, 0)
	raw := rawUDPExchange(t, addr, q)

	if len(raw) > dns.MinMsgSize {
		t.Fatalf("signed reply with a large authority section was %d bytes; a 512-byte client cannot receive it", len(raw))
	}
	reply := unpackReply(t, raw)
	if !reply.Truncated {
		t.Fatal("authority records were dropped but TC was not set")
	}
	if len(reply.Ns) != 0 {
		t.Fatalf("reply carried %d authority records; the emptying must cover every section, not just the answer", len(reply.Ns))
	}
	verifyReplyMAC(t, raw, requestMAC)
}

// RFC 8945 §5.3: the reply that fits "has an RCODE of 0 (NOERROR)". An NXDOMAIN
// whose authority section overflows must not go out as a 118-byte TC still
// claiming NXDOMAIN -- the reply that had to drop its records makes no claim it
// cannot carry the records for. The client learns the real rcode over TCP.
func TestTsigSignedReplyTruncatedToNothingIsNOERROR(t *testing.T) {
	keys := sizeKeys(t)
	addr := tsigServerShaping(t, keys, bigAuthority(t, dns.RcodeNameError))

	q, requestMAC := signedQueryBytes(t, sizeQName, 0)
	raw := rawUDPExchange(t, addr, q)

	reply := unpackReply(t, raw)
	if !reply.Truncated {
		t.Fatal("TC was not set")
	}
	if reply.Rcode != dns.RcodeSuccess {
		t.Fatalf("truncated-to-nothing reply carried rcode %s; RFC 8945 §5.3 requires NOERROR",
			dns.RcodeToString[reply.Rcode])
	}
	verifyReplyMAC(t, raw, requestMAC)
}

// The NOERROR above has to hold for an extended rcode as well, and that one
// does not live in the header: it is the top byte of the OPT's TTL, which
// Msg.Unpack ORs back into Rcode (msg.go:871). An upstream response carrying
// one arrives at the pipeline with the bits already set in the record, so
// clearing Msg.Rcode alone would leave a reply that reads as NOERROR here and
// as BADVERS at the client.
//
// It holds because Msg.Pack rewrites that field from Rcode whenever an OPT is
// present, not only for rcodes above 15 (msg.go:744-747). This is pinned rather
// than trusted: the guarantee belongs to the library, an explicit reset here
// was tried and was dead code, and a library that narrowed that branch would
// otherwise break this silently.
func TestTsigSignedReplyTruncatedToNothingClearsTheExtendedRcode(t *testing.T) {
	keys := sizeKeys(t)
	ns := answerOfLen(t, sizeQName, 1200)
	addr := tsigServerShaping(t, keys, func(m *dns.Msg) {
		m.SetEdns0(1232, false)
		m.IsEdns0().SetExtendedRcode(dns.RcodeBadVers)
		m.Rcode = dns.RcodeBadVers
		m.Ns = append(m.Ns, ns...)
	})

	q, requestMAC := signedQueryBytes(t, sizeQName, 512)
	raw := rawUDPExchange(t, addr, q)

	reply := unpackReply(t, raw)
	if !reply.Truncated {
		t.Fatal("TC was not set")
	}
	if reply.Rcode != dns.RcodeSuccess {
		// Printed as a number as well: miekg maps 16 to "BADSIG", since RFC
		// 8945's BADSIG and RFC 6891's BADVERS share the value.
		t.Fatalf("truncated-to-nothing reply resolved to rcode %d (%s); the extended bits in the OPT outlived the reset",
			reply.Rcode, dns.RcodeToString[reply.Rcode])
	}
	verifyReplyMAC(t, raw, requestMAC)
}

// RFC 6891 §6.2.3: "Values lower than 512 MUST be treated as equal to 512."
//
// The floor has to be applied before the signature is reserved, not left to
// Msg.Truncate, or a client advertising 256 gets a budget the reservation eats
// whole and a TC for an answer that fits comfortably. TestUDPSizeFloor covers
// the floor on the unsigned path only, where Truncate applies it for us.
func TestTsigSignedReplyFloorsATinyAdvertisedSize(t *testing.T) {
	keys := sizeKeys(t)
	answer := answerOfLen(t, sizeQName, 479)
	addr := tsigServerAnswering(t, keys, answer)

	q, requestMAC := signedQueryBytes(t, sizeQName, 256)
	raw := rawUDPExchange(t, addr, q)

	if len(raw) > dns.MinMsgSize {
		t.Fatalf("signed reply was %d bytes", len(raw))
	}
	reply := unpackReply(t, raw)
	if reply.Truncated {
		t.Fatal("an advertised 256 was taken at face value; floored to 512 the answer fits, and the client is sent to TCP for nothing")
	}
	if len(reply.Answer) != len(answer) {
		t.Fatalf("reply carried %d of %d records", len(reply.Answer), len(answer))
	}
	verifyReplyMAC(t, raw, requestMAC)
}

// The case around the fix: an unsigned reply to a non-EDNS client is truncated
// exactly as it always was, partial answer and all. Nothing about reserving
// room for a signature may reach a reply that has none.
func TestUnsignedReplyToNonEDNSClientTruncatesAsBefore(t *testing.T) {
	keys := sizeKeys(t)
	addr := tsigServerAnswering(t, keys, answerOfLen(t, sizeQName, 2000))

	q := new(dns.Msg)
	q.SetQuestion(sizeQName, dns.TypeA)
	packed, err := q.Pack()
	if err != nil {
		t.Fatalf("packing query: %v", err)
	}
	raw := rawUDPExchange(t, addr, packed)

	if len(raw) > dns.MinMsgSize {
		t.Fatalf("unsigned reply to a non-EDNS client was %d bytes", len(raw))
	}
	reply := unpackReply(t, raw)
	if !reply.Truncated {
		t.Fatal("a 2000-byte answer was cut to fit 512 but TC was not set")
	}
	if len(reply.Answer) == 0 {
		t.Fatal("unsigned truncation lost every record; it used to keep as many as fit")
	}
	if reply.IsTsig() != nil {
		t.Fatal("an unsigned query got a signed reply")
	}
}

// TCP is never truncated: there is no 512-byte budget to reserve from, and a
// zone transfer -- the thing D2 signs -- is TCP-only.
func TestTsigSignedReplyOverTCPIsNotTruncated(t *testing.T) {
	keys := sizeKeys(t)
	answer := answerOfLen(t, sizeQName, 2000)
	addr := tsigServerAnswering(t, keys, answer)

	c := &dns.Client{Net: "tcp", TsigSecret: map[string]string{"xfer.e412.in.": goodSecret}}
	m := new(dns.Msg)
	m.SetQuestion(sizeQName, dns.TypeA)
	m.SetTsig("xfer.e412.in.", dns.HmacSHA256, 300, time.Now().Unix())
	reply, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("tcp exchange: %v", err)
	}
	if reply.Truncated {
		t.Fatal("TC was set on a TCP reply")
	}
	if len(reply.Answer) != len(answer) {
		t.Fatalf("tcp reply carried %d of %d records", len(reply.Answer), len(answer))
	}
	if reply.IsTsig() == nil {
		t.Fatal("tcp reply carried no TSIG")
	}
}

// TCP gets the same provider as UDP; a transfer never arrives over UDP.
func TestTsigVerifiesOverTCP(t *testing.T) {
	keys := newFakeKeys(store.TSIGKey{
		Name: "xfer.e412.in.", Algorithm: dns.HmacSHA256, Secret: goodSecret,
	})
	addr, seen := startTSIGServer(t, keys)

	c := new(dns.Client)
	c.Net = "tcp"
	c.TsigSecret = map[string]string{"xfer.e412.in.": goodSecret}
	m := new(dns.Msg)
	m.SetQuestion("bifrost.e412.in.", dns.TypeA)
	m.SetTsig("xfer.e412.in.", dns.HmacSHA256, 300, time.Now().Unix())
	reply, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("tcp exchange: %v", err)
	}
	if reply.IsTsig() == nil {
		t.Fatal("tcp reply carried no TSIG")
	}
	if got := mustSee(t, seen); got.err != nil {
		t.Fatalf("tcp: handler saw TSIG error %v", got.err)
	}
}
