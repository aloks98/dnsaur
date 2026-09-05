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

// notifyAttemptsIn is how many attempts have been made against the round the
// zone is in *now* — which is what `attempts` means everywhere else in this
// feature, and the number the notifier will act on next.
//
// zones.Notifier.maybeSend is the definition: `if row.pending_serial != want
// { attempts = 0 } // a new round`. A target that used its whole budget
// against serial 100 has made no attempt at all against 101, and the next
// pass will send to it. Reading n.Attempts raw here is what made the API and
// the notifier mean different things by one word.
func notifyAttemptsIn(zoneSerial uint32, n store.ZoneNotify) int {
	if n.PendingSerial != zoneSerial {
		return 0
	}
	return n.Attempts
}

// notifyStateOf reduces a target's row to the one word the dashboard shows.
//
// Derived here rather than in the client for the reason D3's transfer status
// is: a status two clients could compute differently is not a status. The
// same rule applied once more, between the API and the notifier: `gave_up`
// is scoped to the round, exactly as maybeSend scopes it, so the screen
// cannot say a target was given up on while the pass is about to send to it.
//
// **The order is the logic.** `never` is not simply notified_at == 0 — a
// target that has never been delivered *and* has exhausted its attempts is
// gave_up, and reading notified_at alone would show a target that has failed
// five times as though nothing had been tried.
//
// And `never` is the one check that reads n.Attempts *raw* rather than
// through notifyAttemptsIn. It is the only state the dashboard does not
// count as behind, so it has to mean "nothing has ever happened to this
// target", not "nothing has happened in this round" — a target whose old
// round failed five times and whose new round has not started yet is
// retrying, and it is behind.
func notifyStateOf(zoneSerial uint32, n store.ZoneNotify) string {
	behind := n.NotifiedAt == 0 || zones.SerialNewer(zoneSerial, n.NotifiedSerial)
	if !behind {
		return "current"
	}
	if notifyAttemptsIn(zoneSerial, n) >= zones.MaxNotifyAttempts {
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
			// Round-scoped, like State above: "retrying · try 5/5" against a
			// serial nothing has been attempted for is the same stale
			// reading, one column over.
			Attempts:    notifyAttemptsIn(zone.SOASerial, n),
			MaxAttempts: zones.MaxNotifyAttempts,
			LastError:   n.LastError,
			CreatedAt:   n.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
