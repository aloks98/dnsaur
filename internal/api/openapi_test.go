package api

import (
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// specOperationKeys are the sibling keys under an openapi.yaml path item
// that are actual HTTP operations — the full set OpenAPI 3.1's Path Item
// Object defines (get, put, post, delete, options, head, patch, trace).
// This codebase doesn't register head/options/trace routes today, but a
// documented operation under one of those keys still has to be caught in
// both directions, the same as any other method; a narrower set would let a
// phantom "head:" (or one of its siblings) pass silently. A path item can
// also carry "parameters" (path-level parameters shared by every operation,
// e.g. {id}), which is not an operation and must not be mistaken for one.
var specOperationKeys = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

// TestOpenAPIServedAndCoversRoutes asserts the served OpenAPI document
// against the routes the server actually registers — not against a second
// hand-maintained list, which can (and did) drift from both the spec and
// the code silently. Server.route() (server.go) records every pattern in
// the "METHOD /api/v1/path" shape http.ServeMux takes, before any mux
// exists to register it on; that's the only source of truth here, since
// http.ServeMux itself exposes no way to enumerate its own patterns, and
// it's also why a stray s.mux.HandleFunc bypassing route() can't silently
// go unnoticed — see the routes field doc in server.go.
//
// Guarantees, in both directions:
//  1. every registered route is documented (an undocumented endpoint fails)
//  2. every documented path is registered (a spec entry for something that
//     doesn't exist fails — this is what would have caught the deleted
//     /records and /records/{id} entries)
//  3. for each path present in both, the registered methods and the
//     documented operations are the same set (a GET in the spec for a
//     route registered as POST fails)
//
// Does not check: response codes, response/request schemas, parameters, or
// security requirements. Those stay hand-maintained and unverified — a spec
// can list the wrong status codes, or the wrong body shape, and this test
// will not notice (see task #75, response-code completeness, for a
// follow-up that would close part of that gap).
//
// Can be bypassed. route() is the only supported way to add an endpoint,
// but nothing stops a deliberate evasion: registering directly on the mux
// inside buildMux's sync.Once, or putting real logic behind the "/api/"
// catch-all this test excludes by name (see the loop below). Both require
// putting a route where nobody would naturally put one — the accidental
// drift this test exists to catch (an endpoint added the normal way and
// never documented) is what it actually guards against, not a determined
// bypass.
func TestOpenAPIServedAndCoversRoutes(t *testing.T) {
	srv, _, _ := testServer(t)
	w := doReq(t, srv.Handler(), "GET", "/api/v1/openapi.yaml", "", nil)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "yaml") {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
	var doc struct {
		OpenAPI string                    `yaml:"openapi"`
		Paths   map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("invalid yaml: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		t.Fatalf("openapi version: %q", doc.OpenAPI)
	}

	// specMethods maps each documented path to the set of HTTP methods it
	// declares an operation for.
	specMethods := map[string]map[string]bool{}
	for path, ops := range doc.Paths {
		methods := map[string]bool{}
		for key := range ops {
			if specOperationKeys[key] {
				methods[strings.ToUpper(key)] = true
			}
		}
		specMethods[path] = methods
	}

	// routeMethods maps each route's path — normalised from the registered
	// "METHOD /api/v1/path" shape down to the spec's "/path" shape, since
	// the document says nothing about a version prefix — to the set of
	// methods actually registered for it.
	routeMethods := map[string]map[string]bool{}
	for _, rt := range srv.routes {
		pattern := rt.pattern
		if pattern == "/api/" {
			// The bare "/api/" catch-all (see registerRoutes in server.go)
			// has no method verb and documents no resource of its own — it
			// only turns an unmatched "/api/..." request into a JSON 404
			// instead of falling through to the SPA mount. There is
			// nothing here for a spec to describe, so it's deliberately
			// excluded rather than forced into this comparison.
			continue
		}
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			t.Fatalf("route pattern %q has no method prefix and is not the known /api/ catch-all", pattern)
		}
		if !strings.HasPrefix(path, "/api/v1/") {
			t.Fatalf("route pattern %q does not start with /api/v1/", pattern)
		}
		path = strings.TrimPrefix(path, "/api/v1")
		if routeMethods[path] == nil {
			routeMethods[path] = map[string]bool{}
		}
		routeMethods[path][method] = true
	}

	allPaths := map[string]bool{}
	for p := range specMethods {
		allPaths[p] = true
	}
	for p := range routeMethods {
		allPaths[p] = true
	}

	for _, path := range sortedKeys(allPaths) {
		spec, documented := specMethods[path]
		route, registered := routeMethods[path]
		switch {
		case registered && !documented:
			t.Errorf("route %s is registered (methods %v) but openapi.yaml has no such path", path, sortedKeys(route))
		case documented && !registered:
			t.Errorf("openapi.yaml documents path %s but no route registers it", path)
		default:
			for m := range route {
				if !spec[m] {
					t.Errorf("%s %s is registered but openapi.yaml does not document that method for %s (documented: %v)", m, path, path, sortedKeys(spec))
				}
			}
			for m := range spec {
				if !route[m] {
					t.Errorf("openapi.yaml documents %s %s but no route registers that method for %s (registered: %v)", m, path, path, sortedKeys(route))
				}
			}
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
