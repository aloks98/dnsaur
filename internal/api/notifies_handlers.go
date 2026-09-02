package api

import (
	"net/http"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
)

func (s *Server) notifiesRoutes() {
	s.route("GET /api/v1/zones/{id}/notifies", s.requireAuth(s.handleZoneNotifiesList))
}

// notifyRow is the API's view of one target's outbound NOTIFY delivery
// state: store.ZoneNotify plus the one field the store doesn't carry —
// state — and minus pending_serial, which is bookkeeping the dashboard has
// no use for.
type notifyRow struct {
	Target         string `json:"target"`
	State          string `json:"state"`
	NotifiedSerial uint32 `json:"notified_serial"`
	NotifiedAt     int64  `json:"notified_at"`
	Attempts       int    `json:"attempts"`
	MaxAttempts    int    `json:"max_attempts"`
	LastError      string `json:"last_error"`
	CreatedAt      int64  `json:"created_at"`
}

// notifyStateOf reduces a target's row to the one word the dashboard shows.
//
// Derived here rather than in the client for the reason D3's transfer status
// is: a status two clients could compute differently is not a status.
//
// **The order is the logic.** `never` is not simply notified_at == 0 — a
// target that has never been delivered *and* has exhausted its attempts is
// gave_up, and reading notified_at alone would show a target that has failed
// five times as though nothing had been tried.
func notifyStateOf(zoneSerial uint32, n store.ZoneNotify) string {
	behind := n.NotifiedAt == 0 || zones.SerialNewer(zoneSerial, n.NotifiedSerial)
	if !behind {
		return "current"
	}
	if n.Attempts >= zones.MaxNotifyAttempts {
		return "gave_up"
	}
	if n.NotifiedAt == 0 && n.Attempts == 0 {
		return "never"
	}
	return "retrying"
}

func (s *Server) handleZoneNotifiesList(w http.ResponseWriter, r *http.Request) {
	zid, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	zone, err := s.deps.Store.Zones().Zone(r.Context(), zid)
	if err != nil {
		storeErr(w, err)
		return
	}
	rows, err := s.deps.Store.Notifies().ByZone(r.Context(), zid)
	if err != nil {
		storeErr(w, err)
		return
	}
	// []notifyRow{}, not var rows []notifyRow: the empty case must marshal
	// as [], not null — the dashboard maps over the response, and null
	// would be a runtime error on the overwhelmingly common case of a zone
	// with no notify targets.
	out := []notifyRow{}
	for _, n := range rows {
		out = append(out, notifyRow{
			Target:         n.Target,
			State:          notifyStateOf(zone.SOASerial, n),
			NotifiedSerial: n.NotifiedSerial,
			NotifiedAt:     n.NotifiedAt,
			Attempts:       n.Attempts,
			MaxAttempts:    zones.MaxNotifyAttempts,
			LastError:      n.LastError,
			CreatedAt:      n.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
