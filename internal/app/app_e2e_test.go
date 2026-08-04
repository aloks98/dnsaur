package app

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/config"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

func mockUpstream(t *testing.T, counter *atomic.Int64) string {
	t.Helper()
	pc, _ := net.ListenPacket("udp", "127.0.0.1:0")
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		counter.Add(1)
		r := new(dns.Msg)
		r.SetReply(m)
		rr, _ := dns.NewRR(m.Question[0].Name + " 300 IN A 9.9.9.9")
		r.Answer = []dns.RR{rr}
		_ = w.WriteMsg(r)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String()
}

func TestEndToEnd(t *testing.T) {
	ctx := context.Background()
	var upstreamHits atomic.Int64
	upAddr := mockUpstream(t, &upstreamHits)
	listSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("0.0.0.0 ads.example.com\n"))
	}))
	defer listSrv.Close()

	dir := t.TempDir()
	cfg := &config.Config{DNSListen: []string{"127.0.0.1:0"}, HTTPListen: ":0", DataDir: dir, LogLevel: "error"}
	cfg.Storage.Driver = "sqlite"
	cfg.Storage.DSN = dir + "/t.db"

	a, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := a.Store()
	if err := s.Settings().SetInternal(ctx, "upstreams", upAddr); err != nil {
		t.Fatal(err)
	}
	lid, err := s.Filters().AddList(ctx, store.List{URL: listSrv.URL, Kind: "block", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Filters().AssignList(ctx, 1, lid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Records().Add(ctx, store.LocalRecord{Name: "nas.home.lan", Type: "A", Value: "10.0.0.9", TTL: 300}); err != nil {
		t.Fatal(err)
	}

	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Shutdown(context.Background()) }()
	a.WaitReady(5 * time.Second) // blocks until initial RefreshAll + reloads done

	c := new(dns.Client)
	ask := func(name string) *dns.Msg {
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(name), dns.TypeA)
		r, _, err := c.Exchange(m, a.DNSAddr())
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	if r := ask("ads.example.com"); r.Answer[0].(*dns.A).A.String() != "0.0.0.0" {
		t.Fatalf("not blocked: %v", r.Answer)
	}
	if r := ask("nas.home.lan"); r.Answer[0].(*dns.A).A.String() != "10.0.0.9" {
		t.Fatalf("local record: %v", r.Answer)
	}
	if r := ask("clean.example.org"); r.Answer[0].(*dns.A).A.String() != "9.9.9.9" {
		t.Fatalf("forward: %v", r.Answer)
	}
	before := upstreamHits.Load()
	ask("clean.example.org") // cached now
	if upstreamHits.Load() != before {
		t.Fatal("cache miss on repeat query")
	}

	// Hot-reload: Set (unlike SetInternal) bumps config_version and notifies
	// Changes(), which the app's watcher goroutine picks up to re-apply
	// settings live, without a restart.
	if err := s.Settings().Set(ctx, "blocking.mode", "nxdomain"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r := ask("ads.example.com")
		if r.Rcode == dns.RcodeNameError {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("blocking mode change did not take effect: rcode=%v answer=%v", r.Rcode, r.Answer)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
