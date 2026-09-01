package dnssrv_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// startServer brings up a server on an ephemeral port with h as its pipeline
// handler and opts applied, shuts it down when the test ends, and returns the
// address it bound.
func startServer(t *testing.T, h dnssrv.Handler, opts ...dnssrv.Option) string {
	t.Helper()
	s := dnssrv.NewServer("127.0.0.1:0", h, opts...)
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

// countingHandler increments *n and answers NOERROR, so "the pipeline ran" is
// observable from the test after the exchange completes.
func countingHandler(t *testing.T, n *int) dnssrv.Handler {
	t.Helper()
	return dnssrv.HandlerFunc(func(_ context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		*n++
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		return &dnssrv.Response{Msg: m}, nil
	})
}

// noopHandler is a pipeline handler for tests that assert entirely through
// the fake transfer handler and only need the pipeline to answer something if
// it is ever reached.
func noopHandler(t *testing.T) dnssrv.Handler {
	t.Helper()
	return countingHandler(t, new(int))
}

// exchangeTCP sends m to addr over TCP and returns the reply, failing the
// test on error. TCP, not dns.Exchange (which is UDP): a transfer only ever
// arrives over TCP, and Task 6 makes an AXFR over UDP answer NOTIMP, which
// would make a UDP-based test here pass for the wrong reason.
func exchangeTCP(t *testing.T, addr string, m *dns.Msg) *dns.Msg {
	t.Helper()
	c := &dns.Client{Net: "tcp"}
	r, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("tcp exchange: %v", err)
	}
	return r
}

// askAXFR sends a plain, unsigned AXFR query for e412.in. over TCP and
// returns the reply.
func askAXFR(t *testing.T, addr string) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion("e412.in.", dns.TypeAXFR)
	return exchangeTCP(t, addr, m)
}

// fakeKeyStore is a minimal dnssrv.TSIGKeys for the one test here that needs a
// real, verifying key store. tsig_test.go has its own fakeKeys, but that file
// is package dnssrv and this one is package dnssrv_test: an external test
// package cannot reach an unexported type in another file's internal
// package, so this is the smallest thing that satisfies the exported
// interface rather than a copy of the internal one.
type fakeKeyStore struct {
	mu   sync.Mutex
	keys map[string]store.TSIGKey
}

func newFakeKeyStore(ks ...store.TSIGKey) *fakeKeyStore {
	f := &fakeKeyStore{keys: map[string]store.TSIGKey{}}
	for _, k := range ks {
		f.keys[k.Name] = k
	}
	return f
}

func (f *fakeKeyStore) ByName(_ context.Context, name string) (store.TSIGKey, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.keys[name]
	return k, ok, nil
}

// fakeTransfers records what the intercept handed it and answers NOTIMP, so
// a test can tell "the intercept fired" from "the pipeline answered".
type fakeTransfers struct {
	mu       sync.Mutex
	calls    int
	key      string
	tsigErr  error
	deadline time.Time
	qtype    uint16
}

func (f *fakeTransfers) ServeTransfer(ctx context.Context, w dns.ResponseWriter, q *dns.Msg, key string, tsigErr error) {
	f.mu.Lock()
	f.calls++
	f.key, f.tsigErr, f.qtype = key, tsigErr, q.Question[0].Qtype
	f.deadline, _ = ctx.Deadline()
	f.mu.Unlock()
	m := new(dns.Msg)
	m.SetRcode(q, dns.RcodeNotImplemented)
	_ = w.WriteMsg(m)
}

// snapshot reads every field under the lock that ServeTransfer writes them
// under. A direct field read from the test goroutine would race the write in
// ServeTransfer's goroutine under go test -race even though the TCP exchange
// that precedes it already guarantees the write happened first in real time:
// the race detector's happens-before edges come from synchronization it
// tracks, not from wall-clock ordering, so the read needs the same mutex.
func (f *fakeTransfers) snapshot() (calls int, key string, deadline time.Time, qtype uint16, tsigErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.key, f.deadline, f.qtype, f.tsigErr
}

func TestAXFRGoesToTheTransferHandlerAndNotThePipeline(t *testing.T) {
	ft := &fakeTransfers{}
	pipelineCalls := 0
	h := dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		pipelineCalls++
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		return &dnssrv.Response{Msg: m}, nil
	})
	addr := startServer(t, h, dnssrv.WithTransfers(ft))

	m := new(dns.Msg)
	m.SetQuestion("e412.in.", dns.TypeAXFR)
	reply := exchangeTCP(t, addr, m)
	if reply.Rcode != dns.RcodeNotImplemented {
		t.Fatalf("rcode = %s, want NOTIMP from the fake", dns.RcodeToString[reply.Rcode])
	}
	if calls, _, _, _, _ := ft.snapshot(); calls != 1 {
		t.Fatalf("ServeTransfer called %d times, want 1", calls)
	}
	if pipelineCalls != 0 {
		t.Fatal("the pipeline ran for an AXFR; the intercept exists so it does not")
	}
}

func TestIXFRIsInterceptedToo(t *testing.T) {
	ft := &fakeTransfers{}
	addr := startServer(t, countingHandler(t, new(int)), dnssrv.WithTransfers(ft))
	m := new(dns.Msg)
	m.SetQuestion("e412.in.", dns.TypeIXFR)
	exchangeTCP(t, addr, m)
	calls, _, _, qtype, _ := ft.snapshot()
	if calls != 1 || qtype != dns.TypeIXFR {
		t.Fatalf("calls = %d, qtype = %d; an IXFR is the transfer handler's too", calls, qtype)
	}
}

// The two shapes isTransferQuery's one-question condition is usually assumed
// to be about. Neither is: the library decides both before this server sees
// them, and these two tests are where that assumption is pinned rather than
// asserted in a comment. §9.5.5's "not exactly one question is FORMERR" is
// satisfied on the wire — by miekg, not by the gate, which is why the gate's
// own row is only reachable by calling ServeTransfer directly (see
// zones/transferserver_test.go).

func TestAMalformedQuestionCountIsRejectedBeforeEitherBranch(t *testing.T) {
	// DefaultMsgAcceptFunc rejects a header whose QDCOUNT is not 1 with
	// FORMERR (acceptfunc.go:44), at server.go:639-660, before Server.serve
	// runs; dnsaur never replaces that accept function. So a two-question
	// AXFR is answered the rcode §9.5.5 asks for without reaching the
	// intercept or the pipeline — no filter, no cache, and no AXFR handed to
	// the forwarder. If the library ever stops doing that, this fails here
	// rather than quietly becoming an upstream query.
	ft := &fakeTransfers{}
	pipelineCalls := 0
	addr := startServer(t, countingHandler(t, &pipelineCalls), dnssrv.WithTransfers(ft))

	m := new(dns.Msg)
	m.SetQuestion("e412.in.", dns.TypeAXFR)
	m.Question = append(m.Question, dns.Question{
		Name: "second.e412.in.", Qtype: dns.TypeAXFR, Qclass: dns.ClassINET,
	})
	reply := exchangeTCP(t, addr, m)

	if reply.Rcode != dns.RcodeFormatError {
		t.Fatalf("rcode = %s, want FORMERR", dns.RcodeToString[reply.Rcode])
	}
	if calls, _, _, _, _ := ft.snapshot(); calls != 0 {
		t.Fatalf("ServeTransfer ran %d times for a message the library rejects", calls)
	}
	if pipelineCalls != 0 {
		t.Fatal("a two-question AXFR reached the pipeline")
	}
}

func TestAHeaderOnlyQueryReachesThePipelineWithNoQuestion(t *testing.T) {
	// The one shape that arrives at a handler carrying no question at all: a
	// header claiming QDCOUNT 1 with nothing after it. The accept function
	// passes it — the header says one — and unpack takes msg.go:830-835's
	// "off == len(msg)" path, which resets every section and returns a Msg
	// with no Question. It has no qtype, so it is not a transfer under any
	// reading, and goes to the pipeline, where QName and QType return zero
	// values written for exactly this (pipeline.go:40-52).
	//
	// What the pipeline then answers is its own pre-existing behaviour and
	// not this branch's business; what is asserted here is only which of the
	// two branches it reached.
	ft := &fakeTransfers{}
	pipelineCalls := 0
	addr := startServer(t, countingHandler(t, &pipelineCalls), dnssrv.WithTransfers(ft))

	header := make([]byte, 12)
	binary.BigEndian.PutUint16(header[0:], 0x2A2A) // ID
	binary.BigEndian.PutUint16(header[4:], 1)      // QDCOUNT, with no question to match
	reply := sendRawTCP(t, addr, header)

	if len(reply.Question) != 0 {
		t.Fatalf("reply carries %d questions, want none echoed back", len(reply.Question))
	}
	if calls, _, _, _, _ := ft.snapshot(); calls != 0 {
		t.Fatal("a message with no question reached the transfer handler")
	}
	if pipelineCalls != 1 {
		t.Fatalf("pipeline ran %d times, want 1", pipelineCalls)
	}
}

// sendRawTCP writes wire to addr with DNS-over-TCP's two-byte length prefix
// and unpacks the one message that comes back. dns.Client packs from a
// dns.Msg, whose QDCOUNT is always len(Question) — so a header that
// disagrees with its own body has to be assembled by hand.
func sendRawTCP(t *testing.T, addr string, wire []byte) *dns.Msg {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("conn.Close: %v", err)
		}
	}()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	framed := make([]byte, 2+len(wire))
	binary.BigEndian.PutUint16(framed, uint16(len(wire)))
	copy(framed[2:], wire)
	if _, err := conn.Write(framed); err != nil {
		t.Fatalf("write: %v", err)
	}
	var n uint16
	if err := binary.Read(conn, binary.BigEndian, &n); err != nil {
		t.Fatalf("read length: %v", err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read message: %v", err)
	}
	m := new(dns.Msg)
	if err := m.Unpack(buf); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	return m
}

func TestOrdinaryQueriesStillReachThePipeline(t *testing.T) {
	ft := &fakeTransfers{}
	pipelineCalls := 0
	addr := startServer(t, countingHandler(t, &pipelineCalls), dnssrv.WithTransfers(ft))
	m := new(dns.Msg)
	m.SetQuestion("e412.in.", dns.TypeA)
	exchangeTCP(t, addr, m)
	if calls, _, _, _, _ := ft.snapshot(); calls != 0 {
		t.Fatal("an A query reached the transfer handler")
	}
	if pipelineCalls != 1 {
		t.Fatalf("pipeline ran %d times, want 1", pipelineCalls)
	}
}

func TestWithoutATransferHandlerAnAXFRFallsThrough(t *testing.T) {
	// Unchanged behaviour is the point: this is the configuration every
	// server built before D3 runs in, including every existing test.
	pipelineCalls := 0
	addr := startServer(t, countingHandler(t, &pipelineCalls)) // no WithTransfers
	m := new(dns.Msg)
	m.SetQuestion("e412.in.", dns.TypeAXFR)
	exchangeTCP(t, addr, m)
	if pipelineCalls != 1 {
		t.Fatalf("pipeline ran %d times, want 1: with no handler attached the branch must not be taken", pipelineCalls)
	}
}

func TestTheTransferContextIsNotThePipelines(t *testing.T) {
	// The 5s handler context would abort a large transfer (spec §9.1). Assert
	// the deadline the handler received is more than a minute out.
	ft := &fakeTransfers{}
	addr := startServer(t, noopHandler(t), dnssrv.WithTransfers(ft))
	askAXFR(t, addr)
	_, _, deadline, _, _ := ft.snapshot()
	if d := time.Until(deadline); d < time.Minute {
		t.Fatalf("transfer deadline is %s away, want the transfer timeout, not the pipeline's 5s", d)
	}
}

func TestTheTSIGVerdictIsPassedToTheHandler(t *testing.T) {
	const keyName = "xfer.e412.in."
	const secret = "c2VjcmV0LXNlY3JldC1zZWNyZXQ="
	keys := newFakeKeyStore(store.TSIGKey{Name: keyName, Algorithm: dns.HmacSHA256, Secret: secret})

	ft := &fakeTransfers{}
	addr := startServer(t, noopHandler(t), dnssrv.WithTSIGKeys(keys), dnssrv.WithTransfers(ft))

	// A correctly signed AXFR: the fake sees the canonical key name and no
	// TSIG error.
	signed := new(dns.Msg)
	signed.SetQuestion("e412.in.", dns.TypeAXFR)
	signed.SetTsig(keyName, dns.HmacSHA256, 300, time.Now().Unix())
	c := &dns.Client{Net: "tcp", TsigSecret: map[string]string{keyName: secret}}
	if _, _, err := c.Exchange(signed, addr); err != nil {
		t.Fatalf("signed axfr exchange: %v", err)
	}
	if _, key, _, _, tsigErr := ft.snapshot(); key != keyName || tsigErr != nil {
		t.Fatalf("signed axfr: key = %q, err = %v; want %q, nil", key, tsigErr, keyName)
	}

	// An unsigned AXFR: the fake sees no key and ErrTSIGUnsigned.
	askAXFR(t, addr)
	if _, key, _, _, tsigErr := ft.snapshot(); key != "" || !errors.Is(tsigErr, dnssrv.ErrTSIGUnsigned) {
		t.Fatalf("unsigned axfr: key = %q, err = %v; want \"\", ErrTSIGUnsigned", key, tsigErr)
	}
}
