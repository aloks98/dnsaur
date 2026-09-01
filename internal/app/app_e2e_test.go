package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
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

	a, err := New(ctx, cfg, "test")
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
	// A record inside a zone we hold: seeded through the zones store since
	// the zones CRUD API doesn't exist yet (Task 8). Serving this
	// authoritatively — never forwarding it — is this milestone's payoff.
	zid, err := s.Zones().AddZone(ctx, store.Zone{
		Name: "home.lan", Type: "primary", Enabled: true,
		SOANS: "ns1.home.lan", SOAMbox: "hostadmin.home.lan",
		SOASerial: 1, SOARefresh: 900, SOARetry: 300, SOAExpire: 604800,
		SOAMinimum: 900, SOATTL: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Zones().AddRecord(ctx, store.ZoneRecord{ZoneID: zid, Name: "nas", Type: "A", TTL: 300, RData: "10.0.0.9", Enabled: true}); err != nil {
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
		t.Fatalf("zone record: %v", r.Answer)
	}

	// The actual deliverable: a name inside our zone with no record must
	// come back NXDOMAIN from us, and must never reach the upstream. Rcode
	// alone would not prove that — the upstream could answer NXDOMAIN too —
	// so the upstream hit count is what actually distinguishes "we refused
	// it" from "we forwarded it and relayed the refusal."
	hitsBeforeZoneMiss := upstreamHits.Load()
	if r := ask("nothere.home.lan"); r.Rcode != dns.RcodeNameError {
		t.Fatalf("undefined name inside our zone: rcode=%d, want NXDOMAIN", r.Rcode)
	}
	if upstreamHits.Load() != hitsBeforeZoneMiss {
		t.Fatal("undefined name inside our zone reached the upstream — the leak this milestone exists to close")
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

// The seam the TSIG milestone is made of, and the one place it can be seen:
// the key the REST API writes has to be the key the DNS server's provider
// finds. Both sides are covered on their own -- internal/api against a store,
// internal/dnssrv against a fake key store -- and a pair of layers that each
// pass against their own stub still prove nothing about whether they agree.
// internal/app/app.go is where a real store.TSIGKeyStore meets the provider,
// so this is where that agreement is testable.
//
// The key is created over the API in a form that is neither lowercase nor
// fully qualified, then used to sign a real query against the live listener
// with nothing restarted in between: canonicalisation on write and
// canonicalisation on lookup have to mean the same thing, or the lookup misses
// and the reply comes back unsigned.
func TestTSIGKeyFromAPIVerifiesOnTheDNSServer(t *testing.T) {
	ctx := t.Context()
	var upstreamHits atomic.Int64
	upAddr := mockUpstream(t, &upstreamHits)

	dir := t.TempDir()
	a, err := New(ctx, testConfig(dir), "e2e")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Store().Settings().SetInternal(ctx, "upstreams", upAddr); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Shutdown(context.Background()) }()
	a.WaitReady(5 * time.Second)

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp := postJSON(t, c, apiURL(a, "/api/v1/setup"), `{"username":"admin","password":"password123"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	resp = postJSON(t, c, apiURL(a, "/api/v1/auth/login"), `{"username":"admin","password":"password123"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", resp.StatusCode)
	}

	const secret = "c2VjcmV0LXNlY3JldC1zZWNyZXQ="
	resp = postJSON(t, c, apiURL(a, "/api/v1/tsig-keys"),
		`{"name":"XFER.E412.IN","algorithm":"hmac-sha256.","secret":"`+secret+`"}`)
	var created struct {
		ID int64 `json:"id"`
	}
	derr := json.NewDecoder(resp.Body).Decode(&created)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || derr != nil {
		t.Fatalf("tsig key create: status=%d decode=%v", resp.StatusCode, derr)
	}

	// The peer signs under the canonical name, which is what the API was
	// given a non-canonical spelling of.
	sign := func() *dns.Msg {
		t.Helper()
		dc := new(dns.Client)
		dc.TsigSecret = map[string]string{"xfer.e412.in.": secret}
		m := new(dns.Msg)
		m.SetQuestion("bifrost.e412.test.", dns.TypeA)
		m.SetTsig("xfer.e412.in.", dns.HmacSHA256, 300, time.Now().Unix())
		// dns.Client verifies the reply's MAC with this same secret, so a
		// signed answer here also proves the server signed it correctly.
		r, _, err := dc.Exchange(m, a.DNSAddr())
		if err != nil {
			t.Fatalf("signed exchange: %v", err)
		}
		return r
	}

	if sign().IsTsig() == nil {
		t.Fatal("reply carried no TSIG: the key the API stored is not the key the DNS server found")
	}

	// Revocation travels the same path on the same terms.
	req, err := http.NewRequest(http.MethodDelete, apiURL(a, fmt.Sprintf("/api/v1/tsig-keys/%d", created.ID)), nil)
	if err != nil {
		t.Fatal(err)
	}
	dresp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = dresp.Body.Close()
	if dresp.StatusCode != http.StatusNoContent {
		t.Fatalf("tsig key delete: %d", dresp.StatusCode)
	}
	if sign().IsTsig() != nil {
		t.Fatal("reply still signed after the key was deleted through the API")
	}
}

// TestZoneTransferIsServedForAZoneCreatedOverTheAPI is the D3 counterpart of
// TestTSIGKeyFromAPIVerifiesOnTheDNSServer: a seam that only exists where a
// real store meets a real dnssrv.Server, so it can only be tested here, not
// in internal/zones (which builds its own TransferServer against its own
// server, never through App) or internal/api (which never opens a DNS
// socket).
//
// It proves the wiring itself — that App.New's a.xfrOut reaches
// dnssrv.WithTransfers on every listener App.Start opens — as distinct from
// internal/zones/loopback_test.go, which proves D2's client and D3's server
// agree with each other regardless of how either is wired into a process.
// Before this wiring, s.transfers is nil (dnssrv/server.go), a transfer
// query never reaches TransferServer at all, and falls through to the
// ordinary pipeline instead — which is what "the app's own server refuses"
// means for this test: not a TSIG or ACL refusal, but the intercept never
// firing.
func TestZoneTransferIsServedForAZoneCreatedOverTheAPI(t *testing.T) {
	ctx := t.Context()
	var upstreamHits atomic.Int64
	upAddr := mockUpstream(t, &upstreamHits)

	dir := t.TempDir()
	a, err := New(ctx, testConfig(dir), "e2e")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Store().Settings().SetInternal(ctx, "upstreams", upAddr); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Shutdown(context.Background()) }()
	a.WaitReady(5 * time.Second)

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp := postJSON(t, c, apiURL(a, "/api/v1/setup"), `{"username":"admin","password":"password123"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	resp = postJSON(t, c, apiURL(a, "/api/v1/auth/login"), `{"username":"admin","password":"password123"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", resp.StatusCode)
	}

	const zoneName = "xfer-wire.test"
	resp = postJSON(t, c, apiURL(a, "/api/v1/zones"),
		`{"name":"`+zoneName+`","allow_transfer":"127.0.0.0/8"}`)
	var zoneCreated struct{ ID int64 }
	derr := json.NewDecoder(resp.Body).Decode(&zoneCreated)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || derr != nil {
		t.Fatalf("zone create: status=%d decode=%v", resp.StatusCode, derr)
	}
	resp = postJSON(t, c, apiURL(a, fmt.Sprintf("/api/v1/zones/%d/records", zoneCreated.ID)),
		`{"name":"bifrost","type":"A","ttl":300,"rdata":"10.30.0.1"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("zone record create: %d", resp.StatusCode)
	}

	// AXFR the zone straight off the running server's own DNS listener — the
	// same address ordinary queries answer on, since a transfer arrives on
	// both listeners the same way a query does (dnssrv/server.go's serve is
	// the handler for each).
	q := new(dns.Msg)
	q.SetAxfr(dns.Fqdn(zoneName))
	tr := new(dns.Transfer)
	ch, err := tr.In(q, a.DNSAddr())
	if err != nil {
		t.Fatalf("AXFR dial: %v", err)
	}
	var rrs []dns.RR
	for env := range ch {
		if env.Error != nil {
			t.Fatalf("AXFR: %v (nothing wires WithTransfers, or allow_transfer was not honoured)", env.Error)
		}
		rrs = append(rrs, env.RR...)
	}

	found := false
	for _, rr := range rrs {
		arec, ok := rr.(*dns.A)
		if ok && arec.Hdr.Name == "bifrost."+zoneName+"." && arec.A.String() == "10.30.0.1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("AXFR did not carry the record created through the API: %v", rrs)
	}
}
