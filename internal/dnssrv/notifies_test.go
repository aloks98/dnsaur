package dnssrv_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
)

// recordingNotifies captures what the intercept handed over and answers
// NOERROR, so a test can assert on the routing rather than on any policy.
//
// It mirrors fakeTransfers' shape, snapshot method included, rather than
// inventing a second convention in the same package — and the snapshot is
// what lets a test assert the TSIG verdict actually arrived, instead of
// merely recording it and never looking.
type recordingNotifies struct {
	mu      sync.Mutex
	calls   int
	key     string
	tsigErr error
	opcode  int
}

func (r *recordingNotifies) ServeNotify(_ context.Context, w dns.ResponseWriter, m *dns.Msg, key string, tsigErr error) {
	r.mu.Lock()
	r.calls++
	r.key = key
	r.tsigErr = tsigErr
	r.opcode = m.Opcode
	r.mu.Unlock()

	reply := new(dns.Msg)
	reply.SetReply(m)
	_ = w.WriteMsg(reply)
}

func (r *recordingNotifies) snapshot() (calls int, key string, opcode int, tsigErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.key, r.opcode, r.tsigErr
}

// notifyPipelineHandler is the pipeline. A NOTIFY reaching it is the defect.
//
// Named distinctly from transfers_test.go's countingHandler: that identifier
// is already a top-level function in this package (dnssrv_test), and a type
// of the same name here would not compile as a second declaration of one
// identifier in one package.
type notifyPipelineHandler struct{ calls atomic.Int64 }

func (c *notifyPipelineHandler) ServeDNS(_ context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
	c.calls.Add(1)
	m := new(dns.Msg)
	m.SetReply(req.Msg)
	return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionForwarded}, nil
}

// THE REGRESSION. On the commit before this task, a NOTIFY falls through to
// the pipeline and — for an apex this server does not hold — is forwarded
// upstream. The assertion is on the pipeline never being entered, because
// that is the property, not on any particular downstream behaviour.
func TestNotifyNeverReachesThePipeline(t *testing.T) {
	h := &notifyPipelineHandler{}
	n := &recordingNotifies{}
	srv := dnssrv.NewServer("127.0.0.1:0", h, dnssrv.WithNotifies(n))
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	m := new(dns.Msg).SetNotify("example.com.")
	c := new(dns.Client)
	reply, _, err := c.Exchange(m, srv.Addr())
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if reply == nil {
		t.Fatal("no reply")
	}
	if got := h.calls.Load(); got != 0 {
		t.Errorf("the pipeline handled %d NOTIFY messages, want 0", got)
	}
	if calls, _, _, _ := n.snapshot(); calls != 1 {
		t.Errorf("ServeNotify called %d times, want 1", calls)
	}
}

// Without the option the branch is not taken at all, which is what every
// server built before D4 did. A NOTIFY then falls through as it always has —
// pinned so that removing the option is a known behaviour change rather than
// a silent one.
func TestNotifyFallsThroughWithoutTheOption(t *testing.T) {
	h := &notifyPipelineHandler{}
	srv := dnssrv.NewServer("127.0.0.1:0", h)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	m := new(dns.Msg).SetNotify("example.com.")
	c := new(dns.Client)
	if _, _, err := c.Exchange(m, srv.Addr()); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if got := h.calls.Load(); got != 1 {
		t.Errorf("the pipeline handled %d messages, want 1", got)
	}
}

// An ordinary SOA query is not a NOTIFY, and must keep reaching the
// pipeline. The two differ only in the opcode, so a branch that keyed on
// qtype would swallow every SOA query in the server.
func TestOrdinarySOAQueryStillReachesThePipeline(t *testing.T) {
	h := &notifyPipelineHandler{}
	n := &recordingNotifies{}
	srv := dnssrv.NewServer("127.0.0.1:0", h, dnssrv.WithNotifies(n))
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	m := new(dns.Msg).SetQuestion("example.com.", dns.TypeSOA)
	c := new(dns.Client)
	if _, _, err := c.Exchange(m, srv.Addr()); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if got := h.calls.Load(); got != 1 {
		t.Errorf("the pipeline handled %d messages, want 1", got)
	}
	if calls, _, _, _ := n.snapshot(); calls != 0 {
		t.Errorf("ServeNotify handled %d messages, want 0", calls)
	}
}

// A NOTIFY arrives over TCP as readily as UDP, and both listeners share one
// handler, so the branch has to be on the shared path rather than on either
// socket's.
func TestNotifyIsInterceptedOverTCP(t *testing.T) {
	h := &notifyPipelineHandler{}
	n := &recordingNotifies{}
	srv := dnssrv.NewServer("127.0.0.1:0", h, dnssrv.WithNotifies(n))
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	m := new(dns.Msg).SetNotify("example.com.")
	c := &dns.Client{Net: "tcp"}
	if _, _, err := c.Exchange(m, srv.Addr()); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if got := h.calls.Load(); got != 0 {
		t.Errorf("the pipeline handled %d NOTIFY messages over TCP, want 0", got)
	}
	if calls, _, _, _ := n.snapshot(); calls != 1 {
		t.Errorf("ServeNotify called %d times over TCP, want 1", calls)
	}
}

// A transfer is still a transfer: the two intercepts must not shadow one
// another. AXFR carries opcode QUERY, so only the qtype branch may claim it.
func TestTransferStillRoutesToTransfers(t *testing.T) {
	h := &notifyPipelineHandler{}
	n := &recordingNotifies{}
	tr := &fakeTransfers{} // defined in transfers_test.go
	srv := dnssrv.NewServer("127.0.0.1:0", h,
		dnssrv.WithNotifies(n), dnssrv.WithTransfers(tr))
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	m := new(dns.Msg)
	m.SetAxfr("example.com.")
	c := &dns.Client{Net: "tcp"}
	_, _, _ = c.Exchange(m, srv.Addr())

	// Both halves. Asserting only that Notifies did not claim it would pass
	// just as well if the AXFR reached *neither* handler — which is the bug
	// a future change to isTransferQuery would actually cause.
	if calls, _, _, _ := n.snapshot(); calls != 0 {
		t.Errorf("ServeNotify claimed an AXFR %d times, want 0", calls)
	}
	if calls, _, _, qtype, _ := tr.snapshot(); calls != 1 || qtype != dns.TypeAXFR {
		t.Errorf("Transfers saw %d calls of qtype %d, want 1 of AXFR", calls, qtype)
	}
}

// The one invariant the Notifies doc comment names: key and tsigErr are what
// Server.RequireTSIG concluded, passed rather than recomputed, because a
// disagreement between two computations would be a NOTIFY acted on without
// the signature the zone requires.
//
// Mirrors D3's TestTheTSIGVerdictIsPassedToTheHandler for the transfer path.
// Without it the interface's central promise is documented and untested.
func TestTheTSIGVerdictIsPassedToTheNotifyHandler(t *testing.T) {
	const keyName = "notify.e412.in."
	const secret = "c2VjcmV0LXNlY3JldC1zZWNyZXQ="
	keys := newFakeKeyStore(store.TSIGKey{Name: keyName, Algorithm: dns.HmacSHA256, Secret: secret})

	n := &recordingNotifies{}
	addr := startServer(t, noopHandler(t), dnssrv.WithTSIGKeys(keys), dnssrv.WithNotifies(n))

	// Signed: the handler sees the canonical key name and no error.
	signed := new(dns.Msg).SetNotify("e412.in.")
	signed.SetTsig(keyName, dns.HmacSHA256, 300, time.Now().Unix())
	c := &dns.Client{TsigSecret: map[string]string{keyName: secret}}
	if _, _, err := c.Exchange(signed, addr); err != nil {
		t.Fatalf("signed notify exchange: %v", err)
	}
	if _, key, _, tsigErr := n.snapshot(); key != keyName || tsigErr != nil {
		t.Fatalf("signed notify: key = %q, err = %v; want %q, nil", key, tsigErr, keyName)
	}

	// Unsigned: no key, and ErrTSIGUnsigned.
	plain := new(dns.Msg).SetNotify("e412.in.")
	if _, _, err := (&dns.Client{}).Exchange(plain, addr); err != nil {
		t.Fatalf("unsigned notify exchange: %v", err)
	}
	if _, key, _, tsigErr := n.snapshot(); key != "" || !errors.Is(tsigErr, dnssrv.ErrTSIGUnsigned) {
		t.Fatalf("unsigned notify: key = %q, err = %v; want \"\", ErrTSIGUnsigned", key, tsigErr)
	}
}

// The deadline is the transfer branch's reasoning applied to a smaller job:
// the pipeline's 5s context is cancelled when serve returns, and the SOA
// probe and any transfer the notify triggers outlive the reply. NotifyTimeout
// bounds the reply itself; the work behind it takes its own background
// context (Task 7).
func TestNotifyTimeoutIsItsOwn(t *testing.T) {
	if dnssrv.NotifyTimeout <= 0 || dnssrv.NotifyTimeout >= dnssrv.TransferTimeout {
		t.Fatalf("NotifyTimeout = %v, want a positive bound below TransferTimeout (%v)",
			dnssrv.NotifyTimeout, dnssrv.TransferTimeout)
	}
}
