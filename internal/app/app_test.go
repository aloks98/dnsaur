package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/config"
	"github.com/miekg/dns"
)

func TestParseUpstreams(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"trims spaces", " 1.1.1.1:53 , 8.8.8.8:53 ", []string{"1.1.1.1:53", "8.8.8.8:53"}},
		{"drops empties", "1.1.1.1:53,,  ,9.9.9.9:53", []string{"1.1.1.1:53", "9.9.9.9:53"}},
		{"appends missing port", "1.1.1.1,dns.example.com", []string{"1.1.1.1:53", "dns.example.com:53"}},
		{"keeps existing port", "1.1.1.1:5353", []string{"1.1.1.1:5353"}},
		{"brackets bare ipv6", "::1", []string{"[::1]:53"}},
		{"leaves bracketed ipv6 with port as-is", "[::1]:53", []string{"[::1]:53"}},
		{"adds port to bracketed ipv6 missing one", "[::1]", []string{"[::1]:53"}},
		{"empty string yields nothing", "", nil},
		{"all whitespace/commas yields nothing", " , , ", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseUpstreams(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("parseUpstreams(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseUpstreams(%q) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

// TestApplySettingsFallsBackWhenNoForwarderYet reproduces the "nil forwarder
// on first applySettings failure" bug: if upstream.New fails while
// swappable.h has never been set, every DNS query used to panic (recovered,
// but resolution was permanently dead) instead of falling back to something
// usable.
//
// The settings store is seeded with a corrupt upstream.strategy but valid
// (mock) upstreams *before* Start runs applySettings for the first time.
// Since real default upstreams point at the public internet, this asserts
// the fallback retries upstream.New with the *stored* upstreams under the
// default strategy (rather than jumping straight to hardcoded defaults),
// and that resolution actually goes through the mock upstream.
func TestApplySettingsFallsBackWhenNoForwarderYet(t *testing.T) {
	ctx := context.Background()
	var upstreamHits atomic.Int64
	upAddr := mockUpstream(t, &upstreamHits)

	dir := t.TempDir()
	cfg := &config.Config{DNSListen: []string{"127.0.0.1:0"}, HTTPListen: ":0", DataDir: dir, LogLevel: "error"}
	cfg.Storage.Driver = "sqlite"
	cfg.Storage.DSN = dir + "/t.db"

	a, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := a.Store()
	if err := s.Settings().SetInternal(ctx, "upstream.strategy", "not-a-real-strategy"); err != nil {
		t.Fatal(err)
	}
	if err := s.Settings().SetInternal(ctx, "upstreams", upAddr); err != nil {
		t.Fatal(err)
	}

	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Shutdown(context.Background()) }()
	a.WaitReady(5 * time.Second)

	if !a.fwd.isSet() {
		t.Fatal("forwarder was never installed; applySettings left swappable.h nil")
	}

	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn("example.org"), dns.TypeA)
	r, _, err := c.Exchange(m, a.DNSAddr())
	if err != nil {
		t.Fatal(err)
	}
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) == 0 {
		t.Fatalf("resolution failed after forwarder fallback: rcode=%v answer=%v", r.Rcode, r.Answer)
	}
	if upstreamHits.Load() == 0 {
		t.Fatal("mock upstream was never queried; fallback did not use the stored (valid) upstreams")
	}
}
