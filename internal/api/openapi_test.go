package api

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAPIServedAndCoversRoutes(t *testing.T) {
	srv, _, _ := testServer(t)
	w := doReq(t, srv.Handler(), "GET", "/api/v1/openapi.yaml", "", nil)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "yaml") {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
	var doc struct {
		OpenAPI string         `yaml:"openapi"`
		Paths   map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("invalid yaml: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		t.Fatalf("openapi version: %q", doc.OpenAPI)
	}
	for _, p := range []string{
		"/health", "/setup", "/auth/login", "/auth/logout", "/auth/me",
		"/auth/totp/start", "/auth/totp/confirm", "/auth/totp/disable",
		"/settings", "/blocking", "/blocking/pause",
		"/groups", "/groups/{id}", "/groups/{id}/lists", "/groups/{id}/rules",
		"/clients", "/clients/{id}",
		"/filters/lists", "/filters/lists/{id}", "/filters/rules/{id}", "/filters/refresh",
		"/zones", "/zones/{id}", "/zones/{id}/records", "/zones/{id}/records/{rid}",
		"/queries", "/queries/tail",
		"/stats/overview", "/stats/timeline", "/stats/top",
		"/tokens", "/tokens/{id}",
	} {
		if _, ok := doc.Paths[p]; !ok {
			t.Errorf("openapi missing path %s", p)
		}
	}
}
