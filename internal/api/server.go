package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/aloks98/dnsaur/internal/auth"
	"github.com/aloks98/dnsaur/internal/filter"
	"github.com/aloks98/dnsaur/internal/qlog"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/zones"
)

//go:embed openapi.yaml
var openapiDoc []byte

type Reloader interface {
	ReloadClients(ctx context.Context) error
	ReloadZones(ctx context.Context) error
	RefreshFilters(ctx context.Context) error
	// NotifyZones wakes the outbound NOTIFY pass. See zones.Notifier.Wake:
	// it is promptness, never correctness, so a handler that forgets this
	// call only delays delivery by one tick rather than losing it.
	NotifyZones()
}

// ZoneRefresher transfers one secondary zone on demand — the manual path
// behind POST /zones/{id}/refresh, as opposed to the SOA schedule the same
// component runs on its own.
//
// An interface rather than *zones.Refresher (which is what Deps.Refresher
// does for filter lists) because this one is reachable from a handler, and a
// handler that can only be exercised by standing up a real Transferrer with
// a real primary to pull from is a handler nothing tests. The concrete type
// satisfies it as written.
type ZoneRefresher interface {
	Refresh(ctx context.Context, zoneID int64) (zones.TransferResult, error)
}

// ResolverStatus reports state of the running resolver that the dashboard
// has to show but that is not a setting. It started (E1) as exactly one
// fact — whether the forwarder in use is the hardcoded plaintext fallback,
// installed because the stored `upstreams` value asked for an encrypted
// transport and would not parse — and that doc comment's "today" was an
// invitation, not a boundary: it now also carries whether the encrypted
// listeners actually bound, and whether the certificate they serve is
// close to expiring.
//
// It is not folded into GET /settings on purpose. That response is a flat
// key -> value map of *settings*, pure enough that handleSettingsGet strips
// the internal keys out of it; this is server state, and a value in that map
// that no PUT could ever write would be a different kind of thing wearing
// the same shape.
//
// An interface, and nil-tolerant (see handleResolverStatus): internal/app is
// what implements it, and internal/app imports this package, so the concrete
// type cannot be named here.
type ResolverStatus interface {
	// UpstreamDowngrade reports whether encrypted upstreams were replaced by
	// plaintext defaults, and why.
	UpstreamDowngrade() (active bool, reason string)
	// Serving reports each encrypted protocol's intent (from settings) and
	// reality (whether a socket is actually open) separately — DoT and DoH
	// fail independently (one can bind while the other, on a privileged
	// port, is refused), so a single aggregated boolean would leave the
	// operator unable to tell which one is down.
	Serving() (dot, doh ProtocolStatus)
	// CertExpiry reports the loaded certificate's NotAfter and whether it
	// falls within the warning threshold. ok is false when no certificate
	// has ever loaded successfully — a fresh install with nothing configured
	// yet, or a configured pair that has never once read cleanly — which is
	// a different fact from "expires soon" and must not be reported as one.
	CertExpiry() (notAfter time.Time, expiringSoon, ok bool)
}

// ProtocolStatus is one encrypted protocol's status as the API reports it.
// Declared here, not in internal/app, because ResolverStatus is an api
// interface and internal/app imports this package — an import cycle if the
// interface referred to a type declared over there. internal/app converts
// its own unexported protocolState (serve.go) into this on the way out.
type ProtocolStatus struct {
	Enabled   bool   `json:"enabled"`
	Listening bool   `json:"listening"`
	Addr      string `json:"addr"`
	Err       string `json:"error,omitempty"`
}

type Deps struct {
	Store    store.Store
	Auth     *auth.Service
	Engine   *filter.Engine
	Reloader Reloader
	Logger   *qlog.Logger
	// Refresher keeps filter lists current; ZoneRefresher keeps secondary
	// zones current. Different schedules, different stores, same shape — see
	// App's own pair in internal/app/app.go.
	Refresher *filter.Refresher
	// ZoneRefresher may be nil, and is in every test server that has no
	// business transferring zones. The handler answers 503 rather than
	// panicking; see handleZoneRefresh.
	ZoneRefresher ZoneRefresher
	// ResolverStatus may be nil, and is in every test server with no App
	// behind it. The handler then answers "nothing wrong", which is the
	// truthful answer for a server that has no forwarder to have downgraded.
	ResolverStatus ResolverStatus
	Version        string
	// Static serves the embedded web dashboard on non-/api paths. Nil
	// disables it (e.g. tests that don't care about the SPA).
	Static fs.FS
	// TrustedProxies are the networks a reverse proxy in front of this
	// server may connect from (config.Config.TrustedProxies). A request
	// arriving from one of them has its X-Forwarded-* headers believed;
	// every other request does not, because those headers are just headers.
	// Empty — the default — trusts nothing.
	TrustedProxies []netip.Prefix
}

// routeReg is one handler registered through route(): the pattern handed to
// the mux, and the handler itself.
type routeReg struct {
	pattern string
	handler http.HandlerFunc
}

type Server struct {
	deps Deps
	// routes records every handler registered through route(), in
	// registration order, so the OpenAPI test (openapi_test.go) can assert
	// the served spec against the routes that actually exist instead of
	// against a second hand-maintained list that can drift from both.
	// http.ServeMux exposes no way to enumerate its own patterns, so this
	// is the only route to that information.
	//
	// mux does not exist until Handler() builds it from this slice (see
	// buildMux). That is deliberate: New()/registerRoutes() only ever call
	// route(), never touch a mux directly, so there is no live *http.ServeMux
	// in scope during route registration for a stray s.mux.HandleFunc to
	// reach for. Such a call — bypassing route(), and so the recording this
	// test depends on — nil-panics instead of silently registering an
	// undocumented-and-invisible endpoint. A test fixture that needs an
	// extra route (see server_test.go) calls route() too, the same as
	// production code; it's the only supported way in.
	routes      []routeReg
	mux         *http.ServeMux
	buildMuxOne sync.Once
	// attempts throttles the two unauthenticated endpoints that cost an
	// argon2id hash. See attemptLimiter in auth_handlers.go.
	attempts *attemptLimiter
}

func New(d Deps) *Server {
	s := &Server{deps: d, attempts: newAttemptLimiter()}
	s.registerRoutes()
	return s
}

// route registers a handler, remembering both the pattern and the handler —
// see the routes field. It does not touch a mux; that only happens once, in
// buildMux, the first time Handler() is called. It is the only supported way
// to add an endpoint.
//
// Ordering constraint: a route() call after the first Handler() call is
// recorded — it still shows up to the OpenAPI test — but never served,
// because buildMux has already run and won't run again. In practice this
// means all route() calls belong in registerRoutes (or something it calls),
// finished before anything ever calls Handler(); that's what every
// production call site and every test fixture already does.
func (s *Server) route(pattern string, h http.HandlerFunc) {
	s.routes = append(s.routes, routeReg{pattern, h})
}

func (s *Server) registerRoutes() {
	s.route("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.deps.Version})
	})
	s.route("GET /api/v1/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(openapiDoc)
	})
	s.authRoutes()
	s.settingsRoutes()
	s.clientsRoutes()
	s.filtersRoutes()
	s.queriesRoutes()
	s.tokensRoutes()
	s.zonesRoutes()
	s.zoneRecordsRoutes()
	s.zoneFileRoutes()
	s.tsigKeysRoutes()
	s.notifiesRoutes()
	// Later tasks append their routes here.
	//
	// This catch-all is registered through route() like everything else —
	// it is one of the 51 HandleFunc call sites — but the OpenAPI test
	// excludes the bare "/api/" pattern from the spec comparison: it has no
	// method verb and does not name a resource, it only turns any
	// unmatched "/api/..." request into a JSON 404 instead of falling
	// through to the SPA mount below. There is nothing for a spec to
	// document.
	s.route("/api/", func(w http.ResponseWriter, r *http.Request) {
		errJSON(w, http.StatusNotFound, "not found")
	})
}

// buildMux turns the recorded routes slice into a real *http.ServeMux. It
// runs at most once (see Handler): registration must be finished — every
// route() call made — before the mux exists, which is what keeps a stray
// s.mux.HandleFunc from bypassing route()'s recording (see the routes field
// doc).
func (s *Server) buildMux() {
	s.buildMuxOne.Do(func() {
		mux := http.NewServeMux()
		for _, rt := range s.routes {
			mux.HandleFunc(rt.pattern, rt.handler)
		}
		// The SPA owns everything else. More specific patterns above
		// (including the "/api/" catch-all) take precedence, so this only
		// ever sees non-API paths. Deliberately not routed through route():
		// it serves the dashboard, not a JSON API endpoint, so it has no
		// place in the OpenAPI spec either.
		if s.deps.Static != nil {
			mux.Handle("/", StaticHandler(s.deps.Static))
		}
		s.mux = mux
	})
}

func (s *Server) Handler() http.Handler {
	s.buildMux()
	var h http.Handler = s.mux
	h = requestLog(h)
	h = recoverPanic(h)
	return h
}

func recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is the one panic value that is not a
			// failure: net/http defines it as "abandon this response
			// quietly", recovers it itself, and expects nothing else to be
			// written. Catching it here logged a panic that never happened
			// and then tried to write a 500 onto a connection the handler
			// had deliberately given up on.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			slog.Error("api panic", "path", r.URL.Path, "panic", rec)
			errJSON(w, http.StatusInternalServerError, "internal error")
		}()
		next.ServeHTTP(w, r)
	})
}

func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		slog.Debug("api", "method", r.Method, "path", r.URL.Path, "status", sw.status, "dur", time.Since(start))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func storeErr(w http.ResponseWriter, err error) {
	storeErrDup(w, err, "already exists")
}

// storeErrDup is storeErr with a caller-supplied message for the duplicate
// case, so a uniqueness violation can name the field the user actually
// collided on instead of answering "storage unavailable" to what is really
// user input error.
func storeErrDup(w http.ResponseWriter, err error, dupMsg string) {
	storeErrDupRef(w, err, dupMsg, "")
}

// storeErrDupRef is storeErrDup with the reference case named too, for a
// write whose foreign key came from the request *body*: refMsg says which
// field named a row that does not exist, and the answer is 400.
//
// A reference failure has no single right status, which is why the choice
// is the call site's. When the only reference in the write is the one the
// URL named — POST /groups/{id}/rules, whose group comes from the path —
// the missing row *is* the resource the request addressed, and 404 is the
// answer; that is what refMsg == "" selects. When it came from the body
// there is no missing resource, only a bad field, and answering 404 would
// claim the URL's own resource was gone.
func storeErrDupRef(w http.ResponseWriter, err error, dupMsg, refMsg string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		errJSON(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrInUse):
		errJSON(w, http.StatusConflict, "resource in use")
	case errors.Is(err, store.ErrDuplicate):
		errJSON(w, http.StatusConflict, dupMsg)
	case errors.Is(err, store.ErrReference):
		if refMsg == "" {
			errJSON(w, http.StatusNotFound, "not found")
			return
		}
		errJSON(w, http.StatusBadRequest, refMsg)
	default:
		errJSON(w, http.StatusServiceUnavailable, "storage unavailable")
	}
}

func decode[T any](r *http.Request) (T, error) {
	var v T
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, err
	}
	return v, nil
}

// decodeOr400 decodes the body and answers 400 "invalid json" itself when it
// cannot, returning ok == false so the handler simply returns.
//
// It exists to keep that answer one answer. Handlers used to fold the decode
// error into the field check — `if err != nil || body.Name == ""` — so
// `{"name": 5}` came back as "name required", which is a claim about a field
// that was in fact sent, and every endpoint documented a different string for
// the same failure. Field validation belongs after this call, where it can
// say something true.
func decodeOr400[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	v, err := decode[T](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return v, false
	}
	return v, true
}

type ctxKey int

const (
	userKey ctxKey = iota
	// tokenKey carries the credential the request authenticated with, not
	// just the account behind it. A handler that has to distinguish one
	// session from another (revoking every session but the caller's) or one
	// credential from another (a read-scoped token asking for a secret)
	// cannot get that from the user.
	tokenKey
)

// bearerPrefix is the Authorization scheme, with its separating space.
// Compared case-insensitively — see requireAuth.
const bearerPrefix = "Bearer "

func userFrom(r *http.Request) store.User {
	u, _ := r.Context().Value(userKey).(store.User)
	return u
}

// tokenFrom returns the token the request authenticated with. The zero
// value comes back on an unauthenticated route, which no caller of this
// reaches: every one of them is behind requireAuth.
func tokenFrom(r *http.Request) store.AuthToken {
	t, _ := r.Context().Value(tokenKey).(store.AuthToken)
	return t
}

// remoteIP is the address the request's connection actually came from —
// the socket's peer, never a header. False for anything that is not an
// address at all (a unix socket), which no forwarding decision may treat as
// trusted.
func remoteIP(r *http.Request) (netip.Addr, bool) {
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr().Unmap(), true
	}
	if a, err := netip.ParseAddr(r.RemoteAddr); err == nil {
		return a.Unmap(), true
	}
	return netip.Addr{}, false
}

func (s *Server) isTrustedProxy(addr netip.Addr) bool {
	for _, p := range s.deps.TrustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// fromTrustedProxy reports whether this request's X-Forwarded-* headers may
// be believed: it arrived from one of the networks the operator named in
// trusted_proxies.
func (s *Server) fromTrustedProxy(r *http.Request) bool {
	addr, ok := remoteIP(r)
	return ok && s.isTrustedProxy(addr)
}

// clientIP is the address a request is attributed to when counting login
// attempts. Behind a proxy every request arrives from the same socket, so
// counting by RemoteAddr alone would put the whole install on one budget
// and let one attacker lock everybody out — which is why this reads
// X-Forwarded-For, and why it only does so from a trusted source.
//
// The list is walked right to left, skipping hops that are themselves
// trusted proxies: entries a client prepended are to the left of the ones
// the proxies appended, so the rightmost untrusted address is the nearest
// hop nobody could have forged.
func (s *Server) clientIP(r *http.Request) string {
	addr, ok := remoteIP(r)
	if !ok {
		return r.RemoteAddr
	}
	if !s.isTrustedProxy(addr) {
		return addr.String()
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		if hop = hop.Unmap(); !s.isTrustedProxy(hop) {
			return hop.String()
		}
	}
	return addr.String()
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var token string
		var viaCookie bool
		// RFC 9110 §11.1: the scheme name is case-insensitive, so
		// "bearer <token>" is the same request as "Bearer <token>". A
		// case-sensitive prefix answered it 401 "authentication required",
		// which reads as a rejected credential rather than a rejected
		// spelling.
		if h := r.Header.Get("Authorization"); len(h) > len(bearerPrefix) && strings.EqualFold(h[:len(bearerPrefix)], bearerPrefix) {
			token = h[len(bearerPrefix):]
		} else if c, err := r.Cookie("dnsaur_session"); err == nil {
			token, viaCookie = c.Value, true
		}
		if token == "" {
			errJSON(w, http.StatusUnauthorized, "authentication required")
			return
		}
		// CSRF, behind SameSite=Strict rather than instead of it. The
		// cookie is ambient — a browser attaches it to a cross-origin form
		// post without being asked — so a write that announces itself as
		// cross-site is refused. Sec-Fetch-Site is set by the browser and
		// cannot be forged by page script; its absence means a client that
		// does not send fetch metadata (curl, a script), which is not a
		// cross-site request and is left alone. A bearer token is not
		// ambient, so it is not checked at all.
		if viaCookie && r.Method != http.MethodGet && r.Method != http.MethodHead &&
			strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
			errJSON(w, http.StatusForbidden, "cross-site request")
			return
		}
		u, at, err := s.deps.Auth.Authenticate(r.Context(), token)
		if err != nil {
			errJSON(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if at.Scope == "read" && r.Method != http.MethodGet && r.Method != http.MethodHead {
			errJSON(w, http.StatusForbidden, "read-only token")
			return
		}
		ctx := context.WithValue(r.Context(), userKey, u)
		next(w, r.WithContext(context.WithValue(ctx, tokenKey, at)))
	}
}
