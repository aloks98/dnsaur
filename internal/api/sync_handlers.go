package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aloks98/dnsaur/internal/filter"
	"github.com/aloks98/dnsaur/internal/store"
)

// Syncer is what this package needs from the sync subsystem: a replica's
// pull loop on one side, a main's registry of replicas on the other. It is
// an interface, and nil-tolerant, for the reason ResolverStatus is —
// internal/app implements it and imports this package, so the concrete type
// cannot be named here, and every test server has no App behind it.
//
// A nil Syncer is a main that follows nobody and has no replicas. That is
// the truthful answer for a server with no sync subsystem, not an error:
// role "main", no peer, so the write guard lifts and the status endpoint
// still answers.
type Syncer interface {
	// PeerURL is the main this instance follows, and "" on a main. It is
	// the one fact that decides which kind of instance this is (spec §2),
	// and the only one the write guard reads.
	PeerURL() string
	// Status is everything the settings screen and the warning strip show.
	Status() SyncStatus
	// Forget removes one registered replica, by the id it paired with, and
	// revokes its secret. Idempotent: an id that is not registered is
	// already in the state the caller asked for, so it is not an error.
	Forget(ctx context.Context, instanceID string) error
	// NewPairingCode mints the code the operator carries from this main's
	// screen to the replica, replacing any code still live — one door open
	// at a time (§6). expiresAt is unix ms.
	NewPairingCode(ctx context.Context) (code string, expiresAt int64, err error)
	// Pair spends a correct code on one replica: it records the box, mints
	// the secret that box authenticates its pulls with, and makes sure this
	// main has a sync key for its transfers (§6). ErrPairingRefused for a
	// code this main will not spend, whichever of §9's reasons it was.
	Pair(ctx context.Context, req PairRequest) (PairResult, error)
	// Authenticate maps the secret a replica presents to the instance id it
	// was minted for. A secret nobody holds is ("", false, nil): not an
	// error, just not a replica.
	Authenticate(ctx context.Context, secret string) (instanceID string, ok bool, err error)
	// Heartbeat stamps a registered replica from its version probe, which
	// is the only heartbeat there is (§3).
	Heartbeat(ctx context.Context, instanceID string, applied int64) error
	// Follow is the replica's half: pair with the main at peerURL by
	// spending code, store what comes back, and pull once.
	// ErrFollowRefused is a main that would not spend the code.
	Follow(ctx context.Context, peerURL, code string) error
	// DNSPort is the port this box answers DNS on, which is what a replica
	// joins its peer URL's host to when sync.primary_dns names no override
	// (§8).
	DNSPort() int
}

// ErrPairingRefused and ErrFollowRefused are the two refusals a Syncer
// reports that are not failures: a code this main will not spend, and a
// main that would not spend the code this box offered.
//
// internal/confsync declares the same two sentinels and internal/app maps
// its own onto these, rather than this package importing that one and
// matching the originals. The dependency only runs one way — confsync imports
// internal/api for api.Replica, api.PairRequest and api.SyncStatus — so the
// package that answers the status code is the package that has to name the
// error.
var (
	ErrPairingRefused = errors.New("pairing code refused")
	ErrFollowRefused  = errors.New("the main refused the pairing code")
	// ErrNotRegistered is the third: a heartbeat for a replica this main no
	// longer knows, which is "Forget" landing while that box was mid-probe.
	// Not a failure either — the box has to pair again, which is what the
	// 401 it gets tells it.
	ErrNotRegistered = errors.New("no such replica is registered")
)

// Replica is one registered replica as the main records and reports it.
type Replica struct {
	InstanceID string `json:"instance_id"`
	// DNSAddr is where this replica answers DNS (host:port) — the address
	// an AXFR arrives from and a NOTIFY is sent to, which is why it is
	// validated as an address rather than stored as whatever was sent.
	DNSAddr        string `json:"dns_addr"`
	VersionApplied int64  `json:"version_applied"`
	LastSeen       int64  `json:"last_seen"`
	// Stale is set by the main when nothing has been heard for three
	// intervals. Never a reason to delete the entry — an operator removes
	// one deliberately (§6).
	Stale bool `json:"stale"`
}

// PairRequest is what a replica posts to POST /sync/pair: the code the
// operator carried from the main's screen, and who is spending it. No
// credential — the code is the proof (§3), and it is spent by being used.
type PairRequest struct {
	Code       string `json:"code"`
	InstanceID string `json:"instance_id"`
	// DNSAddr is where this box answers DNS, for the main's registry: the
	// address an AXFR arrives from and a NOTIFY goes to (§6). A replica
	// bound to the wildcard sends the port alone and the main completes the
	// host from the connection.
	DNSAddr string `json:"dns_addr"`
}

// PairResult is what a correct code buys: the secret this replica
// authenticates every later call with, and the DNS port the main answers on,
// which is what the peer URL's host is joined to when sync.primary_dns names
// no override (§8). The secret exists in plain text here and nowhere else.
//
// Not the sync key: the bundle the first pull brings carries its id, so
// naming it here as well would be the same fact from two sources, and the
// one a replica actually reads is the bundle's.
type PairResult struct {
	Secret  string `json:"secret"`
	DNSPort int    `json:"dns_port"`
}

// SyncStatus is the sync subsystem's whole visible state, on either kind of
// instance. The replica fields and the main fields are both optional, so one
// shape serves both roles and a client reads Role to know which half to show.
type SyncStatus struct {
	Role           string    `json:"role"` // "main" | "replica"
	PeerURL        string    `json:"peer_url,omitempty"`
	PeerVersion    int64     `json:"peer_version,omitempty"`
	AppliedVersion int64     `json:"applied_version,omitempty"`
	AppliedAt      int64     `json:"applied_at,omitempty"`
	LastPullAt     int64     `json:"last_pull_at,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	PlainHTTP      bool      `json:"plain_http,omitempty"`
	SyncKey        string    `json:"sync_key,omitempty"`
	Replicas       []Replica `json:"replicas,omitempty"`
}

func (s *Server) syncRoutes() {
	// The two pull-loop reads take a replica's pairing secret and nothing
	// else — not a session, not an API token (§9). The probe is also the
	// heartbeat, so it has to know which replica is asking.
	//
	// managed outside requireReplica on both: a replica serves nobody a
	// bundle — the one it could answer with is the main's, one pull stale,
	// so a box following it would be following a copy of a copy — and it
	// answers nobody's probe either, because a box that was a main until a
	// moment ago must stop stamping and serving the replicas it used to
	// have rather than keep them polling a configuration that is now
	// somebody else's. The 409 names the main to point at instead, which is
	// the answer whatever credential the caller brought.
	s.route("GET /api/v1/sync/version", s.managed(s.requireReplica(s.handleSyncVersion)))
	s.route("GET /api/v1/sync/bundle", s.managed(s.requireReplica(s.handleSyncBundle)))
	s.route("GET /api/v1/sync/status", s.requireAuth(s.handleSyncStatus))
	// Minting a code and spending one are both a main's business: a replica
	// keeps no registry, so a box that paired with it would be recorded
	// where nothing notifies it and no transfer is let through (§6).
	s.route("POST /api/v1/sync/pairing-code", s.requireAuth(s.managed(s.handlePairingCode)))
	// No auth: the code the operator carried from the main's screen is the
	// proof (§3). See handlePair for what stands in for a credential.
	s.route("POST /api/v1/sync/pair", s.managed(s.handlePair))
	// The replica's own side of the same handshake, and not managed: it is
	// what makes this box a replica, and a box that already follows one is
	// refused by the handler with the peer it is following.
	s.route("POST /api/v1/sync/follow", s.requireAuth(s.handleFollow))
	s.route("DELETE /api/v1/sync/replicas/{instance_id}", s.requireAuth(s.handleReplicaForget))
}

// peerURL is the main this instance follows, "" when it follows none. The
// nil check lives here rather than at each call site: Deps.Sync is nil in
// every server with no App behind it, and a nil Syncer is a main.
func (s *Server) peerURL() string {
	if s.deps.Sync == nil {
		return ""
	}
	return s.deps.Sync.PeerURL()
}

// syncStatus is Syncer.Status with the nil case folded in.
func (s *Server) syncStatus() SyncStatus {
	if s.deps.Sync == nil {
		return SyncStatus{Role: "main"}
	}
	return s.deps.Sync.Status()
}

// managed refuses a write to synced configuration on a replica (spec §7).
// One box accepts writes, and a replica says which one: answering 409 before
// the handler runs is what keeps a config that two people edit from becoming
// a config that drifts, and clearing sync.peer_url is what lifts it.
//
// Applied at registration rather than inside each handler so the route
// registry — which is what the auth and OpenAPI tests walk — still lists the
// route, and so a handler cannot be added to a synced resource and quietly
// miss the guard.
func (s *Server) managed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if peer := s.peerURL(); peer != "" {
			errJSON(w, http.StatusConflict, "managed by "+peer)
			return
		}
		h(w, r)
	}
}

// requireReplica is the credential the two pull-loop reads take: the secret
// one paired replica was given, presented as a bearer token, and never a
// session or an API token (§9). The instance id it belongs to goes into the
// request context, because the probe behind it is also that replica's
// heartbeat.
//
// One answer for every way it can fail — no header, the wrong scheme, a
// secret nobody holds, no sync subsystem at all — for ErrPairingRefused's
// reason: the caller learns whether its own credential works, and nothing
// else.
func (s *Server) requireReplica(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if len(header) <= len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) ||
			s.deps.Sync == nil {
			errJSON(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		id, ok, err := s.deps.Sync.Authenticate(r.Context(), header[len(bearerPrefix):])
		if err != nil {
			storeErr(w, err)
			return
		}
		if !ok {
			errJSON(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), replicaKey, id)))
	}
}

// replicaFrom is the instance id requireReplica authenticated. Only the two
// routes behind it ever read this, so the zero value is unreachable.
func replicaFrom(r *http.Request) string {
	id, _ := r.Context().Value(replicaKey).(string)
	return id
}

// syncVersion is the probe a replica makes every interval: the counter that
// says whether anything changed, the id of the box that answered — so a
// replica pointed at itself by a misconfiguration can see that it is — and
// the port that box answers DNS on, which is what the peer URL's host is
// joined to when sync.primary_dns names no override (§8).
type syncVersion struct {
	ConfigVersion int64  `json:"config_version"`
	InstanceID    string `json:"instance_id"`
	DNSPort       int    `json:"dns_port"`
}

func (s *Server) handleSyncVersion(w http.ResponseWriter, r *http.Request) {
	// The probe is the heartbeat: there is no separate registration call
	// (§3). ?applied is what this replica has actually installed, so a value
	// that is not a number — or one below zero, which no count of writes
	// ever reaches — is 0, "nothing yet", which is what a box that has only
	// just paired truthfully reports.
	switch err := s.deps.Sync.Heartbeat(r.Context(), replicaFrom(r), max(qInt(r, "applied"), 0)); {
	case errors.Is(err, ErrNotRegistered):
		// Forgotten between the secret being checked and the entry being
		// stamped. The answer is the one the next probe would get anyway,
		// and the one this box can act on: pair again.
		errJSON(w, http.StatusUnauthorized, "unauthorized")
		return
	case err != nil:
		storeErr(w, err)
		return
	}
	settings := s.deps.Store.Settings()
	version, err := settings.ConfigVersion(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	// instance.id is seeded at first start and never absent in practice;
	// an empty string is a truthful answer either way, and not a reason to
	// fail a probe.
	id, _, err := settings.Get(r.Context(), instanceIDSetting)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, syncVersion{
		ConfigVersion: version, InstanceID: id, DNSPort: s.deps.Sync.DNSPort(),
	})
}

const (
	// instanceIDSetting is this box's identity. The store names the same
	// key; it is unexported there because nothing outside that package had
	// reason to read it until the version probe did.
	instanceIDSetting = "instance.id"
	// syncTokenSetting is the credential a replica pulls with. Editable,
	// and stripped from GET /settings — see handleSettingsGet.
	syncTokenSetting = "sync.token"
	// syncPeerURLSetting is the main this instance follows, and the one
	// setting that decides which kind of instance it is.
	syncPeerURLSetting = "sync.peer_url"
	// syncReplicasSetting is the main's registry of registered replicas —
	// bookkeeping, not configuration, and stripped from GET /settings for
	// the reason the rollup watermark is.
	syncReplicasSetting = "sync.replicas"
	// syncPairingSetting is the live pairing code — its hash, its expiry and
	// the guesses made against it. Internal, and for a reason of its own:
	// the hash is of a 40-bit code, so anyone who could read it could grind
	// it offline in the ten minutes it is alive (§9).
	syncPairingSetting = "sync.pairing"
	// syncKeyIDSetting is the TSIG key this main's replicas transfer under.
	// Internal because the first pairing creates it and nobody picks it
	// (§6); GET /sync/status reports the key's name.
	syncKeyIDSetting = "sync.tsig_key_id"
	// The other half of that bookkeeping, written by a replica about its
	// own pulls (internal/confsync). Same treatment, same reason: the Sync
	// band reads them from GET /sync/status, which reports them in a shape
	// the screen can use, and no PUT may write them.
	syncAppliedVersionSetting = "sync.applied_version"
	// syncAppliedPeerSetting is the peer that version was applied from, so a
	// re-point to a main that happens to be at the same number still pulls.
	syncAppliedPeerSetting = "sync.applied_peer"
	syncAppliedAtSetting   = "sync.applied_at"
	syncLastPullAtSetting  = "sync.last_pull_at"
	syncLastErrorSetting   = "sync.last_error"
)

func (s *Server) handleSyncBundle(w http.ResponseWriter, r *http.Request) {
	b, err := s.deps.Store.ExportBundle(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.syncStatus())
}

// pairingCode is what POST /sync/pairing-code answers: the code the
// operator reads off this main's screen, shown once, and the moment it stops
// working (unix ms), which is the "Expires in 10 minutes" the band counts
// down.
type pairingCode struct {
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expires_at"`
}

func (s *Server) handlePairingCode(w http.ResponseWriter, r *http.Request) {
	if s.deps.Sync == nil {
		errJSON(w, http.StatusServiceUnavailable, "sync unavailable")
		return
	}
	code, expiresAt, err := s.deps.Sync.NewPairingCode(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pairingCode{Code: code, ExpiresAt: expiresAt})
}

// handlePair is the one unauthenticated write in this API: a box that has
// not paired yet holds no credential to present, so the code the operator
// carried from this main's screen is the proof (§3).
//
// What stands in for a credential is the throttle — login's own budget, per
// source address — in front of a code that is 40 bits, lives ten minutes and
// dies after five wrong guesses (§9). The throttle runs before the body is
// read, so a malformed body and a wrong code cost the same one attempt, and
// after the replica guard, so a box pointed at the wrong instance is told
// which one to use without spending anything.
//
// Sharing login's budget is deliberate rather than convenient: a guesser
// grinding codes from one address spends that address's login attempts too,
// and locks itself out of the form it would go to next.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if !s.throttle(w, r) {
		return
	}
	body, ok := decodeOr400[PairRequest](w, r)
	if !ok {
		return
	}
	switch {
	case body.InstanceID == "":
		// An entry keyed on nothing would make Authenticate answer for a
		// stranger; the registry refuses it too, and this says why.
		errJSON(w, http.StatusBadRequest, "instance_id required")
		return
	case strings.Contains(body.InstanceID, "/"):
		// DELETE /sync/replicas/{instance_id} is the only address such an
		// entry has, and a slash in the id is an entry with no address.
		errJSON(w, http.StatusBadRequest, "instance_id must not contain a slash")
		return
	}
	// clientIP falls back to RemoteAddr verbatim when that is not an address
	// at all (a unix socket), and a dns_addr completed from it then fails
	// hostPort below — closed, with the 400 saying so, never recorded.
	addr := withClientHost(body.DNSAddr, s.clientIP(r))
	if err := hostPort(addr); err != nil {
		errJSON(w, http.StatusBadRequest, "dns_addr "+err.Error())
		return
	}
	// A box pairing with itself: the URL in the Follow form names this very
	// instance. Left to run it would store a peer_url pointing here, and
	// this box would pull its own configuration over itself forever. The
	// replica end refuses the same mistake at its next probe (§4.1); this
	// end refuses it before a peer is ever stored.
	id, _, err := s.deps.Store.Settings().Get(r.Context(), instanceIDSetting)
	if err != nil {
		storeErr(w, err)
		return
	}
	if id != "" && id == body.InstanceID {
		errJSON(w, http.StatusBadRequest, "a main cannot pair with itself")
		return
	}
	if s.deps.Sync == nil {
		errJSON(w, http.StatusServiceUnavailable, "sync unavailable")
		return
	}
	result, err := s.deps.Sync.Pair(r.Context(), PairRequest{
		Code: body.Code, InstanceID: body.InstanceID, DNSAddr: addr,
	})
	switch {
	case errors.Is(err, ErrPairingRefused):
		// One answer for wrong, expired, voided and already spent. A
		// guesser told which one it was learns whether a code is live.
		errJSON(w, http.StatusForbidden, "pairing code refused")
	case err != nil:
		storeErr(w, err)
	default:
		writeJSON(w, http.StatusOK, result)
	}
}

// followReq is the body of POST /sync/follow: what the operator typed into
// the Sync band on a box that follows nobody yet.
type followReq struct {
	PeerURL string `json:"peer_url"`
	Code    string `json:"code"`
}

// syncFollowTimeout bounds the Follow action, and is what
// Server.followTimeout is set to. What it bounds is the one pairing request:
// Follow kicks the first pull onto the poll loop's own context and answers as
// soon as the pairing is stored, so nothing here waits out a bundle fetch.
const syncFollowTimeout = 15 * time.Second

func (s *Server) handleFollow(w http.ResponseWriter, r *http.Request) {
	// Not s.managed: following is what makes this box a replica, and the
	// answer to "follow a second main" is to stop following the first, not
	// to go and ask it.
	if peer := s.peerURL(); peer != "" {
		errJSON(w, http.StatusConflict, "already following "+peer)
		return
	}
	body, ok := decodeOr400[followReq](w, r)
	if !ok {
		return
	}
	// Normalised and judged here, by the grammar PUT /settings judges
	// sync.peer_url with — this action writes that setting through the
	// store rather than through that endpoint, so a value it would refuse
	// has to be refused here instead. (The two graders are two copies
	// because internal/confsync imports this package and not the other way
	// round; TestPeerURLGrammarsAgree is what keeps them one grammar.)
	peer := strings.TrimRight(strings.TrimSpace(body.PeerURL), "/")
	if err := peerURL(peer); peer == "" || err != nil {
		errJSON(w, http.StatusBadRequest, "peer_url must be an absolute http or https URL, scheme and host only")
		return
	}
	if strings.TrimSpace(body.Code) == "" {
		// Sent anyway it would cost one of the five guesses the main allows
		// before it voids the code, which is the operator's code to lose.
		errJSON(w, http.StatusBadRequest, "code required")
		return
	}
	if s.deps.Sync == nil {
		errJSON(w, http.StatusServiceUnavailable, "sync unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.followTimeout)
	defer cancel()
	err := s.deps.Sync.Follow(ctx, peer, body.Code)
	switch {
	case errors.Is(err, ErrFollowRefused):
		// 502: this box did its part and the far one said no. The message
		// is fixed, because what the main was willing to say about which
		// way the code was wrong is not much (§9).
		errJSON(w, http.StatusBadGateway, "the main refused the pairing code")
	case err != nil:
		// Every other failure is the same 502 in its own words: a peer that
		// is a replica of somebody, one with no pairing route, one that is
		// throttling — none of them is a code to retype, and the operator
		// of the box that was dialling has nothing to be kept from.
		errJSON(w, http.StatusBadGateway, err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// withClientHost fills in a pairing whose dns_addr names a port and no
// host, which is what a replica sends when its dns_listen is the default
// ":53" — a wildcard is what it binds, and it has no way to know which of its
// addresses this main can reach it on.
//
// The connection does know: the request arrived from that box. clientIP is
// the same address the login throttle attributes a request to, so it is the
// forwarded one only when the request came from a trusted proxy — which is
// also the only case where the socket's peer is not the replica itself.
//
// Anything else is returned untouched, including a value that names neither:
// filling a host into something that is not an address would turn a typo
// into a plausible-looking entry the operator never wrote.
func withClientHost(addr, clientIP string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host != "" || clientIP == "" {
		return addr
	}
	return net.JoinHostPort(clientIP, port)
}

// internalSetting reports whether a row in the settings table is
// bookkeeping rather than configuration: written by the server about itself,
// editable by nobody (none of these is in editableSettings, so PUT answers
// "setting not editable"), and reported — where it is reported at all — by
// an endpoint of its own.
//
// Named by key rather than by prefix, because every one of these prefixes
// also holds real settings: stats.retention_days, blocking.mode,
// sync.interval_seconds.
func internalSetting(key string) bool {
	switch key {
	case store.StatsWatermarkKey, filter.PausesKey, syncReplicasSetting,
		syncPairingSetting, syncKeyIDSetting,
		syncAppliedVersionSetting, syncAppliedPeerSetting, syncAppliedAtSetting,
		syncLastPullAtSetting, syncLastErrorSetting:
		return true
	}
	return false
}

func (s *Server) handleReplicaForget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("instance_id")
	if id == "" {
		errJSON(w, http.StatusBadRequest, "instance_id required")
		return
	}
	if s.deps.Sync == nil {
		errJSON(w, http.StatusServiceUnavailable, "sync unavailable")
		return
	}
	// Removing a replica that is not registered is the state the caller
	// asked for, so it is not an error — the operator's button is idempotent.
	if err := s.deps.Sync.Forget(r.Context(), id); err != nil {
		storeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
