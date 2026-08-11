package dnssrv

import (
	"context"
	"errors"
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
