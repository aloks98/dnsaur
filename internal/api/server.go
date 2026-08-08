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
	"time"

	"github.com/aloks98/dnsaur/internal/auth"
	"github.com/aloks98/dnsaur/internal/filter"
	"github.com/aloks98/dnsaur/internal/qlog"
	"github.com/aloks98/dnsaur/internal/store"
)

//go:embed openapi.yaml
var openapiDoc []byte

type Reloader interface {
	ReloadClients(ctx context.Context) error
	ReloadZones(ctx context.Context) error
	RefreshFilters(ctx context.Context) error
}

type Deps struct {
	Store     store.Store
	Auth      *auth.Service
	Engine    *filter.Engine
	Reloader  Reloader
	Logger    *qlog.Logger
	Refresher *filter.Refresher
	Version   string
	// Static serves the embedded web dashboard on non-/api paths. Nil
	// disables it (e.g. tests that don't care about the SPA).
	Static fs.FS
}

type Server struct {
	deps Deps
	mux  *http.ServeMux
}

func New(d Deps) *Server {
	s := &Server{deps: d, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.deps.Version})
	})
	s.mux.HandleFunc("GET /api/v1/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
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
	// Later tasks append their routes here.
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		errJSON(w, http.StatusNotFound, "not found")
	})
	// The SPA owns everything else. More specific patterns above (including
	// the "/api/" catch-all) take precedence, so this only ever sees
	// non-API paths.
	if s.deps.Static != nil {
		s.mux.Handle("/", StaticHandler(s.deps.Static))
	}
}

func (s *Server) Handler() http.Handler {
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
