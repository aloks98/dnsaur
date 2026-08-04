package api

import (
	"encoding/json"
	"net/http"
	"testing"
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
