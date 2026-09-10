package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/auth"
	"github.com/aloks98/dnsaur/internal/store"
)

func TestAPITokenLifecycle(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()

	w := doReq(t, h, "POST", "/api/v1/tokens", `{"name":"homeassistant"}`, cookie)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	plain, _ := created["token"].(string)
	if len(plain) < 40 {
		t.Fatalf("no plain token returned: %v", created)
	}
	// read-scope token: GET allowed, mutation forbidden
	w = doReq(t, h, "POST", "/api/v1/tokens", `{"name":"ro","scope":"read"}`, cookie)
	var roCreated map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &roCreated)
	ro, _ := roCreated["token"].(string)
	// Any mutating, requireAuth-wrapped route works as the scope probe here;
	// POST /api/v1/zones is used since /api/v1/records (Task 8) no longer
	// exists.
	req0 := httptest.NewRequest("POST", "/api/v1/zones", stringsReader(`{"name":"ro-probe.test"}`))
	req0.Header.Set("Authorization", "Bearer "+ro)
	req0.Header.Set("Content-Type", "application/json")
	rec0 := httptest.NewRecorder()
	h.ServeHTTP(rec0, req0)
	if rec0.Code != http.StatusForbidden {
		t.Fatalf("read token allowed mutation: %d", rec0.Code)
	}
	req := httptest.NewRequest("GET", "/api/v1/tokens", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("bearer list: %d", rec.Code)
	}
	var list []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) < 1 {
		t.Fatalf("list empty: %v", list)
	}
	// Find the homeassistant token in the list
	found := false
	for _, token := range list {
		if token["name"] == "homeassistant" {
			found = true
			if _, hashLeaked := token["token_hash"]; hashLeaked {
				t.Fatal("token hash serialized")
			}
			break
		}
	}
	if !found {
		t.Fatalf("homeassistant token not found in list: %v", list)
	}
	id := int64(list[0]["id"].(float64))
	if w := doReq(t, h, "DELETE", fmt.Sprintf("/api/v1/tokens/%d", id), "", cookie); w.Code != 204 {
		t.Fatalf("revoke: %d", w.Code)
	}
	req = httptest.NewRequest("GET", "/api/v1/tokens", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked bearer still works: %d", rec.Code)
	}
}

// The create response's id used to be a guess: the handler discarded the
// insert id, re-listed the user's tokens and picked the highest one with a
// matching name. Two tokens called the same thing made it ambiguous, and a
// listing that failed made it 0 — silently, since that error was discarded
// too, so a 201 handed back an id naming no row at all.
//
// The listing is made to fail here because that is the case the guess got
// *wrong* rather than merely got right by luck: created in sequence, the
// highest id with a matching name does happen to be the row just inserted,
// so a test that only creates two tokens proves nothing.
func TestTokenCreateReturnsTheInsertID(t *testing.T) {
	var flaky *flakyTokenStore
	srv, s, _ := testServer(t, func(d *Deps) {
		flaky = &flakyTokenStore{TokenStore: d.Store.Tokens()}
		d.Auth = auth.New(d.Store.Users(), flaky)
	})
	cookie := login(t, srv, s)
	h := srv.Handler()

	flaky.fail = true
	w := doReq(t, h, "POST", "/api/v1/tokens", `{"name":"grafana"}`, cookie)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Token == "" {
		t.Fatalf("no plaintext token: %s", w.Body.String())
	}
	// The row the create actually inserted, found the one way that does not
	// go through the listing: by the hash of the plaintext just handed back.
	row, found, err := s.Tokens().ByHash(t.Context(), auth.HashToken(got.Token))
	if err != nil || !found {
		t.Fatalf("token row: found=%v err=%v", found, err)
	}
	if got.ID != row.ID {
		t.Fatalf("create answered id %d; the row it inserted is %d", got.ID, row.ID)
	}

	// And two tokens sharing a name get distinct ids, each its own row.
	flaky.fail = false
	first := createToken(t, h, cookie, `{"name":"grafana"}`)
	second := createToken(t, h, cookie, `{"name":"grafana"}`)
	if first == second {
		t.Fatalf("two tokens with one name got the same id: %d", first)
	}
	if w := doReq(t, h, "DELETE", fmt.Sprintf("/api/v1/tokens/%d", first), "", cookie); w.Code != http.StatusNoContent {
		t.Fatalf("revoke by returned id: %d %s", w.Code, w.Body.String())
	}
	after, _ := srv.deps.Auth.ListAPITokens(t.Context(), 1)
	for _, tok := range after {
		if tok.ID == first {
			t.Fatalf("revoke removed some other row; %d survives", first)
		}
	}
}

func createToken(t *testing.T, h http.Handler, cookie *http.Cookie, body string) int64 {
	t.Helper()
	w := doReq(t, h, "POST", "/api/v1/tokens", body, cookie)
	if w.Code != http.StatusCreated {
		t.Fatalf("create %s: %d %s", body, w.Code, w.Body.String())
	}
	var got struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID == 0 {
		t.Fatalf("create %s answered %s", body, w.Body.String())
	}
	return got.ID
}

// flakyTokenStore fails the one read RevokeToken uses to decide ownership,
// and nothing else — Authenticate goes through ByHash/Touch, so the request
// still gets as far as the handler. Everything else is the real store.
type flakyTokenStore struct {
	store.TokenStore
	fail bool
}

func (f *flakyTokenStore) ListAPI(ctx context.Context, userID int64) ([]store.AuthToken, error) {
	if f.fail {
		return nil, errors.New("connection refused")
	}
	return f.TokenStore.ListAPI(ctx, userID)
}

// Every RevokeToken error became 404 "not found", storage failures included,
// so a script that saw one concluded the token was gone when the database
// was merely unreachable. Only "you do not own this token" is a 404 now.
func TestTokenRevokeSeparatesNotOwnedFromStorageFailure(t *testing.T) {
	var flaky *flakyTokenStore
	srv, s, _ := testServer(t, func(d *Deps) {
		flaky = &flakyTokenStore{TokenStore: d.Store.Tokens()}
		d.Auth = auth.New(d.Store.Users(), flaky)
	})
	cookie := login(t, srv, s)
	h := srv.Handler()

	if w := doReq(t, h, "DELETE", "/api/v1/tokens/999999", "", cookie); w.Code != http.StatusNotFound {
		t.Fatalf("unknown token: %d %s, want 404", w.Code, w.Body.String())
	}

	flaky.fail = true
	if w := doReq(t, h, "DELETE", "/api/v1/tokens/1", "", cookie); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("storage failure: %d %s, want 503 — a 404 here says the token is gone", w.Code, w.Body.String())
	}
}

// The dashboard draws the enrolment QR from what this endpoint hands back, so
// the response has to carry a decodable image and not only the otpauth:// URL
// — the browser has no QR encoder left to fall back on.
func TestTOTPStartReturnsAScannableQR(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)

	w := doReq(t, srv.Handler(), "POST", "/api/v1/auth/totp/start", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Secret     string `json:"secret"`
		OTPAuthURL string `json:"otpauth_url"`
		QRPNG      string `json:"qr_png"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Secret == "" || !strings.HasPrefix(body.OTPAuthURL, "otpauth://totp/") {
		t.Fatalf("no enrolment returned: %+v", body)
	}
	raw, err := base64.StdEncoding.DecodeString(body.QRPNG)
	if err != nil {
		t.Fatalf("qr_png is not base64: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("qr_png is not a PNG: %v", err)
	}
	if got := img.Bounds().Dx(); got != qrPixels {
		t.Fatalf("qr width = %d, want %d", got, qrPixels)
	}
}

// TestTokenExpiry covers the optional expiry on POST /tokens end to end: a
// stamp in the past is refused, one in the future is stored and listed, and
// the token stops authenticating the moment it passes.
//
// The last part is the one that matters and the one nothing asserted:
// auth.Service.Authenticate has always refused an expired token — that is
// what makes a session expire — but until now no API token could ever carry
// an expiry, so the branch was unreachable from this endpoint. A token
// minted for a script that runs for a month is a credential that stops being
// one on its own, which is the whole reason to offer the field.
func TestTokenExpiry(t *testing.T) {
	srv, s, _ := testServer(t)
	h := srv.Handler()
	cookie := login(t, srv, s)
	now := time.Unix(1_700_000_000, 0)
	srv.deps.Auth.Now = func() time.Time { return now }

	past := now.Add(-time.Minute).UnixMilli()
	future := now.Add(24 * time.Hour).UnixMilli()

	for _, tc := range []struct{ name, body string }{
		{"in the past", fmt.Sprintf(`{"name":"stale","expires_at":%d}`, past)},
		{"now", fmt.Sprintf(`{"name":"stale","expires_at":%d}`, now.UnixMilli())},
		{"negative", `{"name":"stale","expires_at":-1}`},
	} {
		w := doReq(t, h, "POST", "/api/v1/tokens", tc.body, cookie)
		// The message is asserted, not just the status: before the field
		// existed every one of these was 400 "invalid json" from the
		// decoder's unknown-field check, which is the same status for a
		// different reason and would pass a bare 400 assertion.
		var body map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != 400 || !strings.Contains(body["error"], "expires_at") {
			t.Errorf("expires_at %s = %d %s, want 400 naming expires_at", tc.name, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}

	w := doReq(t, h, "POST", "/api/v1/tokens",
		fmt.Sprintf(`{"name":"month","scope":"write","expires_at":%d}`, future), cookie)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	w = doReq(t, h, "GET", "/api/v1/tokens", "", cookie)
	var list []store.AuthToken
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ExpiresAt != future {
		t.Fatalf("listed tokens = %+v, want one expiring at %d", list, future)
	}

	if w := bearerReq(h, "GET", "/api/v1/auth/me", created.Token); w.Code != 200 {
		t.Fatalf("token refused before its expiry: %d %s", w.Code, w.Body.String())
	}
	now = now.Add(25 * time.Hour)
	if w := bearerReq(h, "GET", "/api/v1/auth/me", created.Token); w.Code != 401 {
		t.Errorf("expired token still authenticates: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}

	// An omitted expiry still means never, which is what every token minted
	// before this field existed carries.
	w = doReq(t, h, "POST", "/api/v1/tokens", `{"name":"forever"}`, cookie)
	if w.Code != 201 {
		t.Fatalf("create without an expiry: %d %s", w.Code, w.Body.String())
	}
	var forever struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &forever); err != nil {
		t.Fatal(err)
	}
	w = doReq(t, h, "GET", "/api/v1/tokens", "", cookie)
	list = nil
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, tok := range list {
		if tok.ID == forever.ID && tok.ExpiresAt != 0 {
			t.Errorf("a token created with no expires_at reports %d", tok.ExpiresAt)
		}
	}
}
