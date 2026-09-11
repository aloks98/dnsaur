package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// This file drives Server.routes the same way routeauth_test.go does, for a
// different property: a request that creates something answers 201 with a
// Location header naming what it created. A client that has to parse a body
// to learn where the new row lives is a client doing by hand what one
// header says outright, and RFC 9110 §15.3.2 says a 201 SHOULD carry it.
//
// Coverage comes from the registry rather than from a list somebody
// remembers to extend: every POST route must be named either by a probe
// below (which really issues the request and checks the header) or by
// noLocationRoutes (which has to state why that route creates nothing).
// Adding a POST route without doing one or the other fails this test.

// noLocationRoutes are the registered POST patterns that do not answer 201
// with a Location, mapped to the reason. Exact whole patterns, for the same
// reason unauthenticatedRoutes uses them: an exemption written as a prefix
// grows on its own.
var noLocationRoutes = map[string]string{
	"POST /api/v1/setup": "creates the admin account, which this API exposes no URL for — " +
		"GET /auth/me needs a session that setup deliberately does not mint.",
	"POST /api/v1/auth/login":        "mints a session cookie, not a resource with an address.",
	"POST /api/v1/auth/logout":       "revokes the caller's session; 204.",
	"POST /api/v1/auth/password":     "replaces the password on the account the caller is already at; 204.",
	"POST /api/v1/auth/totp/start":   "returns an enrollment secret that is not persisted at all.",
	"POST /api/v1/auth/totp/confirm": "turns 2FA on for the existing account; 204.",
	"POST /api/v1/auth/totp/disable": "turns 2FA off for the existing account; 204.",
	"POST /api/v1/blocking/pause":    "sets a pause on an existing scope; 204.",
	"POST /api/v1/backup": "writes a file, not a row. It does answer 201 with a Location — the " +
		"path of the backup on the server's disk — but there is no id and no URL that serves it, " +
		"so the id-based probe below has nothing to check. See TestBackupNamesTheFileItWrote.",
	"POST /api/v1/filters/refresh": "starts background work; 202, and there is no new row.",
	"POST /api/v1/filters/lists/{id}/refresh": "refreshes the list the URL already names; 202 with " +
		"that same row.",
	"POST /api/v1/zones/{id}/refresh": "transfers into the zone the URL already names; 200.",
	"POST /api/v1/zones/{id}/file": "replaces the records of the zone the URL already names; 200 " +
		"with a summary of what it imported.",
	"POST /api/v1/sync/replicas": "records a replica the caller already has an id for, in the " +
		"main's own settings; there is no row and no URL that serves one. DELETE " +
		"/sync/replicas/{instance_id} is the only address a replica has, and the caller minted " +
		"that id itself.",
}

// createProbe is one POST that must answer 201 + Location: the URL to send
// it to, the body, and where the answer has to point given the id it
// reports back.
type createProbe struct {
	pattern  string
	url      string
	body     string
	location func(id int64) string
}

// TestEveryCreateAnswersLocation issues every creating POST for real and
// asserts the answer is 201 with a Location naming the new row, and that
// the body carries the row's id (the created object, not a bare envelope —
// so the header and the body cannot disagree about which row it is).
func TestEveryCreateAnswersLocation(t *testing.T) {
	srv, s, _ := testServer(t)
	h := srv.Handler() // also builds srv.mux, which assertResolvesTo needs
	cookie := login(t, srv, s)

	// Two rows the probes hang off. Zone 1 is the built-in localhost, whose
	// records refuse every write with 409, so the record probe needs a zone
	// this API created; groups are not seeded at all.
	groupID := mustCreate(t, h, cookie, "/api/v1/groups", `{"name":"location-probe"}`)
	zoneID := mustCreate(t, h, cookie, "/api/v1/zones", `{"name":"location-probe.test"}`)

	probes := []createProbe{
		{
			pattern: "POST /api/v1/groups", url: "/api/v1/groups",
			body:     `{"name":"location-probe-2"}`,
			location: func(id int64) string { return fmt.Sprintf("/api/v1/groups/%d", id) },
		},
		{
			pattern: "POST /api/v1/clients", url: "/api/v1/clients",
			body:     fmt.Sprintf(`{"name":"probe","matcher":"10.77.0.1","group_id":%d}`, groupID),
			location: func(id int64) string { return fmt.Sprintf("/api/v1/clients/%d", id) },
		},
		{
			pattern: "POST /api/v1/groups/{id}/rules",
			url:     fmt.Sprintf("/api/v1/groups/%d/rules", groupID),
			body:    `{"action":"block","pattern":"probe.example"}`,
			// A rule has no read of its own; DELETE /filters/rules/{id} is
			// the only URL that addresses one, and it is the one a client
			// needs after creating it.
			location: func(id int64) string { return fmt.Sprintf("/api/v1/filters/rules/%d", id) },
		},
		{
			pattern: "POST /api/v1/filters/lists", url: "/api/v1/filters/lists",
			body:     `{"url":"https://example.com/probe.txt","kind":"block"}`,
			location: func(id int64) string { return fmt.Sprintf("/api/v1/filters/lists/%d", id) },
		},
		{
			pattern: "POST /api/v1/tokens", url: "/api/v1/tokens",
			body:     `{"name":"location-probe","scope":"read"}`,
			location: func(id int64) string { return fmt.Sprintf("/api/v1/tokens/%d", id) },
		},
		{
			pattern: "POST /api/v1/tsig-keys", url: "/api/v1/tsig-keys",
			body:     `{"name":"probe-key","algorithm":"hmac-sha256.","secret":"c2VjcmV0"}`,
			location: func(id int64) string { return fmt.Sprintf("/api/v1/tsig-keys/%d", id) },
		},
		{
			pattern: "POST /api/v1/zones", url: "/api/v1/zones",
			body:     `{"name":"location-probe-2.test"}`,
			location: func(id int64) string { return fmt.Sprintf("/api/v1/zones/%d", id) },
		},
		{
			pattern: "POST /api/v1/zones/{id}/clone",
			url:     fmt.Sprintf("/api/v1/zones/%d/clone", zoneID),
			body:    `{"name":"location-probe-clone.test"}`,
			// A clone creates a zone, so the URL it points at is a zone's,
			// not a sub-resource of the one that was copied.
			location: func(id int64) string { return fmt.Sprintf("/api/v1/zones/%d", id) },
		},
		{
			pattern: "POST /api/v1/zones/{id}/records",
			url:     fmt.Sprintf("/api/v1/zones/%d/records", zoneID),
			body:    `{"name":"nas","type":"A","rdata":"192.168.1.10","ttl":300}`,
			location: func(id int64) string {
				return fmt.Sprintf("/api/v1/zones/%d/records/%d", zoneID, id)
			},
		},
	}

	probed := make(map[string]bool, len(probes))
	for _, p := range probes {
		probed[p.pattern] = true
		t.Run(p.pattern, func(t *testing.T) {
			assertResolvesTo(t, srv, http.MethodPost, p.url, p.pattern)
			w := doReq(t, h, http.MethodPost, p.url, p.body, cookie)
			if w.Code != http.StatusCreated {
				t.Fatalf("POST %s = %d %s, want 201", p.url, w.Code, strings.TrimSpace(w.Body.String()))
			}
			var created struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == 0 {
				t.Fatalf("201 body %s carries no id (%v); the created object is what says which row "+
					"the Location points at", strings.TrimSpace(w.Body.String()), err)
			}
			if got, want := w.Header().Get("Location"), p.location(created.ID); got != want {
				t.Errorf("POST %s answered Location %q, want %q", p.url, got, want)
			}
		})
	}

	registered := map[string]bool{}
	for _, rt := range srv.routes {
		registered[rt.pattern] = true
		method, _, ok := strings.Cut(rt.pattern, " ")
		if !ok || method != http.MethodPost || probed[rt.pattern] {
			continue
		}
		if _, exempt := noLocationRoutes[rt.pattern]; !exempt {
			t.Errorf("%s is registered but is neither probed here nor listed in noLocationRoutes. "+
				"If it creates a row, add a probe; if it does not, add it with the reason.", rt.pattern)
		}
	}
	for pattern := range noLocationRoutes {
		if !registered[pattern] {
			t.Errorf("noLocationRoutes exempts %q, but no route registers that pattern. "+
				"If the route was renamed, move the exemption; if it was removed, delete it.", pattern)
		}
	}
}

// mustCreate posts body and returns the id of what it made, failing the
// test if the request did not answer 201.
func mustCreate(t *testing.T, h http.Handler, cookie *http.Cookie, path, body string) int64 {
	t.Helper()
	w := doReq(t, h, http.MethodPost, path, body, cookie)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST %s = %d %s, want 201", path, w.Code, strings.TrimSpace(w.Body.String()))
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == 0 {
		t.Fatalf("POST %s answered %s, which carries no id: %v", path, w.Body.String(), err)
	}
	return created.ID
}
