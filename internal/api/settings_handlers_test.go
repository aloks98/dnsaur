package api

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/certtest"
	"github.com/aloks98/dnsaur/internal/filter"
	"github.com/aloks98/dnsaur/internal/store"
)

func TestSettingsGetPut(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()
	_ = s.Settings().SetInternal(t.Context(), "blocking.mode", "null-ip")
	_ = s.Settings().SetInternal(t.Context(), "instance.id", "secret-id")

	w := doReq(t, h, "GET", "/api/v1/settings", "", cookie)
	var m map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if w.Code != 200 || m["blocking.mode"] != "null-ip" {
		t.Fatalf("get: %d %v", w.Code, m)
	}
	if _, leaked := m["instance.id"]; leaked {
		t.Fatal("internal key leaked")
	}
	if w := doReq(t, h, "PUT", "/api/v1/settings", `{"key":"blocking.mode","value":"nxdomain"}`, cookie); w.Code != 204 {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	if v, _, _ := s.Settings().Get(t.Context(), "blocking.mode"); v != "nxdomain" {
		t.Fatalf("not persisted: %s", v)
	}
	if w := doReq(t, h, "PUT", "/api/v1/settings", `{"key":"blocking.mode","value":"weird"}`, cookie); w.Code != 400 {
		t.Fatalf("bad enum accepted: %d", w.Code)
	}
	if w := doReq(t, h, "PUT", "/api/v1/settings", `{"key":"instance.id","value":"x"}`, cookie); w.Code != 400 {
		t.Fatalf("non-allowlisted key accepted: %d", w.Code)
	}
	if w := doReq(t, h, "PUT", "/api/v1/settings", `{"key":"cache.max_entries","value":"-3"}`, cookie); w.Code != 400 {
		t.Fatalf("negative numeric accepted: %d", w.Code)
	}
}

// stats.retention_days shares a prefix with stats.watermark, which
// GET /settings hides — the two must not be hidden together. One is the
// rollup's own bookkeeping and no operator can edit it; the other is a
// setting like any other, and a settings screen that cannot read it cannot
// show what it is set to. Zero is refused: it would delete every bucket on
// the next prune, leaving the dashboard permanently empty.
func TestStatsRetentionIsEditableAndVisible(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()
	_ = s.Settings().SetInternal(t.Context(), "stats.retention_days", "365")
	_ = s.Settings().SetInternal(t.Context(), "stats.watermark", "4711")

	w := doReq(t, h, "GET", "/api/v1/settings", "", cookie)
	var m map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if m["stats.retention_days"] != "365" {
		t.Fatalf("stats.retention_days not returned: %v", m)
	}
	if _, leaked := m["stats.watermark"]; leaked {
		t.Fatal("the rollup watermark leaked into GET /settings")
	}
	if w := doReq(t, h, "PUT", "/api/v1/settings", `{"key":"stats.retention_days","value":"30"}`, cookie); w.Code != 204 {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	if v, _, _ := s.Settings().Get(t.Context(), "stats.retention_days"); v != "30" {
		t.Fatalf("not persisted: %s", v)
	}
	if w := doReq(t, h, "PUT", "/api/v1/settings", `{"key":"stats.retention_days","value":"0"}`, cookie); w.Code != 400 {
		t.Fatalf("zero retention accepted: %d", w.Code)
	}
	if w := doReq(t, h, "PUT", "/api/v1/settings", `{"key":"stats.watermark","value":"0"}`, cookie); w.Code != 400 {
		t.Fatalf("watermark accepted as editable: %d", w.Code)
	}
}

// lists.refresh_hours is the one interval that becomes a time.Ticker, and 0
// panics one. The setting is restart-required, so a stored 0 does not fail
// the write that made it — it fails the next start, and the one after that,
// until someone edits the row by hand. This is the end of it that can still
// say no.
func TestRefreshHoursRejectsZero(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()

	w := doReq(t, h, "PUT", "/api/v1/settings", `{"key":"lists.refresh_hours","value":"0"}`, cookie)
	if w.Code != 400 {
		t.Fatalf("lists.refresh_hours=0 accepted: %d %s", w.Code, w.Body.String())
	}
	if want := "invalid value for lists.refresh_hours: must be a whole number, one or more"; !strings.Contains(w.Body.String(), want) {
		t.Errorf("rejection = %s, want it to contain %q", w.Body.String(), want)
	}
	if v, ok, _ := s.Settings().Get(t.Context(), "lists.refresh_hours"); ok && v == "0" {
		t.Error("the rejected value was stored anyway")
	}

	if w := doReq(t, h, "PUT", "/api/v1/settings", `{"key":"lists.refresh_hours","value":"1"}`, cookie); w.Code != 204 {
		t.Fatalf("lists.refresh_hours=1 rejected: %d %s", w.Code, w.Body.String())
	}
	// The other integer keys still accept 0 — none of them ticks, and 0 is a
	// meaningful value for every one of them.
	if w := doReq(t, h, "PUT", "/api/v1/settings", `{"key":"qlog.retention_days","value":"0"}`, cookie); w.Code != 204 {
		t.Fatalf("qlog.retention_days=0 rejected: %d %s", w.Code, w.Body.String())
	}
}

// blockingStatusFor reads GET /blocking with the given query.
func blockingStatusFor(t *testing.T, h http.Handler, cookie *http.Cookie, query string) blockingStatus {
	t.Helper()
	w := doReq(t, h, "GET", "/api/v1/blocking"+query, "", cookie)
	if w.Code != 200 {
		t.Fatalf("GET /blocking%s: %d %s", query, w.Code, w.Body.String())
	}
	var st blockingStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("GET /blocking%s: %v (%s)", query, err, w.Body.String())
	}
	return st
}

func TestBlockingPauseResume(t *testing.T) {
	srv, s, _ := testServer(t)
	srv.deps.Engine = filter.NewEngine()
	cookie := login(t, srv, s)
	h := srv.Handler()

	if w := doReq(t, h, "POST", "/api/v1/blocking/pause", `{"group_id":0,"minutes":5}`, cookie); w.Code != 204 {
		t.Fatalf("pause: %d %s", w.Code, w.Body.String())
	}
	st := blockingStatusFor(t, h, cookie, "")
	if st.PausedUntil <= time.Now().UnixMilli() || st.Scope != filter.PauseGlobal {
		t.Fatalf("status: %+v", st)
	}
	if w := doReq(t, h, "DELETE", "/api/v1/blocking/pause?group_id=0", "", cookie); w.Code != 204 {
		t.Fatalf("resume: %d", w.Code)
	}
	if st := blockingStatusFor(t, h, cookie, ""); st.PausedUntil != 0 || st.Scope != "" {
		t.Fatalf("still paused: %+v", st)
	}
	if w := doReq(t, h, "POST", "/api/v1/blocking/pause", `{"group_id":0,"minutes":0}`, cookie); w.Code != 400 {
		t.Fatalf("zero minutes accepted: %d", w.Code)
	}
}

// TestBlockingPauseRejectsBothIDs: a pause covers one scope. A body or query
// naming a group *and* a client names none of them, and picking one would
// pause something the caller never asked for.
func TestBlockingPauseRejectsBothIDs(t *testing.T) {
	srv, s, _ := testServer(t)
	srv.deps.Engine = filter.NewEngine()
	cookie := login(t, srv, s)
	h := srv.Handler()

	for _, tc := range []struct{ method, url, body string }{
		{"POST", "/api/v1/blocking/pause", `{"group_id":2,"client_id":4,"minutes":5}`},
		{"DELETE", "/api/v1/blocking/pause?group_id=2&client_id=4", ""},
		{"GET", "/api/v1/blocking?group_id=2&client_id=4", ""},
	} {
		w := doReq(t, h, tc.method, tc.url, tc.body, cookie)
		if w.Code != 400 {
			t.Errorf("%s %s: %d %s, want 400", tc.method, tc.url, w.Code, w.Body.String())
		}
		if want := "send group_id or client_id, not both"; !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s %s: %s, want it to contain %q", tc.method, tc.url, w.Body.String(), want)
		}
	}
	// And nothing was paused on the way to the rejection.
	if st := blockingStatusFor(t, h, cookie, ""); st.PausedUntil != 0 {
		t.Fatalf("a refused pause took effect anyway: %+v", st)
	}
}

// TestBlockingReportsTheEffectiveScope: a client's pause control reads one
// number and has to know whose it is. A pause on the client's group shows up
// on the client as scope "group" — its Resume would clear the client's own
// pause, not the group's, so a control that could not tell them apart would
// offer an action that does nothing.
func TestBlockingReportsTheEffectiveScope(t *testing.T) {
	srv, s, _ := testServer(t)
	srv.deps.Engine = filter.NewEngine()
	cookie := login(t, srv, s)
	h := srv.Handler()

	gid, err := s.Clients().AddGroup(t.Context(), "office")
	if err != nil {
		t.Fatal(err)
	}
	cid, err := s.Clients().AddClient(t.Context(), store.Client{Name: "laptop", Matcher: "10.0.0.5", GroupID: gid})
	if err != nil {
		t.Fatal(err)
	}
	client := "?client_id=" + strconv.FormatInt(cid, 10)

	if w := doReq(t, h, "POST", "/api/v1/blocking/pause", `{"group_id":`+strconv.FormatInt(gid, 10)+`,"minutes":5}`, cookie); w.Code != 204 {
		t.Fatalf("pause the group: %d %s", w.Code, w.Body.String())
	}
	st := blockingStatusFor(t, h, cookie, client)
	if st.PausedUntil <= time.Now().UnixMilli() || st.Scope != filter.PauseGroup {
		t.Fatalf("the client did not inherit its group's pause: %+v", st)
	}
	groupUntil := st.PausedUntil

	// The client's own, longer pause takes over — and says so.
	if w := doReq(t, h, "POST", "/api/v1/blocking/pause", `{"client_id":`+strconv.FormatInt(cid, 10)+`,"minutes":60}`, cookie); w.Code != 204 {
		t.Fatalf("pause the client: %d %s", w.Code, w.Body.String())
	}
	st = blockingStatusFor(t, h, cookie, client)
	if st.Scope != filter.PauseClient || st.PausedUntil <= groupUntil {
		t.Fatalf("the client's own pause did not win: %+v", st)
	}
	// …while the group's other clients are unaffected by it.
	if st := blockingStatusFor(t, h, cookie, "?group_id="+strconv.FormatInt(gid, 10)); st.Scope != filter.PauseGroup || st.PausedUntil != groupUntil {
		t.Fatalf("pausing one client moved the group's pause: %+v", st)
	}

	// Resuming the client leaves the group's pause running under it.
	if w := doReq(t, h, "DELETE", "/api/v1/blocking/pause"+client, "", cookie); w.Code != 204 {
		t.Fatalf("resume the client: %d", w.Code)
	}
	if st := blockingStatusFor(t, h, cookie, client); st.Scope != filter.PauseGroup || st.PausedUntil != groupUntil {
		t.Fatalf("resuming the client cleared its group's pause: %+v", st)
	}
}

// TestPausesAreNotAnEditableSetting: the pause state lives in a settings row
// like the rollup watermark does, and gets the same treatment — hidden from
// GET /settings and refused by PUT. blocking.mode and blocking.ttl share its
// prefix and must stay readable.
func TestPausesAreNotAnEditableSetting(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()
	_ = s.Settings().SetInternal(t.Context(), "blocking.mode", "null-ip")
	_ = s.Settings().SetInternal(t.Context(), filter.PausesKey, `{"global":4711}`)

	w := doReq(t, h, "GET", "/api/v1/settings", "", cookie)
	var m map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if m["blocking.mode"] != "null-ip" {
		t.Fatalf("blocking.mode not returned: %v", m)
	}
	if _, leaked := m[filter.PausesKey]; leaked {
		t.Fatalf("the pause state leaked into GET /settings: %v", m)
	}
	if w := doReq(t, h, "PUT", "/api/v1/settings", `{"key":"`+filter.PausesKey+`","value":"{}"}`, cookie); w.Code != 400 {
		t.Fatalf("the pause state accepted as editable: %d", w.Code)
	}
}

// A malformed upstreams value is refused at the moment it is saved. Before
// this, editableSettings["upstreams"] was `TrimSpace(v) != ""`: the value
// was stored, and the entry was silently dropped at the next restart — so
// the typo surfaced as DNS being down, hours later, with nothing pointing
// back at the edit that caused it.
func TestSettingsPutRejectsMalformedUpstreams(t *testing.T) {
	for _, tc := range []struct {
		name, value, wantIn string
	}{
		{"hostname where an address is required", "tls://cloudflare-dns.com:853#cloudflare-dns.com", "is a name"},
		{"encrypted entry with no certificate name", "tls://1.1.1.1:853", `missing "#name"`},
		{"transports mixed", "1.1.1.1:53,tls://9.9.9.9:853#dns.quad9.net", "same transport"},
		{"unknown transport", "quic://1.1.1.1:853#dns.adguard.com", "unknown transport"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t)
			rec := ts.do(t, http.MethodPut, "/api/v1/settings",
				`{"key":"upstreams","value":`+strconv.Quote(tc.value)+`}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT /settings = %d, want 400", rec.Code)
			}
			// Decoded rather than matched against the raw body: the parser's
			// messages quote the offending token (e.g. `missing "#name"`),
			// and JSON escapes those quotes on the wire, so a raw
			// strings.Contains on rec.Body would never see the unescaped
			// substring back.
			var got struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal %s: %v", rec.Body, err)
			}
			// The prefix is what other tests assert on; the reason is what
			// this task adds. Both have to be there.
			if !strings.Contains(got.Error, "invalid value for upstreams") {
				t.Errorf("error %q lost the existing prefix", got.Error)
			}
			if !strings.Contains(got.Error, tc.wantIn) {
				t.Errorf("error %q does not say why; want it to contain %q", got.Error, tc.wantIn)
			}
		})
	}
}

// A well-formed encrypted value still saves — the guard against a validator
// so strict it rejects the feature it exists to enable.
func TestSettingsPutAcceptsEncryptedUpstreams(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, http.MethodPut, "/api/v1/settings",
		`{"key":"upstreams","value":"tls://1.1.1.1:853#cloudflare-dns.com, tls://1.0.0.1:853#cloudflare-dns.com"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PUT /settings = %d (%s), want 204", rec.Code, rec.Body.String())
	}
}

// fakeResolverStatus stands in for *app.App, which this package cannot
// import (it imports this one).
type fakeResolverStatus struct {
	active bool
	reason string

	dot, doh ProtocolStatus
	// dnsListening is what App reports for "some socket is answering DNS",
	// which readiness turns on. False by default, so every existing fixture
	// keeps meaning "a server with no listeners behind it".
	dnsListening bool

	notAfter     time.Time
	expiringSoon bool
	certLoaded   bool
}

func (f *fakeResolverStatus) UpstreamDowngrade() (bool, string) { return f.active, f.reason }

func (f *fakeResolverStatus) Serving() (dot, doh ProtocolStatus) { return f.dot, f.doh }

func (f *fakeResolverStatus) CertExpiry() (notAfter time.Time, expiringSoon, ok bool) {
	return f.notAfter, f.expiringSoon, f.certLoaded
}

func (f *fakeResolverStatus) DNSListening() bool { return f.dnsListening }

// The downgrade is server state, and it is reported by its own endpoint —
// not folded into GET /settings, whose response is a flat map of settings
// and nothing else.
func TestResolverStatusReportsTheDowngradeAndSettingsDoesNot(t *testing.T) {
	fake := &fakeResolverStatus{active: true, reason: `upstream "tls://1.1.1.1:853": missing "#name"`}
	srv, s, _ := testServer(t, func(d *Deps) { d.ResolverStatus = fake })
	cookie := login(t, srv, s)
	h := srv.Handler()

	w := doReq(t, h, "GET", "/api/v1/resolver/status", "", cookie)
	if w.Code != 200 {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		EncryptionDowngraded bool   `json:"encryption_downgraded"`
		Reason               string `json:"reason"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if !got.EncryptionDowngraded || got.Reason != fake.reason {
		t.Fatalf("got %+v, want the downgrade and its reason", got)
	}

	// GET /settings stays a map of settings. A key in there that no PUT
	// could ever write would be a different kind of thing in the same shape.
	ws := doReq(t, h, "GET", "/api/v1/settings", "", cookie)
	var m map[string]string
	if err := json.Unmarshal(ws.Body.Bytes(), &m); err != nil {
		t.Fatalf("settings is not a flat string map: %v", err)
	}
	for k := range m {
		if strings.Contains(k, "downgrade") || strings.Contains(k, "encryption") {
			t.Errorf("GET /settings carries %q; server state does not belong in the settings map", k)
		}
	}

	// And it clears rather than sticking: a warning that outlives the fix
	// teaches the operator to ignore it.
	fake.active, fake.reason = false, ""
	w = doReq(t, h, "GET", "/api/v1/resolver/status", "", cookie)
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.EncryptionDowngraded || got.Reason != "" {
		t.Fatalf("got %+v, want cleared", got)
	}
}

// A server with no App behind it has no forwarder to have downgraded.
func TestResolverStatusWithoutAnAppAnswersFalse(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)

	w := doReq(t, srv.Handler(), "GET", "/api/v1/resolver/status", "", cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"encryption_downgraded":false`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// A server with no App behind it has never loaded a certificate either
	// — that is "no certificate loaded", not "expires soon", and must not
	// be reported as a warning by being present with a zero time.
	if strings.Contains(w.Body.String(), "certificate") {
		t.Fatalf("no App behind the server, but response carries a certificate field: %s", w.Body.String())
	}
}

// DoT and DoH fail independently — one bound, one refused for a privileged
// port already in use — and the status endpoint has to say which is which
// rather than collapsing both into a single "serving: false".
func TestResolverStatusReportsServingPerProtocol(t *testing.T) {
	fake := &fakeResolverStatus{
		dot: ProtocolStatus{Enabled: true, Listening: true, Addr: "0.0.0.0:853"},
		doh: ProtocolStatus{Enabled: true, Listening: false, Addr: "0.0.0.0:443", Err: "listen tcp :443: bind: permission denied"},
	}
	srv, s, _ := testServer(t, func(d *Deps) { d.ResolverStatus = fake })
	cookie := login(t, srv, s)

	w := doReq(t, srv.Handler(), "GET", "/api/v1/resolver/status", "", cookie)
	if w.Code != 200 {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		Serving struct {
			DoT ProtocolStatus `json:"dot"`
			DoH ProtocolStatus `json:"doh"`
		} `json:"serving"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.Serving.DoT != fake.dot {
		t.Errorf("dot = %+v, want %+v", got.Serving.DoT, fake.dot)
	}
	if got.Serving.DoH != fake.doh {
		t.Errorf("doh = %+v, want %+v", got.Serving.DoH, fake.doh)
	}
}

// The certificate object appears, with its expiry, exactly when a
// certificate has actually loaded — and expiring_soon reflects what the
// implementation behind ResolverStatus decided, which is the 14-day
// threshold's job (proven directly against internal/app in
// TestCertExpiryWarnsInsideTheThresholdNotOutsideIt, serve_test.go).
func TestResolverStatusReportsCertificateExpiry(t *testing.T) {
	t.Run("a certificate that has loaded reports its expiry", func(t *testing.T) {
		cert, _ := certtest.ForWithExpiry(t, "dot.test", time.Now().Add(10*24*time.Hour))
		fake := &fakeResolverStatus{certLoaded: true, notAfter: cert.Leaf.NotAfter, expiringSoon: true}
		srv, s, _ := testServer(t, func(d *Deps) { d.ResolverStatus = fake })
		cookie := login(t, srv, s)

		w := doReq(t, srv.Handler(), "GET", "/api/v1/resolver/status", "", cookie)
		var got struct {
			Certificate *struct {
				NotAfter     time.Time `json:"not_after"`
				ExpiringSoon bool      `json:"expiring_soon"`
			} `json:"certificate"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if got.Certificate == nil {
			t.Fatal("certificate field missing while a certificate has loaded")
		}
		if !got.Certificate.NotAfter.Equal(cert.Leaf.NotAfter) {
			t.Errorf("not_after = %v, want %v", got.Certificate.NotAfter, cert.Leaf.NotAfter)
		}
		if !got.Certificate.ExpiringSoon {
			t.Error("expiring_soon = false, want true")
		}
	})

	t.Run("no certificate loaded omits the field rather than warning", func(t *testing.T) {
		fake := &fakeResolverStatus{certLoaded: false}
		srv, s, _ := testServer(t, func(d *Deps) { d.ResolverStatus = fake })
		cookie := login(t, srv, s)

		w := doReq(t, srv.Handler(), "GET", "/api/v1/resolver/status", "", cookie)
		if strings.Contains(w.Body.String(), "certificate") {
			t.Fatalf("no certificate loaded, but response carries a certificate field: %s", w.Body.String())
		}
	})
}

// writeKeypair puts a keypair on disk, as dnssrv's certs_test.go does for
// the same reason, and returns the two paths.
func writeKeypair(t *testing.T, dir, name string, cert tls.Certificate) (certPath, keyPath string) {
	t.Helper()
	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath, certtest.EncodeCert(cert), 0o600); err != nil {
		t.Fatalf("writing the certificate: %v", err)
	}
	if err := os.WriteFile(keyPath, certtest.EncodeKey(t, cert), 0o600); err != nil {
		t.Fatalf("writing the key: %v", err)
	}
	return certPath, keyPath
}

// putSetting is doReq/ts.do narrowed to the one shape every case here needs.
func putSetting(t *testing.T, ts *zoneTestServer, key, value string) *httptest.ResponseRecorder {
	t.Helper()
	return ts.do(t, http.MethodPut, "/api/v1/settings",
		`{"key":`+strconv.Quote(key)+`,"value":`+strconv.Quote(value)+`}`)
}

// settingsErr decodes rec's error field rather than substring-matching the
// raw body — errJSON JSON-encodes the message, so quotes and paths in it
// are escaped on the wire and a raw match on rec.Body would never see the
// unescaped substring back. E1 lost time to exactly that.
func settingsErr(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", rec.Body, err)
	}
	return got.Error
}

// TestSettingsServeDoTAndDoH covers the six serve.* keys added for encrypted
// serving: per-key validation (booleans, listen addresses, absolute paths)
// and the cross-field check that a per-key validator cannot express —
// enabling a protocol is only coherent once a certificate and key are
// already saved, readable, and load as a pair.
func TestSettingsServeDoTAndDoH(t *testing.T) {
	t.Run("enabling with no certificate names both settings", func(t *testing.T) {
		ts := newTestServer(t)
		rec := putSetting(t, ts, "serve.dot.enabled", "true")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT serve.dot.enabled=true = %d, want 400", rec.Code)
		}
		got := settingsErr(t, rec)
		if !strings.Contains(got, "serve.tls.cert") || !strings.Contains(got, "serve.tls.key") {
			t.Errorf("error %q does not name both serve.tls.cert and serve.tls.key", got)
		}
	})

	t.Run("a certificate path that does not exist is named in the rejection", func(t *testing.T) {
		ts := newTestServer(t)
		dir := t.TempDir()
		cert, _ := certtest.For(t, "dns.example.test")
		_, keyPath := writeKeypair(t, dir, "c", cert)
		// The key half is saved and real first, so the write under test is
		// the one that completes — and breaks — the pair.
		if rec := putSetting(t, ts, "serve.tls.key", keyPath); rec.Code != http.StatusNoContent {
			t.Fatalf("PUT serve.tls.key = %d (%s)", rec.Code, rec.Body.String())
		}
		missing := filepath.Join(dir, "missing.crt")
		rec := putSetting(t, ts, "serve.tls.cert", missing)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT serve.tls.cert=%s = %d, want 400", missing, rec.Code)
		}
		if got := settingsErr(t, rec); !strings.Contains(got, missing) {
			t.Errorf("error %q does not name the missing path %q", got, missing)
		}
	})

	t.Run("a valid pair, then enabling, succeeds for both protocols", func(t *testing.T) {
		ts := newTestServer(t)
		dir := t.TempDir()
		cert, _ := certtest.For(t, "dns.example.test")
		certPath, keyPath := writeKeypair(t, dir, "c", cert)
		if rec := putSetting(t, ts, "serve.tls.cert", certPath); rec.Code != http.StatusNoContent {
			t.Fatalf("PUT serve.tls.cert = %d (%s)", rec.Code, rec.Body.String())
		}
		if rec := putSetting(t, ts, "serve.tls.key", keyPath); rec.Code != http.StatusNoContent {
			t.Fatalf("PUT serve.tls.key = %d (%s)", rec.Code, rec.Body.String())
		}
		if rec := putSetting(t, ts, "serve.dot.enabled", "true"); rec.Code != http.StatusNoContent {
			t.Fatalf("PUT serve.dot.enabled=true = %d (%s)", rec.Code, rec.Body.String())
		}
		if rec := putSetting(t, ts, "serve.doh.enabled", "true"); rec.Code != http.StatusNoContent {
			t.Fatalf("PUT serve.doh.enabled=true = %d (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("listen address validation", func(t *testing.T) {
		ts := newTestServer(t)
		if rec := putSetting(t, ts, "serve.dot.listen", "not-an-address"); rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT serve.dot.listen=not-an-address = %d, want 400", rec.Code)
		}
		if rec := putSetting(t, ts, "serve.dot.listen", ":853"); rec.Code != http.StatusNoContent {
			t.Fatalf("PUT serve.dot.listen=:853 = %d (%s)", rec.Code, rec.Body.String())
		}
	})

	// The per-key grammars, each at the edge that used to get through.
	// Every existing case above is a *cross-field* rejection; nothing
	// exercised the single-value validators at their boundaries, which is
	// how ":0" came to be accepted.
	t.Run("per-key grammar rejections", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			key   string
			value string
			want  int
		}{
			// Port 0 binds to whatever is free, so it saves, listens, and
			// reports an address no client was ever told.
			{"listen port 0", "serve.dot.listen", ":0", http.StatusBadRequest},
			{"listen port 65536", "serve.doh.listen", ":65536", http.StatusBadRequest},
			{"listen port not a number", "serve.dot.listen", ":domain", http.StatusBadRequest},
			{"listen port 1", "serve.dot.listen", ":1", http.StatusNoContent},
			{"listen port 65535", "serve.doh.listen", ":65535", http.StatusNoContent},
			// strconv.ParseBool would take all three of these; the stored
			// value is read back by a template and a JS `=== "true"`.
			{"enabled yes", "serve.dot.enabled", "yes", http.StatusBadRequest},
			{"enabled 1", "serve.doh.enabled", "1", http.StatusBadRequest},
			{"enabled True", "serve.dot.enabled", "True", http.StatusBadRequest},
			// A relative certificate path resolves against whatever the
			// process's working directory happens to be, which is not
			// something an operator can reason about from the settings
			// screen.
			{"relative cert path", "serve.tls.cert", "certs/fullchain.pem", http.StatusBadRequest},
			{"relative key path", "serve.tls.key", "./privkey.pem", http.StatusBadRequest},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ts := newTestServer(t)
				if rec := putSetting(t, ts, tc.key, tc.value); rec.Code != tc.want {
					t.Errorf("PUT %s=%q = %d (%s), want %d", tc.key, tc.value, rec.Code, rec.Body.String(), tc.want)
				}
			})
		}
	})

	// Spec §6 lists "no certificate is configured" as knowable at save
	// time and therefore rejectable. Blanking half the keypair while a
	// protocol is live used to return 204, and the next reconcile then
	// stopped the running listener and failed to restart it with
	// "certificate: stat : no such file or directory" — an error naming an
	// empty path, for a save the API had just accepted.
	t.Run("clearing a certificate path under a live protocol is refused", func(t *testing.T) {
		for _, proto := range []string{"serve.dot.enabled", "serve.doh.enabled"} {
			t.Run(proto, func(t *testing.T) {
				ts := newTestServer(t)
				dir := t.TempDir()
				cert, _ := certtest.For(t, "dns.example.test")
				certPath, keyPath := writeKeypair(t, dir, "c", cert)
				for _, kv := range [][2]string{
					{"serve.tls.cert", certPath}, {"serve.tls.key", keyPath}, {proto, "true"},
				} {
					if rec := putSetting(t, ts, kv[0], kv[1]); rec.Code != http.StatusNoContent {
						t.Fatalf("PUT %s = %d (%s)", kv[0], rec.Code, rec.Body.String())
					}
				}

				if rec := putSetting(t, ts, "serve.tls.cert", ""); rec.Code != http.StatusBadRequest {
					t.Errorf("PUT serve.tls.cert=\"\" = %d (%s), want 400 while %s is true", rec.Code, rec.Body.String(), proto)
				}
				if rec := putSetting(t, ts, "serve.tls.key", ""); rec.Code != http.StatusBadRequest {
					t.Errorf("PUT serve.tls.key=\"\" = %d (%s), want 400 while %s is true", rec.Code, rec.Body.String(), proto)
				}

				// Turning the protocol off first is the supported way out,
				// and it must still work — otherwise the certificate can
				// never be removed at all.
				if rec := putSetting(t, ts, proto, "false"); rec.Code != http.StatusNoContent {
					t.Fatalf("PUT %s=false = %d (%s)", proto, rec.Code, rec.Body.String())
				}
				if rec := putSetting(t, ts, "serve.tls.cert", ""); rec.Code != http.StatusNoContent {
					t.Errorf("PUT serve.tls.cert=\"\" = %d (%s) with %s off", rec.Code, rec.Body.String(), proto)
				}
			})
		}
	})

	t.Run("a certificate and key that are not a pair are rejected", func(t *testing.T) {
		ts := newTestServer(t)
		dir := t.TempDir()
		certA, _ := certtest.For(t, "a.example.test")
		certB, _ := certtest.For(t, "b.example.test")
		certPathA, _ := writeKeypair(t, dir, "a", certA)
		_, keyPathB := writeKeypair(t, dir, "b", certB)
		if rec := putSetting(t, ts, "serve.tls.cert", certPathA); rec.Code != http.StatusNoContent {
			t.Fatalf("PUT serve.tls.cert = %d (%s)", rec.Code, rec.Body.String())
		}
		rec := putSetting(t, ts, "serve.tls.key", keyPathB)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT serve.tls.key (mismatched) = %d, want 400", rec.Code)
		}
		if got := settingsErr(t, rec); got == "" {
			t.Error("expected a reason for the mismatched certificate/key pair")
		}
	})

	t.Run("disabling never requires a certificate", func(t *testing.T) {
		ts := newTestServer(t)
		if rec := putSetting(t, ts, "serve.dot.enabled", "false"); rec.Code != http.StatusNoContent {
			t.Fatalf("PUT serve.dot.enabled=false = %d (%s), want 204 with no certificate configured", rec.Code, rec.Body.String())
		}
		if rec := putSetting(t, ts, "serve.doh.enabled", "false"); rec.Code != http.StatusNoContent {
			t.Fatalf("PUT serve.doh.enabled=false = %d (%s), want 204 with no certificate configured", rec.Code, rec.Body.String())
		}
	})

	// The permissions case from spec §4: certbot writes privkey.pem
	// 0600 root:root, and dnsaur may run without permission to read it.
	// That is exactly when disabling has to keep working — reading the
	// key to validate a disable would turn "the certificate broke" into
	// "and now I can't even turn it off".
	t.Run("disabling never requires a certificate, even an unreadable one", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root, which can read a 0000 file")
		}
		ts := newTestServer(t)
		dir := t.TempDir()
		cert, _ := certtest.For(t, "dns.example.test")
		certPath, keyPath := writeKeypair(t, dir, "c", cert)
		if rec := putSetting(t, ts, "serve.tls.cert", certPath); rec.Code != http.StatusNoContent {
			t.Fatalf("PUT serve.tls.cert = %d (%s)", rec.Code, rec.Body.String())
		}
		if rec := putSetting(t, ts, "serve.tls.key", keyPath); rec.Code != http.StatusNoContent {
			t.Fatalf("PUT serve.tls.key = %d (%s)", rec.Code, rec.Body.String())
		}
		if rec := putSetting(t, ts, "serve.dot.enabled", "true"); rec.Code != http.StatusNoContent {
			t.Fatalf("PUT serve.dot.enabled=true = %d (%s)", rec.Code, rec.Body.String())
		}
		if err := os.Chmod(keyPath, 0o000); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(keyPath, 0o600) })
		if rec := putSetting(t, ts, "serve.dot.enabled", "false"); rec.Code != http.StatusNoContent {
			t.Fatalf("PUT serve.dot.enabled=false = %d (%s), want 204 with the key now unreadable", rec.Code, rec.Body.String())
		}
	})
}

// TestReadyz covers readiness, which is a different question from liveness
// and needs a different answer.
//
// /health says the process is up, which is what a container runtime asks
// before restarting it, and it must stay cheap and always-200 for that.
// /readyz says this instance can actually do its job: the store answers,
// and something is bound to serve DNS. A load balancer pointed at /health
// keeps sending queries to a process that is up and resolving nothing.
//
// Unauthenticated, like /health: a probe has no credentials to present, and
// the answer discloses only whether this instance is usable.
func TestReadyz(t *testing.T) {
	t.Run("no DNS listener", func(t *testing.T) {
		srv, _, _ := testServer(t)
		w := doReq(t, srv.Handler(), "GET", "/api/v1/readyz", "", nil)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("readyz with nothing serving = %d %s, want 503", w.Code, w.Body.String())
		}
		var body map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if !strings.Contains(body["error"], "listener") {
			t.Errorf("503 reason = %q, want it to name the missing listener", body["error"])
		}
	})

	t.Run("serving DNS", func(t *testing.T) {
		srv, _, _ := testServer(t, func(d *Deps) {
			d.ResolverStatus = &fakeResolverStatus{dnsListening: true}
		})
		w := doReq(t, srv.Handler(), "GET", "/api/v1/readyz", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("readyz while serving = %d %s, want 200", w.Code, w.Body.String())
		}
		var body map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if body["status"] != "ok" {
			t.Errorf("body = %v, want status ok", body)
		}
	})

	t.Run("store unreachable", func(t *testing.T) {
		srv, s, _ := testServer(t, func(d *Deps) {
			d.ResolverStatus = &fakeResolverStatus{dnsListening: true}
		})
		h := srv.Handler()
		if w := doReq(t, h, "GET", "/api/v1/readyz", "", nil); w.Code != http.StatusOK {
			t.Fatalf("readyz before closing the store = %d %s, want 200", w.Code, w.Body.String())
		}
		// The real store, closed — not a fake that returns an error. What
		// readiness has to detect is a database that has gone away under a
		// process that is otherwise fine.
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		w := doReq(t, h, "GET", "/api/v1/readyz", "", nil)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("readyz with a closed store = %d %s, want 503", w.Code, w.Body.String())
		}
		var body map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if !strings.Contains(body["error"], "storage") {
			t.Errorf("503 reason = %q, want it to name the storage failure", body["error"])
		}
	})

	// /health is the liveness half and must keep answering 200 regardless:
	// a process that cannot serve is still a process that should not be
	// killed and restarted in a loop.
	t.Run("health stays up", func(t *testing.T) {
		srv, _, _ := testServer(t)
		if w := doReq(t, srv.Handler(), "GET", "/api/v1/health", "", nil); w.Code != http.StatusOK {
			t.Fatalf("health with nothing serving = %d, want 200", w.Code)
		}
	})
}

// TestSettingsPutMap covers the multi-key form of PUT /settings.
//
// One key per request made every dependent change a sequence the client had
// to get right — the dashboard carries a five-phase dispatch table for it
// (SAVE_PHASES in web/src/pages/settings.tsx) — and every save a burst of
// round trips that each bumped the config version, so a save of six fields
// reconfigured the running server six times. A map is one request, one
// validation pass, one write and one bump.
func TestSettingsPutMap(t *testing.T) {
	t.Run("several keys at once, one config version bump", func(t *testing.T) {
		srv, s, _ := testServer(t)
		cookie := login(t, srv, s)
		h := srv.Handler()
		before, err := s.Settings().ConfigVersion(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		body := `{"blocking.mode":"nxdomain","blocking.ttl":"60","qlog.privacy":"anon"}`
		if w := doReq(t, h, "PUT", "/api/v1/settings", body, cookie); w.Code != 204 {
			t.Fatalf("map put: %d %s", w.Code, w.Body.String())
		}
		for key, want := range map[string]string{
			"blocking.mode": "nxdomain", "blocking.ttl": "60", "qlog.privacy": "anon",
		} {
			if got, _, _ := s.Settings().Get(t.Context(), key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
		after, err := s.Settings().ConfigVersion(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if after != before+1 {
			t.Errorf("config version went %d -> %d for one request; three keys must not "+
				"reconfigure the running server three times", before, after)
		}
	})

	t.Run("all or nothing", func(t *testing.T) {
		srv, s, _ := testServer(t)
		cookie := login(t, srv, s)
		h := srv.Handler()
		if w := doReq(t, h, "PUT", "/api/v1/settings", `{"blocking.mode":"null-ip"}`, cookie); w.Code != 204 {
			t.Fatalf("seed: %d %s", w.Code, w.Body.String())
		}
		before, err := s.Settings().ConfigVersion(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		for _, tc := range []struct{ name, body, wantIn string }{
			{"bad value", `{"blocking.mode":"nxdomain","blocking.ttl":"soon"}`, "blocking.ttl"},
			{"key nobody may edit", `{"blocking.mode":"nxdomain","instance.id":"x"}`, "instance.id"},
		} {
			w := doReq(t, h, "PUT", "/api/v1/settings", tc.body, cookie)
			if w.Code != 400 {
				t.Fatalf("%s = %d %s, want 400", tc.name, w.Code, w.Body.String())
			}
			if got := settingsErr(t, w); !strings.Contains(got, tc.wantIn) {
				t.Errorf("%s: error %q does not name %s", tc.name, got, tc.wantIn)
			}
			// The good key in the same body must not have landed. A partial
			// apply is the worst outcome here: the caller is told the
			// request failed and the server is in a state neither of them
			// asked for.
			if got, _, _ := s.Settings().Get(t.Context(), "blocking.mode"); got != "null-ip" {
				t.Fatalf("%s: blocking.mode is %q — a rejected map wrote part of itself", tc.name, got)
			}
		}
		after, err := s.Settings().ConfigVersion(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Errorf("config version moved %d -> %d on a rejected write", before, after)
		}
	})

	t.Run("dependent keys in one request", func(t *testing.T) {
		srv, s, _ := testServer(t)
		cookie := login(t, srv, s)
		h := srv.Handler()
		dir := t.TempDir()
		cert, _ := certtest.For(t, "dns.example.test")
		certPath, keyPath := writeKeypair(t, dir, "c", cert)

		// Turning DoT on is only valid against a stored certificate pair, and
		// the pair is only checked once both halves are present. Sent one at a
		// time this needs three ordered requests; in one map the handler
		// applies the same order itself.
		body := `{"serve.dot.enabled":"true","serve.tls.cert":` + strconv.Quote(certPath) +
			`,"serve.tls.key":` + strconv.Quote(keyPath) + `,"serve.dot.listen":"0.0.0.0:8853"}`
		if w := doReq(t, h, "PUT", "/api/v1/settings", body, cookie); w.Code != 204 {
			t.Fatalf("enabling DoT alongside its certificate: %d %s", w.Code, w.Body.String())
		}
		if got, _, _ := s.Settings().Get(t.Context(), "serve.dot.enabled"); got != "true" {
			t.Errorf("serve.dot.enabled = %q, want true", got)
		}

		// And the other direction: clearing the paths is refused while a
		// protocol is enabled, unless the disable travels with them — which
		// is the phase order read backwards.
		clear := `{"serve.tls.cert":"","serve.tls.key":""}`
		if w := doReq(t, h, "PUT", "/api/v1/settings", clear, cookie); w.Code != 400 {
			t.Errorf("clearing the certificate under an enabled protocol = %d, want 400", w.Code)
		}
		withDisable := `{"serve.dot.enabled":"false","serve.tls.cert":"","serve.tls.key":""}`
		if w := doReq(t, h, "PUT", "/api/v1/settings", withDisable, cookie); w.Code != 204 {
			t.Fatalf("clearing the certificate together with the disable: %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("a bad pair is refused before anything is written", func(t *testing.T) {
		srv, s, _ := testServer(t)
		cookie := login(t, srv, s)
		h := srv.Handler()
		dir := t.TempDir()
		certA, _ := certtest.For(t, "a.example.test")
		certB, _ := certtest.For(t, "b.example.test")
		certPath, _ := writeKeypair(t, dir, "a", certA)
		_, keyPath := writeKeypair(t, dir, "b", certB)

		body := `{"serve.tls.cert":` + strconv.Quote(certPath) + `,"serve.tls.key":` + strconv.Quote(keyPath) + `}`
		if w := doReq(t, h, "PUT", "/api/v1/settings", body, cookie); w.Code != 400 {
			t.Fatalf("mismatched keypair accepted: %d %s", w.Code, w.Body.String())
		}
		if got, _, _ := s.Settings().Get(t.Context(), "serve.tls.cert"); got != "" {
			t.Errorf("serve.tls.cert = %q after a rejected pair; nothing should have been written", got)
		}
	})

	t.Run("the single-key form still works", func(t *testing.T) {
		srv, s, _ := testServer(t)
		cookie := login(t, srv, s)
		h := srv.Handler()
		if w := doReq(t, h, "PUT", "/api/v1/settings", `{"key":"blocking.mode","value":"nxdomain"}`, cookie); w.Code != 204 {
			t.Fatalf("single-key put: %d %s", w.Code, w.Body.String())
		}
		if got, _, _ := s.Settings().Get(t.Context(), "blocking.mode"); got != "nxdomain" {
			t.Fatalf("blocking.mode = %q", got)
		}
		// {"key": ...} with no value is still the single-key shape, so the
		// answer stays a complaint about the value rather than "no such
		// setting: key".
		w := doReq(t, h, "PUT", "/api/v1/settings", `{"key":"blocking.ttl"}`, cookie)
		if w.Code != 400 || !strings.Contains(settingsErr(t, w), "blocking.ttl") {
			t.Errorf(`{"key":"blocking.ttl"} = %d %s, want 400 naming blocking.ttl`, w.Code, w.Body.String())
		}
	})

	t.Run("a body that is not a string map is invalid json", func(t *testing.T) {
		srv, s, _ := testServer(t)
		cookie := login(t, srv, s)
		h := srv.Handler()
		for _, body := range []string{`{"blocking.ttl":60}`, `{"blocking.mode":null}`, `[]`} {
			w := doReq(t, h, "PUT", "/api/v1/settings", body, cookie)
			if w.Code != 400 || settingsErr(t, w) != "invalid json" {
				t.Errorf("PUT %s = %d %s, want 400 invalid json — every value is a string, "+
					"numbers included", body, w.Code, strings.TrimSpace(w.Body.String()))
			}
		}
	})
}
