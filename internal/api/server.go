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
	Version       string
	// Static serves the embedded web dashboard on non-/api paths. Nil
	// disables it (e.g. tests that don't care about the SPA).
	Static fs.FS
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
}

func New(d Deps) *Server {
	s := &Server{deps: d}
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
			if rec := recover(); rec != nil {
				slog.Error("api panic", "path", r.URL.Path, "panic", rec)
				errJSON(w, http.StatusInternalServerError, "internal error")
			}
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
	switch {
	case errors.Is(err, store.ErrNotFound):
		errJSON(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrInUse):
		errJSON(w, http.StatusConflict, "resource in use")
	case errors.Is(err, store.ErrDuplicate):
		errJSON(w, http.StatusConflict, dupMsg)
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

type ctxKey int

const userKey ctxKey = 0

func userFrom(r *http.Request) store.User {
	u, _ := r.Context().Value(userKey).(store.User)
	return u
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var token string
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimPrefix(h, "Bearer ")
		} else if c, err := r.Cookie("dnsaur_session"); err == nil {
			token = c.Value
		}
		if token == "" {
			errJSON(w, http.StatusUnauthorized, "authentication required")
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
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	}
}
