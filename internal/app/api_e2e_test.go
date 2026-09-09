package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/config"
	"github.com/miekg/dns"
)

// testConfig mirrors app_e2e_test.go's inline config literal so both e2e
// files build an identical sqlite-backed, loopback-only App.
func testConfig(dir string) *config.Config {
	cfg := &config.Config{DNSListen: []string{"127.0.0.1:0"}, HTTPListen: ":0", DataDir: dir, LogLevel: "error"}
	cfg.Storage.Driver = "sqlite"
	cfg.Storage.DSN = dir + "/t.db"
	return cfg
}

// digA resolves an A record against addr and returns the first answer's IP
// as a string, failing the test on transport error or an empty answer.
func digA(t *testing.T, addr, name string) string {
	t.Helper()
	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	r, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Answer) == 0 {
		t.Fatalf("no answer for %s: rcode=%d", name, r.Rcode)
	}
	a, ok := r.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("answer for %s is not an A record: %v", name, r.Answer[0])
	}
	return a.A.String()
}

// digRcode resolves an A record against addr and returns the response code,
// without requiring an answer section (used to detect NXDOMAIN).
func digRcode(t *testing.T, addr, name string) int {
	t.Helper()
	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	r, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatal(err)
	}
	return r.Rcode
}

func apiURL(a *App, path string) string { return "http://" + a.HTTPAddr() + path }

func postJSON(t *testing.T, c *http.Client, url, body string) *http.Response {
	t.Helper()
	resp, err := c.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// putJSON issues a PUT with a JSON body and asserts a 2xx response, closing
// the body itself since callers only need the side effect.
func putJSON(t *testing.T, c *http.Client, url, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("PUT %s: %d", url, resp.StatusCode)
	}
}

func TestAPIEndToEnd(t *testing.T) {
	ctx := t.Context()
	var upstreamHits atomic.Int64
	upAddr := mockUpstream(t, &upstreamHits)
	listSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("0.0.0.0 ads.example.com\n"))
	}))
	defer listSrv.Close()

	dir := t.TempDir()
	cfg := testConfig(dir)
	a, err := New(ctx, cfg, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	_ = a.Store().Settings().SetInternal(ctx, "upstreams", upAddr)
	allowLoopbackLists(t, a)
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Shutdown(context.Background()) }()
	a.WaitReady(5 * time.Second)

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	// setup + login
	resp := postJSON(t, c, apiURL(a, "/api/v1/setup"), `{"username":"admin","password":"password123"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	resp = postJSON(t, c, apiURL(a, "/api/v1/auth/login"), `{"username":"admin","password":"password123"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", resp.StatusCode)
	}

	// A zone and a zone record, both created through the API itself (Task 7,
	// Task 8) rather than seeded straight through the store — this exercises
	// the same write paths those tasks added, then proves the record is
	// resolvable via DNS, authoritatively: this milestone's payoff. The
	// local_records endpoint this block used to smoke-test
	// (POST /api/v1/records) is gone entirely as of Task 8, not merely
	// unwired from DNS answers — see internal/api/zonerecords_handlers.go.
	resp = postJSON(t, c, apiURL(a, "/api/v1/zones"), `{"name":"e412.test"}`)
	var zoneCreated struct{ ID int64 }
	_ = json.NewDecoder(resp.Body).Decode(&zoneCreated)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("zone create: %d", resp.StatusCode)
	}
	resp = postJSON(t, c, apiURL(a, fmt.Sprintf("/api/v1/zones/%d/records", zoneCreated.ID)), `{"name":"svc","type":"A","ttl":60,"rdata":"10.0.0.42"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("zone record create: %d", resp.StatusCode)
	}
	if got := digA(t, a.DNSAddr(), "svc.e412.test"); got != "10.0.0.42" {
		t.Fatalf("zone record not live: %s", got)
	}

	// blocklist via API → blocked (nxdomain mode)
	putJSON(t, c, apiURL(a, "/api/v1/settings"), `{"key":"blocking.mode","value":"nxdomain"}`)
	resp = postJSON(t, c, apiURL(a, "/api/v1/filters/lists"), fmt.Sprintf(`{"url":%q,"kind":"block"}`, listSrv.URL))
	var created map[string]int64
	_ = json.NewDecoder(resp.Body).Decode(&created)
	_ = resp.Body.Close()
	putJSON(t, c, apiURL(a, "/api/v1/groups/1/lists"), fmt.Sprintf(`{"list_ids":[%d]}`, created["id"]))
	resp = postJSON(t, c, apiURL(a, "/api/v1/filters/refresh"), ``)
	_ = resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if r := digRcode(t, a.DNSAddr(), "ads.example.com"); r == dns.RcodeNameError {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("blocklist never became live via api")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// pause via API → resolves again
	resp = postJSON(t, c, apiURL(a, "/api/v1/blocking/pause"), `{"group_id":0,"minutes":1}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("pause: %d", resp.StatusCode)
	}
	if r := digRcode(t, a.DNSAddr(), "ads.example.com"); r != dns.RcodeSuccess {
		t.Fatalf("pause not live: rcode %d", r)
	}

	// query log via API. The logger batches writes on a 1s ticker (async,
	// lossy by design), so poll rather than expecting the just-made queries
	// to be visible immediately.
	deadline = time.Now().Add(5 * time.Second)
	var entries []map[string]any
	for {
		qresp, err := c.Get(apiURL(a, "/api/v1/queries?limit=10"))
		if err != nil {
			t.Fatal(err)
		}
		if qresp.StatusCode != http.StatusOK {
			_ = qresp.Body.Close()
			t.Fatalf("queries: %d", qresp.StatusCode)
		}
		entries = nil
		derr := json.NewDecoder(qresp.Body).Decode(&entries)
		_ = qresp.Body.Close()
		if derr != nil {
			t.Fatal(derr)
		}
		if len(entries) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expected query log entries, got none")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestShutdownUnblocksSSETail guards against a regression where an open
// GET /api/v1/queries/tail SSE connection makes Shutdown block until its
// deadline: the tail handler only returns on client disconnect or channel
// close, and http.Server.Shutdown waits for in-flight handlers to return
// before it does. App.Start now cancels a server-scoped base context at
// the start of Shutdown so the handler's `<-r.Context().Done()` fires
// immediately instead.
func TestShutdownUnblocksSSETail(t *testing.T) {
	ctx := context.Background()
	var upstreamHits atomic.Int64
	upAddr := mockUpstream(t, &upstreamHits)

	dir := t.TempDir()
	cfg := testConfig(dir)
	a, err := New(ctx, cfg, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	_ = a.Store().Settings().SetInternal(ctx, "upstreams", upAddr)
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	a.WaitReady(5 * time.Second)

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	resp := postJSON(t, c, apiURL(a, "/api/v1/setup"), `{"username":"admin","password":"password123"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	resp = postJSON(t, c, apiURL(a, "/api/v1/auth/login"), `{"username":"admin","password":"password123"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodGet, apiURL(a, "/api/v1/queries/tail"), nil)
	if err != nil {
		t.Fatal(err)
	}
	tailResp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tailResp.Body.Close() }()
	if tailResp.StatusCode != http.StatusOK {
		t.Fatalf("tail: %d", tailResp.StatusCode)
	}

	tailDone := make(chan struct{})
	go func() {
		buf := make([]byte, 512)
		for {
			if _, err := tailResp.Body.Read(buf); err != nil {
				close(tailDone)
				return
			}
		}
	}()
	time.Sleep(100 * time.Millisecond) // let the handler subscribe

	start := time.Now()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("shutdown took %v with an open SSE tail connection; want well under 2s", elapsed)
	}

	select {
	case <-tailDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE tail connection was not unblocked by shutdown")
	}
}
