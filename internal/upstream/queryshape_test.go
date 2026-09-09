package upstream

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
)

// A message the forwarder must refuse rather than forward, and the shape a
// real client can put on the wire to produce it.
var malformedShapes = []struct {
	name string
	msg  func() *dns.Msg
}{
	{
		// A twelve-byte packet whose header claims QDCOUNT 1 with nothing
		// after it. miekg accepts it — its accept function only reads the
		// header — and unpack returns a Msg with no Question at all;
		// dnssrv/transfers_test.go's TestAHeaderOnlyQueryReachesThePipeline‐
		// WithNoQuestion pins that it arrives here. Every stage above the
		// forwarder falls through on an empty qname, so the forwarder is the
		// first one that has to have an opinion.
		name: "no question",
		msg: func() *dns.Msg {
			m := new(dns.Msg)
			m.Id = 0x2A2A
			return m
		},
	},
	{
		// QDCOUNT 2. The accept function rejects this on :53, but the DoH
		// entry point has no equivalent gate, so it reaches the pipeline
		// there. Only the first question could ever be answered, and
		// forwarding a message whose second question nothing will look at is
		// worse than refusing it.
		name: "two questions",
		msg: func() *dns.Msg {
			m := query("first.example")
			m.Question = append(m.Question, dns.Question{
				Name: "second.example.", Qtype: dns.TypeA, Qclass: dns.ClassINET,
			})
			return m
		},
	},
}

// A malformed question section is answered FORMERR and never forwarded.
//
// FORMERR rather than SERVFAIL because that is what the layer directly below
// already answers for the same defect: miekg's DefaultMsgAcceptFunc rejects
// any QDCOUNT other than 1 with FORMERR before Server.serve runs
// (dnssrv/transfers_test.go pins it for a two-question AXFR). SERVFAIL would
// tell the client dnsaur failed; the message is the thing that is wrong, and
// a client that retries it gets the same answer forever.
func TestAMalformedQuestionSectionIsRefusedUnderEveryStrategy(t *testing.T) {
	for _, strategy := range []string{"race", "failover", "fastest"} {
		for _, shape := range malformedShapes {
			t.Run(strategy+"/"+shape.name, func(t *testing.T) {
				var hits atomic.Int64
				addr := mockUpstream(t, func(w dns.ResponseWriter, m *dns.Msg) {
					hits.Add(1)
					r := new(dns.Msg)
					r.SetReply(m)
					_ = w.WriteMsg(r)
				})
				f, err := New(Config{Upstreams: []string{addr}, Strategy: strategy, Timeout: 300 * time.Millisecond})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				t.Cleanup(func() { _ = f.Close() })

				m := shape.msg()
				resp, err := f.Handler().ServeDNS(context.Background(), &dnssrv.Request{Msg: m})
				if err != nil {
					t.Fatalf("ServeDNS: %v", err)
				}
				if resp == nil || resp.Msg == nil {
					t.Fatalf("no response: %+v", resp)
				}
				if resp.Msg.Rcode != dns.RcodeFormatError {
					t.Errorf("rcode = %s, want FORMERR", dns.RcodeToString[resp.Msg.Rcode])
				}
				if resp.Msg.Id != m.Id {
					t.Errorf("reply id = %d, want the request's %d", resp.Msg.Id, m.Id)
				}
				if !resp.Msg.Response {
					t.Error("the reply is not marked as a response")
				}
				if hits.Load() != 0 {
					t.Errorf("the upstream was asked %d times about a message with %d questions",
						hits.Load(), len(m.Question))
				}
			})
		}
	}
}

// panickingExchanger stands in for any future bug below the transport seam.
type panickingExchanger struct{}

func (panickingExchanger) Exchange(context.Context, *dns.Msg) (*dns.Msg, error) {
	panic("exchanger panic")
}

func (panickingExchanger) Close() error { return nil }

// A panic below the seam must not take the process down.
//
// dnssrv.Recover wraps the handler's own goroutine, and under "race" the
// exchange runs in one this package spawns — so a panic there is not
// recovered by anything above and the process exits. Refusing a question-less
// message closes today's way of reaching one; this closes the class.
func TestRaceContainsAPanicFromAnExchanger(t *testing.T) {
	f, err := New(Config{Upstreams: []string{"127.0.0.1:1"}, Strategy: "race", Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	f.def[0].ex = panickingExchanger{}

	if _, err := f.Handler().ServeDNS(context.Background(), req("boom.test")); err == nil {
		t.Fatal("a panicking exchanger produced a successful answer")
	}
}

// captureUpstream records every query it is asked, so a test can assert on
// what left dnsaur rather than on what it was handed.
func captureUpstream(t *testing.T) (addr string, seen chan *dns.Msg) {
	t.Helper()
	seen = make(chan *dns.Msg, 8)
	addr = mockUpstream(t, func(w dns.ResponseWriter, m *dns.Msg) {
		select {
		case seen <- m.Copy():
		default:
		}
		r := new(dns.Msg)
		r.SetReply(m)
		if rr, err := dns.NewRR(m.Question[0].Name + " 300 IN A 5.6.7.8"); err == nil {
			r.Answer = []dns.RR{rr}
		}
		_ = w.WriteMsg(r)
	})
	return addr, seen
}

func recv(t *testing.T, seen chan *dns.Msg) *dns.Msg {
	t.Helper()
	select {
	case m := <-seen:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("the upstream never recorded a query")
		return nil
	}
}

// The outbound query is built from the question, not copied from the client.
//
// Three separate failures fall out of copying the client's whole message, and
// one shape change removes all of them: a client's TSIG RR travels upstream,
// where miekg's client refuses to send it (ErrSecret) and each refusal is a
// health strike against an upstream that is fine; a client's EDNS options
// (ECS, cookies, NSID) leave the network, and the answer they shaped is
// cached under (qname, qtype) for every other client; and a message with no
// question at all reaches an exchanger that indexes Question[0].
func TestTheUpstreamQueryIsBuiltFromTheQuestionAlone(t *testing.T) {
	addr, seen := captureUpstream(t)
	f, err := New(Config{Upstreams: []string{addr}, Strategy: "failover", Timeout: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	m := query("Example.COM")
	m.Id = 0x2A2A
	m.SetEdns0(4096, true) // DO=1, and a UDP size only this client asked for
	opt := m.IsEdns0()
	opt.Option = append(opt.Option,
		&dns.EDNS0_NSID{Code: dns.EDNS0NSID},
		&dns.EDNS0_SUBNET{Code: dns.EDNS0SUBNET, Family: 1, SourceNetmask: 24,
			Address: []byte{203, 0, 113, 0}},
	)
	// A signed ordinary query, which is what a BIND secondary sends when it
	// refreshes an apex this server does not hold.
	m.SetTsig("key.example.", dns.HmacSHA256, 300, time.Now().Unix())

	resp, err := f.Handler().ServeDNS(context.Background(), &dnssrv.Request{Msg: m})
	if err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	if resp.Msg.Id != m.Id {
		t.Errorf("reply id = %d, want the client's %d", resp.Msg.Id, m.Id)
	}
	if len(resp.Msg.Question) != 1 || resp.Msg.Question[0].Name != "Example.COM." {
		t.Errorf("reply question = %+v, want the client's exact spelling", resp.Msg.Question)
	}

	q := recv(t, seen)
	if len(q.Question) != 1 || !strings.EqualFold(q.Question[0].Name, "example.com.") {
		t.Errorf("upstream question = %+v, want one question for example.com.", q.Question)
	}
	if !q.RecursionDesired {
		t.Error("the upstream query does not ask for recursion")
	}
	if q.IsTsig() != nil {
		t.Error("the client's TSIG RR was forwarded upstream")
	}
	if len(q.Answer) != 0 || len(q.Ns) != 0 {
		t.Errorf("the upstream query carries sections the question did not: answer=%d ns=%d",
			len(q.Answer), len(q.Ns))
	}
	if len(q.Extra) != 1 {
		t.Errorf("the upstream query's additional section has %d records, want only the OPT: %v",
			len(q.Extra), q.Extra)
	}
	uo := q.IsEdns0()
	if uo == nil {
		t.Fatal("the upstream query carries no OPT record")
	}
	if !uo.Do() {
		t.Error("the client set DO and the upstream query did not: a DNSSEC-aware client would lose its RRSIGs")
	}
	if got := uo.UDPSize(); got != 1232 {
		t.Errorf("advertised UDP size = %d, want 1232", got)
	}
	if len(uo.Option) != 0 {
		t.Errorf("the client's EDNS options travelled upstream: %v", uo.Option)
	}
}

// The DO bit is copied, not asserted: a client that did not ask for DNSSEC
// records must not have them fetched (and cached) on its behalf.
func TestTheUpstreamQueryCopiesTheClientsDOBit(t *testing.T) {
	addr, seen := captureUpstream(t)
	f, err := New(Config{Upstreams: []string{addr}, Strategy: "failover", Timeout: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	for _, tc := range []struct {
		name string
		edns func(*dns.Msg)
		want bool
	}{
		{"no OPT at all", func(*dns.Msg) {}, false},
		{"OPT with DO clear", func(m *dns.Msg) { m.SetEdns0(512, false) }, false},
		{"OPT with DO set", func(m *dns.Msg) { m.SetEdns0(512, true) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := query("do.example")
			tc.edns(m)
			if _, err := f.Handler().ServeDNS(context.Background(), &dnssrv.Request{Msg: m}); err != nil {
				t.Fatalf("ServeDNS: %v", err)
			}
			q := recv(t, seen)
			uo := q.IsEdns0()
			if uo == nil {
				t.Fatal("the upstream query carries no OPT record")
			}
			if uo.Do() != tc.want {
				t.Errorf("upstream DO = %v, want %v", uo.Do(), tc.want)
			}
		})
	}
}

// The outbound ID is minted here, not taken from the client, so an off-path
// attacker who can see or guess the client's ID still cannot guess ours. The
// reply must come back wearing the client's ID all the same.
func TestTheUpstreamQueryUsesAFreshID(t *testing.T) {
	addr, seen := captureUpstream(t)
	f, err := New(Config{Upstreams: []string{addr}, Strategy: "failover", Timeout: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	const clientID = 0x2A2A
	// One query could match by chance one time in 65536; four cannot.
	fresh := false
	for i := range 4 {
		m := query("fresh.example")
		m.Id = clientID
		resp, err := f.Handler().ServeDNS(context.Background(), &dnssrv.Request{Msg: m})
		if err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
		if resp.Msg.Id != clientID {
			t.Fatalf("query %d: reply id = %d, want the client's %d", i, resp.Msg.Id, clientID)
		}
		if recv(t, seen).Id != clientID {
			fresh = true
		}
	}
	if !fresh {
		t.Error("every upstream query carried the client's own message ID")
	}
}
