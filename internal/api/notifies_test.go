package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
)

// Every row with a non-zero attempts count also names the round those
// attempts were made in. That is not decoration: NoteAttempt writes
// pending_serial and attempts in the same statement, so a row claiming five
// attempts against no round at all is a state the notifier cannot produce —
// and the state derivation is now scoped to the round, so a fixture in an
// unreachable state would be asserting about nothing.
func TestNotifyStateDerivation(t *testing.T) {
	tests := []struct {
		name       string
		zoneSerial uint32
		row        store.ZoneNotify
		want       string
	}{
		{
			name:       "never told and never tried",
			zoneSerial: 47,
			row:        store.ZoneNotify{NotifiedAt: 0, Attempts: 0},
			want:       "never",
		},
		{
			name:       "acknowledged the current serial",
			zoneSerial: 47,
			row:        store.ZoneNotify{NotifiedAt: 1000, NotifiedSerial: 47},
			want:       "current",
		},
		{
			name:       "behind, still trying",
			zoneSerial: 48,
			row:        store.ZoneNotify{NotifiedAt: 1000, NotifiedSerial: 47, PendingSerial: 48, Attempts: 3},
			want:       "retrying",
		},
		{
			name:       "behind, budget exhausted",
			zoneSerial: 48,
			row: store.ZoneNotify{
				NotifiedAt: 1000, NotifiedSerial: 47,
				PendingSerial: 48, Attempts: zones.MaxNotifyAttempts,
			},
			want: "gave_up",
		},
		{
			// THE TRAP: never delivered *and* exhausted is gave_up, not
			// never. Reading notified_at alone would show a target that has
			// failed five times as though nothing had been tried.
			name:       "never delivered and exhausted",
			zoneSerial: 47,
			row:        store.ZoneNotify{NotifiedAt: 0, PendingSerial: 47, Attempts: zones.MaxNotifyAttempts},
			want:       "gave_up",
		},
		{
			name:       "never delivered, partway through the budget",
			zoneSerial: 47,
			row:        store.ZoneNotify{NotifiedAt: 0, PendingSerial: 47, Attempts: 2},
			want:       "retrying",
		},
		{
			// A wrapped serial is behind, not current — the API's own use of
			// SerialNewer, and the reason it is one shared helper.
			name:       "the zone wrapped past the target",
			zoneSerial: 0,
			row:        store.ZoneNotify{NotifiedAt: 1000, NotifiedSerial: 4294967295},
			want:       "retrying",
		},
		{
			// THE DISAGREEMENT: the budget was exhausted against serial 100
			// and the zone has since moved to 101. maybeSend scopes give-up
			// to the *round*: pending_serial != want resets attempts, so the
			// notifier is about to try this target again on its next pass.
			// Reading attempts without pending_serial made the screen say
			// gave_up for up to the remaining back-off about a target the
			// notifier had already picked back up.
			name:       "the round it gave up on is not the zone's round any more",
			zoneSerial: 101,
			row: store.ZoneNotify{
				NotifiedAt: 1000, NotifiedSerial: 99,
				PendingSerial: 100, Attempts: zones.MaxNotifyAttempts,
			},
			want: "retrying",
		},
		{
			// The same, for a target that has never been delivered at all.
			// Not `never`: it is behind and the notifier will try it this
			// pass, and `never` is the one state the dashboard does not
			// count as behind. `never` stays "nothing has ever happened
			// here", which is what the attempts column, unscoped, still
			// answers.
			name:       "never delivered, and the round it gave up on has passed",
			zoneSerial: 101,
			row: store.ZoneNotify{
				NotifiedAt: 0, PendingSerial: 100, Attempts: zones.MaxNotifyAttempts,
			},
			want: "retrying",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := notifyStateOf(tc.zoneSerial, tc.row); got != tc.want {
				t.Errorf("state = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestListZoneNotifies(t *testing.T) {
	ts := newTestServer(t)
	ctx := t.Context()

	rec := ts.do(t, "POST", "/api/v1/zones",
		`{"name":"example.com","type":"primary","notify_to":"10.0.0.2, 10.0.0.3"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %s", rec.Body)
	}
	zoneID := createdID(t, rec)
	if err := ts.store.Notifies().Reconcile(ctx, zoneID, []string{"10.0.0.2:53", "10.0.0.3:53"}, 1000); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := ts.do(t, "GET", "/api/v1/zones/"+strconv.FormatInt(zoneID, 10)+"/notifies", "")
	if got.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", got.Code, got.Body)
	}
	var rows []map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal %s: %v", got.Body, err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r["state"] != "never" {
			t.Errorf("target %v: state = %v, want never", r["target"], r["state"])
		}
		// The screen renders "try 3/5" and must not hardcode the 5.
		if r["max_attempts"] != float64(zones.MaxNotifyAttempts) {
			t.Errorf("max_attempts = %v, want %d", r["max_attempts"], zones.MaxNotifyAttempts)
		}
		// created_at is what dates a never-notified target.
		if r["created_at"] != float64(1000) {
			t.Errorf("created_at = %v, want 1000", r["created_at"])
		}
	}
}

// The end of the same argument, through the handler rather than the helper:
// the row the dashboard renders must describe the round the notifier is
// actually in.
//
// A target exhausted its five attempts against serial 100, and the zone has
// since moved to 101. maybeSend's next pass resets attempts and sends
// (pending_serial != want is "a new round"), so the screen must not say the
// target was given up on, and must not claim five attempts have been made
// against 101 either. Both fields are scoped to the current round or
// neither is: "gave_up" with a stale count is the same disagreement wearing
// a different word.
func TestNotifyStateFollowsTheNotifiersRoundAfterASerialBump(t *testing.T) {
	ts := newTestServer(t)
	ctx := t.Context()

	rec := ts.do(t, "POST", "/api/v1/zones", `{"name":"example.com","type":"primary","notify_to":"10.0.0.2"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %s", rec.Body)
	}
	zoneID := createdID(t, rec)
	if err := ts.store.Notifies().Reconcile(ctx, zoneID, []string{"10.0.0.2:53"}, 1000); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	rows, err := ts.store.Notifies().ByZone(ctx, zoneID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ByZone = %v, %v; want 1 row", rows, err)
	}
	// The round that ran out, exactly as the notifier records one.
	if err := ts.store.Notifies().NoteAttempt(ctx, rows[0].ID, 100, zones.MaxNotifyAttempts, 0, "i/o timeout"); err != nil {
		t.Fatalf("NoteAttempt: %v", err)
	}
	// And then an edit moves the zone on, which is what starts a new round.
	z := ts.zone(t, zoneID)
	z.SOASerial = 101
	if err := ts.store.Zones().UpdateZone(ctx, z); err != nil {
		t.Fatalf("UpdateZone: %v", err)
	}

	got := ts.do(t, "GET", "/api/v1/zones/"+strconv.FormatInt(zoneID, 10)+"/notifies", "")
	if got.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", got.Code, got.Body)
	}
	var out []map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal %s: %v", got.Body, err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d rows, want 1", len(out))
	}
	if out[0]["state"] != "retrying" {
		t.Errorf("state = %v, want retrying — the notifier resets attempts on a new round and will send this pass",
			out[0]["state"])
	}
	if out[0]["attempts"] != float64(0) {
		t.Errorf("attempts = %v, want 0 — five attempts were made against serial 100, none against 101",
			out[0]["attempts"])
	}
	// The failure that ended the old round is still worth showing: it is the
	// most recent thing that happened to this target.
	if out[0]["last_error"] != "i/o timeout" {
		t.Errorf("last_error = %v, want the failure that ended the previous round", out[0]["last_error"])
	}
}

func TestListZoneNotifiesUnknownZoneIs404(t *testing.T) {
	ts := newTestServer(t)
	got := ts.do(t, "GET", "/api/v1/zones/9999/notifies", "")
	if got.Code != http.StatusNotFound {
		t.Fatalf("status %d, body %s", got.Code, got.Body)
	}
}

// A zone with no targets returns an empty array, not null — the dashboard
// maps over it, and null would be a runtime error on the common case.
func TestListZoneNotifiesEmptyIsAnArray(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, "POST", "/api/v1/zones", `{"name":"example.com","type":"primary"}`)
	zoneID := createdID(t, rec)

	got := ts.do(t, "GET", "/api/v1/zones/"+strconv.FormatInt(zoneID, 10)+"/notifies", "")
	if body := strings.TrimSpace(got.Body.String()); body != "[]" {
		t.Errorf("body = %s, want []", body)
	}
}
