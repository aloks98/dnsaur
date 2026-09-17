package dhcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dhcp"
	"github.com/aloks98/dnsaur/internal/dhcp/keatest"
)

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestCommandRoundTrip(t *testing.T) {
	srv := keatest.NewServer(t)
	sent := make(chan json.RawMessage, 1)
	srv.Handle("config-get", func(args json.RawMessage) dhcp.Response {
		sent <- args
		return dhcp.Response{
			Result:    0,
			Text:      "Configuration found.",
			Arguments: json.RawMessage(`{"Dhcp4":{"valid-lifetime":3600},"hash":"0BE9"}`),
		}
	})

	c := dhcp.NewClient(srv.Socket())
	resp, err := c.Command(t.Context(), "config-get", map[string]any{"service": []string{"dhcp4"}})
	if err != nil {
		t.Fatalf("config-get: %v", err)
	}
	if resp.Result != 0 || resp.Text != "Configuration found." {
		t.Errorf("reply is %d %q, want 0 %q", resp.Result, resp.Text, "Configuration found.")
	}
	var args struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(resp.Arguments, &args); err != nil {
		t.Fatalf("decoding the reply's arguments: %v", err)
	}
	if args.Hash != "0BE9" {
		t.Errorf("hash is %q, want %q", args.Hash, "0BE9")
	}
	if got := string(<-sent); got != `{"service":["dhcp4"]}` {
		t.Errorf("the engine saw arguments %s, want %s", got, `{"service":["dhcp4"]}`)
	}
	if calls := srv.Calls(); len(calls) != 1 || calls[0] != "config-get" {
		t.Errorf("the engine saw %v, want [config-get]", calls)
	}

	// The same reply through the typed call the renderer uses.
	cfg, err := c.ConfigGet(t.Context())
	if err != nil {
		t.Fatalf("ConfigGet: %v", err)
	}
	if _, ok := cfg["Dhcp4"]; !ok {
		t.Errorf("ConfigGet returned %v, want a Dhcp4 key", cfg)
	}
}

func TestUnknownCommandIsUnsupported(t *testing.T) {
	srv := keatest.NewServer(t)
	resp, err := dhcp.NewClient(srv.Socket()).Command(t.Context(), "lease6-get-all", nil)
	if err != nil {
		t.Fatalf("lease6-get-all: %v", err)
	}
	if resp.Result != 2 || resp.Text != "'lease6-get-all' command not supported." {
		t.Errorf("reply is %d %q, want Kea's unsupported-command answer", resp.Result, resp.Text)
	}
}

func TestConfigSetRejectedCarriesKeaText(t *testing.T) {
	srv := keatest.NewServer(t)
	sent := make(chan json.RawMessage, 1)
	srv.Handle("config-set", func(args json.RawMessage) dhcp.Response {
		sent <- args
		return dhcp.Response{Result: 1, Text: "invalid path in output"}
	})

	err := dhcp.NewClient(srv.Socket()).ConfigSet(t.Context(), map[string]any{"valid-lifetime": 3600})
	var rejected *dhcp.RejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("ConfigSet returned %v, want a *dhcp.RejectedError", err)
	}
	if rejected.Text != "invalid path in output" {
		t.Errorf("the error carries %q, want Kea's text %q", rejected.Text, "invalid path in output")
	}
	if got := string(<-sent); got != `{"Dhcp4":{"valid-lifetime":3600}}` {
		t.Errorf("the engine saw arguments %s, want the config wrapped in Dhcp4", got)
	}
}

func TestConfigSetAcceptedIsNoError(t *testing.T) {
	srv := keatest.NewServer(t)
	srv.Handle("config-set", func(json.RawMessage) dhcp.Response {
		return dhcp.Response{Result: 0, Text: "Configuration successful.", Arguments: json.RawMessage(`{"hash":"0BE9"}`)}
	})
	if err := dhcp.NewClient(srv.Socket()).ConfigSet(t.Context(), map[string]any{"valid-lifetime": 3600}); err != nil {
		t.Fatalf("ConfigSet: %v", err)
	}
}

func TestLeasePagePagesUntilShortPage(t *testing.T) {
	lease := func(last int, name string) dhcp.Lease {
		return dhcp.Lease{
			IP:       fmt.Sprintf("192.168.1.%d", last),
			MAC:      fmt.Sprintf("aa:bb:cc:dd:ee:%02d", last),
			Hostname: name,
			SubnetID: 7,
			CLTT:     1757800000,
			ValidLft: 3600,
			State:    0,
		}
	}
	pages := map[string][]dhcp.Lease{
		"start":        {lease(10, "laptop"), lease(11, "phone")},
		"192.168.1.11": {lease(12, "printer"), lease(13, "tv")},
		"192.168.1.13": {lease(14, "nas"), lease(15, "doorbell")},
		"192.168.1.15": {lease(16, "thermostat")}, // short, so it is the last
	}

	srv := keatest.NewServer(t)
	srv.Handle("lease4-get-page", func(args json.RawMessage) dhcp.Response {
		var a struct {
			From  string `json:"from"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return dhcp.Response{Result: 1, Text: err.Error()}
		}
		if a.Limit != 2 {
			return dhcp.Response{Result: 1, Text: "limit is not the one the caller asked for"}
		}
		page, known := pages[a.From]
		if !known {
			return dhcp.Response{Result: 1, Text: "asked for a page past the last one, from " + a.From}
		}
		return dhcp.Response{Result: 0, Arguments: mustJSON(map[string]any{"count": len(page), "leases": page})}
	})

	c := dhcp.NewClient(srv.Socket())
	var got []dhcp.Lease
	from := ""
	for {
		page, next, err := c.LeasePage(t.Context(), from, 2)
		if err != nil {
			t.Fatalf("LeasePage from %q: %v", from, err)
		}
		got = append(got, page...)
		if next == "" {
			break
		}
		if len(got) > 8 {
			t.Fatalf("LeasePage kept asking with the same cursor: %d leases", len(got))
		}
		from = next
	}

	if len(got) != 7 {
		t.Fatalf("collected %d leases, want 7", len(got))
	}
	if want := lease(10, "laptop"); got[0] != want {
		t.Errorf("first lease is %+v, want %+v", got[0], want)
	}
	if want := lease(16, "thermostat"); got[6] != want {
		t.Errorf("last lease is %+v, want %+v", got[6], want)
	}
	if calls := srv.Calls(); len(calls) != 4 {
		t.Errorf("the engine saw %d calls, want 4: three full pages and the short one that ends it", len(calls))
	}
}

func TestLeasePageEndsOnWhatItDecoded(t *testing.T) {
	full := []dhcp.Lease{{IP: "192.168.1.10"}, {IP: "192.168.1.11"}}
	for _, tc := range []struct {
		name  string
		resp  dhcp.Response
		count int
		next  string
	}{
		{
			// The decoded leases decide, not the counter beside them.
			name:  "count disagrees",
			resp:  dhcp.Response{Result: 0, Arguments: mustJSON(map[string]any{"count": 0, "leases": full})},
			count: 2,
			next:  "192.168.1.11",
		},
		{
			// The page past the last lease, which is what Kea answers when
			// the total was an exact multiple of the limit.
			name:  "empty page",
			resp:  dhcp.Response{Result: 3, Text: "0 IPv4 lease(s) found.", Arguments: mustJSON(map[string]any{"count": 0, "leases": []dhcp.Lease{}})},
			count: 0,
			next:  "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := keatest.NewServer(t)
			srv.Handle("lease4-get-page", func(json.RawMessage) dhcp.Response { return tc.resp })
			page, next, err := dhcp.NewClient(srv.Socket()).LeasePage(t.Context(), "", 2)
			if err != nil {
				t.Fatalf("LeasePage: %v", err)
			}
			if len(page) != tc.count || next != tc.next {
				t.Errorf("LeasePage returned %d leases and next %q, want %d and %q", len(page), next, tc.count, tc.next)
			}
		})
	}
}

// The shape is the one the 2026-09-13 spike recorded from Kea 2.6.3 with the
// HA hook loaded.
const statusWithHA = `{
  "pid": 1234,
  "uptime": 87,
  "reload": 87,
  "high-availability": [{
    "ha-mode": "hot-standby",
    "ha-servers": {
      "local": {"role": "primary", "scopes": ["server1"], "server-name": "main", "state": "waiting"},
      "remote": {"age": 0, "analyzed-packets": 0, "communication-interrupted": true,
                 "connecting-clients": 0, "in-touch": false, "last-scopes": [],
                 "last-state": "syncing", "role": "standby", "server-name": "replica",
                 "unacked-clients": 2}
    }
  }]
}`

func TestStatusParsesHA(t *testing.T) {
	srv := keatest.NewServer(t)
	srv.Handle("status-get", func(json.RawMessage) dhcp.Response {
		return dhcp.Response{Result: 0, Arguments: json.RawMessage(statusWithHA)}
	})

	uptime, ha, err := dhcp.NewClient(srv.Socket()).Status(t.Context())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if uptime != 87 {
		t.Errorf("uptime is %d, want 87", uptime)
	}
	if ha == nil {
		t.Fatal("Status reported no HA, want the hook's block")
	}
	// RemoteName is the partner's own name for itself, which is what the
	// status line says it is paired with. Re-deriving it from the pair this
	// box rendered would name who it meant to be talking to.
	want := dhcp.HAStatus{
		Mode: "hot-standby", LocalState: "waiting",
		RemoteName: "replica", RemoteState: "syncing",
		CommunicationInterrupted: true, UnackedClients: 2,
	}
	if *ha != want {
		t.Errorf("HA is %+v, want %+v", *ha, want)
	}
}

// TestStatusWithoutARemoteName: an engine whose status-get carries no
// server-name for the partner is not a broken pair, it is one fewer word on
// a status line — so it parses, with the name empty.
func TestStatusWithoutARemoteName(t *testing.T) {
	srv := keatest.NewServer(t)
	srv.Handle("status-get", func(json.RawMessage) dhcp.Response {
		return dhcp.Response{Arguments: json.RawMessage(`{"uptime":87,"high-availability":[
			{"ha-mode":"hot-standby","ha-servers":{"local":{"state":"hot-standby"},
			 "remote":{"last-state":"hot-standby"}}}]}`)}
	})

	_, ha, err := dhcp.NewClient(srv.Socket()).Status(t.Context())
	if err != nil || ha == nil {
		t.Fatalf("Status: ha=%+v err=%v", ha, err)
	}
	if ha.RemoteName != "" || ha.RemoteState != "hot-standby" {
		t.Errorf("HA is %+v, want the state read and the name empty", *ha)
	}
}

func TestStatusWithoutTheHAHookIsNil(t *testing.T) {
	srv := keatest.NewServer(t)
	srv.Handle("status-get", func(json.RawMessage) dhcp.Response {
		return dhcp.Response{Result: 0, Arguments: json.RawMessage(`{"pid":1234,"uptime":87,"reload":87}`)}
	})

	uptime, ha, err := dhcp.NewClient(srv.Socket()).Status(t.Context())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if uptime != 87 || ha != nil {
		t.Errorf("Status returned uptime %d and HA %+v, want 87 and no HA", uptime, ha)
	}
}

func TestVersionUsesTextOrExtended(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp dhcp.Response
	}{
		{"text", dhcp.Response{Result: 0, Text: "2.6.3", Arguments: json.RawMessage(`{"extended":"2.6.3 (isc20250115113709)"}`)}},
		{"extended", dhcp.Response{Result: 0, Arguments: json.RawMessage(`{"extended":"2.6.3 (isc20250115113709)"}`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := keatest.NewServer(t)
			srv.Handle("version-get", func(json.RawMessage) dhcp.Response { return tc.resp })
			v, err := dhcp.NewClient(srv.Socket()).Version(t.Context())
			if err != nil {
				t.Fatalf("Version: %v", err)
			}
			if v != "2.6.3" {
				t.Errorf("version is %q, want %q", v, "2.6.3")
			}
		})
	}
}

func TestDeleteLease(t *testing.T) {
	srv := keatest.NewServer(t)
	sent := make(chan json.RawMessage, 1)
	srv.Handle("lease4-del", func(args json.RawMessage) dhcp.Response {
		sent <- args
		return dhcp.Response{Result: 0, Text: "IPv4 lease deleted."}
	})

	c := dhcp.NewClient(srv.Socket())
	if err := c.DeleteLease(t.Context(), "192.168.1.10"); err != nil {
		t.Fatalf("DeleteLease: %v", err)
	}
	if got := string(<-sent); got != `{"ip-address":"192.168.1.10"}` {
		t.Errorf("the engine saw arguments %s, want the address", got)
	}

	srv.Handle("lease4-del", func(json.RawMessage) dhcp.Response {
		return dhcp.Response{Result: 3, Text: "IPv4 lease not found."}
	})
	err := c.DeleteLease(t.Context(), "192.168.1.99")
	var rejected *dhcp.RejectedError
	if !errors.As(err, &rejected) || rejected.Text != "IPv4 lease not found." {
		t.Errorf("deleting an unknown lease returned %v, want a *dhcp.RejectedError carrying Kea's text", err)
	}
}

func TestAnEngineThatClosesMidCommandIsUnreachable(t *testing.T) {
	srv := keatest.NewServer(t)
	srv.Drop()

	_, err := dhcp.NewClient(srv.Socket()).Command(t.Context(), "status-get", nil)
	if !errors.Is(err, dhcp.ErrUnreachable) {
		t.Fatalf("Command against an engine that closed without answering returned %v, want ErrUnreachable", err)
	}
	if calls := srv.Calls(); len(calls) != 1 {
		t.Errorf("the engine saw %v, want the one command that reached it", calls)
	}
}

func TestUnreachableSocketIsNamed(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "nothing-here.sock")
	_, err := dhcp.NewClient(socket).Command(t.Context(), "status-get", nil)
	if !errors.Is(err, dhcp.ErrUnreachable) {
		t.Fatalf("Command against a missing socket returned %v, want ErrUnreachable", err)
	}
	if !strings.Contains(err.Error(), socket) {
		t.Errorf("the error is %q, want the socket path in it", err)
	}
}

// hangUntilGivesUp runs one command against an engine that never answers and
// returns how long the client took to give up. A client that never gives up
// fails here, rather than blocking until the package's own timeout.
func hangUntilGivesUp(ctx context.Context, t *testing.T, socket string, guard time.Duration) (time.Duration, error) {
	t.Helper()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := dhcp.NewClient(socket).Command(ctx, "status-get", nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Command against an engine that never answers returned no error")
		}
		return time.Since(start), err
	case <-time.After(guard):
		t.Fatalf("Command was still waiting after %v", guard)
		return 0, nil
	}
}

func TestCommandDeadline(t *testing.T) {
	srv := keatest.NewServer(t)
	srv.Hang()

	elapsed, err := hangUntilGivesUp(context.Background(), t, srv.Socket(), 6*time.Second)
	if elapsed < 4*time.Second {
		t.Errorf("Command gave up after %v, want the 5 s default deadline", elapsed)
	}
	// A deadline is the caller's own, not the engine being unreachable.
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, dhcp.ErrUnreachable) {
		t.Errorf("Command returned %v, want the context's deadline and not ErrUnreachable", err)
	}
}

func TestCommandHonoursTheContextDeadline(t *testing.T) {
	srv := keatest.NewServer(t)
	srv.Hang()

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	// The guard is well under the 5 s default, so a client that ignores the
	// context fails here.
	if _, err := hangUntilGivesUp(ctx, t, srv.Socket(), 2*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Command returned %v, want the context's deadline", err)
	}
}
