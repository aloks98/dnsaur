package api

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	s.route("POST /api/v1/backup", s.requireAuth(s.handleBackup))
	s.route("GET /api/v1/blocking", s.requireAuth(s.handleBlockingGet))
	s.route("POST /api/v1/blocking/pause", s.requireAuth(s.handlePause))
	s.route("DELETE /api/v1/blocking/pause", s.requireAuth(s.handleResume))
}

// handleBackup writes a copy of the database into <data_dir>/backups and
// answers with the file it wrote — the path is the point, since copying it
// off the box is what the operator does next and nothing else says where it
// went. Location carries the same path: it is where the new thing is, even
// though no endpoint serves it (the file is on the server's disk, and an
// endpoint that streamed the whole database out over HTTP would be a
// different feature with a different set of questions).
//
// No schedule and no retention: the file is named for the moment it was
// taken and left alone after that. A timer that fills a disk on its own is
// worse than a button.
//
// The 409 is postgres, which the store reports by refusing (store.ErrNoBackup)
// — it has pg_dump and does not need this process to reimplement it.
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	path, size, err := s.deps.Store.Backup(r.Context(), filepath.Join(s.deps.DataDir, "backups"))
	switch {
	case errors.Is(err, store.ErrNoBackup):
		errJSON(w, http.StatusConflict, err.Error())
	case err != nil:
		// Named rather than folded into "storage unavailable": every failure
		// here is about the filesystem — a full disk, a data_dir the process
		// cannot write — and the operator needs to know it was the copy that
		// failed, not the database.
		slog.Error("backup failed", "err", err)
		errJSON(w, http.StatusInternalServerError, "couldn't write the backup: "+err.Error())
	default:
		created(w, path, backupResult{Path: path, Bytes: size})
	}
}

type backupResult struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
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

// protocolEnabledKey reports whether key turns an encrypted protocol on or
// off — the two keys whose validity depends on what the certificate keys
// hold, and the reason a multi-key write has an order at all.
func protocolEnabledKey(key string) bool {
	return key == "serve.dot.enabled" || key == "serve.doh.enabled"
}

// settingsPhases is the order a multi-key write is validated and applied
// in, and it is the dashboard's own five phases (SAVE_PHASES in
// web/src/pages/settings.tsx) moved to the end that can enforce them:
//
//  1. Disables first. Nothing below can be refused for a protocol that is
//     already off.
//  2. serve.tls.cert, then
//  3. serve.tls.key — one after the other, because the pair is only checked
//     once both halves are present, and a mismatched pair sent together
//     would otherwise be judged against whatever was stored before.
//  4. Everything else. Independent of each other and of the certificate.
//  5. Enables last, against a certificate that is now in place.
//
// Every key falls in exactly one phase: it either turns a protocol on or
// off (1 or 5), is one half of the certificate (2 or 3), or is neither (4).
var settingsPhases = []func(key, value string) bool{
	func(k, v string) bool { return protocolEnabledKey(k) && v != "true" },
	func(k, _ string) bool { return k == "serve.tls.cert" },
	func(k, _ string) bool { return k == "serve.tls.key" },
	func(k, _ string) bool { return !protocolEnabledKey(k) && k != "serve.tls.cert" && k != "serve.tls.key" },
	func(k, v string) bool { return protocolEnabledKey(k) && v == "true" },
}

// orderSettings sorts the keys of a write into settingsPhases order, sorted
// within each phase so the same body always fails on the same key.
func orderSettings(values map[string]string) []string {
	keys := slices.Sorted(maps.Keys(values))
	ordered := make([]string, 0, len(keys))
	for _, inPhase := range settingsPhases {
		for _, k := range keys {
			if inPhase(k, values[k]) {
				ordered = append(ordered, k)
			}
		}
	}
	return ordered
}

// settingsWrite reads the body of PUT /settings, which takes two shapes: a
// map of settings, and the original single `{"key": ..., "value": ...}`.
//
// They are told apart by the "key" entry, since no editable setting is
// called that. A body carrying it and nothing else (or only "value" beside
// it) is the single-key form, so `{"key": "cache.min_ttl"}` still answers a
// complaint about the value rather than "setting not editable: key".
//
// Every value is a string, numbers included — that is what the settings
// table holds — so a number or a null is a decode failure and answers the
// same "invalid json" every other endpoint does, rather than being coerced
// into a value nobody sent.
func settingsWrite(w http.ResponseWriter, r *http.Request) (map[string]string, bool) {
	raw, ok := decodeOr400[map[string]any](w, r)
	if !ok {
		return nil, false
	}
	values := make(map[string]string, len(raw))
	for k, v := range raw {
		sv, isString := v.(string)
		if !isString {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return nil, false
		}
		values[k] = sv
	}
	if key, single := values["key"]; single && len(values) <= 2 {
		return map[string]string{key: values["value"]}, true
	}
	return values, true
}

// handleSettingsPut writes one setting or several.
//
// Every key is validated before any of them is written, and the whole write
// then lands in one transaction with one config-version bump. Half-applying
// a rejected body would leave the caller told "no" and the server in a
// state neither of them chose; bumping per key would reconfigure the
// running server once per field of a six-field save.
//
// The cross-field check runs against the store's current values *updated as
// the phases go*, which is what lets a certificate and the enable that
// depends on it travel in one request: by the time the enable is judged,
// the simulated state already holds the paths that are about to be written.
func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	values, ok := settingsWrite(w, r)
	if !ok {
		return
	}
	if len(values) == 0 {
		errJSON(w, http.StatusBadRequest, "no settings to write")
		return
	}
	// The per-key validators only ever see one value each. Whether the
	// result is coherent — enabling DoT against a certificate that is
	// missing or broken — needs the rest of the configuration, so the
	// current values are read before anything is accepted.
	current, err := s.deps.Store.Settings().All(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	for _, key := range orderSettings(values) {
		validate, editable := editableSettings[key]
		if !editable {
			errJSON(w, http.StatusBadRequest, "setting not editable: "+key)
			return
		}
		value := values[key]
		if err := validate(value); err != nil {
			// The prefix stays: it is what the existing suite and the web
			// form both key off. The reason is appended, not substituted.
			errJSON(w, http.StatusBadRequest, "invalid value for "+key+": "+err.Error())
			return
		}
		if err := validateCrossField(key, value, current); err != nil {
			// Same shape as the per-key rejection above: one handler, one
			// error format, whether or not the field that failed is the one
			// the message names.
			errJSON(w, http.StatusBadRequest, "invalid value for "+key+": "+err.Error())
			return
		}
		current[key] = value
	}
	if err := s.deps.Store.Settings().SetMany(r.Context(), values); err != nil {
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
