package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
)

// fakeSync stands in for internal/app's sync subsystem, which does not exist
// in this package and would drag a whole App behind it if it did. Everything
// the API needs from it is the four methods of Syncer.
type fakeSync struct {
	peer       string
	status     SyncStatus
	registered []Replica
	forgotten  []string
}

func (f *fakeSync) PeerURL() string { return f.peer }

func (f *fakeSync) Status() SyncStatus { return f.status }

func (f *fakeSync) Register(_ context.Context, r Replica) error {
	f.registered = append(f.registered, r)
	return nil
}

func (f *fakeSync) Forget(_ context.Context, instanceID string) error {
	f.forgotten = append(f.forgotten, instanceID)
	return nil
}

// tokenReq is doReq with a bearer token instead of a session cookie, for the
// endpoints whose whole point is which scope the credential carries.
func tokenReq(t *testing.T, h http.Handler, method, url, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, url, nil)
	} else {
		req = httptest.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// apiToken mints an API token of the given scope for the admin account.
func apiToken(t *testing.T, srv *Server, scope string) string {
	t.Helper()
	_, tok, err := srv.deps.Auth.CreateAPIToken(t.Context(), 1, "sync-"+scope, scope, 0)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// TestSyncBundleNeedsWriteScope pins the one place scope is not decided by
// method: the bundle is a GET, so requireAuth's method check waves a read
// token through, and the body carries every TSIG secret on the box.
func TestSyncBundleNeedsWriteScope(t *testing.T) {
	srv, s, _ := testServer(t)
	h := srv.Handler()
	_ = login(t, srv, s)
	// One synced setting and two local ones, so "carries no local key" is a
	// claim about a populated map rather than about an empty one.
	if err := s.Settings().SetMany(t.Context(), map[string]string{
		"blocking.mode": "nxdomain", "serve.dot.listen": ":853", "sync.token": "s3cr3t",
	}); err != nil {
		t.Fatal(err)
	}

	w := tokenReq(t, h, "GET", "/api/v1/sync/bundle", "", apiToken(t, srv, "read"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("read scope: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if got := errorOf(t, w); got != "write scope required" {
		t.Errorf("read scope error = %q, want %q", got, "write scope required")
	}

	w = tokenReq(t, h, "GET", "/api/v1/sync/bundle", "", apiToken(t, srv, "write"))
	if w.Code != http.StatusOK {
		t.Fatalf("write scope: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	var b store.Bundle
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.Format != store.BundleFormat {
		t.Errorf("bundle format = %d, want %d", b.Format, store.BundleFormat)
	}
	if b.Settings["blocking.mode"] != "nxdomain" {
		t.Errorf("bundle settings = %v, want the synced keys in it", b.Settings)
	}
	for k := range b.Settings {
		if store.LocalSettingKey(k) {
			t.Errorf("bundle carries local key %q", k)
		}
	}
}

// TestSyncVersionAnswersAnyScope: the version probe is the cheap half of the
// pull loop and discloses a counter and this box's id, so it is behind auth
// like everything else but not behind write scope.
func TestSyncVersionAnswersAnyScope(t *testing.T) {
	srv, s, _ := testServer(t)
	h := srv.Handler()
	_ = login(t, srv, s)
	if err := s.Settings().Set(t.Context(), "instance.id", "V1StGXR8Z5jdHi6BmyT"); err != nil {
		t.Fatal(err)
	}

	w := tokenReq(t, h, "GET", "/api/v1/sync/version", "", apiToken(t, srv, "read"))
	if w.Code != http.StatusOK {
		t.Fatalf("version: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	var got struct {
		ConfigVersion int64  `json:"config_version"`
		InstanceID    string `json:"instance_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want, err := s.Settings().ConfigVersion(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfigVersion != want {
		t.Errorf("config_version = %d, want %d", got.ConfigVersion, want)
	}
	id, _, err := s.Settings().Get(t.Context(), "instance.id")
	if err != nil {
		t.Fatal(err)
	}
	if got.InstanceID != id || id == "" {
		t.Errorf("instance_id = %q, want %q", got.InstanceID, id)
	}
}

// TestRegisterReplicaValidatesAddr: the registered address is what turns on
// the implicit AXFR allow and becomes a NOTIFY target (spec §6), so a value
// that is not an address must not be recorded as one.
func TestRegisterReplicaValidatesAddr(t *testing.T) {
	srv, s, _ := testServer(t)
	fs := &fakeSync{}
	srv.deps.Sync = fs
	h := srv.Handler()
	_ = login(t, srv, s)
	write := apiToken(t, srv, "write")

	for _, bad := range []string{
		`{"instance_id":"r1","dns_addr":"not-an-addr","version_applied":3}`,
		`{"instance_id":"r1","dns_addr":":53","version_applied":3}`,
		`{"instance_id":"","dns_addr":"10.0.0.6:53","version_applied":3}`,
	} {
		if w := tokenReq(t, h, "POST", "/api/v1/sync/replicas", bad, write); w.Code != http.StatusBadRequest {
			t.Errorf("POST %s = %d, want 400", bad, w.Code)
		}
	}
	if len(fs.registered) != 0 {
		t.Fatalf("a rejected registration was recorded: %+v", fs.registered)
	}

	body := `{"instance_id":"r1","dns_addr":"10.0.0.6:53","version_applied":3}`
	w := tokenReq(t, h, "POST", "/api/v1/sync/replicas", body, write)
	if w.Code != http.StatusNoContent || len(fs.registered) != 1 {
		t.Fatalf("register: %d %+v", w.Code, fs.registered)
	}
	got := fs.registered[0]
	if got.InstanceID != "r1" || got.DNSAddr != "10.0.0.6:53" || got.VersionApplied != 3 {
		t.Errorf("registered %+v", got)
	}
	if got.LastSeen == 0 {
		t.Error("registered replica has no last_seen; the main is what dates a registration")
	}

	if w := tokenReq(t, h, "DELETE", "/api/v1/sync/replicas/r1", "", write); w.Code != http.StatusNoContent {
		t.Fatalf("forget: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if len(fs.forgotten) != 1 || fs.forgotten[0] != "r1" {
		t.Errorf("forgotten = %v", fs.forgotten)
	}
}

// TestSyncStatusWithoutASyncer: Deps.Sync is nil in every server with no App
// behind it, which is a main that follows nobody and has no replicas — the
// truthful answer, not a panic and not a 503.
func TestSyncStatusWithoutASyncer(t *testing.T) {
	srv, s, _ := testServer(t)
	h := srv.Handler()
	cookie := login(t, srv, s)

	w := doReq(t, h, "GET", "/api/v1/sync/status", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	var st SyncStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Role != "main" || st.PeerURL != "" || len(st.Replicas) != 0 {
		t.Errorf("status with no syncer = %+v, want role main and nothing else", st)
	}

	// The same fact reaches the warning strip through resolver/status.
	w = doReq(t, h, "GET", "/api/v1/resolver/status", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("resolver status: %d", w.Code)
	}
	var rs struct {
		Sync *SyncStatus `json:"sync"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rs); err != nil {
		t.Fatal(err)
	}
	if rs.Sync == nil || rs.Sync.Role != "main" {
		t.Errorf("resolver/status sync = %+v, want role main", rs.Sync)
	}
}

// TestResolverStatusCarriesSync proves the strip sees a replica's own state,
// not just the nil-syncer default above.
func TestResolverStatusCarriesSync(t *testing.T) {
	srv, s, _ := testServer(t)
	srv.deps.Sync = &fakeSync{
		peer: "https://main.lan",
		status: SyncStatus{
			Role: "replica", PeerURL: "https://main.lan", PeerVersion: 412,
			AppliedVersion: 411, LastError: "peer refused the token",
		},
	}
	cookie := login(t, srv, s)
	w := doReq(t, srv.Handler(), "GET", "/api/v1/resolver/status", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("resolver status: %d", w.Code)
	}
	var rs struct {
		Sync SyncStatus `json:"sync"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rs); err != nil {
		t.Fatal(err)
	}
	if rs.Sync.Role != "replica" || rs.Sync.PeerVersion != 412 || rs.Sync.LastError != "peer refused the token" {
		t.Errorf("sync = %+v", rs.Sync)
	}
}

// syncUnguardedWrites are the registered write patterns a replica still
// answers, mapped to the reason each one is not a write to synced
// configuration. Exact whole patterns, for the reason unauthenticatedRoutes
// uses them: an exemption written as a prefix grows on its own, and the next
// route added under it inherits a hole in the guard silently.
//
// Checked in both directions by TestReplicaRefusesSyncedWrites: an entry
// naming no registered route fails, and a registered write that is neither
// listed here nor answered 409 on a replica fails.
var syncUnguardedWrites = map[string]string{
	"POST /api/v1/setup":             "creates this box's own admin account; users are local (§4.3).",
	"POST /api/v1/auth/login":        "mints this box's own session.",
	"POST /api/v1/auth/logout":       "revokes this box's own session.",
	"POST /api/v1/auth/password":     "this box's own admin password.",
	"DELETE /api/v1/auth/sessions":   "this box's own sessions.",
	"POST /api/v1/auth/totp/start":   "this box's own 2FA enrollment.",
	"POST /api/v1/auth/totp/confirm": "this box's own 2FA.",
	"POST /api/v1/auth/totp/disable": "this box's own 2FA.",
	"POST /api/v1/tokens":            "API tokens are local (§4.3); a replica mints its own.",
	"DELETE /api/v1/tokens/{id}":     "same, and revoking must keep working on a replica.",

	"PUT /api/v1/settings": "key by key, not route-wide: the local keys of §4.3 stay writable on " +
		"a replica — clearing sync.peer_url is the promotion — and everything else is refused " +
		"inside the handler. TestReplicaRefusesSyncedWrites checks both halves below.",
	"POST /api/v1/backup":           "copies this box's own database to this box's own disk.",
	"POST /api/v1/blocking/pause":   "engine state, not a row this API writes.",
	"DELETE /api/v1/blocking/pause": "same.",

	"POST /api/v1/filters/refresh": "downloads the lists this box already has; the replica keeps " +
		"its own copies current, which is how a list it has never seen ever gets fetched (§5).",
	"POST /api/v1/filters/lists/{id}/refresh": "operational, not config (§7): one list's Refresh now.",
	"POST /api/v1/zones/{id}/refresh":         "operational, not config (§7): one zone's transfer.",

	"POST /api/v1/sync/replicas":                 "the main's registry; a replica is not one.",
	"DELETE /api/v1/sync/replicas/{instance_id}": "same.",
}

// operationalWrites are the two of the above that the spec calls out by
// name, and the only ones this test issues for real — the rest either have
// side effects that do not belong in a test about a guard (a background
// download, a database copy) or are already covered by their own suites.
var operationalWrites = []string{
	"POST /api/v1/filters/lists/{id}/refresh",
	"POST /api/v1/zones/{id}/refresh",
}

// TestReplicaRefusesSyncedWrites drives the route registry the way
// TestEveryRouteEnforcesAuth does: every registered write to a synced
// resource answers 409 on a replica, before it touches the store.
//
// Registry-driven rather than a list of endpoints, for the same reason that
// test gives: a route added tomorrow is checked the moment it is registered,
// and a synced resource that quietly became writable on a replica is a
// config that drifts — which is the whole thing this feature exists to stop.
func TestReplicaRefusesSyncedWrites(t *testing.T) {
	srv, s, _ := testServer(t)
	srv.deps.Sync = &fakeSync{peer: "https://main.lan"}
	h := srv.Handler()
	cookie := login(t, srv, s)

	registered := map[string]bool{}
	for _, rt := range srv.routes {
		registered[rt.pattern] = true
	}
	for pattern := range syncUnguardedWrites {
		if !registered[pattern] {
			t.Errorf("syncUnguardedWrites exempts %q, but no route registers that pattern. "+
				"If the route was renamed, move the exemption; if it was removed, delete it.", pattern)
		}
	}

	fill := strings.NewReplacer("{id}", "1", "{rid}", "1", "{instance_id}", "r1")
	for _, rt := range srv.routes {
		method, path, ok := strings.Cut(rt.pattern, " ")
		if !ok || method == http.MethodGet || method == http.MethodHead {
			continue
		}
		if _, exempt := syncUnguardedWrites[rt.pattern]; exempt {
			continue
		}
		t.Run(rt.pattern, func(t *testing.T) {
			url := fill.Replace(path)
			assertResolvesTo(t, srv, method, url, rt.pattern)
			w := doReq(t, h, method, url, "{}", cookie)
			if w.Code != http.StatusConflict {
				t.Fatalf("%s %s on a replica = %d %s, want 409. Either the route is missing "+
					"s.managed(), or it is not a write to synced config and belongs in "+
					"syncUnguardedWrites with the reason.", method, url, w.Code, strings.TrimSpace(w.Body.String()))
			}
			if got, want := errorOf(t, w), "managed by https://main.lan"; got != want {
				t.Errorf("%s %s error = %q, want %q", method, url, got, want)
			}
		})
	}

	for _, pattern := range operationalWrites {
		t.Run(pattern, func(t *testing.T) {
			method, path, _ := strings.Cut(pattern, " ")
			w := doReq(t, h, method, fill.Replace(path), "{}", cookie)
			if w.Code == http.StatusConflict && strings.Contains(w.Body.String(), "managed by") {
				t.Errorf("%s is operational, not config, and must stay usable on a replica", pattern)
			}
		})
	}

	// PUT /settings is guarded key by key rather than route-wide.
	w := doReq(t, h, "PUT", "/api/v1/settings", `{"serve.dot.listen":":8853"}`, cookie)
	if w.Code == http.StatusConflict {
		t.Errorf("a local setting was refused on a replica: %s", strings.TrimSpace(w.Body.String()))
	}
	w = doReq(t, h, "PUT", "/api/v1/settings", `{"key":"sync.peer_url","value":""}`, cookie)
	if w.Code == http.StatusConflict {
		t.Errorf("clearing sync.peer_url is the promotion and must not be refused: %s",
			strings.TrimSpace(w.Body.String()))
	}
	w = doReq(t, h, "PUT", "/api/v1/settings", `{"key":"blocking.mode","value":"nxdomain"}`, cookie)
	if w.Code != http.StatusConflict {
		t.Fatalf("a synced setting on a replica = %d %s, want 409", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if got, want := errorOf(t, w), "managed by https://main.lan"; got != want {
		t.Errorf("settings error = %q, want %q", got, want)
	}
	// A map mixing the two is refused whole, like every other rejection in
	// this handler: half of a save is a state nobody chose.
	w = doReq(t, h, "PUT", "/api/v1/settings",
		`{"serve.dot.listen":":8854","blocking.mode":"nxdomain"}`, cookie)
	if w.Code != http.StatusConflict {
		t.Fatalf("mixed map on a replica = %d, want 409", w.Code)
	}
	if got, _, _ := s.Settings().Get(t.Context(), "serve.dot.listen"); got == ":8854" {
		t.Error("the local half of a refused map was written anyway")
	}
}

// TestSyncTokenIsWriteOnly: the token authenticates this replica's pull from
// the main, so GET /settings must not hand it back to anyone who can read
// settings (spec §8).
func TestSyncTokenIsWriteOnly(t *testing.T) {
	srv, s, _ := testServer(t)
	h := srv.Handler()
	cookie := login(t, srv, s)

	if w := doReq(t, h, "PUT", "/api/v1/settings",
		`{"sync.token":"s3cr3t","sync.peer_url":"https://main.lan"}`, cookie); w.Code != http.StatusNoContent {
		t.Fatalf("set sync.token: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if got, _, _ := s.Settings().Get(t.Context(), "sync.token"); got != "s3cr3t" {
		t.Fatalf("sync.token stored as %q; the write must still happen", got)
	}
	w := doReq(t, h, "GET", "/api/v1/settings", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("get settings: %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "s3cr3t") {
		t.Fatal("sync.token leaked through GET /settings")
	}
	var all map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if _, present := all["sync.token"]; present {
		t.Error("GET /settings still carries a sync.token entry")
	}
	if all["sync.peer_url"] != "https://main.lan" {
		t.Errorf("sync.peer_url = %q; the rest of sync.* must stay readable, only the token is write-only",
			all["sync.peer_url"])
	}
}

// TestSyncSettingsValidation covers the five keys' grammars and the two
// cross-field rules (§8): a peer with no token is a replica that cannot
// pull, and a sync key id that names nothing signs nothing.
func TestSyncSettingsValidation(t *testing.T) {
	srv, s, _ := testServer(t)
	h := srv.Handler()
	cookie := login(t, srv, s)

	single := func(key, value string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]string{"key": key, "value": value})
		if err != nil {
			t.Fatal(err)
		}
		return doReq(t, h, "PUT", "/api/v1/settings", string(body), cookie)
	}

	for _, tc := range []struct {
		key, value string
		want       int
	}{
		{"sync.peer_url", "main.lan", http.StatusBadRequest},
		{"sync.peer_url", "ftp://main.lan", http.StatusBadRequest},
		{"sync.peer_url", "https://", http.StatusBadRequest},
		{"sync.interval_seconds", "4", http.StatusBadRequest},
		{"sync.interval_seconds", "0", http.StatusBadRequest},
		{"sync.interval_seconds", "abc", http.StatusBadRequest},
		{"sync.interval_seconds", "5", http.StatusNoContent},
		{"sync.primary_dns", "10.0.0.5", http.StatusBadRequest},
		{"sync.primary_dns", ":53", http.StatusBadRequest},
		{"sync.primary_dns", "", http.StatusNoContent},
		{"sync.primary_dns", "10.0.0.5:53", http.StatusNoContent},
		{"sync.tsig_key_id", "-1", http.StatusBadRequest},
		{"sync.tsig_key_id", "99", http.StatusBadRequest},
		{"sync.tsig_key_id", "0", http.StatusNoContent},
	} {
		if w := single(tc.key, tc.value); w.Code != tc.want {
			t.Errorf("PUT %s=%q = %d %s, want %d", tc.key, tc.value, w.Code,
				strings.TrimSpace(w.Body.String()), tc.want)
		}
	}

	// A peer with no token stored is refused; the same request carrying the
	// token is not, which is what the settings screen sends.
	if w := single("sync.peer_url", "https://main.lan"); w.Code != http.StatusBadRequest {
		t.Errorf("peer_url with no token = %d %s, want 400", w.Code, strings.TrimSpace(w.Body.String()))
	}
	w := doReq(t, h, "PUT", "/api/v1/settings",
		`{"sync.peer_url":"https://main.lan","sync.token":"tok"}`, cookie)
	if w.Code != http.StatusNoContent {
		t.Fatalf("peer_url and token together = %d %s, want 204", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if got, _, _ := s.Settings().Get(t.Context(), "sync.peer_url"); got != "https://main.lan" {
		t.Errorf("sync.peer_url = %q", got)
	}

	// An existing key id is accepted; the replica uses the main's id, so
	// there is no separate check for which box the row lives on.
	id := mustCreate(t, h, cookie, "/api/v1/tsig-keys",
		`{"name":"xfer.example.","algorithm":"hmac-sha256.","secret":"c2VjcmV0"}`)
	if w := single("sync.tsig_key_id", strconv.FormatInt(id, 10)); w.Code != http.StatusNoContent {
		t.Errorf("existing key id = %d %s, want 204", w.Code, strings.TrimSpace(w.Body.String()))
	}
}

// TestSyncKeyCheckSurvivesAStoreFailure: the sync.tsig_key_id check is the
// one settings validator that reads the store, so it is the one that can
// fail for a reason that is not the value. Answering 400 there would tell
// the operator their input was wrong and quote a driver error at them.
func TestSyncKeyCheckSurvivesAStoreFailure(t *testing.T) {
	srv, s, _ := testServer(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	err := srv.validateCrossField(t.Context(), "sync.tsig_key_id", "1", map[string]string{})
	if !errors.Is(err, errStorage) {
		t.Fatalf("store failure = %v, want it marked as one so the answer is 503", err)
	}
	w := httptest.NewRecorder()
	storeErr(w, err)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("storeErr = %d, want 503", w.Code)
	}
	if got := errorOf(t, w); got != "storage unavailable" {
		t.Errorf("error = %q; the driver error must not reach the body", got)
	}
}

// errorOf reads the "error" field of an envelope, failing the test if the
// body is not one — a status assertion that passes on a body nobody parsed
// is half an assertion.
func errorOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body %q is not an error envelope: %v", strings.TrimSpace(w.Body.String()), err)
	}
	return env.Error
}
