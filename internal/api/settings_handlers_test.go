package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/filter"
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

func TestBlockingPauseResume(t *testing.T) {
	srv, s, _ := testServer(t)
	srv.deps.Engine = filter.NewEngine()
	cookie := login(t, srv, s)
	h := srv.Handler()

	if w := doReq(t, h, "POST", "/api/v1/blocking/pause", `{"group_id":0,"minutes":5}`, cookie); w.Code != 204 {
		t.Fatalf("pause: %d %s", w.Code, w.Body.String())
	}
	w := doReq(t, h, "GET", "/api/v1/blocking", "", cookie)
	var st map[string]int64
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st["paused_until"] <= time.Now().UnixMilli() {
		t.Fatalf("paused_until: %v", st)
	}
	if w := doReq(t, h, "DELETE", "/api/v1/blocking/pause?group_id=0", "", cookie); w.Code != 204 {
		t.Fatalf("resume: %d", w.Code)
	}
	w = doReq(t, h, "GET", "/api/v1/blocking", "", cookie)
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st["paused_until"] != 0 {
		t.Fatalf("still paused: %v", st)
	}
	if w := doReq(t, h, "POST", "/api/v1/blocking/pause", `{"group_id":0,"minutes":0}`, cookie); w.Code != 400 {
		t.Fatalf("zero minutes accepted: %d", w.Code)
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
}

func (f *fakeResolverStatus) UpstreamDowngrade() (bool, string) { return f.active, f.reason }

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
}
