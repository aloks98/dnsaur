package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

func TestSetupFlow(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()

	w := doReq(t, h, "GET", "/api/v1/setup", "", nil)
	var st map[string]bool
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if w.Code != 200 || !st["setup_required"] {
		t.Fatalf("fresh setup state: %d %v", w.Code, st)
	}
	if w := doReq(t, h, "POST", "/api/v1/setup", `{"username":"admin","password":"pw"}`, nil); w.Code != 400 {
		t.Fatalf("short password accepted: %d", w.Code)
	}
	if w := doReq(t, h, "POST", "/api/v1/setup", `{"username":"admin","password":"password123"}`, nil); w.Code != 201 {
		t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
	if w := doReq(t, h, "POST", "/api/v1/setup", `{"username":"x","password":"password123"}`, nil); w.Code != 409 {
		t.Fatalf("second setup: %d", w.Code)
	}
	w = doReq(t, h, "GET", "/api/v1/setup", "", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st["setup_required"] {
		t.Fatal("setup still required after admin created")
	}
}

// TestLoginBeforeSetupIs409 asserts the plan's global constraint: while
// first-run setup hasn't happened, login must refuse with 409 rather than
// falling through to (and leaking) bad-credentials/store behavior against
// an empty users table.
func TestLoginBeforeSetupIs409(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()

	w := doReq(t, h, "POST", "/api/v1/auth/login", `{"username":"admin","password":"whatever1"}`, nil)
	if w.Code != 409 {
		t.Fatalf("login before setup: %d %s", w.Code, w.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "setup required" {
		t.Fatalf("body: %v", body)
	}
}

// fromIP issues a request to the auth endpoints as a given source address,
// which is what the throttle counts by.
func fromIP(h http.Handler, method, path, body, ip string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, stringsReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":40000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestSetupAndLoginAreThrottled — both endpoints are unauthenticated and
// both pay for an argon2id hash, so an unthrottled loop over either one is
// a memory and CPU burner, and an unthrottled loop over login is also a
// password oracle with no cost attached.
//
// The budget is per source and shared between the two endpoints: they are
// the same expense, and letting a locked-out client keep hammering /setup
// would leave the hole open under a different name.
func TestSetupAndLoginAreThrottled(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()
	now := time.Unix(1_700_000_000, 0)
	srv.attempts.now = func() time.Time { return now }
	const attacker, bystander = "198.51.100.4", "198.51.100.9"

	good := `{"username":"admin","password":"password123"}`
	bad := `{"username":"admin","password":"wrong-password"}`
	if w := fromIP(h, "POST", "/api/v1/setup", good, attacker); w.Code != 201 {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	// One attempt spent on setup; nine more are answered on their merits.
	for i := range attemptLimit - 1 {
		if w := fromIP(h, "POST", "/api/v1/auth/login", bad, attacker); w.Code != 401 {
			t.Fatalf("attempt %d: %d %s", i+2, w.Code, w.Body)
		}
	}
	w := fromIP(h, "POST", "/api/v1/auth/login", bad, attacker)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d = %d %s, want 429", attemptLimit+1, w.Code, w.Body)
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "too many attempts" {
		t.Fatalf("error string = %q", body["error"])
	}
	if got := w.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want 60", got)
	}
	// Locked out means locked out: the right password is refused too, or
	// the endpoint is still an oracle.
	if w := fromIP(h, "POST", "/api/v1/auth/login", good, attacker); w.Code != http.StatusTooManyRequests {
		t.Fatalf("correct password during lockout: %d", w.Code)
	}
	// And so is the other endpoint on the same budget.
	if w := fromIP(h, "POST", "/api/v1/setup", good, attacker); w.Code != http.StatusTooManyRequests {
		t.Fatalf("setup during lockout: %d", w.Code)
	}
	// Somebody else is not locked out by their neighbour.
	if w := fromIP(h, "POST", "/api/v1/auth/login", good, bystander); w.Code != 200 {
		t.Fatalf("bystander login: %d %s", w.Code, w.Body)
	}

	// The lockout ends.
	now = now.Add(attemptLockout + time.Second)
	if w := fromIP(h, "POST", "/api/v1/auth/login", good, attacker); w.Code != 200 {
		t.Fatalf("login after the lockout expired: %d %s", w.Code, w.Body)
	}
}

// TestTOTPRequiredAnswerIsThrottled — 428 "totp code required" is only
// given once the password verified, so it confirms a guessed password
// without needing the second factor. The UI contract needs that split, so
// the answer stays; what makes it acceptable is that it costs an attempt
// like every other answer.
func TestTOTPRequiredAnswerIsThrottled(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()
	now := time.Unix(1_700_000_000, 0)
	srv.attempts.now = func() time.Time { return now }

	ctx := t.Context()
	if err := srv.deps.Auth.CreateAdmin(ctx, "admin", "password123"); err != nil {
		t.Fatal(err)
	}
	secret, _, err := srv.deps.Auth.EnableTOTPStart(ctx, 1, "admin")
	if err != nil {
		t.Fatal(err)
	}
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.deps.Auth.EnableTOTPConfirm(ctx, 1, secret, code, 0); err != nil {
		t.Fatal(err)
	}

	good := `{"username":"admin","password":"password123"}`
	for i := range attemptLimit {
		if w := fromIP(h, "POST", "/api/v1/auth/login", good, "198.51.100.20"); w.Code != http.StatusPreconditionRequired {
			t.Fatalf("attempt %d = %d %s, want 428", i+1, w.Code, w.Body)
		}
	}
	if w := fromIP(h, "POST", "/api/v1/auth/login", good, "198.51.100.20"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("428 answers are free: attempt %d = %d", attemptLimit+1, w.Code)
	}
}

// TestSessionCookieSecureBehindProxy — the cookie used to be marked Secure
// only when Go itself terminated TLS, so the standard deployment (nginx or
// Caddy in front, dnsaur on plain HTTP behind it) shipped a 30-day session
// cookie without it, and any plaintext request to the same host would carry
// the session out over the wire.
//
// X-Forwarded-Proto is believed only from an address the operator named in
// trusted_proxies. It is a header: anyone can send it, so trusting it
// unconditionally would let a client on the LAN decide its own cookie
// attributes, which is not a security decision worth making for them.
func TestSessionCookieSecureBehindProxy(t *testing.T) {
	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	for _, tc := range []struct {
		name       string
		trusted    []netip.Prefix
		url        string
		remoteAddr string
		proto      string
		want       bool
	}{
		{"plain http, nothing configured", nil, "http://dns.lan/api/v1/auth/login", "192.0.2.7:5000", "", false},
		{"go terminated tls", nil, "https://dns.lan/api/v1/auth/login", "192.0.2.7:5000", "", true},
		{"header without a trusted proxy", nil, "http://dns.lan/api/v1/auth/login", "10.0.0.5:5000", "https", false},
		{"trusted proxy, https", proxies, "http://dns.lan/api/v1/auth/login", "10.0.0.5:5000", "https", true},
		{"trusted proxy, uppercase scheme", proxies, "http://dns.lan/api/v1/auth/login", "10.0.0.5:5000", "HTTPS", true},
		{"trusted proxy, proxy chain", proxies, "http://dns.lan/api/v1/auth/login", "10.0.0.5:5000", "https, http", true},
		{"trusted proxy, plain http", proxies, "http://dns.lan/api/v1/auth/login", "10.0.0.5:5000", "http", false},
		{"untrusted source claiming https", proxies, "http://dns.lan/api/v1/auth/login", "203.0.113.9:5000", "https", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := testServer(t, func(d *Deps) { d.TrustedProxies = tc.trusted })
			h := srv.Handler()
			doReq(t, h, "POST", "/api/v1/setup", `{"username":"admin","password":"password123"}`, nil)

			req := httptest.NewRequest("POST", tc.url, stringsReader(`{"username":"admin","password":"password123"}`))
			req.Header.Set("Content-Type", "application/json")
			req.RemoteAddr = tc.remoteAddr
			if tc.proto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatalf("login: %d %s", w.Code, w.Body.String())
			}
			var cookie *http.Cookie
			for _, c := range w.Result().Cookies() {
				if c.Name == "dnsaur_session" {
					cookie = c
				}
			}
			if cookie == nil {
				t.Fatal("no session cookie")
			}
			if cookie.Secure != tc.want {
				t.Fatalf("Secure = %v, want %v", cookie.Secure, tc.want)
			}

			// The clearing cookie has to match, or a browser that stored a
			// Secure cookie keeps it after logout.
			logout := httptest.NewRequest("POST", tc.url[:strings.LastIndex(tc.url, "/")]+"/logout", nil)
			logout.RemoteAddr = tc.remoteAddr
			logout.AddCookie(cookie)
			if tc.proto != "" {
				logout.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			lw := httptest.NewRecorder()
			h.ServeHTTP(lw, logout)
			for _, c := range lw.Result().Cookies() {
				if c.Name == "dnsaur_session" && c.Secure != tc.want {
					t.Fatalf("logout cookie Secure = %v, want %v", c.Secure, tc.want)
				}
			}
		})
	}
}

func TestLoginLogoutMe(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()
	doReq(t, h, "POST", "/api/v1/setup", `{"username":"admin","password":"password123"}`, nil)

	if w := doReq(t, h, "POST", "/api/v1/auth/login", `{"username":"admin","password":"nope"}`, nil); w.Code != 401 {
		t.Fatalf("bad login: %d", w.Code)
	}
	w := doReq(t, h, "POST", "/api/v1/auth/login", `{"username":"admin","password":"password123"}`, nil)
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "dnsaur_session" {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie attrs: %+v", cookie)
	}
	w = doReq(t, h, "GET", "/api/v1/auth/me", "", cookie)
	var me map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &me)
	if w.Code != 200 || me["username"] != "admin" || me["totp_enabled"] != false {
		t.Fatalf("me: %d %v", w.Code, me)
	}
	if w := doReq(t, h, "POST", "/api/v1/auth/logout", "", cookie); w.Code != 204 {
		t.Fatalf("logout: %d", w.Code)
	}
	if w := doReq(t, h, "GET", "/api/v1/auth/me", "", cookie); w.Code != 401 {
		t.Fatalf("session survives logout: %d", w.Code)
	}
}

// loginCookie logs an existing admin in and returns the session cookie, so
// a test can hold more than one session at once.
func loginCookie(t *testing.T, h http.Handler, username, password string) *http.Cookie {
	t.Helper()
	w := doReq(t, h, "POST", "/api/v1/auth/login", `{"username":"`+username+`","password":"`+password+`"}`, nil)
	if w.Code != 200 {
		t.Fatalf("login as %s: %d %s", username, w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "dnsaur_session" {
			return c
		}
	}
	t.Fatal("login answered 200 with no session cookie")
	return nil
}

// TestChangePassword covers the whole of POST /auth/password: the current
// password really is checked, the new one has to clear the same minimum
// setup enforces, and a successful change ends every *other* session while
// leaving the caller's own — and the account's API tokens — alone.
//
// The session sweep is the point of the endpoint rather than a nicety.
// Changing a password is what someone does when they think it is known, and
// it means nothing while the sessions minted with the old one keep working;
// this is the same rule EnableTOTPConfirm already follows.
func TestChangePassword(t *testing.T) {
	srv, s, _ := testServer(t)
	h := srv.Handler()
	caller := login(t, srv, s) // creates the admin with password123
	other := loginCookie(t, h, "admin", "password123")
	_, apiTok, err := srv.deps.Auth.CreateAPIToken(t.Context(), 1, "kept", "write", 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, body string }{
		{"wrong current", `{"current_password":"not-it","new_password":"new-password1"}`},
		{"short new", `{"current_password":"password123","new_password":"short"}`},
		{"empty new", `{"current_password":"password123","new_password":""}`},
	} {
		if w := doReq(t, h, "POST", "/api/v1/auth/password", tc.body, caller); w.Code != 400 {
			t.Errorf("%s = %d %s, want 400", tc.name, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
	// Nothing above may have changed anything.
	if w := doReq(t, h, "GET", "/api/v1/auth/me", "", other); w.Code != 200 {
		t.Fatalf("a refused change logged another session out: %d", w.Code)
	}

	if w := doReq(t, h, "POST", "/api/v1/auth/password",
		`{"current_password":"password123","new_password":"new-password1"}`, caller); w.Code != 204 {
		t.Fatalf("change: %d %s", w.Code, w.Body.String())
	}

	if w := doReq(t, h, "POST", "/api/v1/auth/login", `{"username":"admin","password":"password123"}`, nil); w.Code != 401 {
		t.Errorf("the old password still logs in: %d", w.Code)
	}
	if w := doReq(t, h, "POST", "/api/v1/auth/login", `{"username":"admin","password":"new-password1"}`, nil); w.Code != 200 {
		t.Errorf("the new password does not log in: %d %s", w.Code, w.Body.String())
	}
	if w := doReq(t, h, "GET", "/api/v1/auth/me", "", caller); w.Code != 200 {
		t.Errorf("the session that changed the password was logged out: %d", w.Code)
	}
	if w := doReq(t, h, "GET", "/api/v1/auth/me", "", other); w.Code != 401 {
		t.Errorf("another browser's session survived the password change: %d", w.Code)
	}
	if w := bearerReq(h, "GET", "/api/v1/auth/me", apiTok); w.Code != 200 {
		t.Errorf("an API token was revoked by a password change: %d", w.Code)
	}
}

// TestChangePasswordIsThrottled — the endpoint verifies a password with
// argon2id and answers whether it was right, which is the same expense and
// the same oracle login is, one authenticated step further in. It shares
// login's budget for exactly that reason.
func TestChangePasswordIsThrottled(t *testing.T) {
	srv, s, _ := testServer(t)
	h := srv.Handler()
	cookie := login(t, srv, s)
	now := time.Unix(1_700_000_000, 0)
	srv.attempts.now = func() time.Time { return now }

	const bad = `{"current_password":"wrong-password","new_password":"new-password1"}`
	change := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/v1/auth/password", stringsReader(bad))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	for i := range attemptLimit {
		if w := change(); w.Code != 400 {
			t.Fatalf("attempt %d = %d %s, want 400", i+1, w.Code, w.Body)
		}
	}
	w := change()
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d = %d %s, want 429", attemptLimit+1, w.Code, w.Body)
	}
	if got := w.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60", got)
	}
}

// TestDeleteSessionsKeepsTheCaller — "log out everywhere" has to leave the
// browser that asked for it logged in, or the admin who suspects a stolen
// cookie logs themselves out along with it and has to sign in again to find
// out whether it worked. API tokens are a separate credential and are not
// swept: they are revoked by name, on the account page.
func TestDeleteSessionsKeepsTheCaller(t *testing.T) {
	srv, s, _ := testServer(t)
	h := srv.Handler()
	caller := login(t, srv, s)
	other := loginCookie(t, h, "admin", "password123")
	_, apiTok, err := srv.deps.Auth.CreateAPIToken(t.Context(), 1, "kept", "write", 0)
	if err != nil {
		t.Fatal(err)
	}

	if w := doReq(t, h, "DELETE", "/api/v1/auth/sessions", "", caller); w.Code != 204 {
		t.Fatalf("delete sessions: %d %s", w.Code, w.Body.String())
	}
	if w := doReq(t, h, "GET", "/api/v1/auth/me", "", caller); w.Code != 200 {
		t.Errorf("the caller's own session was revoked: %d", w.Code)
	}
	if w := doReq(t, h, "GET", "/api/v1/auth/me", "", other); w.Code != 401 {
		t.Errorf("another browser's session survived: %d", w.Code)
	}
	if w := bearerReq(h, "GET", "/api/v1/auth/me", apiTok); w.Code != 200 {
		t.Errorf("an API token was revoked: %d", w.Code)
	}
}
