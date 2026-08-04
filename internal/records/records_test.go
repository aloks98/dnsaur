package records

import (
	"context"
	"testing"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

type fakeRecordStore struct{ recs []store.LocalRecord }

func (f *fakeRecordStore) All(ctx context.Context) ([]store.LocalRecord, error) { return f.recs, nil }
func (f *fakeRecordStore) Add(ctx context.Context, r store.LocalRecord) (int64, error) {
	return 0, nil
}
func (f *fakeRecordStore) Update(ctx context.Context, r store.LocalRecord) error {
	return nil
}
func (f *fakeRecordStore) Delete(ctx context.Context, id int64) error {
	return nil
}

func resolver(t *testing.T, recs ...store.LocalRecord) *Resolver {
	r := NewResolver(&fakeRecordStore{recs: recs})
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

func ask(t *testing.T, h dnssrv.Handler, name string, qtype uint16) *dnssrv.Response {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	resp, err := h.ServeDNS(context.Background(), &dnssrv.Request{Msg: m})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func nextCounter(n *int) dnssrv.Handler {
	return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		*n++
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionForwarded}, nil
	})
}

func TestExactAndWildcardAndPassthrough(t *testing.T) {
	r := resolver(t,
		store.LocalRecord{Name: "nas.home.lan", Type: "A", Value: "10.0.0.9", TTL: 300},
		store.LocalRecord{Name: "*.apps.home.lan", Type: "A", Value: "10.0.0.10", TTL: 60},
	)
	calls := 0
	h := r.Middleware()(nextCounter(&calls))

	resp := ask(t, h, "nas.home.lan", dns.TypeA)
	if resp.Decision != dnssrv.DecisionLocal || len(resp.Msg.Answer) != 1 {
		t.Fatalf("%+v", resp)
	}
	if a := resp.Msg.Answer[0].(*dns.A); a.A.String() != "10.0.0.9" || a.Hdr.Ttl != 300 {
		t.Fatalf("%v", resp.Msg.Answer)
	}

	resp = ask(t, h, "grafana.apps.home.lan", dns.TypeA)
	if len(resp.Msg.Answer) != 1 || resp.Msg.Answer[0].(*dns.A).A.String() != "10.0.0.10" {
		t.Fatalf("wildcard: %v", resp.Msg.Answer)
	}

	// NODATA: name exists, wrong type
	resp = ask(t, h, "nas.home.lan", dns.TypeAAAA)
	if resp.Decision != dnssrv.DecisionLocal || len(resp.Msg.Answer) != 0 || resp.Msg.Rcode != dns.RcodeSuccess {
		t.Fatalf("nodata: %+v", resp)
	}

	if calls != 0 {
		t.Fatalf("next called %d times", calls)
	}
	ask(t, h, "example.com", dns.TypeA)
	if calls != 1 {
		t.Fatalf("passthrough missing, calls=%d", calls)
	}
}

func TestCNAMEChaseLocalAndRemote(t *testing.T) {
	r := resolver(t,
		store.LocalRecord{Name: "www.home.lan", Type: "CNAME", Value: "nas.home.lan", TTL: 300},
		store.LocalRecord{Name: "nas.home.lan", Type: "A", Value: "10.0.0.9", TTL: 300},
		store.LocalRecord{Name: "ext.home.lan", Type: "CNAME", Value: "external.example.com", TTL: 300},
	)
	calls := 0
	h := r.Middleware()(nextCounter(&calls))

	resp := ask(t, h, "www.home.lan", dns.TypeA)
	if len(resp.Msg.Answer) != 2 {
		t.Fatalf("cname+target expected: %v", resp.Msg.Answer)
	}

	resp = ask(t, h, "ext.home.lan", dns.TypeA)
	if calls != 1 {
		t.Fatalf("remote target should hit next, calls=%d", calls)
	}
	if _, ok := resp.Msg.Answer[0].(*dns.CNAME); !ok {
		t.Fatalf("first answer must be the CNAME: %v", resp.Msg.Answer)
	}

	// Remote CNAME target with NXDOMAIN response
	nextNXDOMAIN := dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		m.Rcode = dns.RcodeNameError
		return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionForwarded}, nil
	})
	h = r.Middleware()(nextNXDOMAIN)
	resp = ask(t, h, "ext.home.lan", dns.TypeA)
	if resp.Msg.Rcode != dns.RcodeNameError {
		t.Fatalf("should propagate NXDOMAIN rcode, got %d", resp.Msg.Rcode)
	}
	if len(resp.Msg.Answer) < 1 {
		t.Fatalf("CNAME must still be in answers with NXDOMAIN: %v", resp.Msg.Answer)
	}
	if _, ok := resp.Msg.Answer[0].(*dns.CNAME); !ok {
		t.Fatalf("first answer must be CNAME with NXDOMAIN: %v", resp.Msg.Answer)
	}
}

func TestCNAMECycle(t *testing.T) {
	r := resolver(t,
		store.LocalRecord{Name: "a.loop.lan", Type: "CNAME", Value: "b.loop.lan", TTL: 300},
		store.LocalRecord{Name: "b.loop.lan", Type: "CNAME", Value: "a.loop.lan", TTL: 300},
	)
	calls := 0
	h := r.Middleware()(nextCounter(&calls))

	resp := ask(t, h, "a.loop.lan", dns.TypeA)
	if resp.Msg == nil {
		t.Fatal("response should not be nil")
	}
	if len(resp.Msg.Answer) > 8 {
		t.Fatalf("should be bounded by hop limit, got %d answers", len(resp.Msg.Answer))
	}
	if calls != 0 {
		t.Fatalf("should not call next for local cycle, calls=%d", calls)
	}
}

func TestTXTValues(t *testing.T) {
	r := resolver(t,
		store.LocalRecord{ID: 1, Name: "dmarc.home.lan", Type: "TXT", Value: "v=DMARC1; p=reject; rua=mailto:x@y.z", TTL: 300},
		store.LocalRecord{ID: 2, Name: "spaced.home.lan", Type: "TXT", Value: "hello world", TTL: 300},
	)
	calls := 0
	h := r.Middleware()(nextCounter(&calls))

	resp := ask(t, h, "dmarc.home.lan", dns.TypeTXT)
	if len(resp.Msg.Answer) != 1 {
		t.Fatalf("expected 1 TXT answer, got %d", len(resp.Msg.Answer))
	}
	txt := resp.Msg.Answer[0].(*dns.TXT)
	if len(txt.Txt) != 1 || txt.Txt[0] != "v=DMARC1; p=reject; rua=mailto:x@y.z" {
		t.Fatalf("TXT value corrupted: %v", txt.Txt)
	}

	resp = ask(t, h, "spaced.home.lan", dns.TypeTXT)
	if len(resp.Msg.Answer) != 1 {
		t.Fatalf("expected 1 TXT answer, got %d", len(resp.Msg.Answer))
	}
	txt = resp.Msg.Answer[0].(*dns.TXT)
	if len(txt.Txt) != 1 || txt.Txt[0] != "hello world" {
		t.Fatalf("TXT value with spaces corrupted: %v", txt.Txt)
	}

	if calls != 0 {
		t.Fatalf("should not call next, calls=%d", calls)
	}
}
