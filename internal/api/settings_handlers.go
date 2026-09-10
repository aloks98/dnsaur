package api

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aloks98/dnsaur/internal/filter"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/upstream"
)

// A validator returns why a value is refused, so the 400 can say it. It was
// a bool: every rejection read "invalid value for upstreams", which names
// the field and not the problem — unhelpful for a free-text setting with a
// grammar, and the reason the upstreams entry was never given a real check
// at all.
var editableSettings = map[string]func(string) error{
	"upstreams":             validUpstreams,
	"upstream.strategy":     oneOf("failover", "fastest", "race"),
	"blocking.mode":         oneOf("null-ip", "nxdomain"),
	"blocking.ttl":          nonNegInt,
	"cache.min_ttl":         nonNegInt,
	"cache.max_ttl":         nonNegInt,
	"cache.max_entries":     nonNegInt,
	"cache.serve_stale_for": nonNegInt,
	"lists.refresh_hours":   positiveInt,
	"qlog.retention_days":   nonNegInt,
	"qlog.privacy":          oneOf("full", "anon", "none"),
	"stats.retention_days":  positiveInt,
	"serve.dot.enabled":     boolean,
	"serve.dot.listen":      listenAddr,
	"serve.doh.enabled":     boolean,
	"serve.doh.listen":      listenAddr,
	"serve.tls.cert":        absPathOrEmpty,
	"serve.tls.key":         absPathOrEmpty,
}

// validUpstreams runs the same parser applySettings runs, so a value that
// saves is a value that will build a forwarder.
func validUpstreams(v string) error {
	_, err := upstream.ParseUpstreams(v)
	return err
}

func oneOf(vals ...string) func(string) error {
	return func(v string) error {
		if slices.Contains(vals, v) {
			return nil
		}
		return fmt.Errorf("must be one of: %s", strings.Join(vals, ", "))
	}
}

func nonNegInt(v string) error {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return errors.New("must be a whole number, zero or more")
	}
	return nil
}

// positiveInt is nonNegInt with zero refused, for lists.refresh_hours: it is
// the only integer setting that becomes a tick interval, and 0 is not a
// slower schedule but an interval time.NewTicker refuses to build. The key is
// restart-required, so a stored 0 does not fail the write that made it — it
// fails the next start, which is why this end has to refuse it.
func positiveInt(v string) error {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 {
		return errors.New("must be a whole number, one or more")
	}
	return nil
}

// boolean is the grammar for serve.*.enabled: exactly "true" or "false",
// not anything strconv.ParseBool would also accept ("1", "T", "on"), so the
// stored value is what a template or a JS `=== "true"` check expects.
func boolean(v string) error {
	if v == "true" || v == "false" {
		return nil
	}
	return errors.New("must be true or false")
}

// listenAddr checks the shape a net.Listen call needs — a host (possibly
// empty, meaning all interfaces) and a numeric port — without attempting to
// bind it. Whether the port is actually free is a runtime question the
// reconciler answers; checking it here would race the reconciler's own bind
// a moment later and reject a value for a reason that has nothing to do
// with the value itself.
func listenAddr(v string) error {
	_, port, err := net.SplitHostPort(v)
	if err != nil {
		return fmt.Errorf("must be a host:port address: %w", err)
	}
	n, err := strconv.Atoi(port)
	// 1, not 0. Port 0 is a valid argument to net.Listen and means "give me
	// whatever is free", so it saves, binds, and reports Listening: true on
	// an address no client was ever told and that changes on every restart.
	// There is no configuration in which that is what the operator meant.
	if err != nil || n < 1 || n > 65535 {
		return errors.New("port must be numeric, 1-65535")
	}
	return nil
}

// absPathOrEmpty is the grammar for serve.tls.cert/key: empty (no
// certificate configured yet) or an absolute path. It does not check that
// the file exists or is readable — that needs the *other* path too (a cert
// alone can't be loaded), which is exactly what validateCrossField is for.
func absPathOrEmpty(v string) error {
	if v == "" || filepath.IsAbs(v) {
		return nil
	}
	return errors.New("must be an absolute path")
}

// validateCrossField runs after the per-key validator, with the value that
// is about to be written and the store's current values for everything
// else. It exists because "enable DoT" is only valid against the state of
// serve.tls.cert and serve.tls.key, which a per-key validator cannot see.
//
// It also catches a broken certificate pair as soon as both halves would
// exist, whether or not a protocol is being enabled at that moment — the
// same "fail at the save that caused it" reasoning as the upstreams
// validator, rather than waiting until the unrelated later save that
// happens to flip enabled to true.
func validateCrossField(key, value string, current map[string]string) error {
	switch key {
	case "serve.dot.enabled", "serve.doh.enabled":
		if value != "true" {
			// Disabling never requires a certificate: an operator must
			// always be able to turn a protocol off, including when the
			// certificate has gone missing — which is exactly when they
			// most need to.
			return nil
		}
		return validCertPair(current["serve.tls.cert"], current["serve.tls.key"])
	case "serve.tls.cert", "serve.tls.key":
		cert, certKey := current["serve.tls.cert"], current["serve.tls.key"]
		if key == "serve.tls.cert" {
			cert = value
		} else {
			certKey = value
		}
		if cert == "" || certKey == "" {
			// Staging the configuration one path at a time, in either
			// order, is how an operator gets to a complete keypair before
			// enabling anything — that must not fail. Once a protocol is
			// enabled, though, an incomplete keypair is no longer a stage
			// on the way somewhere: it is the live configuration, and
			// accepting it takes the listener down at the next reconcile
			// with "certificate: stat : no such file or directory", an
			// error naming an empty path. Spec §6 lists "no certificate is
			// configured" as knowable at save time and so rejectable —
			// this is that check, at the save that would cause it.
			if current["serve.dot.enabled"] == "true" || current["serve.doh.enabled"] == "true" {
				return errors.New("turn DNS-over-TLS and DNS-over-HTTPS off before clearing the certificate")
			}
			return nil
		}
		return validCertPair(cert, certKey)
	default:
		return nil
	}
}

// validCertPair is the one place that decides a certificate configuration
// is usable: both paths set, and tls.LoadX509KeyPair — the exact call the
// reconciler will make to build a tls.Config — succeeds on them. A value
// that saves here is a value that will load there.
func validCertPair(cert, key string) error {
	if cert == "" || key == "" {
		return errors.New("set serve.tls.cert and serve.tls.key first")
	}
	if _, err := tls.LoadX509KeyPair(cert, key); err != nil {
		return err
	}
	return nil
}

func (s *Server) settingsRoutes() {
	s.route("GET /api/v1/settings", s.requireAuth(s.handleSettingsGet))
	s.route("PUT /api/v1/settings", s.requireAuth(s.handleSettingsPut))
	s.route("GET /api/v1/resolver/status", s.requireAuth(s.handleResolverStatus))
	s.route("GET /api/v1/blocking", s.requireAuth(s.handleBlockingGet))
	s.route("POST /api/v1/blocking/pause", s.requireAuth(s.handlePause))
	s.route("DELETE /api/v1/blocking/pause", s.requireAuth(s.handleResume))
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	all, err := s.deps.Store.Settings().All(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	// The rollup's watermark and the pause state are bookkeeping, not
	// configuration, and nothing may edit them — but they are named by key,
	// not by prefix: stats.* also holds stats.retention_days and blocking.*
	// holds blocking.mode and blocking.ttl, which are ordinary settings the
	// screen has to be able to read.
	for k := range all {
		if strings.HasPrefix(k, "instance.") || k == store.StatsWatermarkKey || k == filter.PausesKey {
			delete(all, k)
		}
	}
	writeJSON(w, http.StatusOK, all)
}

// resolverStatus is what the settings screen needs to know about the running
// resolver that is not a setting: the upstream-encryption downgrade (E1),
// what the two encrypted listeners are actually doing (Task 8's
// servingState), and the certificate's expiry — a flat object so the next
// such fact does not need a second endpoint.
type resolverStatus struct {
	// EncryptionDowngraded: the stored `upstreams` asked for tls:// or
	// https://, would not parse, and the server is resolving through the
	// hardcoded plaintext defaults instead. Queries are going out in the
	// clear while the settings page still shows the operator's encrypted
	// value, which is why this needs saying somewhere other than the log.
	EncryptionDowngraded bool `json:"encryption_downgraded"`
	// Reason is the parse failure, verbatim, or "" when nothing is wrong.
	Reason string `json:"reason"`
	// Serving is intent (from settings) alongside reality (whether the
	// socket actually bound), per encrypted protocol — they fail
	// independently, so this is never collapsed to one boolean.
	Serving servingStatus `json:"serving"`
	// Certificate is the loaded certificate's expiry, or absent entirely
	// when none has ever loaded successfully.
	Certificate *certificateStatus `json:"certificate,omitempty"`
}

// servingStatus carries DoT and DoH's api.ProtocolStatus side by side.
type servingStatus struct {
	DoT ProtocolStatus `json:"dot"`
	DoH ProtocolStatus `json:"doh"`
}

// certificateStatus is the loaded certificate's expiry as the API reports
// it. Only present in resolverStatus.Certificate when a certificate has
// actually loaded — see handleResolverStatus.
type certificateStatus struct {
	NotAfter     time.Time `json:"not_after"`
	ExpiringSoon bool      `json:"expiring_soon"`
}

// handleResolverStatus answers the one round trip the settings page makes
// for server state. Deps.ResolverStatus is nil in test servers with no App
// behind them; a server with no forwarder has downgraded nothing, no
// listeners to report, and no certificate loaded, so those all answer their
// zero values.
func (s *Server) handleResolverStatus(w http.ResponseWriter, r *http.Request) {
	var out resolverStatus
	if s.deps.ResolverStatus != nil {
		out.EncryptionDowngraded, out.Reason = s.deps.ResolverStatus.UpstreamDowngrade()
		dot, doh := s.deps.ResolverStatus.Serving()
		out.Serving = servingStatus{DoT: dot, DoH: doh}
		if notAfter, expiringSoon, ok := s.deps.ResolverStatus.CertExpiry(); ok {
			out.Certificate = &certificateStatus{NotAfter: notAfter, ExpiringSoon: expiringSoon}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

type settingPut struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	body, err := decode[settingPut](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return
	}
	validate, ok := editableSettings[body.Key]
	if !ok {
		errJSON(w, http.StatusBadRequest, "setting not editable: "+body.Key)
		return
	}
	if err := validate(body.Value); err != nil {
		// The prefix stays: it is what the existing suite and the web form
		// both key off. The reason is appended, not substituted.
		errJSON(w, http.StatusBadRequest, "invalid value for "+body.Key+": "+err.Error())
		return
	}
	// The per-key validator only ever sees this one value. Whether it is
	// coherent with the rest of the configuration — enabling DoT against a
	// certificate that is missing or broken — needs the current settings,
	// so that check reads the store before the write is accepted.
	current, err := s.deps.Store.Settings().All(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	if err := validateCrossField(body.Key, body.Value, current); err != nil {
		// Same shape as the per-key rejection above: one handler, one error
		// format, whether the field that failed is spelled body.Key.
		errJSON(w, http.StatusBadRequest, "invalid value for "+body.Key+": "+err.Error())
		return
	}
	if err := s.deps.Store.Settings().Set(r.Context(), body.Key, body.Value); err != nil {
		storeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type pauseReq struct {
	GroupID  int64 `json:"group_id"`
	ClientID int64 `json:"client_id"`
	Minutes  int   `json:"minutes"`
}

// pauseScope turns the two optional ids into the one scope a pause targets.
// Groups and clients are numbered from 1, so 0 means "not given" and neither
// id means the global pause — which is also what the shell's control has
// always sent as group_id 0.
//
// Both at once names no scope. Picking one would pause something the caller
// did not ask for and leave them with no way to tell which, so it is
// refused rather than guessed.
func pauseScope(groupID, clientID int64) (filter.PauseKind, int64, error) {
	switch {
	case groupID != 0 && clientID != 0:
		return "", 0, errors.New("send group_id or client_id, not both")
	case clientID != 0:
		return filter.PauseClient, clientID, nil
	case groupID != 0:
		return filter.PauseGroup, groupID, nil
	default:
		return filter.PauseGlobal, 0, nil
	}
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	body, err := decode[pauseReq](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Minutes < 1 || body.Minutes > 1440 {
		errJSON(w, http.StatusBadRequest, "minutes must be 1-1440")
		return
	}
	kind, id, err := pauseScope(body.GroupID, body.ClientID)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	s.deps.Engine.Pause(kind, id, time.Duration(body.Minutes)*time.Minute)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	kind, id, err := pauseScope(qInt(r, "group_id"), qInt(r, "client_id"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	// Only this scope's own pause. A client whose group is paused stays
	// paused, which is what "the later wins" means from the other side.
	s.deps.Engine.Pause(kind, id, 0)
	w.WriteHeader(http.StatusNoContent)
}

// blockingStatus is what a pause control reads. Scope names which of the
// three pauses is the one in force, because a control can only resume its
// own: a client row showing a countdown it inherited from its group needs
// to say so rather than offer a Resume that would change nothing.
type blockingStatus struct {
	PausedUntil int64            `json:"paused_until"`
	Scope       filter.PauseKind `json:"scope,omitempty"`
}

func (s *Server) handleBlockingGet(w http.ResponseWriter, r *http.Request) {
	groupID, clientID := qInt(r, "group_id"), qInt(r, "client_id")
	if _, _, err := pauseScope(groupID, clientID); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if clientID != 0 {
		// A client inherits its group's pause, so reporting the effective
		// one needs the group too. There is no lookup by id and the client
		// list is a handful of rows; an id naming no client leaves the
		// group at 0, which matches none, so the answer is the global pause
		// and the client's own.
		cs, err := s.deps.Store.Clients().Clients(r.Context())
		if err != nil {
			storeErr(w, err)
			return
		}
		for _, c := range cs {
			if c.ID == clientID {
				groupID = c.GroupID
				break
			}
		}
	}
	until, kind := s.deps.Engine.PausedUntil(groupID, clientID)
	var ms int64
	if !until.IsZero() {
		ms = until.UnixMilli()
	}
	writeJSON(w, http.StatusOK, blockingStatus{PausedUntil: ms, Scope: kind})
}
