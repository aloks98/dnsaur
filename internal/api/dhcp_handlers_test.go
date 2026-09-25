package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aloks98/dnsaur/internal/dhcp"
	"github.com/aloks98/dnsaur/internal/dhcp/keatest"
	"github.com/aloks98/dnsaur/internal/store"
)

// The DHCP handlers are exercised against a real *dhcp.Manager talking to a
// real unix socket (internal/dhcp/keatest), not a stand-in for the manager:
// the two facts these routes turn on — a write is answered 201 even when the
// engine refuses the config it produced, and the lease table is what the
// engine last handed over — are both properties of that seam, and a mock of
// the manager would be free to have either of them the other way round.
//
// The store is the real one too, so the validators of §4.3 and §4.4 run
// against rows that were actually written.

// keaFake is a fake kea-dhcp4 on a unix socket: it answers the five commands
// the manager sends, records every config-set it was given, and can be told
// to refuse the next one the way an engine missing a hook library does.
type keaFake struct {
	srv *keatest.Server

	mu      sync.Mutex
	leases  []dhcp.Lease
	configs []map[string]any
	refuse  string
}

func newKeaFake(t *testing.T) *keaFake {
	t.Helper()
	f := &keaFake{srv: keatest.NewServer(t)}
	f.srv.Handle("version-get", func(json.RawMessage) dhcp.Response {
		return dhcp.Response{Text: "2.6.3"}
	})
	f.srv.Handle("config-get", func(json.RawMessage) dhcp.Response {
		return dhcp.Response{Arguments: json.RawMessage(
			`{"Dhcp4":{"hooks-libraries":[{"library":"/usr/lib/kea/hooks/libdhcp_lease_cmds.so"}]}}`)}
	})
	f.srv.Handle("config-set", func(args json.RawMessage) dhcp.Response {
		var body struct {
			Dhcp4 map[string]any `json:"Dhcp4"`
		}
		if err := json.Unmarshal(args, &body); err != nil {
			return dhcp.Response{Result: 1, Text: err.Error()}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.refuse != "" {
			return dhcp.Response{Result: 1, Text: f.refuse}
		}
		f.configs = append(f.configs, body.Dhcp4)
		return dhcp.Response{Text: "Configuration successful."}
	})
	f.srv.Handle("lease4-get-page", func(json.RawMessage) dhcp.Response {
		f.mu.Lock()
		defer f.mu.Unlock()
		page, err := json.Marshal(map[string]any{"leases": f.leases})
		if err != nil {
			return dhcp.Response{Result: 1, Text: err.Error()}
		}
		return dhcp.Response{Arguments: page}
	})
	f.srv.Handle("status-get", func(json.RawMessage) dhcp.Response {
		return dhcp.Response{Arguments: json.RawMessage(`{"uptime": 42}`)}
	})
	f.srv.Handle("lease4-del", func(args json.RawMessage) dhcp.Response {
		var body struct {
			IP string `json:"ip-address"`
		}
		_ = json.Unmarshal(args, &body)
		f.mu.Lock()
		defer f.mu.Unlock()
		for i, l := range f.leases {
			if l.IP == body.IP {
				f.leases = append(f.leases[:i], f.leases[i+1:]...)
				return dhcp.Response{Text: "IPv4 lease deleted."}
			}
		}
		return dhcp.Response{Result: 3, Text: "IPv4 lease not found."}
	})
	return f
}

// serve replaces what the next poll will find.
func (f *keaFake) serve(leases ...dhcp.Lease) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leases = leases
}

// sent is every Dhcp4 object the engine was given, in order.
func (f *keaFake) sent() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.configs...)
}

// refuseWith makes every later config-set come back as Kea's own refusal.
func (f *keaFake) refuseWith(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuse = text
}

// storeInputs is the dhcp.Inputs the App implements for real: the scopes and
// reservations as stored, plus the local facts a render needs. The DNS list
// is a literal address so no test here depends on which interfaces the
// machine running it happens to have.
type storeInputs struct {
	st     store.Store
	socket string
}

func (i storeInputs) RenderInput(ctx context.Context) (dhcp.RenderInput, error) {
	scopes, err := i.st.DHCP().Scopes(ctx)
	if err != nil {
		return dhcp.RenderInput{}, err
	}
	reservations, err := i.st.DHCP().Reservations(ctx)
	if err != nil {
		return dhcp.RenderInput{}, err
	}
	classes, err := i.st.DHCP().Classes(ctx)
	if err != nil {
		return dhcp.RenderInput{}, err
	}
	return dhcp.RenderInput{
		Scopes: scopes, Classes: classes, Reservations: reservations,
		Domain: "home.lan", LeaseSeconds: 3600, Socket: i.socket,
		ThisServer: "main-1", DNSServers: []string{"192.168.1.1"},
	}, nil
}

// Rendered is the App's half that publishes the HA pair and records what
// the engine accepted. Nothing here asserts on either, so this is the
// method's whole implementation.
func (i storeInputs) Rendered(context.Context, dhcp.RenderInput, error) {}

// dhcpServer is testServer with an engine behind it: a manager wired to the
// fake, already discovered and rendered once, so a test asserts on what the
// engine was sent rather than on whether it was reachable.
func dhcpServer(t *testing.T) (*Server, store.Store, *keaFake) {
	t.Helper()
	fake := newKeaFake(t)
	var srv *Server
	var st store.Store
	srv, st, _ = testServer(t, func(d *Deps) {
		d.DHCP = dhcp.NewManager(dhcp.NewClient(fake.srv.Socket()),
			storeInputs{st: d.Store, socket: fake.srv.Socket()}, d.Store.Settings(),
			slog.New(slog.NewTextHandler(io.Discard, nil)))
	})
	// Start, not just Apply: it is the sequence a real box goes through, and
	// it leaves the manager knowing the engine's version and hook directory
	// — which is what decides the control-socket spelling it renders.
	if err := srv.deps.DHCP.Start(t.Context()); err != nil {
		t.Fatalf("starting the dhcp manager: %v", err)
	}
	return srv, st, fake
}

// mustJSON decodes a response body, failing the test with what was there.
func mustJSON[T any](t *testing.T, w interface{ String() string }) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(w.String()), &v); err != nil {
		t.Fatalf("decoding %s: %v", strings.TrimSpace(w.String()), err)
	}
	return v
}

const probeScope = `{"name":"lan","cidr":"192.168.1.0/24","pools":[{"start":"192.168.1.100",` +
	`"end":"192.168.1.200"}],"gateway":"192.168.1.1","enabled":true}`

// TestDHCPStatusDisabledWithoutASocket is §10's first row: kea_socket empty
// means nothing DHCP runs. The status route still answers — "is DHCP running
// here" is what it is for — and every other route says the resource is not
// there, rather than 503, which would promise it is coming back.
func TestDHCPStatusDisabledWithoutASocket(t *testing.T) {
	srv, s, _ := testServer(t)
	h := srv.Handler()
	cookie := login(t, srv, s)

	w := doReq(t, h, "GET", "/api/v1/dhcp/status", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /dhcp/status with no engine = %d %s, want 200", w.Code, strings.TrimSpace(w.Body.String()))
	}
	status := mustJSON[dhcpStatusView](t, w.Body)
	if status.Enabled || status.Engine != "" || status.Scopes == nil {
		t.Fatalf("status = %+v, want {enabled:false} with an empty scope list and no engine state", status)
	}

	for _, probe := range []struct{ method, url, body string }{
		{"GET", "/api/v1/dhcp/scopes", ""},
		{"POST", "/api/v1/dhcp/scopes", probeScope},
		{"GET", "/api/v1/dhcp/reservations", ""},
		{"GET", "/api/v1/dhcp/leases", ""},
		{"DELETE", "/api/v1/dhcp/leases/192.168.1.50", ""},
		{"POST", "/api/v1/dhcp/leases/192.168.1.50/reserve", ""},
		{"POST", "/api/v1/dhcp/apply", ""},
	} {
		w := doReq(t, h, probe.method, probe.url, probe.body, cookie)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s with no engine = %d %s, want 404", probe.method, probe.url, w.Code,
				strings.TrimSpace(w.Body.String()))
			continue
		}
		if got, want := errorOf(t, w), "dhcp is not enabled"; got != want {
			t.Errorf("%s %s error = %q, want %q", probe.method, probe.url, got, want)
		}
	}
	// Nothing was written on the way to those 404s.
	if scopes, err := s.DHCP().Scopes(t.Context()); err != nil || len(scopes) != 0 {
		t.Fatalf("the store holds %d scopes (err %v) after every route answered 404", len(scopes), err)
	}

	// And the warning strip reads the same fact off the one round trip it
	// already makes.
	w = doReq(t, h, "GET", "/api/v1/resolver/status", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /resolver/status: %d", w.Code)
	}
	if rs := mustJSON[resolverStatus](t, w.Body); rs.DHCP.Enabled {
		t.Errorf("resolver status reports dhcp enabled on a box with no engine: %+v", rs.DHCP)
	}
}

// TestDHCPScopeWritesReachTheEngine: every write to synced DHCP config is
// followed by one config-set, which is §5.1's "on every change". Without it
// an operator edits a pool, sees it saved, and the engine goes on handing out
// of the old one until something unrelated re-renders.
func TestDHCPScopeWritesReachTheEngine(t *testing.T) {
	srv, s, fake := dhcpServer(t)
	h := srv.Handler()
	cookie := login(t, srv, s)

	renders := len(fake.sent())
	id := mustCreate(t, h, cookie, "/api/v1/dhcp/scopes", probeScope)
	sent := fake.sent()
	if len(sent) != renders+1 {
		t.Fatalf("creating a scope sent %d config-sets, want one", len(sent)-renders)
	}
	subnets, _ := sent[len(sent)-1]["subnet4"].([]any)
	if len(subnets) != 1 {
		t.Fatalf("the engine was sent %d subnets after one scope was created: %v", len(subnets), sent[len(sent)-1])
	}

	// The edit, and the disable. Both are merges: the body names one field.
	if w := doReq(t, h, "PATCH", resourceURL("dhcp/scopes", id),
		`{"pools":[{"start":"192.168.1.100","end":"192.168.1.150"}]}`, cookie); w.Code != http.StatusNoContent {
		t.Fatalf("PATCH pools = %d %s, want 204", w.Code, strings.TrimSpace(w.Body.String()))
	}
	scope, err := s.DHCP().Scope(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(scope.Pools) != 1 || scope.Pools[0].End != "192.168.1.150" || scope.Gateway != "192.168.1.1" || scope.Name != "lan" {
		t.Fatalf("after a one-field patch the scope is %+v; a merge leaves every other field alone", scope)
	}
	if w := doReq(t, h, "DELETE", resourceURL("dhcp/scopes", id), "", cookie); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE scope = %d %s, want 204", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if len(fake.sent()) != renders+3 {
		t.Fatalf("create, patch and delete sent %d config-sets, want three", len(fake.sent())-renders)
	}
	last := fake.sent()[len(fake.sent())-1]
	if subnets, _ := last["subnet4"].([]any); len(subnets) != 0 {
		t.Fatalf("the engine still holds %d subnets after the scope was deleted", len(subnets))
	}
}

// TestDHCPScopeRefusals pins the three answers §4.3's rules get: the
// validator's own message with 400, the overlap sentinel with 400 (it is a
// rule about a set of rows, not a failure), and the unique name with 409.
func TestDHCPScopeRefusals(t *testing.T) {
	srv, s, _ := dhcpServer(t)
	h := srv.Handler()
	cookie := login(t, srv, s)
	mustCreate(t, h, cookie, "/api/v1/dhcp/scopes", probeScope)

	for _, tc := range []struct {
		name, body string
		code       int
		contains   string
	}{
		{"host bits set", `{"name":"a","cidr":"192.168.9.5/24","pools":[{"start":"192.168.9.10","end":"192.168.9.20"}]}`,
			http.StatusBadRequest, "host bits set"},
		{"pool outside the subnet", `{"name":"b","cidr":"10.9.0.0/24","pools":[{"start":"10.9.1.10","end":"10.9.1.20"}]}`,
			http.StatusBadRequest, "not inside"},
		{"lease below the floor", `{"name":"c","cidr":"10.8.0.0/24","pools":[{"start":"10.8.0.10","end":"10.8.0.20"}],"lease_seconds":60}`,
			http.StatusBadRequest, "floor"},
		{"overlaps an enabled scope", `{"name":"d","cidr":"192.168.1.0/25","pools":[{"start":"192.168.1.10","end":"192.168.1.20"}],"enabled":true}`,
			http.StatusBadRequest, "overlaps an enabled scope"},
		{"duplicate name", `{"name":"lan","cidr":"10.7.0.0/24","pools":[{"start":"10.7.0.10","end":"10.7.0.20"}]}`,
			http.StatusConflict, "already exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doReq(t, h, "POST", "/api/v1/dhcp/scopes", tc.body, cookie)
			if w.Code != tc.code {
				t.Fatalf("= %d %s, want %d", w.Code, strings.TrimSpace(w.Body.String()), tc.code)
			}
			if got := errorOf(t, w); !strings.Contains(got, tc.contains) {
				t.Errorf("error = %q, want it to name %q", got, tc.contains)
			}
		})
	}
}

// TestDHCPScopeRefusesUnknownFields: a body's key is either a column or a
// typo, and a typo that is accepted is a pool an operator believes they set.
// store.Scope's own UnmarshalJSON is what makes DisallowUnknownFields inert
// — encoding/json hands the whole object to it and never looks at the keys —
// so both handlers decode through an alias that drops the method first.
func TestDHCPScopeRefusesUnknownFields(t *testing.T) {
	srv, s, _ := dhcpServer(t)
	h := srv.Handler()
	cookie := login(t, srv, s)

	// Valid in every other respect, so the only thing that can produce a 400
	// is the key nobody knows — a body the validator would refuse anyway
	// would pass this test with the strictness gone.
	w := doReq(t, h, "POST", "/api/v1/dhcp/scopes",
		`{"name":"typo","cidr":"10.3.0.0/24","pools":[{"start":"10.3.0.10","end":"10.3.0.20"}],"poolstart":"10.3.0.11"}`, cookie)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a misspelled key = %d %s, want 400", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if scopes, err := s.DHCP().Scopes(t.Context()); err != nil || len(scopes) != 0 {
		t.Fatalf("the store holds %d scopes (err %v) after a refused body", len(scopes), err)
	}

	id := mustCreate(t, h, cookie, "/api/v1/dhcp/scopes", probeScope)
	if w := doReq(t, h, "PATCH", resourceURL("dhcp/scopes", id), `{"poolstart":"10.3.0.10"}`, cookie); w.Code != http.StatusBadRequest {
		t.Fatalf("a misspelled key on a patch = %d %s, want 400", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if sc, err := s.DHCP().Scope(t.Context(), id); err != nil || len(sc.Pools) != 1 || sc.Pools[0].Start != "192.168.1.100" {
		t.Fatalf("the scope's pools are %+v (err %v) after a refused patch", sc.Pools, err)
	}
	// A reservation body is decoded strictly too — it has no unmarshaler of
	// its own, so this is the ordinary path, and it must stay that way.
	if w := doReq(t, h, "POST", "/api/v1/dhcp/reservations",
		`{"scope_id":`+strconv.FormatInt(id, 10)+`,"mac":"aa:bb:cc:dd:ee:01","ip":"192.168.1.50","ipaddr":"192.168.1.51"}`,
		cookie); w.Code != http.StatusBadRequest {
		t.Fatalf("a misspelled reservation key = %d %s, want 400", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if all, err := s.DHCP().Reservations(t.Context()); err != nil || len(all) != 0 {
		t.Fatalf("the store holds %d reservations (err %v) after a refused body", len(all), err)
	}
}

// TestDHCPScopePatchKeepsMatchClientID: match_client_id is the one field
// whose column default (true) and Go zero value (false) disagree, so the
// store's decode reads an absent key as true. That is right for a create and
// wrong for a merge — a scope of cloned VMs with client-id matching
// deliberately off would have it turned back on by the row's enable toggle,
// with nothing anywhere saying so.
func TestDHCPScopePatchKeepsMatchClientID(t *testing.T) {
	srv, s, _ := dhcpServer(t)
	h := srv.Handler()
	cookie := login(t, srv, s)

	id := mustCreate(t, h, cookie, "/api/v1/dhcp/scopes",
		`{"name":"vms","cidr":"10.6.0.0/24","pools":[{"start":"10.6.0.10","end":"10.6.0.20"}],"enabled":true,"match_client_id":false}`)
	if sc, err := s.DHCP().Scope(t.Context(), id); err != nil || sc.MatchClientID {
		t.Fatalf("created scope has match_client_id %v (err %v), want the false that was sent", sc.MatchClientID, err)
	}
	if w := doReq(t, h, "PATCH", resourceURL("dhcp/scopes", id), `{"enabled":false}`, cookie); w.Code != http.StatusNoContent {
		t.Fatalf("PATCH enabled = %d %s, want 204", w.Code, strings.TrimSpace(w.Body.String()))
	}
	sc, err := s.DHCP().Scope(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if sc.MatchClientID {
		t.Error("a PATCH that never mentioned match_client_id turned it back on")
	}
	if sc.Enabled {
		t.Error("the field the PATCH did name was not written")
	}
	// And a PATCH that does name it is still obeyed, in both directions.
	if w := doReq(t, h, "PATCH", resourceURL("dhcp/scopes", id), `{"match_client_id":true}`, cookie); w.Code != http.StatusNoContent {
		t.Fatalf("PATCH match_client_id = %d %s, want 204", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if sc, err := s.DHCP().Scope(t.Context(), id); err != nil || !sc.MatchClientID {
		t.Fatalf("match_client_id = %v (err %v) after a patch that asked for true", sc.MatchClientID, err)
	}
}

// TestDHCPScopePatchRefusesAStrandedReservation: renumbering a scope is what
// puts its own reservations outside it, and the renderer refuses the whole
// configuration for one of them — every other scope included, behind a
// message about an address nobody can find. The edit is refused instead,
// naming the reservation.
func TestDHCPScopePatchRefusesAStrandedReservation(t *testing.T) {
	srv, s, _ := dhcpServer(t)
	h := srv.Handler()
	cookie := login(t, srv, s)

	id := mustCreate(t, h, cookie, "/api/v1/dhcp/scopes", probeScope)
	mustCreate(t, h, cookie, "/api/v1/dhcp/reservations",
		`{"scope_id":`+strconv.FormatInt(id, 10)+`,"mac":"aa:bb:cc:dd:ee:01","ip":"192.168.1.50","hostname":"nas"}`)

	w := doReq(t, h, "PATCH", resourceURL("dhcp/scopes", id),
		`{"cidr":"10.5.0.0/24","pools":[{"start":"10.5.0.100","end":"10.5.0.200"}],"gateway":"10.5.0.1"}`, cookie)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("renumbering out from under a reservation = %d %s, want 400", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if got := errorOf(t, w); !strings.Contains(got, "192.168.1.50") || !strings.Contains(got, "stranded") {
		t.Errorf("error = %q, want it to name the reservation that would be stranded", got)
	}
	if sc, err := s.DHCP().Scope(t.Context(), id); err != nil || sc.CIDR != "192.168.1.0/24" {
		t.Fatalf("the scope is %q (err %v) after a refused patch; nothing may have been written", sc.CIDR, err)
	}

	// The gateway moving onto a reserved address is the same rule from the
	// other side, and it is refused for the same reason.
	w = doReq(t, h, "PATCH", resourceURL("dhcp/scopes", id), `{"gateway":"192.168.1.50"}`, cookie)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("moving the gateway onto a reserved address = %d %s, want 400", w.Code, strings.TrimSpace(w.Body.String()))
	}

	// An edit that touches neither is not held up by any of this.
	if w := doReq(t, h, "PATCH", resourceURL("dhcp/scopes", id), `{"domain":"lab.lan"}`, cookie); w.Code != http.StatusNoContent {
		t.Fatalf("PATCH domain = %d %s, want 204", w.Code, strings.TrimSpace(w.Body.String()))
	}
}

// TestDHCPReservationRules pins §4.4's answers: a parent that is not there is
// 404, a rule about this row is 400 with the validator's words, and a
// collision with another row in the same scope is 409.
func TestDHCPReservationRules(t *testing.T) {
	srv, s, _ := dhcpServer(t)
	h := srv.Handler()
	cookie := login(t, srv, s)
	id := mustCreate(t, h, cookie, "/api/v1/dhcp/scopes", probeScope)
	scope := strconv.FormatInt(id, 10)

	mustCreate(t, h, cookie, "/api/v1/dhcp/reservations",
		`{"scope_id":`+scope+`,"mac":"AA-BB-CC-DD-EE-01","ip":"192.168.1.50","hostname":"nas"}`)
	// Stored canonically whatever notation was pasted in, which is what
	// makes the unique index mean "this NIC".
	all, err := s.DHCP().Reservations(t.Context())
	if err != nil || len(all) != 1 || all[0].MAC != "aa:bb:cc:dd:ee:01" {
		t.Fatalf("stored %+v (err %v), want one reservation with a canonical mac", all, err)
	}

	for _, tc := range []struct {
		name, body string
		code       int
		contains   string
	}{
		{"no such scope", `{"scope_id":9999,"mac":"aa:bb:cc:dd:ee:02","ip":"192.168.1.51"}`,
			http.StatusNotFound, "not found"},
		{"not a mac", `{"scope_id":` + scope + `,"mac":"nope","ip":"192.168.1.51"}`,
			http.StatusBadRequest, "is not a MAC address"},
		{"outside the scope", `{"scope_id":` + scope + `,"mac":"aa:bb:cc:dd:ee:02","ip":"10.4.0.5"}`,
			http.StatusBadRequest, "outside the scope"},
		{"the gateway", `{"scope_id":` + scope + `,"mac":"aa:bb:cc:dd:ee:02","ip":"192.168.1.1"}`,
			http.StatusBadRequest, "gateway"},
		{"mac already reserved", `{"scope_id":` + scope + `,"mac":"aa:bb:cc:dd:ee:01","ip":"192.168.1.52"}`,
			http.StatusConflict, "reserved in this scope"},
		{"address already reserved", `{"scope_id":` + scope + `,"mac":"aa:bb:cc:dd:ee:03","ip":"192.168.1.50"}`,
			http.StatusConflict, "reserved in this scope"},
		{"hostname already used", `{"scope_id":` + scope + `,"mac":"aa:bb:cc:dd:ee:04","ip":"192.168.1.53","hostname":"NAS"}`,
			http.StatusConflict, "used in this scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doReq(t, h, "POST", "/api/v1/dhcp/reservations", tc.body, cookie)
			if w.Code != tc.code {
				t.Fatalf("= %d %s, want %d", w.Code, strings.TrimSpace(w.Body.String()), tc.code)
			}
			if got := errorOf(t, w); !strings.Contains(got, tc.contains) {
				t.Errorf("error = %q, want it to name %q", got, tc.contains)
			}
		})
	}

	// A merge on the row itself, and scope_id is not the caller's to move.
	rid := all[0].ID
	if w := doReq(t, h, "PATCH", resourceURL("dhcp/reservations", rid),
		`{"hostname":"vault","scope_id":9999}`, cookie); w.Code != http.StatusNoContent {
		t.Fatalf("PATCH reservation = %d %s, want 204", w.Code, strings.TrimSpace(w.Body.String()))
	}
	all, err = s.DHCP().Reservations(t.Context())
	if err != nil || len(all) != 1 {
		t.Fatal(err)
	}
	if got := all[0]; got.Hostname != "vault" || got.ScopeID != id || got.IP != "192.168.1.50" {
		t.Fatalf("after the patch the reservation is %+v; scope_id and ip must be untouched", got)
	}
	if w := doReq(t, h, "DELETE", resourceURL("dhcp/reservations", rid), "", cookie); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE reservation = %d %s, want 204", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if w := doReq(t, h, "PATCH", resourceURL("dhcp/reservations", rid), `{"hostname":"gone"}`, cookie); w.Code != http.StatusNotFound {
		t.Fatalf("PATCH of a deleted reservation = %d, want 404", w.Code)
	}
}

// TestReserveFromALease is the Leases page's per-row Reserve: the row is
// already on screen, so the button sends no body and the reservation is
// built from the lease table entry.
func TestReserveFromALease(t *testing.T) {
	srv, s, fake := dhcpServer(t)
	h := srv.Handler()
	cookie := login(t, srv, s)
	id := mustCreate(t, h, cookie, "/api/v1/dhcp/scopes", probeScope)

	fake.serve(dhcp.Lease{
		IP: "192.168.1.120", MAC: "aa:bb:cc:dd:ee:07", Hostname: "Anna's iPad",
		SubnetID: id, CLTT: 4_000_000_000, ValidLft: 3600,
	})
	if err := srv.deps.DHCP.Poll(t.Context()); err != nil {
		t.Fatalf("polling: %v", err)
	}

	// The table is what the page shows.
	w := doReq(t, h, "GET", "/api/v1/dhcp/leases", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /dhcp/leases = %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	rows := mustJSON[[]leaseRow](t, w.Body)
	if len(rows) != 1 || rows[0].IP != "192.168.1.120" || rows[0].Reserved || rows[0].ExpiresAt == 0 {
		t.Fatalf("lease rows = %+v, want one unreserved lease with an expiry", rows)
	}

	w = doReq(t, h, "POST", "/api/v1/dhcp/leases/192.168.1.120/reserve", "", cookie)
	if w.Code != http.StatusCreated {
		t.Fatalf("reserve = %d %s, want 201", w.Code, strings.TrimSpace(w.Body.String()))
	}
	res := mustJSON[store.Reservation](t, w.Body)
	if res.ScopeID != id || res.MAC != "aa:bb:cc:dd:ee:07" || res.IP != "192.168.1.120" {
		t.Fatalf("reserved %+v, want the lease's own scope, mac and address", res)
	}
	// The client's own name, as an RFC 1123 label — the validator refuses
	// anything else, and the operator typed none of it.
	if res.Hostname != "anna-s-ipad" {
		t.Errorf("hostname = %q, want the sanitised label the DNS stage would publish", res.Hostname)
	}
	if got := w.Header().Get("Location"); got != resourceURL("dhcp/reservations", res.ID) {
		t.Errorf("Location = %q, want the new reservation's own URL", got)
	}

	// It reaches the engine, and the row is marked reserved on the next
	// poll — which is what the page's "reserved" marker reads.
	sent := fake.sent()
	subnets, _ := sent[len(sent)-1]["subnet4"].([]any)
	subnet, _ := subnets[0].(map[string]any)
	if reservations, _ := subnet["reservations"].([]any); len(reservations) != 1 {
		t.Fatalf("the rendered subnet carries %d reservations after Reserve: %v", len(reservations), subnet)
	}
	if err := srv.deps.DHCP.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows = mustJSON[[]leaseRow](t, doReq(t, h, "GET", "/api/v1/dhcp/leases", "", cookie).Body)
	if len(rows) != 1 || !rows[0].Reserved {
		t.Fatalf("lease rows = %+v, want the one row marked reserved", rows)
	}

	// Reserving it twice is the same MAC pinned twice in one scope.
	if w := doReq(t, h, "POST", "/api/v1/dhcp/leases/192.168.1.120/reserve", "", cookie); w.Code != http.StatusConflict {
		t.Fatalf("reserving the same lease twice = %d %s, want 409", w.Code, strings.TrimSpace(w.Body.String()))
	}
	// And an address nothing has leased is not a row to reserve.
	if w := doReq(t, h, "POST", "/api/v1/dhcp/leases/192.168.1.199/reserve", "", cookie); w.Code != http.StatusNotFound {
		t.Fatalf("reserving an address with no lease = %d, want 404", w.Code)
	}
	if w := doReq(t, h, "POST", "/api/v1/dhcp/leases/not-an-address/reserve", "", cookie); w.Code != http.StatusBadRequest {
		t.Fatalf("reserving a path that is not an address = %d, want 400", w.Code)
	}
}

// TestDHCPStatusCarriesTheHAPeer: the Scopes page's status line is "Engine
// <version> · hot-standby with <peer> · <state>", and the peer is the name
// the engine that is talking to the partner reports for it — not a name
// dnsaur re-derives from the pair it rendered, which would say who it meant
// to be talking to.
func TestDHCPStatusCarriesTheHAPeer(t *testing.T) {
	srv, s, fake := dhcpServer(t)
	h := srv.Handler()
	cookie := login(t, srv, s)

	fake.srv.Handle("status-get", func(json.RawMessage) dhcp.Response {
		return dhcp.Response{Arguments: json.RawMessage(`{"uptime":87,"high-availability":[{
			"ha-mode":"hot-standby",
			"ha-servers":{"local":{"state":"hot-standby"},
			 "remote":{"server-name":"backup","last-state":"hot-standby",
			           "communication-interrupted":true,"unacked-clients":2}}}]}`)}
	})
	if err := srv.deps.DHCP.Poll(t.Context()); err != nil {
		t.Fatalf("polling: %v", err)
	}

	status := mustJSON[dhcpStatusView](t, doReq(t, h, "GET", "/api/v1/dhcp/status", "", cookie).Body)
	if status.HA == nil {
		t.Fatal("status carries no ha block")
	}
	want := dhcpHAView{
		Mode: "hot-standby", LocalState: "hot-standby", Peer: "backup",
		RemoteState: "hot-standby", CommunicationInterrupted: true, UnackedClients: 2,
	}
	if *status.HA != want {
		t.Fatalf("ha = %+v, want %+v", *status.HA, want)
	}
	// A single box has no ha block at all, which is a different fact from a
	// pair that is not talking.
	if !strings.Contains(doReq(t, h, "GET", "/api/v1/dhcp/status", "", cookie).Body.String(), `"peer":"backup"`) {
		t.Error("the ha peer is not on the wire under the name the dashboard reads")
	}
}

// TestReleaseALease: the release goes to the engine, not to the store, which
// is why it is the one DHCP write a replica may make.
func TestReleaseALease(t *testing.T) {
	srv, s, fake := dhcpServer(t)
	h := srv.Handler()
	cookie := login(t, srv, s)
	id := mustCreate(t, h, cookie, "/api/v1/dhcp/scopes", probeScope)

	fake.serve(dhcp.Lease{IP: "192.168.1.120", MAC: "aa:bb:cc:dd:ee:07", SubnetID: id, CLTT: 4_000_000_000, ValidLft: 3600})
	if err := srv.deps.DHCP.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if w := doReq(t, h, "DELETE", "/api/v1/dhcp/leases/192.168.1.120", "", cookie); w.Code != http.StatusNoContent {
		t.Fatalf("release = %d %s, want 204", w.Code, strings.TrimSpace(w.Body.String()))
	}
	// Gone from the table at once, not at the next poll: the page reads the
	// table, and a row that outlives its own Release by an interval reads as
	// a button that did nothing.
	if rows := mustJSON[[]leaseRow](t, doReq(t, h, "GET", "/api/v1/dhcp/leases", "", cookie).Body); len(rows) != 0 {
		t.Fatalf("the table still holds %+v immediately after the release", rows)
	}
	// And the engine agrees, which the next poll is what shows.
	if err := srv.deps.DHCP.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if rows := mustJSON[[]leaseRow](t, doReq(t, h, "GET", "/api/v1/dhcp/leases", "", cookie).Body); len(rows) != 0 {
		t.Fatalf("the table still holds %+v after the release", rows)
	}
	// A lease the engine does not hold is its refusal, in its own words.
	w := doReq(t, h, "DELETE", "/api/v1/dhcp/leases/192.168.1.120", "", cookie)
	if w.Code != http.StatusNotFound {
		t.Fatalf("releasing a lease the engine does not hold = %d %s, want 404", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if got := errorOf(t, w); !strings.Contains(got, "not found") {
		t.Errorf("error = %q, want the engine's own words", got)
	}
	if w := doReq(t, h, "DELETE", "/api/v1/dhcp/leases/nope", "", cookie); w.Code != http.StatusBadRequest {
		t.Fatalf("releasing a path that is not an address = %d, want 400", w.Code)
	}
}

// TestAWriteIsSavedEvenWhenTheEngineRefuses is §5.1's rule, and the reason
// the render is never the write's answer: a config Kea will not take leaves
// the row stored and the engine on its previous configuration. Answering the
// caller 5xx would say the scope was not saved, which is false — and the
// operator would have no row to fix.
func TestAWriteIsSavedEvenWhenTheEngineRefuses(t *testing.T) {
	srv, s, fake := dhcpServer(t)
	h := srv.Handler()
	cookie := login(t, srv, s)

	fake.refuseWith("missing hook library libdhcp_lease_cmds.so")
	id := mustCreate(t, h, cookie, "/api/v1/dhcp/scopes", probeScope)
	if _, err := s.DHCP().Scope(t.Context(), id); err != nil {
		t.Fatalf("the scope was not stored: %v", err)
	}

	// The refusal is in the status, in Kea's own words, and stands until a
	// render is accepted.
	status := mustJSON[dhcpStatusView](t, doReq(t, h, "GET", "/api/v1/dhcp/status", "", cookie).Body)
	if status.Engine != "config rejected" || !strings.Contains(status.Message, "libdhcp_lease_cmds") {
		t.Fatalf("status = %+v, want the engine's refusal", status)
	}
	if len(status.Scopes) != 1 || status.Scopes[0].ID != id || status.Scopes[0].PoolSize != 101 {
		t.Fatalf("status scopes = %+v, want the one scope with its pool size", status.Scopes)
	}

	// "Apply again" is what re-sends it once the operator has fixed the box,
	// and it answers with the state that render left behind rather than the
	// one before it.
	fake.refuseWith("")
	w := doReq(t, h, "POST", "/api/v1/dhcp/apply", "", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("apply = %d %s, want 200", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if status := mustJSON[dhcpStatusView](t, w.Body); status.Engine != "ok" || status.Message != "" {
		t.Fatalf("apply answered %+v, want an engine that took the configuration", status)
	}
}

// TestDHCPClassRoutes is §5's class half: CRUD that re-renders like a scope
// write, a pool naming a class that is not there refused as 422 naming the
// pool, and a class a pool still names kept, with the store's own words.
func TestDHCPClassRoutes(t *testing.T) {
	srv, s, fake := dhcpServer(t)
	h := srv.Handler()
	cookie := login(t, srv, s)

	renders := len(fake.sent())
	w := doReq(t, h, "POST", "/api/v1/dhcp/classes", `{"name":"iot","matchers":["mac:a4:cf:12"],"domain":"iot.lan"}`, cookie)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST class = %d %s, want 201", w.Code, strings.TrimSpace(w.Body.String()))
	}
	cls := mustJSON[store.Class](t, w.Body)
	if got := w.Header().Get("Location"); got != resourceURL("dhcp/classes", cls.ID) {
		t.Errorf("Location = %q", got)
	}
	if len(fake.sent()) != renders+1 {
		t.Fatalf("creating a class sent %d config-sets, want one", len(fake.sent())-renders)
	}
	cid := strconv.FormatInt(cls.ID, 10)

	if list := mustJSON[[]store.Class](t, doReq(t, h, "GET", "/api/v1/dhcp/classes", "", cookie).Body); len(list) != 1 || list[0].Name != "iot" {
		t.Fatalf("GET classes = %+v", list)
	}
	if w := doReq(t, h, "PATCH", resourceURL("dhcp/classes", cls.ID), `{"matchers":["mac:a4:cf:12","vendor:ESP"]}`, cookie); w.Code != http.StatusNoContent {
		t.Fatalf("PATCH matchers = %d %s, want 204", w.Code, strings.TrimSpace(w.Body.String()))
	}
	got := mustJSON[store.Class](t, doReq(t, h, "GET", resourceURL("dhcp/classes", cls.ID), "", cookie).Body)
	if len(got.Matchers) != 2 || got.Domain != "iot.lan" || got.Name != "iot" {
		t.Fatalf("after a matchers patch the class is %+v; a merge leaves the rest alone", got)
	}

	for _, tc := range []struct {
		name, method, url, body string
		code                    int
		want                    string
	}{
		{"duplicate name", "POST", "/api/v1/dhcp/classes", `{"name":"IoT","matchers":["mac:aa"]}`,
			http.StatusConflict, `a class named "iot" exists`},
		{"bad matcher", "POST", "/api/v1/dhcp/classes", `{"name":"x","matchers":["host:x"]}`, http.StatusBadRequest, "matchers[0]"},
		{"unknown key", "POST", "/api/v1/dhcp/classes", `{"name":"x","matchers":["mac:aa"],"matcher":"mac:bb"}`,
			http.StatusBadRequest, "invalid json"},
		{"unknown class", "POST", "/api/v1/dhcp/scopes", `{"name":"lan","cidr":"192.168.1.0/24","pools":[` +
			`{"start":"192.168.1.10","end":"192.168.1.20"},{"start":"192.168.1.30","end":"192.168.1.40","class_id":9999}]}`,
			http.StatusUnprocessableEntity, "pools[1]: no class with id 9999"},
		{"old keys on a create", "POST", "/api/v1/dhcp/scopes",
			`{"name":"lan","cidr":"192.168.1.0/24","pool_start":"192.168.1.10","pool_end":"192.168.1.20"}`,
			http.StatusUnprocessableEntity, "pools replaces pool_start and pool_end"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doReq(t, h, tc.method, tc.url, tc.body, cookie)
			if w.Code != tc.code || errorOf(t, w) != tc.want && !strings.Contains(errorOf(t, w), tc.want) {
				t.Fatalf("= %d %s, want %d naming %q", w.Code, strings.TrimSpace(w.Body.String()), tc.code, tc.want)
			}
		})
	}

	// A scope with a pool of the class's own, then the class may not go.
	sid := mustCreate(t, h, cookie, "/api/v1/dhcp/scopes", `{"name":"lan","cidr":"192.168.1.0/24","enabled":true,"pools":[`+
		`{"start":"192.168.1.10","end":"192.168.1.20"},{"start":"192.168.1.30","end":"192.168.1.40","class_id":`+cid+`}]}`)
	if w := doReq(t, h, "PATCH", resourceURL("dhcp/scopes", sid), `{"pool_end":"192.168.1.50"}`, cookie); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("old key on a patch = %d %s, want 422", w.Code, strings.TrimSpace(w.Body.String()))
	}
	last := fake.sent()[len(fake.sent())-1]
	if classes, _ := last["client-classes"].([]any); len(classes) == 0 {
		t.Fatalf("the engine was sent no client-classes after a class and its pool were written: %v", last)
	}
	w = doReq(t, h, "DELETE", resourceURL("dhcp/classes", cls.ID), "", cookie)
	if w.Code != http.StatusConflict || errorOf(t, w) != `class "iot" is in use by 1 pool (lan)` {
		t.Fatalf("DELETE a class in use = %d %s, want 409 with the store's text", w.Code, strings.TrimSpace(w.Body.String()))
	}

	// Pool sizes sum; a reservations_only scope hands out none of its pools.
	mustCreate(t, h, cookie, "/api/v1/dhcp/scopes", `{"name":"pins","cidr":"10.2.0.0/24","enabled":true,`+
		`"reservations_only":true,"pools":[{"start":"10.2.0.10","end":"10.2.0.20"}]}`)
	sizes := map[int64]int{}
	for _, u := range mustJSON[dhcpStatusView](t, doReq(t, h, "GET", "/api/v1/dhcp/status", "", cookie).Body).Scopes {
		sizes[u.ID] = u.PoolSize
	}
	if sizes[sid] != 22 || len(sizes) != 2 {
		t.Fatalf("pool sizes %v, want lan at 11+11 and pins at 0", sizes)
	}
	for id, n := range sizes {
		if id != sid && n != 0 {
			t.Fatalf("a reservations_only scope reports pool_size %d, want 0", n)
		}
	}

	if w := doReq(t, h, "DELETE", resourceURL("dhcp/scopes", sid), "", cookie); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE scope = %d", w.Code)
	}
	if w := doReq(t, h, "DELETE", resourceURL("dhcp/classes", cls.ID), "", cookie); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE an unused class = %d %s, want 204", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if w := doReq(t, h, "GET", resourceURL("dhcp/classes", cls.ID), "", cookie); w.Code != http.StatusNotFound {
		t.Fatalf("GET a deleted class = %d, want 404", w.Code)
	}
}
