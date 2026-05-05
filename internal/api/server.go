package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/sevenofnine/proton-calendar-bridge/internal/caldav"
	"github.com/sevenofnine/proton-calendar-bridge/internal/domain"
	"github.com/sevenofnine/proton-calendar-bridge/internal/protonapi"
	"github.com/sevenofnine/proton-calendar-bridge/internal/provider"
	"github.com/sevenofnine/proton-calendar-bridge/internal/security"
)

type Server struct {
	provider      provider.CalendarProvider
	auth          security.BearerAuth
	authenticator Authenticator
	log           *slog.Logger
	httpSrv       *http.Server
}

// Authenticator handles Proton session login. Nil means auth endpoints are disabled.
type Authenticator interface {
	Login(ctx context.Context, username, password string) (protonapi.Auth, error)
	Submit2FA(ctx context.Context, totpCode string) error
}

type Options struct {
	Provider      provider.CalendarProvider
	Auth          security.BearerAuth
	Authenticator Authenticator
	Logger        *slog.Logger
}

func New(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{provider: opts.Provider, auth: opts.Auth, authenticator: opts.Authenticator, log: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/capabilities", s.handleCapabilities)
	mux.HandleFunc("/v1/calendars", s.handleCalendars)
	mux.HandleFunc("/v1/events", s.handleEvents)
	mux.HandleFunc("/v1/events/create", s.handleCreateEvent)
	mux.HandleFunc("/v1/events/update", s.handleUpdateEvent)
	mux.HandleFunc("/v1/events/delete", s.handleDeleteEvent)
	mux.HandleFunc("/v1/auth/login", s.handleLogin)
	mux.HandleFunc("/v1/auth/2fa", s.handleSubmit2FA)

	// CalDAV endpoint — GNOME Calendar / evolution-data-server compatible.
	// Clients should point to: http://<host>/caldav/
	caldavHandler := caldav.New(opts.Provider, logger)
	mux.Handle("/caldav/", http.StripPrefix("/caldav", caldavHandler))

	s.httpSrv = &http.Server{Handler: s.wrapAuth(mux), ReadHeaderTimeout: 5 * time.Second}
	return s
}

func (s *Server) ServeTCP(ctx context.Context, bind string) error {
	if bind == "" {
		return errors.New("bind required")
	}
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return err
	}
	go s.shutdownOnContext(ctx)
	return s.httpSrv.Serve(ln)
}

func (s *Server) ServeUnix(ctx context.Context, path string) error {
	if path == "" {
		return errors.New("socket path required")
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	go s.shutdownOnContext(ctx)
	return s.httpSrv.Serve(ln)
}

func (s *Server) wrapAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /healthz is always public.
		// CalDAV paths (/caldav/*) use standard HTTP Basic auth which is handled
		// by the same Authorize check (Bearer token in the Authorization header).
		if r.URL.Path != "/healthz" && !isAuthPath(r.URL.Path) && !s.auth.Authorize(r) {
			// For CalDAV clients that don't send auth on the first request,
			// advertise WWW-Authenticate so they know to send credentials.
			if isCalDAVPath(r.URL.Path) {
				w.Header().Set("WWW-Authenticate", `Basic realm="Proton Calendar Bridge"`)
				w.Header().Set("DAV", "1, 3, calendar-access")
			}
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) shutdownOnContext(ctx context.Context) {
	<-ctx.Done()
	timeout, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.httpSrv.Shutdown(timeout)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "provider": s.provider.Name()})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	caps := provider.CapabilitySet{ReadOnly: true, WriteSupported: false, Notes: []string{"provider does not expose capability metadata"}}
	if cp, ok := s.provider.(provider.CapabilityProvider); ok {
		c, err := cp.Capabilities(r.Context())
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		caps = c
	}
	writeJSON(w, http.StatusOK, caps)
}

func (s *Server) handleCalendars(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	items, err := s.provider.ListCalendars(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	calendarID := r.URL.Query().Get("calendar_id")
	from, _ := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
	to, _ := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
	items, err := s.provider.ListEvents(r.Context(), calendarID, from, to)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleCreateEvent(w http.ResponseWriter, r *http.Request) {
	s.handleMutation(w, r, func(ctx context.Context, payload mutationRequest) (any, error) {
		return s.provider.CreateEvent(ctx, payload.Mutation)
	})
}

func (s *Server) handleUpdateEvent(w http.ResponseWriter, r *http.Request) {
	s.handleMutation(w, r, func(ctx context.Context, payload mutationRequest) (any, error) {
		return s.provider.UpdateEvent(ctx, payload.EventID, payload.Mutation)
	})
}

func (s *Server) handleDeleteEvent(w http.ResponseWriter, r *http.Request) {
	s.handleMutation(w, r, func(ctx context.Context, payload mutationRequest) (any, error) {
		return map[string]string{"event_id": payload.EventID}, s.provider.DeleteEvent(ctx, payload.EventID)
	})
}

type mutationRequest struct {
	EventID  string               `json:"event_id"`
	Mutation domain.EventMutation `json:"mutation"`
}

func (s *Server) handleMutation(w http.ResponseWriter, r *http.Request, run func(context.Context, mutationRequest) (any, error)) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var payload mutationRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	out, err := run(r.Context(), payload)
	if err != nil {
		if errors.Is(err, provider.ErrNotSupported) {
			writeErr(w, http.StatusNotImplemented, err.Error())
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.authenticator == nil {
		writeErr(w, http.StatusNotImplemented, "auth not available for this provider")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	result, err := s.authenticator.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		s.log.Error("login failed", "error", err)
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	s.log.Info("login succeeded", "username", req.Username)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleSubmit2FA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.authenticator == nil {
		writeErr(w, http.StatusNotImplemented, "auth not available for this provider")
		return
	}
	var req struct {
		TOTP string `json:"totp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := s.authenticator.Submit2FA(r.Context(), req.TOTP); err != nil {
		s.log.Error("2fa failed", "error", err)
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	s.log.Info("2fa succeeded")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// isCalDAVPath returns true if the request path is under /caldav/.
func isCalDAVPath(path string) bool {
	return len(path) >= 7 && path[:7] == "/caldav"
}

// isAuthPath returns true for /v1/auth/* endpoints which must be reachable
// without a bearer token (you need to log in before you have a token).
func isAuthPath(path string) bool {
	return len(path) >= 9 && path[:9] == "/v1/auth/"
}
