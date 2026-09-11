package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
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
	// Register records a replica that just applied a version. On the main
	// it is what turns on the implicit AXFR allow and adds a NOTIFY target
	// (§6), which is why the endpoint behind it needs write scope.
	// Idempotent: a known instance_id has its address, applied version and
	// last-seen updated rather than being recorded twice.
	Register(ctx context.Context, r Replica) error
	// Forget removes one registered replica, by the id it registered with.
	// Idempotent: an id that is not registered is already in the state the
	// caller asked for, so it is not an error.
	Forget(ctx context.Context, instanceID string) error
}

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
	s.route("GET /api/v1/sync/version", s.requireAuth(s.handleSyncVersion))
	s.route("GET /api/v1/sync/bundle", s.requireAuth(s.requireWriteScope(s.handleSyncBundle)))
	s.route("GET /api/v1/sync/status", s.requireAuth(s.handleSyncStatus))
	s.route("POST /api/v1/sync/replicas", s.requireAuth(s.handleReplicaRegister))
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

// requireWriteScope refuses a read-scoped token on a GET.
//
// requireAuth decides scope by method, which is right everywhere else: a GET
// discloses configuration a read token is entitled to. GET /sync/bundle is
// the exception — it carries every TSIG secret on the box, the same
// credentials #54 stopped handing to read tokens through GET /tsig-keys —
// so this one route asks the question the method cannot answer.
func (s *Server) requireWriteScope(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tokenFrom(r).Scope == "read" {
			errJSON(w, http.StatusForbidden, "write scope required")
			return
		}
		h(w, r)
	}
}

// syncVersion is the probe a replica makes every interval: the counter that
// says whether anything changed, and the id of the box that answered, so a
// replica pointed at itself by a misconfiguration can see that it is.
type syncVersion struct {
	ConfigVersion int64  `json:"config_version"`
	InstanceID    string `json:"instance_id"`
}

func (s *Server) handleSyncVersion(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, syncVersion{ConfigVersion: version, InstanceID: id})
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

// replicaRegister is the body of POST /sync/replicas. last_seen is not in
// it: a replica reporting when it thinks it called would let a clock skew
// decide whether it looks stale, so the main dates the registration itself.
type replicaRegister struct {
	InstanceID     string `json:"instance_id"`
	DNSAddr        string `json:"dns_addr"`
	VersionApplied int64  `json:"version_applied"`
}

func (s *Server) handleReplicaRegister(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeOr400[replicaRegister](w, r)
	if !ok {
		return
	}
	if body.InstanceID == "" {
		errJSON(w, http.StatusBadRequest, "instance_id required")
		return
	}
	addr := withClientHost(body.DNSAddr, s.clientIP(r))
	if err := hostPort(addr); err != nil {
		errJSON(w, http.StatusBadRequest, "dns_addr "+err.Error())
		return
	}
	if s.deps.Sync == nil {
		errJSON(w, http.StatusServiceUnavailable, "sync unavailable")
		return
	}
	err := s.deps.Sync.Register(r.Context(), Replica{
		InstanceID:     body.InstanceID,
		DNSAddr:        addr,
		VersionApplied: body.VersionApplied,
		LastSeen:       time.Now().UnixMilli(),
	})
	if err != nil {
		storeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// withClientHost fills in a registration whose dns_addr names a port and no
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

// errStorage marks a cross-field check that failed because the store did,
// rather than because the value did. The difference is the answer: a value
// this handler could not judge is a 503, not a 400 telling the operator
// their input was wrong and quoting a driver error at them.
var errStorage = errors.New("cross-field check could not read the store")

// validSyncKey checks sync.tsig_key_id: 0 (no key designated, which every
// main starts as) or a key that exists. On a replica the id is the main's,
// but ids are shared across the pair (§4.1), so the same lookup answers on
// both boxes.
func (s *Server) validSyncKey(ctx context.Context, value string) error {
	if err := nonNegInt(value); err != nil {
		return err
	}
	id, _ := strconv.ParseInt(value, 10, 64)
	if id == 0 {
		return nil
	}
	if _, found, err := s.deps.Store.TSIGKeys().Get(ctx, id); err != nil {
		return fmt.Errorf("%w: %w", errStorage, err)
	} else if !found {
		return errors.New("no TSIG key has that id")
	}
	return nil
}
