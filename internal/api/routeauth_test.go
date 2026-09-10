package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// This file holds two tests that share one mechanism: both drive
// Server.routes — the registry every endpoint is recorded in by route()
// (see server.go) — instead of a hand-maintained list of endpoints. A list
// that has to be updated by hand to stay correct is a list that silently
// stops being correct; openapi_test.go exists because the previous version
// of that check compared two hardcoded lists and so could not fail. The
// same reasoning applies here, doubly so for auth: coverage has to come
// from registration, so that a route added tomorrow is checked the moment
// it is registered rather than when somebody remembers to write a test
// for it.
//
// They also share unauthenticatedRoutes below, which is the single place
// either test is allowed to say "this endpoint answers without
// credentials".

// unauthenticatedRoutes is the allowlist of registered patterns that
// deliberately answer without credentials, mapped to the reason each one
// is exempt.
//
// Deliberately keyed by the exact, whole pattern string rather than by a
// prefix or a regexp. An exemption expressed as a pattern ("anything under
// /api/v1/auth/") grows on its own: the next route added under that prefix
// inherits the exemption silently and ships unprotected. An exact key can
// only ever exempt the one endpoint it names, and adding an entry is a
// visible, reviewable diff that has to state a reason.
//
// The exemptions are checked in both directions. TestEveryRouteEnforcesAuth
// fails if an entry here names a route that is not registered (a stale
// exemption left behind by a rename would otherwise sit here silently
// covering nothing — or worse, be re-used later by an unrelated route with
// the same name), and it also asserts that each exempt route really is
// reachable unauthenticated. So an entry is a claim that gets tested, not
// a way to opt out of testing.
var unauthenticatedRoutes = map[string]string{
	"GET /api/v1/health": "liveness probe: container/monitoring healthchecks have no credentials to " +
		"present, and it discloses only that the process is up and its version.",

	"GET /api/v1/readyz": "readiness probe, the counterpart of the liveness one above: a load " +
		"balancer or container runtime asking whether this instance can serve has no credentials " +
		"to present. It discloses only whether the store answers and whether a DNS socket is " +
		"bound — nothing about what is stored, and nothing an unauthenticated caller could not " +
		"learn by sending one query.",

	"GET /api/v1/openapi.yaml": "serves this API's own description, which is public documentation " +
		"rather than user data; the dashboard also fetches it before anyone has logged in.",

	"POST /api/v1/auth/login": "mints the session in the first place. Requiring a session to log in " +
		"is circular. It authenticates by credentials in the body and answers 401 on bad ones.",

	"GET /api/v1/setup": "first-run state. Reports only whether an admin exists yet; the dashboard " +
		"must be able to ask before any credential can exist.",

	"POST /api/v1/setup": "first-run admin creation. Cannot require a credential because it is what " +
		"creates the first one. Self-guarding rather than unprotected: once an admin exists it " +
		"refuses with 409 (auth.ErrSetupDone), so it is only usable on a virgin install.",

	"/api/": "not an endpoint. The method-less catch-all that turns any unmatched /api/... request " +
		"into a JSON 404 instead of letting it fall through to the SPA mount. It reaches no data " +
		"and answers 404 to everything, authenticated or not. openapi_test.go excludes it from the " +
		"spec comparison for the same reason.",
}

// knownRouteParams are the path-parameter names the probe URL builder knows
// how to fill in. Anything else is a hard failure rather than a guess: a
// parameter substituted with a value that does not route (or worse, that
// routes somewhere else) turns these tests into assertions about the
// catch-all's 404 instead of about the route under test.
var knownRouteParams = map[string]bool{
	"id":  true,
	"rid": true,
}

// catchAllProbePath is the concrete URL used to exercise the method-less
// "/api/" catch-all, which has no path of its own to substitute into.
const catchAllProbePath = "/api/v1/_route_auth_probe_unmatched"

// probeURL turns a registered pattern into a concrete request, filling every
// path parameter with value. It returns the method and URL.
//
// The caller must additionally confirm the URL actually resolves to the
// pattern it came from (see assertResolvesTo). Substituting a parameter is
// the one step here that can silently aim a request somewhere other than
// the route under test, and "somewhere other than the route under test" is
// overwhelmingly the catch-all, which answers 404 — a status these tests
// must never accept in place of a 401.
func probeURL(t *testing.T, pattern, value string) (method, url string) {
	t.Helper()
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		// The only method-less pattern is the "/api/" catch-all; it is in
		// unauthenticatedRoutes, and the caller checks that. Give it a URL
		// that matches nothing more specific, which is the only thing it
		// ever serves.
		if pattern != "/api/" {
			t.Fatalf("route pattern %q has no method prefix and is not the known /api/ catch-all", pattern)
		}
		return http.MethodGet, catchAllProbePath
	}
	segs := strings.Split(path, "/") // leading "" keeps the leading slash on rejoin
	for i, seg := range segs {
		if !strings.HasPrefix(seg, "{") || !strings.HasSuffix(seg, "}") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}")
		if !knownRouteParams[name] {
			t.Fatalf("route %q has path parameter {%s}, which probeURL has no value for. "+
				"Add it to knownRouteParams with a value that routes to this pattern — do not "+
				"let it be guessed: a request that misses the route answers 404, and a 404 must "+
				"never be mistaken for the 401 these tests are asserting.", pattern, name)
		}
		segs[i] = value
	}
	return method, strings.Join(segs, "/")
}

// assertResolvesTo proves that url is served by want and nothing else.
//
// This is what makes "a 404 is not a 401" a checked property rather than a
// hope. http.ServeMux.Handler reports the pattern it matched, so a probe
// URL that missed its route (a bad parameter substitution, a pattern that
// changed shape) is caught here, at the point of the mistake, instead of
// surfacing later as a mystery status that happens not to be 401.
func assertResolvesTo(t *testing.T, srv *Server, method, url, want string) {
	t.Helper()
	if srv.mux == nil {
		t.Fatal("srv.mux is nil: call srv.Handler() before resolving patterns")
	}
	_, pattern := srv.mux.Handler(httptest.NewRequest(method, url, nil))
	if pattern != want {
		t.Fatalf("probe %s %s resolves to pattern %q, want %q — the probe is not hitting the route "+
			"under test, so any status it returns says nothing about that route's auth", method, url, pattern, want)
	}
}

// bearerReq issues method+url against h with an optional bearer token.
func bearerReq(h http.Handler, method, url, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestEveryRouteEnforcesAuth asserts, for every route in the registry, that
// the route is actually wired to requireAuth — not that requireAuth's own
// logic is right (TestReadScopeToken and TestAuthRequired pin that), but
// that this particular endpoint is behind it.
//
// That gap was real and was silent: removing requireAuth from
// POST /api/v1/zones/{id}/file, and nothing else, left the whole package
// green. Every handler test authenticates through login()/a token helper
// that always presents working admin credentials, so no existing test ever
// asked a handler what it does when credentials are absent or insufficient.
// A route that lost its middleware in a refactor would have shipped.
//
// For each registered route this asserts the two things requireAuth
// guarantees, both observed through the full Handler stack:
//
//   - no credentials            -> 401
//   - read-scope token, non-GET -> 403
//
// Exactly 401 and exactly 403; a 404 (from a path parameter that names
// nothing) is not evidence of authentication and is not accepted as such.
// The probe URL is separately proven to resolve to the route under test
// (assertResolvesTo), so a status can always be attributed to that route.
//
// GET routes get one extra probe — a write-scope token must not be answered
// 401/403 — which makes the 401 above meaningful: it proves the route
// refuses *because* the credential was missing, rather than being a handler
// that answers 401 unconditionally and would pass a bare "is it 401?" check
// while being broken. Mutating routes need no such probe because their two
// assertions already differ from each other (401 vs 403), which no
// unconditional status can satisfy. Restricting the extra probe to GETs is
// also what keeps this test side-effect free: it never executes a handler
// that writes. POST /api/v1/filters/refresh and POST /api/v1/filters/lists
// spawn background goroutines, and several routes mutate the store, none of
// which belongs in a test about middleware wiring.
func TestEveryRouteEnforcesAuth(t *testing.T) {
	srv, s, _ := testServer(t)
	h := srv.Handler() // also builds srv.mux, which assertResolvesTo needs

	registered := make(map[string]bool, len(srv.routes))
	for _, rt := range srv.routes {
		if registered[rt.pattern] {
			t.Errorf("route %q is registered twice", rt.pattern)
		}
		registered[rt.pattern] = true
	}
	// An exemption that names nothing is a stale exemption. Fail rather than
	// let it sit here looking like considered policy.
	for pattern := range unauthenticatedRoutes {
		if !registered[pattern] {
			t.Errorf("unauthenticatedRoutes exempts %q, but no route registers that pattern. "+
				"If the route was renamed, move the exemption; if it was removed, delete it.", pattern)
		}
	}

	_ = login(t, srv, s) // creates admin user id 1
	_, readTok, err := srv.deps.Auth.CreateAPIToken(context.Background(), 1, "route-auth-read", "read", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeTok, err := srv.deps.Auth.CreateAPIToken(context.Background(), 1, "route-auth-write", "write", 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, rt := range srv.routes {
		t.Run(rt.pattern, func(t *testing.T) {
			method, url := probeURL(t, rt.pattern, "1")
			assertResolvesTo(t, srv, method, url, rt.pattern)

			if reason, exempt := unauthenticatedRoutes[rt.pattern]; exempt {
				// The exemption is itself a claim: this endpoint is supposed
				// to be usable with no credentials. Check it, so that putting
				// a route behind requireAuth without removing its entry here
				// fails instead of quietly disagreeing with this file.
				if w := bearerReq(h, method, url, ""); w.Code == http.StatusUnauthorized {
					t.Fatalf("%s is listed in unauthenticatedRoutes but answers 401 unauthenticated.\n"+
						"Recorded reason: %s\nIf it is now authenticated, remove the exemption.", rt.pattern, reason)
				}
				return
			}

			if w := bearerReq(h, method, url, ""); w.Code != http.StatusUnauthorized {
				t.Errorf("unauthenticated %s %s = %d %s, want 401.\n"+
					"This route is not wired to requireAuth (or is exempt and missing from "+
					"unauthenticatedRoutes). Note a 404 here is not an acceptable substitute: the "+
					"probe URL is proven to resolve to this pattern.",
					method, url, w.Code, strings.TrimSpace(w.Body.String()))
			}

			if method == http.MethodGet || method == http.MethodHead {
				// Read-only method: no 403 to assert. Instead prove the 401
				// above is conditional on the credential rather than
				// unconditional. Safe to actually execute — GET handlers do
				// not write.
				w := bearerReq(h, method, url, writeTok)
				if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
					t.Errorf("%s %s with a write-scope token = %d %s, want anything but 401/403. "+
						"The route rejects a valid credential, so its 401 above proves nothing about auth wiring.",
						method, url, w.Code, strings.TrimSpace(w.Body.String()))
				}
				return
			}

			if w := bearerReq(h, method, url, readTok); w.Code != http.StatusForbidden {
				t.Errorf("%s %s with a read-scope token = %d %s, want 403. "+
					"A read-scoped token must not be able to drive a mutating endpoint; this route "+
					"is not wired to requireAuth.", method, url, w.Code, strings.TrimSpace(w.Body.String()))
			}
		})
	}
}

// specResponses maps "/path" -> "METHOD" -> set of documented status codes,
// read from the served openapi.yaml.
func specResponses(t *testing.T, h http.Handler) map[string]map[string]map[string]bool {
	t.Helper()
	w := bearerReq(h, http.MethodGet, "/api/v1/openapi.yaml", "")
	if w.Code != http.StatusOK {
		t.Fatalf("fetch spec: %d", w.Code)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("invalid yaml: %v", err)
	}
	out := map[string]map[string]map[string]bool{}
	for path, item := range doc.Paths {
		for key, val := range item {
			if !specOperationKeys[key] {
				continue
			}
			op, ok := val.(map[string]any)
			if !ok {
				t.Fatalf("openapi.yaml: %s %s is not a mapping", key, path)
			}
			resp, ok := op["responses"].(map[string]any)
			if !ok {
				t.Fatalf("openapi.yaml: %s %s has no responses mapping", key, path)
			}
			codes := map[string]bool{}
			for code := range resp {
				// Normalise: a status written 200 rather than "200" decodes
				// as an int key, and must not read as a different code.
				codes[fmt.Sprint(code)] = true
			}
			if out[path] == nil {
				out[path] = map[string]map[string]bool{}
			}
			out[path][strings.ToUpper(key)] = codes
		}
	}
	return out
}

// specCodesFor returns the documented status codes for a registered route
// pattern, or fails if the spec does not describe that operation at all.
// (TestOpenAPIServedAndCoversRoutes is what guarantees every route has an
// entry; this only has to not silently pass when one is missing.)
func specCodesFor(t *testing.T, spec map[string]map[string]map[string]bool, pattern string) (string, map[string]bool) {
	t.Helper()
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		t.Fatalf("pattern %q has no method", pattern)
	}
	specPath := strings.TrimPrefix(path, "/api/v1")
	codes, ok := spec[specPath][method]
	if !ok {
		t.Fatalf("openapi.yaml documents no %s operation for %s", method, specPath)
	}
	return method, codes
}

// TestOpenAPIDocumentsAuthStatuses closes half of the gap openapi_test.go
// names in its own doc comment: that test checks paths and methods, and
// explicitly does not check response codes, so the spec can document the
// wrong ones and nothing notices (task #75).
//
// Scope, stated plainly. This asserts the status codes that are *provable*
// for every route without executing its handler — the ones requireAuth
// emits before the handler runs:
//
//   - every authenticated route documents 401
//   - every authenticated non-GET/HEAD route also documents 403
//
// These are not assumed. TestEveryRouteEnforcesAuth, in this file, issues
// exactly those requests against exactly these routes and asserts exactly
// those codes, and both tests read the same registry and the same
// unauthenticatedRoutes allowlist. So this test documents what its
// neighbour proves, and the two cannot drift apart: a route added to the
// allowlist drops out of both at once, and a route that stops emitting 401
// fails the other test.
//
// Not in scope: proving the spec lists every code a handler's own body can
// emit. Doing that properly means either static analysis of every writeErr
// path (disproportionate) or executing each handler across enough inputs to
// reach each of its error branches — which for mutating routes means real
// writes, background refresh goroutines (POST /filters/refresh, POST
// /filters/lists), and network fetches, in a test about documentation. What
// is cheaply reachable that way is covered by
// TestOpenAPIDocumentsObservedGETStatuses below; the rest is left
// unverified rather than faked.
func TestOpenAPIDocumentsAuthStatuses(t *testing.T) {
	srv, _, _ := testServer(t)
	spec := specResponses(t, srv.Handler())

	for _, rt := range srv.routes {
		if _, exempt := unauthenticatedRoutes[rt.pattern]; exempt {
			// requireAuth guarantees nothing about a route it does not wrap.
			// What these endpoints do document (e.g. login's own 401 for bad
			// credentials) is their handler's business, not the middleware's.
			continue
		}
		t.Run(rt.pattern, func(t *testing.T) {
			method, codes := specCodesFor(t, spec, rt.pattern)
			if !codes["401"] {
				t.Errorf("%s is behind requireAuth and answers 401 without credentials "+
					"(TestEveryRouteEnforcesAuth asserts it), but openapi.yaml documents only %v",
					rt.pattern, sortedKeys(codes))
			}
			if method != http.MethodGet && method != http.MethodHead && !codes["403"] {
				t.Errorf("%s is a mutating route behind requireAuth and answers 403 to a read-scope "+
					"token (TestEveryRouteEnforcesAuth asserts it), but openapi.yaml documents only %v",
					rt.pattern, sortedKeys(codes))
			}
		})
	}
}

// TestOpenAPIDocumentsObservedGETStatuses drives every registered GET route
// through a fixed matrix of requests and asserts that every status actually
// observed is documented for that operation.
//
// Deliberately GET-only, and deliberately a superset check in one
// direction. GET handlers do not write, so this can execute them for real
// without the side effects that rule out the same treatment for mutating
// routes (see TestOpenAPIDocumentsAuthStatuses). And it claims only "every
// code seen is documented" — never "every documented code is reachable",
// which would demand driving every handler into every error branch,
// including 503s that need a broken store.
//
// It is not vacuous: the matrix reaches 200, 400, 401, 404 and 503 across
// the current route set, so adding an undocumented status to any GET
// handler, or deleting one of those codes from the spec, fails it.
func TestOpenAPIDocumentsObservedGETStatuses(t *testing.T) {
	srv, s, _ := testServer(t)
	h := srv.Handler()
	spec := specResponses(t, h)

	_ = login(t, srv, s)
	_, readTok, err := srv.deps.Auth.CreateAPIToken(context.Background(), 1, "spec-observe-read", "read", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Path-parameter values chosen to reach distinct branches: "1" is the
	// localhost zone/record seeded by migration (a real 200), "999999"
	// exists nowhere (404), "abc" does not parse as an id (400).
	paramValues := []string{"1", "999999", "abc"}

	for _, rt := range srv.routes {
		method, _, hasMethod := strings.Cut(rt.pattern, " ")
		if !hasMethod || method != http.MethodGet {
			continue
		}
		t.Run(rt.pattern, func(t *testing.T) {
			_, codes := specCodesFor(t, spec, rt.pattern)

			values := paramValues
			if !strings.Contains(rt.pattern, "{") {
				values = values[:1] // no parameter to vary
			}
			type probe struct {
				token, label string
			}
			for _, v := range values {
				method, url := probeURL(t, rt.pattern, v)
				assertResolvesTo(t, srv, method, url, rt.pattern)
				for _, p := range []probe{
					{readTok, "read-scope token"},
					{"", "no credentials"},
				} {
					w := bearerReq(h, method, url, p.token)
					got := fmt.Sprint(w.Code)
					if !codes[got] {
						t.Errorf("%s %s (%s) returned %s, which openapi.yaml does not document for %s "+
							"(documented: %v)\nbody: %s",
							method, url, p.label, got, rt.pattern, sortedKeys(codes),
							strings.TrimSpace(w.Body.String()))
					}
				}
			}
		})
	}
}
