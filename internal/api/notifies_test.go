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
			row:        store.ZoneNotify{NotifiedAt: 1000, NotifiedSerial: 47, Attempts: 3},
			want:       "retrying",
		},
		{
			name:       "behind, budget exhausted",
			zoneSerial: 48,
			row:        store.ZoneNotify{NotifiedAt: 1000, NotifiedSerial: 47, Attempts: zones.MaxNotifyAttempts},
			want:       "gave_up",
		},
		{
			// THE TRAP: never delivered *and* exhausted is gave_up, not
			// never. Reading notified_at alone would show a target that has
			// failed five times as though nothing had been tried.
			name:       "never delivered and exhausted",
			zoneSerial: 47,
			row:        store.ZoneNotify{NotifiedAt: 0, Attempts: zones.MaxNotifyAttempts},
			want:       "gave_up",
		},
		{
			name:       "never delivered, partway through the budget",
			zoneSerial: 47,
			row:        store.ZoneNotify{NotifiedAt: 0, Attempts: 2},
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
