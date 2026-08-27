// Package api is the gym system's HTTP layer: the embedded console at "/",
// the /gym/* REST + SSE API, health/metrics probes, and a manual door-open
// endpoint for the reception desk. Everything is same-origin and API-key
// protected (probes and the console shell excepted).
//
// Endpoints:
//
//	GET    /                        gym console (embedded web UI)
//	GET    /healthz                 liveness
//	GET    /readyz                  readiness (panel link state)
//	GET    /config                  non-sensitive runtime flags for the UI
//	GET    /metrics                 Prometheus metrics
//	POST   /doors/{n}/open          {"seconds":5} manual gate open (reception)
//	*      /gym/...                 gym membership API (see gym.go)
package api

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/awadi/zk-c3-middleware/internal/c3"
	"github.com/awadi/zk-c3-middleware/internal/gym"
)

// Options configures the HTTP server. All values come from config (no defaults
// hardcoded here).
type Options struct {
	APIKey          string
	CORSOrigins     []string
	EventBufferSize int
	DoorDefaultSecs int
	AllowWrites     bool
	PanelID         string
	Version         string
	Logger          *slog.Logger
}

type Server struct {
	panel c3.Panel
	opt   Options
	log   *slog.Logger

	ready func() bool // reports panel-link readiness; nil => always ready

	gym       *gym.Store
	gymEngine *gym.Engine

	// metrics
	reqTotal   atomic.Int64
	eventTotal atomic.Int64
	sseActive  atomic.Int64
}

// SetReadiness registers a probe used by GET /readyz (e.g. panel link state).
func (s *Server) SetReadiness(fn func() bool) { s.ready = fn }

// NoteEvent counts one panel event (metrics); called from the main fan-out.
func (s *Server) NoteEvent(c3.Event) { s.eventTotal.Add(1) }

func New(panel c3.Panel, opt Options) *Server {
	if opt.EventBufferSize <= 0 {
		opt.EventBufferSize = 64
	}
	if opt.DoorDefaultSecs <= 0 {
		opt.DoorDefaultSecs = 5
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	return &Server{panel: panel, opt: opt, log: opt.Logger}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /config", s.handleConfig)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("POST /doors/{n}/open", s.handleOpenDoor)

	// Gym membership subsystem (members, subscriptions, cards, gate kiosk).
	s.gymRoutes(mux)

	// The gym console is the site root (same-origin with the API).
	mux.Handle("GET /", uiHandler())

	return s.cors(s.auth(logMW(s.log, mux, &s.reqTotal)))
}

// isPublic reports whether a path bypasses API-key auth (the console shell +
// probes; every /gym/* data route stays protected).
func isPublic(path string) bool {
	switch path {
	case "/", "/index.html", "/healthz", "/readyz", "/metrics", "/config", "/favicon.ico":
		return true
	}
	return false
}

// ---- handlers ----

// handleConfig exposes non-sensitive runtime flags for the web UI.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	ready := s.ready == nil || s.ready()
	writeJSON(w, 200, map[string]any{
		"allow_writes":     s.opt.AllowWrites,
		"panel_id":         s.opt.PanelID,
		"version":          s.opt.Version,
		"panel_connected":  ready,
		"door_default_sec": s.opt.DoorDefaultSecs,
	})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.ready != nil && !s.ready() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "panel_disconnected"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ready"})
}

func (s *Server) handleOpenDoor(w http.ResponseWriter, r *http.Request) {
	door, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad door number"})
		return
	}
	var body struct {
		Seconds int `json:"seconds"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Seconds <= 0 {
		body.Seconds = s.opt.DoorDefaultSecs
	}
	err = s.panel.OpenDoor(r.Context(), door, body.Seconds)
	respond(w, map[string]any{"status": "ok", "door": door, "seconds": body.Seconds}, err)
}

// handleMetrics exposes Prometheus-format metrics (zero external deps).
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ready := 0
	if s.ready == nil || s.ready() {
		ready = 1
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP zc3_http_requests_total Total HTTP requests.\n# TYPE zc3_http_requests_total counter\nzc3_http_requests_total %d\n", s.reqTotal.Load())
	fmt.Fprintf(w, "# HELP zc3_events_total Total access events ingested.\n# TYPE zc3_events_total counter\nzc3_events_total %d\n", s.eventTotal.Load())
	fmt.Fprintf(w, "# HELP zc3_sse_subscribers Active SSE subscribers.\n# TYPE zc3_sse_subscribers gauge\nzc3_sse_subscribers %d\n", s.sseActive.Load())
	fmt.Fprintf(w, "# HELP zc3_panel_connected Panel link state (1=up).\n# TYPE zc3_panel_connected gauge\nzc3_panel_connected %d\n", ready)
}

// ---- middleware & helpers ----

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opt.APIKey != "" && !isPublic(r.URL.Path) {
			// Accept the key via Authorization header or ?api_key= (for EventSource).
			ok := subtleCompare(r.Header.Get("Authorization"), "Bearer "+s.opt.APIKey) ||
				subtleCompare(r.URL.Query().Get("api_key"), s.opt.APIKey)
			if !ok {
				writeJSON(w, 401, map[string]string{"error": "unauthorized"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// cors applies a permissive-but-scoped CORS policy for the configured origins.
func (s *Server) cors(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range s.opt.CORSOrigins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && (allowed["*"] || allowed[origin]) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func logMW(log *slog.Logger, next http.Handler, counter *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.Add(1)
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Info("http", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start).String())
	})
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, v)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid json: " + err.Error()})
		return false
	}
	return true
}

// subtleCompare reports whether got equals want in constant time.
func subtleCompare(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
