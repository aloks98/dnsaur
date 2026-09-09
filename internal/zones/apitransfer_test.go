package zones_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aloks98/dnsaur/internal/api"
	"github.com/aloks98/dnsaur/internal/auth"
	"github.com/aloks98/dnsaur/internal/filter"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
	"github.com/miekg/dns"
)

// The join, tested end to end: a secondary created through the REST API,
// transferred by the scheduler from a real primary, and answered for by the
// resolver.
//
// Every layer already had tests, and every one of them tested its own half
// against something it built itself. The API tests create zones and then hand
// the transfer to a fake refresher. transfer_test.go and refresh_test.go
// transfer real zones over real TCP, but from rows they built with
// store.AddZone — under a fixture comment that says "a secondary as Task 1's
// API creates one", which is a claim no test checks. The e2e drives a UI
// create into a transfer that is *meant* to fail, so it never reaches the
// serving half.
//
// So the agreement between what the API writes into a zone row and what the
// transfer path expects to read out of it was held together by that comment.
// This is the test that would notice it breaking: nothing here constructs a
// zone, a key or a record itself — the API writes them, the scheduler reads
// them, and the resolver is asked what a client would get.
//
// The TSIG key goes through the API too, and deliberately: it is the sharper
// half of the same agreement. The API canonicalises a key's name on write
// (lowercase, trailing dot) and stores an id on the zone; the transfer
// resolves that id back to a key and signs with the stored name; and the
// primary verifies against the same name. A disagreement anywhere along that
// chain is a REFUSED, not a compile error.
func TestAZoneCreatedThroughTheAPIIsTransferredAndServed(t *testing.T) {
	ctx := context.Background()
	st, resolver, h := newAPIAndResolver(t)

	// The primary verifies against the same key store the secondary signs
	// from — one key, two ends, the way a real pair is configured. It reads
	// the store per message, so it is started before the key exists.
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t), withTSIG(st.TSIGKeys()))

	// Typed the way an operator would type it: mixed case, no trailing dot.
	// What the transfer signs with is whatever the API decided to store.
	keyID := apiCreate(t, h, "/api/v1/tsig-keys", fmt.Sprintf(
		`{"name":"XFER.%s","algorithm":"hmac-sha256.","secret":"c2VjcmV0LXNlY3JldC1zZWNyZXQ="}`,
		transferApex))
	zoneID := apiCreate(t, h, "/api/v1/zones", fmt.Sprintf(
		`{"name":%q,"type":"secondary","primaries":%q,"tsig_key_id":%d}`,
		transferApex, primary.addr, keyID))

	// Before the first transfer the zone holds nothing it may speak for, and
	// says so by saying nothing — never NXDOMAIN, which would black-hole the
	// whole suffix for every resolver that believed it.
	if got := askResolver(t, resolver, "bifrost."+transferApex, dns.TypeA).Rcode; got != dns.RcodeServerFailure {
		t.Fatalf("before the transfer: rcode = %s, want SERVFAIL", dns.RcodeToString[got])
	}

	// The scheduler, not a hand-built Transferrer: a zone created moments ago
	// has never transferred, so it is due at once.
	ref := zones.NewRefresher(st.Zones(), zones.NewTransferrer(st.Zones(), st.TSIGKeys(),
		zones.WithReload(resolver.Reload)))
	if err := ref.RefreshDue(ctx); err != nil {
		t.Fatalf("RefreshDue: %v", err)
	}

	// The whole point: a name inside the zone is now answered from data this
	// server never authored.
	m := askResolver(t, resolver, "bifrost."+transferApex, dns.TypeA)
	if m.Rcode != dns.RcodeSuccess {
		t.Fatalf("after the transfer: rcode = %s, want NOERROR", dns.RcodeToString[m.Rcode])
	}
	if len(m.Answer) == 0 {
		t.Fatal("after the transfer: NOERROR with no answer")
	}
	if !m.Authoritative {
		t.Error("the answer is not authoritative; a secondary answers for its zone as an authority")
	}

	// And the zone row agrees, read back through the API that created it.
	z := apiZone(t, h, zoneID)
	if z.SOASerial != primarySerial {
		t.Errorf("soa_serial = %d, want the primary's %d — adopted verbatim, not invented",
			z.SOASerial, primarySerial)
	}
	if z.RefreshedAt == 0 || z.ExpiresAt == 0 {
		t.Errorf("refreshed_at = %d expires_at = %d, want both stamped by the install",
			z.RefreshedAt, z.ExpiresAt)
	}
	if z.LastError != "" {
		t.Errorf("last_error = %q after a transfer that worked, want it cleared", z.LastError)
	}
	if z.LastAttempt == 0 {
		t.Error("last_attempt = 0 after a transfer; a success is an attempt too")
	}
}

// The same span, with the manual path in place of the schedule: the button
// the dashboard actually offers, through the route it actually calls.
//
// Worth its own test rather than a variation of the one above, because the
// two paths reach the transfer differently — RefreshDue decides a zone is due
// and re-reads it under a lock, while this one is handed an id from a URL —
// and only this one crosses the API boundary in both directions.
func TestRefreshNowTransfersAZoneCreatedThroughTheAPI(t *testing.T) {
	resolver, h := newAPIAndResolverWithRefresher(t)
	primary := startTestPrimary(t, transferApex, primaryZoneRRs(t))

	zoneID := apiCreate(t, h, "/api/v1/zones", fmt.Sprintf(
		`{"name":%q,"type":"secondary","primaries":%q}`, transferApex, primary.addr))

	rec := apiDo(t, h, http.MethodPost, fmt.Sprintf("/api/v1/zones/%d/refresh", zoneID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: status = %d body = %s, want 200", rec.Code, rec.Body)
	}
	var got struct {
		Primary string `json:"primary"`
		Serial  uint32 `json:"serial"`
		Records int    `json:"records"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", rec.Body, err)
	}
	if got.Primary != primary.addr {
		t.Errorf("primary = %q, want the one that answered (%q)", got.Primary, primary.addr)
	}
	if got.Serial != primarySerial || got.Records == 0 {
		t.Errorf("serial = %d records = %d, want %d and a non-empty zone", got.Serial, got.Records, primarySerial)
	}

	if m := askResolver(t, resolver, "bifrost."+transferApex, dns.TypeA); m.Rcode != dns.RcodeSuccess {
		t.Errorf("after Refresh now: rcode = %s, want NOERROR", dns.RcodeToString[m.Rcode])
	}
}

// The same button on a stub, which reaches the other worker entirely: two
// ordinary queries to a master rather than an AXFR to a primary.
//
// Worth its own end-to-end pass because three layers have to agree about
// which types have a master at all — the handler's gate, the scheduler's, and
// the fetcher's own — and each of them says so in its own words. The zone is
// created through the API and never touched directly, so a disagreement shows
// up as a 400 or a 502 rather than as a compile error.
func TestRefreshNowFetchesAStubCreatedThroughTheAPI(t *testing.T) {
	resolver, h := newAPIAndResolverWithRefresher(t)
	master := startStubMaster(t, transferApex, stubMasterConfig{
		serial: primarySerial,
		ns:     []string{stubNSLine("ns1." + transferApex)},
		glue:   []string{fmt.Sprintf("ns1.%s. 3600 IN A 10.9.0.1", transferApex)},
	})

	zoneID := apiCreate(t, h, "/api/v1/zones", fmt.Sprintf(
		`{"name":%q,"type":"stub","primaries":%q}`, transferApex, master.addr))

	rec := apiDo(t, h, http.MethodPost, fmt.Sprintf("/api/v1/zones/%d/refresh", zoneID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: status = %d body = %s, want 200", rec.Code, rec.Body)
	}
	var got struct {
		Primary     string `json:"primary"`
		Serial      uint32 `json:"serial"`
		Records     int    `json:"records"`
		RefreshedAt int64  `json:"refreshed_at"`
		ExpiresAt   int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", rec.Body, err)
	}
	if got.Primary != master.addr {
		t.Errorf("primary = %q, want the master that answered (%q)", got.Primary, master.addr)
	}
	if got.Serial != primarySerial {
		t.Errorf("serial = %d, want the master's %d", got.Serial, primarySerial)
	}
	if got.Records != 2 {
		t.Errorf("records = %d, want 2 (the NS record and its glue)", got.Records)
	}
	if got.RefreshedAt == 0 {
		t.Errorf("refreshed_at = 0 after a fetch that worked")
	}
	// Not an oversight and not "unknown": a stub is never given an expiry.
	if got.ExpiresAt != 0 {
		t.Errorf("expires_at = %d, want 0 — a stub does not expire", got.ExpiresAt)
	}

	// And the delegation is routable from what the server is serving, with no
	// reload of this test's own: the fetch published it.
	z := resolver.Snapshot().Apex(transferApex)
	if z == nil {
		t.Fatalf("the stub is not in the served snapshot")
	}
	if u := zones.StubUpstreams(*z); len(u) != 1 || u[0] != "10.9.0.1:53" {
		t.Errorf("StubUpstreams = %v, want [10.9.0.1:53]", u)
	}
}

// ── the harness ─────────────────────────────────────────────────────────────

// apiReloader is the hook the API calls after a write. Wiring it to the
// resolver is what makes a zone created through the API visible to a query
// without the test reloading anything by hand — the same wiring internal/app
// does in production.
type apiReloader struct{ reload func(context.Context) error }

func (a apiReloader) ReloadClients(context.Context) error { return nil }
func (a apiReloader) ReloadZones(ctx context.Context) error {
	return a.reload(ctx)
}
func (a apiReloader) RefreshFilters(context.Context) error   { return nil }
func (a apiReloader) RecompileFilters(context.Context) error { return nil }
func (a apiReloader) NotifyZones()                           {}

// newAPIAndResolver builds one store with a real API server and a real
// resolver over it, plus an authenticated handler to drive the API through.
func newAPIAndResolver(t *testing.T) (store.Store, *zones.Resolver, http.Handler) {
	t.Helper()
	st, resolver, h, _ := buildAPIAndResolver(t, false)
	return st, resolver, h
}

// newAPIAndResolverWithRefresher is the same, with the zone scheduler wired
// into Deps so POST /zones/{id}/refresh has something to call.
func newAPIAndResolverWithRefresher(t *testing.T) (*zones.Resolver, http.Handler) {
	t.Helper()
	_, resolver, h, _ := buildAPIAndResolver(t, true)
	return resolver, h
}

func buildAPIAndResolver(t *testing.T, withRefresher bool) (store.Store, *zones.Resolver, http.Handler, string) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, "sqlite", t.TempDir()+"/t.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	resolver := zones.NewResolver(st.Zones())
	if err := resolver.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	svc := auth.New(st.Users(), st.Tokens())
	if err := svc.CreateAdmin(ctx, "admin", "password123"); err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	token, err := svc.Login(ctx, "admin", "password123", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	deps := api.Deps{
		Store:    st,
		Auth:     svc,
		Engine:   filter.NewEngine(),
		Reloader: apiReloader{reload: resolver.Reload},
		Version:  "test",
	}
	if withRefresher {
		// Both workers, as App.New wires them: a secondary is pulled by the
		// Transferrer and a stub by the StubFetcher, which is a separate
		// object with a reload of its own. The reload here is the resolver's
		// because this harness has no forwarder to route with — production
		// passes App.ReloadZones, and internal/app is where that is pinned.
		deps.ZoneRefresher = zones.NewRefresher(st.Zones(),
			zones.NewTransferrer(st.Zones(), st.TSIGKeys(), zones.WithReload(resolver.Reload)),
			zones.WithStubFetcher(zones.NewStubFetcher(st.Zones(), st.TSIGKeys(),
				zones.WithStubReload(resolver.Reload))))
	}
	h := authedHandler{h: api.New(deps).Handler(), token: token}
	return st, resolver, h, token
}

// authedHandler presents the admin's credentials on every request, so the
// tests above read as what they are about rather than as auth plumbing.
type authedHandler struct {
	h     http.Handler
	token string
}

func (a authedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Header.Set("Authorization", "Bearer "+a.token)
	a.h.ServeHTTP(w, r)
}

func apiDo(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// apiCreate POSTs body and returns the id the handler answered with.
func apiCreate(t *testing.T, h http.Handler, path, body string) int64 {
	t.Helper()
	rec := apiDo(t, h, http.MethodPost, path, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST %s: status = %d body = %s, want 201", path, rec.Code, rec.Body)
	}
	var got struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", rec.Body, err)
	}
	if got.ID == 0 {
		t.Fatalf("POST %s answered with no id: %s", path, rec.Body)
	}
	return got.ID
}

// apiZone reads a zone back through GET /zones/{id}, so what is asserted is
// what a client of this API would see rather than what the store holds.
func apiZone(t *testing.T, h http.Handler, id int64) store.Zone {
	t.Helper()
	rec := apiDo(t, h, http.MethodGet, fmt.Sprintf("/api/v1/zones/%d", id), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET zone %d: status = %d body = %s", id, rec.Code, rec.Body)
	}
	var z store.Zone
	if err := json.Unmarshal(rec.Body.Bytes(), &z); err != nil {
		t.Fatalf("unmarshal %s: %v", rec.Body, err)
	}
	return z
}
