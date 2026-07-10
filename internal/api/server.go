// Package api exposes the C3 panel as a REST + SSE HTTP service.
//
// Endpoints:
//
//	GET    /healthz                 liveness
//	GET    /device                  panel identity/params
//	GET    /params?keys=a,b         read device params
//	PUT    /params                  {"k":"v",...} set device params
//	POST   /time/sync               sync panel clock to server time
//	GET    /cards                   list enrolled cards
//	POST   /cards                   enroll/update a card (Card JSON)
//	DELETE /cards/{cardNo}          revoke a card
//	POST   /doors/{n}/open          {"seconds":5} pulse a lock relay
//	POST   /aux/{n}                 {"on":true,"seconds":5} drive AUX output
//	POST   /alarm/cancel            cancel alarm state
//	GET    /events                  Server-Sent Events realtime stream
package api

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/awadi/zk-c3-middleware/internal/access"
	"github.com/awadi/zk-c3-middleware/internal/c3"
	"github.com/awadi/zk-c3-middleware/internal/store"
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
	audit *store.Store

	ready func() bool // reports panel-link readiness; nil => always ready

	roles  *access.Store
	engine *access.Engine

	// metrics
	reqTotal   atomic.Int64
	eventTotal atomic.Int64
	sseActive  atomic.Int64

	mu   sync.Mutex
	subs map[chan c3.Event]struct{} // SSE subscribers
}

// SetAudit attaches the audit store used by GET /events/history.
func (s *Server) SetAudit(a *store.Store) { s.audit = a }

// SetAccess attaches the roles store + enforcement engine.
func (s *Server) SetAccess(rs *access.Store, e *access.Engine) { s.roles, s.engine = rs, e }

// SetReadiness registers a probe used by GET /readyz (e.g. panel link state).
func (s *Server) SetReadiness(fn func() bool) { s.ready = fn }

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
	return &Server{panel: panel, opt: opt, log: opt.Logger, subs: map[chan c3.Event]struct{}{}}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /config", s.handleConfig)
	mux.HandleFunc("GET /device", s.handleDevice)
	mux.HandleFunc("GET /params", s.handleGetParams)
	mux.HandleFunc("PUT /params", s.handleSetParams)
	mux.HandleFunc("POST /time/sync", s.handleTimeSync)
	mux.HandleFunc("GET /users", s.handleListUsers)
	mux.HandleFunc("GET /cards", s.handleListCards)
	mux.HandleFunc("POST /cards", s.handleSetCard)
	mux.HandleFunc("DELETE /cards/{cardNo}", s.handleDeleteCard)
	mux.HandleFunc("GET /tables", s.handleTables)
	mux.HandleFunc("GET /tables/{name}", s.handleTable)
	mux.HandleFunc("POST /restart", s.handleRestart)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("POST /doors/{n}/open", s.handleOpenDoor)
	mux.HandleFunc("POST /aux/{n}", s.handleAux)
	mux.HandleFunc("POST /alarm/cancel", s.handleCancelAlarm)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /events/history", s.handleHistory)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	// Roles & access enforcement.
	mux.HandleFunc("GET /roles", s.handleRoles)
	mux.HandleFunc("POST /roles", s.handleRoleUpsert)
	mux.HandleFunc("DELETE /roles/{group}", s.handleRoleDelete)
	mux.HandleFunc("GET /access/config", s.handleAccessConfig)
	mux.HandleFunc("PUT /access/config", s.handleAccessConfigSet)
	mux.HandleFunc("GET /access/decisions", s.handleDecisions)

	// Embedded admin web UI (same-origin with the API).
	mux.Handle("GET /ui/", http.StripPrefix("/ui/", uiHandler()))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/ui/", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	return s.cors(s.auth(logMW(s.log, mux, &s.reqTotal)))
}

// isPublic reports whether a path bypasses API-key auth (UI assets + probes).
func isPublic(path string) bool {
	switch path {
	case "/", "/healthz", "/readyz", "/metrics", "/config", "/favicon.ico":
		return true
	}
	return strings.HasPrefix(path, "/ui")
}

// Fanout receives events from the panel loop and pushes them to SSE subscribers.
func (s *Server) Fanout(ev c3.Event) {
	s.eventTotal.Add(1)
	if s.audit != nil {
		s.audit.Append(ev)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- ev:
		default: // drop for slow consumers, never block the panel loop
		}
	}
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
		"roles":            []string{"none", "student", "teacher", "administrator"},
	})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.ready != nil && !s.ready() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "panel_disconnected"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ready"})
}

func (s *Server) handleDevice(w http.ResponseWriter, r *http.Request) {
	info, err := s.panel.DeviceInfo(r.Context())
	respond(w, info, err)
}

func (s *Server) handleGetParams(w http.ResponseWriter, r *http.Request) {
	var keys []string
	if q := r.URL.Query().Get("keys"); q != "" {
		keys = splitComma(q)
	}
	m, err := s.panel.GetParams(r.Context(), keys...)
	respond(w, m, err)
}

func (s *Server) handleSetParams(w http.ResponseWriter, r *http.Request) {
	var kv map[string]string
	if !decode(w, r, &kv) {
		return
	}
	err := s.panel.SetParams(r.Context(), kv)
	respond(w, map[string]string{"status": "ok"}, err)
}

func (s *Server) handleTimeSync(w http.ResponseWriter, r *http.Request) {
	err := s.panel.SyncTime(r.Context(), time.Now())
	respond(w, map[string]string{"status": "ok"}, err)
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.panel.ListUsers(r.Context())
	if err == nil && s.roles != nil {
		for i := range users {
			users[i].Role = s.roles.RoleName(users[i].Group)
		}
	}
	respond(w, users, err)
}

// ---- roles & enforcement ----

func (s *Server) handleRoles(w http.ResponseWriter, r *http.Request) {
	if s.roles == nil {
		writeJSON(w, 200, []any{})
		return
	}
	writeJSON(w, 200, s.roles.Roles())
}

func (s *Server) handleRoleUpsert(w http.ResponseWriter, r *http.Request) {
	if s.roles == nil {
		writeJSON(w, 503, map[string]string{"error": "roles unavailable"})
		return
	}
	var role access.Role
	if !decode(w, r, &role) {
		return
	}
	if err := s.roles.Upsert(role); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleRoleDelete(w http.ResponseWriter, r *http.Request) {
	if s.roles == nil {
		writeJSON(w, 503, map[string]string{"error": "roles unavailable"})
		return
	}
	g, err := strconv.Atoi(r.PathValue("group"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad group id"})
		return
	}
	if err := s.roles.Delete(g); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleAccessConfig(w http.ResponseWriter, r *http.Request) {
	if s.roles == nil {
		writeJSON(w, 200, map[string]any{})
		return
	}
	writeJSON(w, 200, s.roles.Config())
}

func (s *Server) handleAccessConfigSet(w http.ResponseWriter, r *http.Request) {
	if s.roles == nil {
		writeJSON(w, 503, map[string]string{"error": "roles unavailable"})
		return
	}
	var body struct {
		Enabled    bool        `json:"enabled"`
		Holidays   []string    `json:"holidays"`
		ReaderDoor map[int]int `json:"reader_door"`
		SwapInOut  bool        `json:"swap_in_out"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.roles.SetGlobals(body.Enabled, body.Holidays, body.ReaderDoor, body.SwapInOut); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeJSON(w, 200, []any{})
		return
	}
	writeJSON(w, 200, s.engine.Decisions(atoiDefault(r.URL.Query().Get("limit"), 100)))
}

func (s *Server) handleListCards(w http.ResponseWriter, r *http.Request) {
	cards, err := s.panel.ListCards(r.Context())
	respond(w, cards, err)
}

func (s *Server) handleTables(w http.ResponseWriter, r *http.Request) {
	names, err := s.panel.TableNames(r.Context())
	respond(w, names, err)
}

func (s *Server) handleTable(w http.ResponseWriter, r *http.Request) {
	rows, err := s.panel.GetTable(r.Context(), r.PathValue("name"))
	respond(w, rows, err)
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	respond(w, map[string]string{"status": "restarting"}, s.panel.Restart(r.Context()))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.panel.Status(r.Context())
	respond(w, st, err)
}

func (s *Server) handleSetCard(w http.ResponseWriter, r *http.Request) {
	var c c3.Card
	if !decode(w, r, &c) {
		return
	}
	// Resolve the role (incl. user-defined roles) to a panel Group, and decide
	// whether the panel authorizes locally or the engine gates access.
	if s.roles != nil && c.Role != "" {
		if g, ok := s.roles.GroupForName(c.Role); ok {
			c.Group = g
			c.Role = "" // Group is now authoritative for c3.SetCard
			c.NoPanelAuth = s.roles.EnforcedGroup(g)
		}
	}
	if err := s.panel.SetCard(r.Context(), c); err != nil {
		respond(w, nil, err)
		return
	}
	if s.engine != nil {
		s.engine.Refresh(r.Context())
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleDeleteCard(w http.ResponseWriter, r *http.Request) {
	err := s.panel.DeleteCard(r.Context(), r.PathValue("cardNo"))
	if err == nil && s.engine != nil {
		s.engine.Refresh(r.Context())
	}
	respond(w, map[string]string{"status": "ok"}, err)
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

func (s *Server) handleAux(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad aux number"})
		return
	}
	var body struct {
		On      bool `json:"on"`
		Seconds int  `json:"seconds"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	err = s.panel.SetAux(r.Context(), idx, body.On, body.Seconds)
	respond(w, map[string]string{"status": "ok"}, err)
}

func (s *Server) handleCancelAlarm(w http.ResponseWriter, r *http.Request) {
	respond(w, map[string]string{"status": "ok"}, s.panel.CancelAlarm(r.Context()))
}

// handleHistory returns stored events newest-first with filtering + paging.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if s.audit == nil {
		writeJSON(w, 200, []c3.Event{})
		return
	}
	q := r.URL.Query()
	f := store.Filter{
		CardNo: q.Get("card"),
		Door:   atoiDefault(q.Get("door"), 0),
		Limit:  atoiDefault(q.Get("limit"), 100),
		Offset: atoiDefault(q.Get("offset"), 0),
	}
	writeJSON(w, 200, s.audit.Query(f))
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

// handleEvents streams realtime panel events to the client as SSE.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	ch := make(chan c3.Event, s.opt.EventBufferSize)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	s.sseActive.Add(1)
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
		s.sseActive.Add(-1)
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	fl.Flush()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			b, _ := json.Marshal(ev)
			w.Write([]byte("event: access\ndata: "))
			w.Write(b)
			w.Write([]byte("\n\n"))
			fl.Flush()
		case <-ping.C:
			w.Write([]byte(": ping\n\n"))
			fl.Flush()
		}
	}
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

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
