package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
)

// fakeSync stands in for internal/app's sync subsystem, which does not exist
// in this package and would drag a whole App behind it if it did. Everything
// the API needs from it is the methods of Syncer, and what these tests check
// is which of them a request reached and with what.
type fakeSync struct {
	peer      string
	status    SyncStatus
	forgotten []string
	// The main's half: the code Add replica mints, and what a replica
	// spends it on.
	code      string
	expiresAt int64
	codesMade int
	paired    []PairRequest
	pairErr   error
	result    PairResult
	// The secret the two replica-only reads take, and the id it belongs to.
	// An empty secret authenticates nobody, which is what a main with no
	// replicas is.
	secret     string
	instanceID string
	beats      []heartbeat
	beatErr    error
	dnsPort    int
	// The replica's half.
	followed  []followCall
	followErr error
}

// heartbeat is one version probe as the main recorded it.
type heartbeat struct {
	instanceID string
	applied    int64
	dhcp       bool
}

type followCall struct{ peerURL, code string }

func (f *fakeSync) PeerURL() string { return f.peer }

func (f *fakeSync) Status() SyncStatus { return f.status }

func (f *fakeSync) Forget(_ context.Context, instanceID string) error {
	f.forgotten = append(f.forgotten, instanceID)
	return nil
}

func (f *fakeSync) NewPairingCode(context.Context) (string, int64, error) {
	f.codesMade++
	return f.code, f.expiresAt, nil
}

func (f *fakeSync) Pair(_ context.Context, req PairRequest) (PairResult, error) {
	f.paired = append(f.paired, req)
	if f.pairErr != nil {
		return PairResult{}, f.pairErr
	}
	return f.result, nil
}

func (f *fakeSync) Authenticate(_ context.Context, secret string) (string, bool, error) {
	if f.secret == "" || secret != f.secret {
		return "", false, nil
	}
	return f.instanceID, true, nil
}

func (f *fakeSync) Heartbeat(_ context.Context, instanceID string, applied int64, dhcp bool) error {
	if f.beatErr != nil {
		return f.beatErr
	}
	f.beats = append(f.beats, heartbeat{instanceID, applied, dhcp})
	return nil
}

// Follow records the call. It never blocks: the real one answers as soon as
// the pairing is stored and leaves the first pull to the poll loop.
func (f *fakeSync) Follow(_ context.Context, peerURL, code string) error {
	f.followed = append(f.followed, followCall{peerURL, code})
	return f.followErr
}

func (f *fakeSync) DNSPort() int { return f.dnsPort }

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

// fromHost issues a request as if it arrived from clientIP, which is what
// decides the pairing throttle's budget and completes a dns_addr that names
// only a port.
func fromHost(t *testing.T, h http.Handler, method, url, body, clientIP string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = net.JoinHostPort(clientIP, "41234")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestPairingCodeNeedsWriteScope: the code is what buys a place in the
// main's registry, and a place in the registry is an implicit AXFR allow
// (§6). A read-scoped token must not be able to mint one.
func TestPairingCodeNeedsWriteScope(t *testing.T) {
	srv, s, _ := testServer(t)
	fs := &fakeSync{code: "K7M9-QRTW", expiresAt: 1757580000000}
	srv.deps.Sync = fs
	h := srv.Handler()
	_ = login(t, srv, s)

	if w := tokenReq(t, h, "POST", "/api/v1/sync/pairing-code", "", apiToken(t, srv, "read")); w.Code != http.StatusForbidden {
		t.Fatalf("read scope: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if fs.codesMade != 0 {
		t.Fatalf("a read-scoped token minted %d codes", fs.codesMade)
	}

	w := tokenReq(t, h, "POST", "/api/v1/sync/pairing-code", "", apiToken(t, srv, "write"))
	if w.Code != http.StatusOK {
		t.Fatalf("write scope: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	var got struct {
		Code      string `json:"code"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Code != fs.code || got.ExpiresAt != fs.expiresAt {
		t.Errorf("pairing code = %+v, want %q expiring at %d", got, fs.code, fs.expiresAt)
	}
}

// TestPairingCodeRefusedOnAReplica: a replica keeps no registry, so a code
// it minted would buy a place in nothing. The 409 names the main to ask
// instead.
func TestPairingCodeRefusedOnAReplica(t *testing.T) {
	srv, s, _ := testServer(t)
	fs := &fakeSync{peer: "https://main.lan"}
	srv.deps.Sync = fs
	h := srv.Handler()
	cookie := login(t, srv, s)

	w := doReq(t, h, "POST", "/api/v1/sync/pairing-code", "", cookie)
	if w.Code != http.StatusConflict {
		t.Fatalf("pairing-code on a replica = %d %s, want 409", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if got, want := errorOf(t, w), "managed by https://main.lan"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	if fs.codesMade != 0 {
		t.Errorf("a replica minted %d pairing codes", fs.codesMade)
	}
}

// TestPairAnswersTheSecretAndCompletesTheHost: a replica on the default
// ":53" has no address to name, and the request it pairs over came from
// exactly the address this main can reach it on. Without the completion the
// default install pairs into an entry the main can neither notify nor match
// an AXFR against (§6).
func TestPairAnswersTheSecretAndCompletesTheHost(t *testing.T) {
	srv, _, _ := testServer(t)
	fs := &fakeSync{result: PairResult{Secret: "r3plic4-s3cr3t", DNSPort: 5353}}
	srv.deps.Sync = fs

	body := `{"code":"K7M9-QRTW","instance_id":"r1","dns_addr":":53"}`
	w := fromHost(t, srv.Handler(), "POST", "/api/v1/sync/pair", body, "10.0.0.6")
	if w.Code != http.StatusOK {
		t.Fatalf("pair: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if len(fs.paired) != 1 {
		t.Fatalf("the syncer saw %d pairings, want 1", len(fs.paired))
	}
	if got := (fs.paired[0]); got.Code != "K7M9-QRTW" || got.InstanceID != "r1" || got.DNSAddr != "10.0.0.6:53" {
		t.Errorf("paired %+v, want the connection's address on the port it sent", got)
	}
	var result PairResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result != fs.result {
		t.Errorf("pair result = %+v, want %+v", result, fs.result)
	}
}

// TestPairValidatesTheBodyBeforeSpendingTheCode: a body the main could not
// act on must not cost the operator their code, and an address that is not
// one must never reach the registry — it is what the transfer gate compares
// against.
func TestPairValidatesTheBodyBeforeSpendingTheCode(t *testing.T) {
	srv, s, _ := testServer(t)
	fs := &fakeSync{}
	srv.deps.Sync = fs
	h := srv.Handler()
	const id = "V1StGXR8Z5jdHi6BmyT"
	if err := s.Settings().Set(t.Context(), "instance.id", id); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ body, want string }{
		{`{"code":"K7M9-QRTW","instance_id":"","dns_addr":"10.0.0.6:53"}`, "instance_id required"},
		{`{"code":"K7M9-QRTW","instance_id":"a/b","dns_addr":"10.0.0.6:53"}`, "instance_id must not contain a slash"},
		{`{"code":"K7M9-QRTW","instance_id":"r1","dns_addr":"not-an-addr"}`, ""},
		{`{"code":"K7M9-QRTW","instance_id":"r1","dns_addr":""}`, ""},
		{`{"code":"K7M9-QRTW","instance_id":"` + id + `","dns_addr":"10.0.0.6:53"}`, "a main cannot pair with itself"},
	} {
		w := fromHost(t, h, "POST", "/api/v1/sync/pair", tc.body, "10.0.0.6")
		if w.Code != http.StatusBadRequest {
			t.Errorf("POST %s = %d %s, want 400", tc.body, w.Code, strings.TrimSpace(w.Body.String()))
			continue
		}
		if tc.want != "" && errorOf(t, w) != tc.want {
			t.Errorf("POST %s error = %q, want %q", tc.body, errorOf(t, w), tc.want)
		}
	}
	if len(fs.paired) != 0 {
		t.Fatalf("a rejected body reached the registry: %+v", fs.paired)
	}
}

// TestPairWrongCodeIs403AndRateLimited: the code is 40 bits and the
// endpoint is unauthenticated (§9), so the only thing between a guesser and
// the registry is this budget. The refusal never says which way it was
// wrong.
func TestPairWrongCodeIs403AndRateLimited(t *testing.T) {
	srv, _, _ := testServer(t)
	srv.deps.Sync = &fakeSync{pairErr: ErrPairingRefused}
	h := srv.Handler()
	body := `{"code":"AAAA-AAAA","instance_id":"r1","dns_addr":"10.0.0.6:53"}`

	for i := range attemptLimit {
		w := fromHost(t, h, "POST", "/api/v1/sync/pair", body, "10.0.0.6")
		if w.Code != http.StatusForbidden {
			t.Fatalf("attempt %d = %d %s, want 403", i+1, w.Code, strings.TrimSpace(w.Body.String()))
		}
		if got, want := errorOf(t, w), "pairing code refused"; got != want {
			t.Fatalf("attempt %d error = %q, want %q", i+1, got, want)
		}
	}
	w := fromHost(t, h, "POST", "/api/v1/sync/pair", body, "10.0.0.6")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d = %d %s, want 429", attemptLimit+1, w.Code, strings.TrimSpace(w.Body.String()))
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("the 429 carries no Retry-After")
	}
	// Per source address, like login's: one guesser must not lock the
	// operator's own box out of pairing.
	if w := fromHost(t, h, "POST", "/api/v1/sync/pair", body, "10.0.0.7"); w.Code != http.StatusForbidden {
		t.Errorf("another address = %d, want 403; the budget is per source", w.Code)
	}
}

// TestPairRefusedOnAReplica: a replica keeps no registry, so a box that
// paired with it would be recorded where nothing notifies it and no
// transfer is let through (§6). Refused before the throttle spends anything.
func TestPairRefusedOnAReplica(t *testing.T) {
	srv, _, _ := testServer(t)
	fs := &fakeSync{peer: "https://main.lan"}
	srv.deps.Sync = fs

	body := `{"code":"K7M9-QRTW","instance_id":"r1","dns_addr":"10.0.0.6:53"}`
	w := fromHost(t, srv.Handler(), "POST", "/api/v1/sync/pair", body, "10.0.0.6")
	if w.Code != http.StatusConflict {
		t.Fatalf("pair on a replica = %d %s, want 409", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if got, want := errorOf(t, w), "managed by https://main.lan"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	if len(fs.paired) != 0 {
		t.Errorf("a replica paired %+v", fs.paired)
	}
}

// TestSyncReadsTakeOnlyAReplicaSecret: the two pull-loop reads are the one
// place in this API a session or an API token is not a credential. The
// bundle carries every TSIG secret on the box and the probe is the
// heartbeat, so both belong to the replicas this main paired with and to
// nobody else (§3, §9).
func TestSyncReadsTakeOnlyAReplicaSecret(t *testing.T) {
	srv, s, _ := testServer(t)
	fs := &fakeSync{secret: "r3plic4-s3cr3t", instanceID: "r1", dnsPort: 5353}
	srv.deps.Sync = fs
	h := srv.Handler()
	cookie := login(t, srv, s)
	// One synced setting and two local ones, so "carries no local key" is a
	// claim about a populated map rather than about an empty one. instance.id
	// is seeded by internal/app, which no test server here has behind it.
	if err := s.Settings().SetMany(t.Context(), map[string]string{
		"blocking.mode": "nxdomain", "serve.dot.listen": ":853", "sync.token": "s3cr3t",
		"instance.id": "V1StGXR8Z5jdHi6BmyT",
	}); err != nil {
		t.Fatal(err)
	}

	for _, url := range []string{"/api/v1/sync/version", "/api/v1/sync/bundle"} {
		if w := doReq(t, h, "GET", url, "", cookie); w.Code != http.StatusUnauthorized {
			t.Errorf("session cookie on %s = %d %s, want 401", url, w.Code, strings.TrimSpace(w.Body.String()))
		}
		w := tokenReq(t, h, "GET", url, "", apiToken(t, srv, "write"))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("write token on %s = %d %s, want 401", url, w.Code, strings.TrimSpace(w.Body.String()))
		}
		if got, want := errorOf(t, w), "unauthorized"; got != want {
			t.Errorf("%s error = %q, want %q", url, got, want)
		}
	}

	w := tokenReq(t, h, "GET", "/api/v1/sync/version?applied=7", "", fs.secret)
	if w.Code != http.StatusOK {
		t.Fatalf("version: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	var got struct {
		ConfigVersion int64  `json:"config_version"`
		InstanceID    string `json:"instance_id"`
		DNSPort       int    `json:"dns_port"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want, err := s.Settings().ConfigVersion(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := s.Settings().Get(t.Context(), "instance.id")
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfigVersion != want || got.InstanceID != id || id == "" {
		t.Errorf("version = %+v, want {%d %q}", got, want, id)
	}
	// The port the peer URL's host is joined to when sync.primary_dns names
	// no override (§8).
	if got.DNSPort != fs.dnsPort {
		t.Errorf("dns_port = %d, want %d", got.DNSPort, fs.dnsPort)
	}
	// The probe is the heartbeat: there is no separate registration call.
	if len(fs.beats) != 1 || fs.beats[0] != (heartbeat{"r1", 7, false}) {
		t.Errorf("heartbeats = %+v, want one for r1 at version 7", fs.beats)
	}

	w = tokenReq(t, h, "GET", "/api/v1/sync/bundle", "", fs.secret)
	if w.Code != http.StatusOK {
		t.Fatalf("bundle: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
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

// TestVersionProbeWithNoAppliedParameter: a replica that has applied
// nothing yet sends no parameter, and one sending rubbish must not be
// recorded as having applied a version it has not.
func TestVersionProbeWithNoAppliedParameter(t *testing.T) {
	srv, _, _ := testServer(t)
	fs := &fakeSync{secret: "r3plic4-s3cr3t", instanceID: "r1"}
	srv.deps.Sync = fs
	h := srv.Handler()

	for _, url := range []string{
		"/api/v1/sync/version",
		"/api/v1/sync/version?applied=banana",
		// A version is a count that only goes up, so a negative one is not
		// a version at all — and it would sort a replica below the box that
		// has applied nothing.
		"/api/v1/sync/version?applied=-5",
	} {
		if w := tokenReq(t, h, "GET", url, "", fs.secret); w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", url, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
	for _, b := range fs.beats {
		if b != (heartbeat{"r1", 0, false}) {
			t.Errorf("heartbeat = %+v, want r1 at 0", b)
		}
	}
	if len(fs.beats) != 3 {
		t.Errorf("heartbeats = %+v, want three", fs.beats)
	}
}

// TestVersionProbeFromAForgottenReplica: "Forget" can land between the
// secret being checked and the entry being stamped, and the answer the box
// needs then is the one it would get a moment later — pair again — rather
// than a 503 blaming the store that is working fine.
func TestVersionProbeFromAForgottenReplica(t *testing.T) {
	srv, _, _ := testServer(t)
	fs := &fakeSync{secret: "r3plic4-s3cr3t", instanceID: "r1", beatErr: ErrNotRegistered}
	srv.deps.Sync = fs
	h := srv.Handler()

	w := tokenReq(t, h, "GET", "/api/v1/sync/version?applied=7", "", fs.secret)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("probe from a forgotten replica = %d %s, want 401", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if got, want := errorOf(t, w), "unauthorized"; got != want {
		t.Errorf("error = %q, want %q — the same answer requireReplica gives", got, want)
	}

	// A store that will not answer is a different fact and a different
	// status: nothing is wrong with this replica's credential.
	fs.beatErr = errors.New("database is locked")
	w = tokenReq(t, h, "GET", "/api/v1/sync/version?applied=7", "", fs.secret)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("probe against a broken store = %d %s, want 503", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if got, want := errorOf(t, w), "storage unavailable"; got != want {
		t.Errorf("error = %q, want %q; the driver error must not reach the body", got, want)
	}
}

// TestFollowWritesNothingWhenRefused: a code the main would not spend is
// the operator mistyping it, and the box must be exactly as unconfigured
// afterwards as it was before — a peer stored without a secret is a replica
// that pulls a 401 forever (§3).
func TestFollowWritesNothingWhenRefused(t *testing.T) {
	srv, s, _ := testServer(t)
	// Wrapped, the way a refusal comes back from the pull loop with the
	// peer's own words on it: the handler has to recognise the sentinel
	// rather than compare against it, and must not put the peer's words in
	// the answer (§9).
	fs := &fakeSync{followErr: fmt.Errorf("%w: five wrong guesses", ErrFollowRefused)}
	srv.deps.Sync = fs
	h := srv.Handler()
	cookie := login(t, srv, s)

	w := doReq(t, h, "POST", "/api/v1/sync/follow",
		`{"peer_url":"https://main.lan","code":"AAAA-AAAA"}`, cookie)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("follow with a wrong code = %d %s, want 502", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if got, want := errorOf(t, w), "the main refused the pairing code"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	if len(fs.followed) != 1 || fs.followed[0] != (followCall{"https://main.lan", "AAAA-AAAA"}) {
		t.Errorf("followed = %+v", fs.followed)
	}
	for _, key := range []string{"sync.peer_url", "sync.token"} {
		if got, _, _ := s.Settings().Get(t.Context(), key); got != "" {
			t.Errorf("%s = %q after a refused follow, want empty", key, got)
		}
	}

	// A failure that is not a refusal is the same 502 — this box did its
	// part and the far one did not answer usefully — but it says what
	// happened, since there is nothing to withhold from the operator of the
	// box that was dialling.
	fs.followErr = errors.New("dialling https://main.lan: connection refused")
	w = doReq(t, h, "POST", "/api/v1/sync/follow",
		`{"peer_url":"https://main.lan","code":"K7M9-QRTW"}`, cookie)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("follow with an unreachable main = %d %s, want 502", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if got := errorOf(t, w); got != fs.followErr.Error() {
		t.Errorf("error = %q, want the failure verbatim", got)
	}
}

// TestFollowStoresTheNormalisedPeer: the value the action writes into
// sync.peer_url is the one PUT /settings would have taken, so the trailing
// slash an operator pastes is off it before anything is dialled.
func TestFollowStoresTheNormalisedPeer(t *testing.T) {
	srv, s, _ := testServer(t)
	fs := &fakeSync{}
	srv.deps.Sync = fs
	h := srv.Handler()
	cookie := login(t, srv, s)

	w := doReq(t, h, "POST", "/api/v1/sync/follow",
		`{"peer_url":"https://main.lan/","code":"K7M9-QRTW"}`, cookie)
	if w.Code != http.StatusNoContent {
		t.Fatalf("follow = %d %s, want 204", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if len(fs.followed) != 1 || fs.followed[0].peerURL != "https://main.lan" {
		t.Errorf("followed = %+v, want the peer with its trailing slash stripped", fs.followed)
	}
}

// TestFollowIsRefusedWhileFollowing: following a second main would hand two
// boxes the same box to overwrite. Stop following first.
func TestFollowIsRefusedWhileFollowing(t *testing.T) {
	srv, s, _ := testServer(t)
	fs := &fakeSync{peer: "https://main.lan"}
	srv.deps.Sync = fs
	h := srv.Handler()
	cookie := login(t, srv, s)

	w := doReq(t, h, "POST", "/api/v1/sync/follow",
		`{"peer_url":"https://other.lan","code":"K7M9-QRTW"}`, cookie)
	if w.Code != http.StatusConflict {
		t.Fatalf("follow while following = %d %s, want 409", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if got, want := errorOf(t, w), "already following https://main.lan"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	if len(fs.followed) != 0 {
		t.Errorf("a second main was dialled anyway: %+v", fs.followed)
	}
}

// TestFollowRejectsAPeerURLTheSettingsGrammarWould: the Follow action
// writes sync.peer_url through the store rather than through PUT
// /settings, so this is where a value that endpoint would refuse gets
// refused — before a code is spent on it.
func TestFollowRejectsAPeerURLTheSettingsGrammarWould(t *testing.T) {
	srv, s, _ := testServer(t)
	fs := &fakeSync{}
	srv.deps.Sync = fs
	h := srv.Handler()
	cookie := login(t, srv, s)

	for _, bad := range []string{"", "main.lan", "ftp://main.lan", "https://main.lan/dnsaur",
		"https://admin:pw@main.lan"} {
		body, err := json.Marshal(map[string]string{"peer_url": bad, "code": "K7M9-QRTW"})
		if err != nil {
			t.Fatal(err)
		}
		if w := doReq(t, h, "POST", "/api/v1/sync/follow", string(body), cookie); w.Code != http.StatusBadRequest {
			t.Errorf("follow %q = %d %s, want 400", bad, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
	// An empty code costs one of the five guesses the main allows before it
	// voids the code, so it is refused here rather than spent there.
	if w := doReq(t, h, "POST", "/api/v1/sync/follow",
		`{"peer_url":"https://main.lan","code":"  "}`, cookie); w.Code != http.StatusBadRequest {
		t.Errorf("follow with no code = %d %s, want 400", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if len(fs.followed) != 0 {
		t.Errorf("a rejected body was dialled anyway: %+v", fs.followed)
	}
}

// TestForgetRemovesAReplica is the operator's button: it revokes that
// box's secret, and an id that is not registered is already the state the
// caller asked for.
func TestForgetRemovesAReplica(t *testing.T) {
	srv, s, _ := testServer(t)
	fs := &fakeSync{}
	srv.deps.Sync = fs
	h := srv.Handler()
	_ = login(t, srv, s)

	w := tokenReq(t, h, "DELETE", "/api/v1/sync/replicas/r1", "", apiToken(t, srv, "write"))
	if w.Code != http.StatusNoContent {
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
	"POST /api/v1/backup": "copies this box's own database to this box's own disk.",

	"POST /api/v1/filters/refresh": "downloads the lists this box already has; the replica keeps " +
		"its own copies current, which is how a list it has never seen ever gets fetched (§5).",
	"POST /api/v1/filters/lists/{id}/refresh": "operational, not config (§7): one list's Refresh now.",
	"POST /api/v1/zones/{id}/refresh":         "operational, not config (§7): one zone's transfer.",

	"DELETE /api/v1/sync/replicas/{instance_id}": "removing an entry from a registry a replica " +
		"does not keep is already the state the caller asked for, and it is how an operator clears " +
		"one left behind by a box that was a main.",

	"DELETE /api/v1/dhcp/leases/{ip}": "a lease belongs to the engine, not to the " +
		"configuration (DHCP design §8.3): Kea's HA propagates the release to the partner, and " +
		"the Leases page's Release button stays live on a replica. Every other DHCP write is " +
		"synced config and is guarded.",

	"POST /api/v1/sync/follow": "the replica's own action, and the only write that is *about* " +
		"following: it writes sync.peer_url and sync.token, which are local (§4.3). It refuses " +
		"a box that already follows a main, with \"already following <peer>\" rather than " +
		"\"managed by\" — the answer is Stop following first, not go ask the main.",
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

	fill := strings.NewReplacer("{id}", "1", "{rid}", "1", "{instance_id}", "r1", "{ip}", "192.168.1.50")
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

	// The two reads are the reads in this list. A replica's bundle is not
	// its own configuration to hand out — it is the main's, one pull stale
	// and stripped of nothing — so a box pointed at a replica would follow a
	// copy of a copy; and a box that was a main until a moment ago must stop
	// answering the probes of the replicas it used to have, or they go on
	// polling a box whose configuration is now somebody else's. Both say
	// which main to point at instead, whatever credential the caller brought.
	for _, url := range []string{"/api/v1/sync/bundle", "/api/v1/sync/version"} {
		w = doReq(t, h, "GET", url, "", cookie)
		if w.Code != http.StatusConflict {
			t.Fatalf("GET %s on a replica = %d %s, want 409", url, w.Code, strings.TrimSpace(w.Body.String()))
		}
		if got, want := errorOf(t, w), "managed by https://main.lan"; got != want {
			t.Errorf("%s error = %q, want %q", url, got, want)
		}
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
		// Written by the first pairing and by nothing else (§6), so every
		// value is refused for the same reason the pause state is.
		{"sync.tsig_key_id", "0", http.StatusBadRequest},
		{"sync.tsig_key_id", "1", http.StatusBadRequest},
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

	// Scheme and host only: the pull loop joins "/api/v1/sync/..." onto this
	// value, so anything else in it builds a URL nobody meant.
	for _, bad := range []string{
		"https://main.lan/dnsaur", "https://main.lan?x=1",
		"https://main.lan#frag", "https://admin:pw@main.lan",
	} {
		if w := single("sync.peer_url", bad); w.Code != http.StatusBadRequest {
			t.Errorf("PUT sync.peer_url=%q = %d %s, want 400", bad, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
	// A trailing slash is the one extra accepted, and it is normalised away
	// rather than stored — "https://main.lan//api/v1/sync/version" is not a
	// URL anybody meant.
	if w := single("sync.peer_url", "https://main.lan/"); w.Code != http.StatusNoContent {
		t.Fatalf("trailing slash = %d %s, want 204", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if got, _, _ := s.Settings().Get(t.Context(), "sync.peer_url"); got != "https://main.lan" {
		t.Errorf("stored sync.peer_url = %q, want the trailing slash stripped", got)
	}

	// The token rule holds from the other side too: emptying it under a
	// configured peer leaves a replica that pulls a 401 forever.
	if w := single("sync.token", ""); w.Code != http.StatusBadRequest {
		t.Errorf("clearing the token under a peer = %d %s, want 400", w.Code, strings.TrimSpace(w.Body.String()))
	}
	// Clearing both at once is "stop following", and must not be caught by
	// that rule: a peer being cleared is judged before the token.
	w = doReq(t, h, "PUT", "/api/v1/settings", `{"sync.peer_url":"","sync.token":""}`, cookie)
	if w.Code != http.StatusNoContent {
		t.Fatalf("stop following = %d %s, want 204", w.Code, strings.TrimSpace(w.Body.String()))
	}
	for _, key := range []string{"sync.peer_url", "sync.token"} {
		if got, _, _ := s.Settings().Get(t.Context(), key); got != "" {
			t.Errorf("%s = %q after stopping, want empty", key, got)
		}
	}

}

// TestSyncInternalsAreNotEditableSettings: sync.pairing holds the SHA-256 of
// a 40-bit code, so a read token that could see it could grind it offline,
// and sync.tsig_key_id is written by the first pairing rather than chosen
// (§6). Both are bookkeeping, like the pause state, and get its treatment.
func TestSyncInternalsAreNotEditableSettings(t *testing.T) {
	srv, s, _ := testServer(t)
	h := srv.Handler()
	cookie := login(t, srv, s)
	internal := map[string]string{
		"sync.pairing":     `{"hash":"c0ffee","expires_at":1757580000000}`,
		"sync.tsig_key_id": "7",
	}
	for k, v := range internal {
		if err := s.Settings().SetInternal(t.Context(), k, v); err != nil {
			t.Fatal(err)
		}
	}

	w := doReq(t, h, "GET", "/api/v1/settings", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("get settings: %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "c0ffee") {
		t.Fatal("the pairing code's hash leaked through GET /settings")
	}
	var all map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	for k := range internal {
		if _, leaked := all[k]; leaked {
			t.Errorf("GET /settings carries %s", k)
		}
		body, err := json.Marshal(map[string]string{"key": k, "value": internal[k]})
		if err != nil {
			t.Fatal(err)
		}
		w := doReq(t, h, "PUT", "/api/v1/settings", string(body), cookie)
		if w.Code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d %s, want 400", k, w.Code, strings.TrimSpace(w.Body.String()))
			continue
		}
		if got, want := errorOf(t, w), "setting not editable: "+k; got != want {
			t.Errorf("PUT %s error = %q, want %q", k, got, want)
		}
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
