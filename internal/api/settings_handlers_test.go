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

	dot, doh ProtocolStatus

	notAfter     time.Time
	expiringSoon bool
	certLoaded   bool
}

func (f *fakeResolverStatus) UpstreamDowngrade() (bool, string) { return f.active, f.reason }

func (f *fakeResolverStatus) Serving() (dot, doh ProtocolStatus) { return f.dot, f.doh }

func (f *fakeResolverStatus) CertExpiry() (notAfter time.Time, expiringSoon, ok bool) {
	return f.notAfter, f.expiringSoon, f.certLoaded
}

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
