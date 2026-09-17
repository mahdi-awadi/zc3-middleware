package api

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/awadi/zk-c3-middleware/internal/auth"
)

// Console sign-in for the single administrator (see package auth). Every
// non-public route needs an activated copy plus either a session cookie or
// the machine API key; cookie-authenticated writes must also carry the
// X-Amad header, which a cross-site form or image cannot send.

func (s *Server) authRoutes(mux *http.ServeMux) {
	if s.opt.Auth == nil {
		return
	}
	mux.HandleFunc("GET /auth/status", s.handleAuthStatus)
	mux.HandleFunc("POST /auth/setup", s.handleAuthSetup)
	mux.HandleFunc("POST /auth/login", s.handleAuthLogin)
	mux.HandleFunc("POST /auth/logout", s.handleAuthLogout)
	mux.HandleFunc("PUT /auth/credentials", s.handleAuthCredentials)
}

func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(auth.SessionCookie); err == nil {
		return c.Value
	}
	return ""
}

func setSession(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookie, Value: token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
		MaxAge: int(auth.SessionTTL / time.Second),
	})
}

func clearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: -1,
	})
}

// clientKey identifies the caller for login throttling.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// authError maps auth failures to HTTP codes and Arabic messages the console shows as-is.
func authError(w http.ResponseWriter, err error) {
	var locked auth.LockedError
	switch {
	case errors.As(err, &locked):
		secs := int((locked.Retry + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeJSON(w, 429, map[string]any{"error": "محاولات كثيرة خاطئة — انتظر قليلاً ثم أعد المحاولة", "retry_seconds": secs})
	case errors.Is(err, auth.ErrBadLogin):
		writeJSON(w, 401, map[string]string{"error": "اسم المستخدم أو كلمة المرور غير صحيحة"})
	case errors.Is(err, auth.ErrInvalidCode):
		writeJSON(w, 400, map[string]string{"error": "كود التفعيل غير صحيح"})
	case errors.Is(err, auth.ErrBadUsername):
		writeJSON(w, 400, map[string]string{"error": "اسم المستخدم يجب أن يكون من 3 إلى 32 حرفاً"})
	case errors.Is(err, auth.ErrWeakPassword):
		writeJSON(w, 400, map[string]string{"error": "كلمة المرور يجب أن تكون 8 أحرف على الأقل ومختلفة عن اسم المستخدم"})
	case errors.Is(err, auth.ErrAlreadySetUp):
		writeJSON(w, 409, map[string]string{"error": "تم إعداد البرنامج مسبقاً — سجّل الدخول"})
	case errors.Is(err, auth.ErrNotActivated):
		writeJSON(w, 403, map[string]string{"error": "not_activated"})
	case errors.Is(err, auth.ErrNotConfigured):
		writeJSON(w, 409, map[string]string{"error": "not_configured"})
	default:
		writeJSON(w, 500, map[string]string{"error": "خطأ داخلي"})
	}
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.opt.Auth.Status(sessionToken(r)))
}

func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	// First-time setup creates the only account: accept it from the gym PC
	// itself, never from another device on the network.
	if ip := net.ParseIP(clientKey(r)); ip == nil || !ip.IsLoopback() {
		writeJSON(w, 403, map[string]string{"error": "الإعداد الأول متاح من جهاز البرنامج نفسه فقط"})
		return
	}
	var body struct {
		Code     string `json:"code"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	token, err := s.opt.Auth.Setup(clientKey(r), body.Code, body.Username, body.Password)
	if err != nil {
		s.log.Warn("auth: setup refused", "client", clientKey(r), "err", err)
		authError(w, err)
		return
	}
	s.log.Info("auth: copy activated and administrator created", "client", clientKey(r))
	setSession(w, r, token)
	writeJSON(w, 200, s.opt.Auth.Status(token))
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	token, err := s.opt.Auth.Login(clientKey(r), body.Username, body.Password)
	if err != nil {
		s.log.Warn("auth: login failed", "client", clientKey(r), "err", err)
		authError(w, err)
		return
	}
	setSession(w, r, token)
	writeJSON(w, 200, s.opt.Auth.Status(token))
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	s.opt.Auth.Logout(sessionToken(r))
	clearSession(w, r)
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleAuthCredentials(w http.ResponseWriter, r *http.Request) {
	if !s.opt.Auth.Valid(sessionToken(r)) {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Header.Get("X-Amad") != "1" {
		writeJSON(w, 403, map[string]string{"error": "forbidden"})
		return
	}
	var body struct {
		Current  string `json:"current_password"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	token, err := s.opt.Auth.ChangeCredentials(clientKey(r), body.Current, body.Username, body.Password)
	if err != nil {
		authError(w, err)
		return
	}
	s.log.Info("auth: administrator credentials changed", "client", clientKey(r))
	setSession(w, r, token)
	writeJSON(w, 200, s.opt.Auth.Status(token))
}

// consolePublic lists what anyone may load without signing in: the console
// files themselves (so the login screen renders), probes, and sign-in routes.
func consolePublic(r *http.Request) bool {
	p := r.URL.Path
	switch p {
	case "/healthz", "/readyz", "/auth/status", "/auth/setup", "/auth/login", "/auth/logout":
		return true
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	for _, private := range []string{"/gym/", "/doors/", "/auth/", "/metrics", "/config"} {
		if strings.HasPrefix(p, private) {
			return false
		}
	}
	return true // static console files (index.html, logo.png, ...)
}

// guardConsole enforces activation + sign-in for the console build.
func (s *Server) guardConsole(w http.ResponseWriter, r *http.Request) bool {
	if consolePublic(r) {
		return true
	}
	if !s.opt.Auth.Activated() {
		writeJSON(w, 403, map[string]string{"error": "not_activated"})
		return false
	}
	if s.opt.APIKey != "" && (subtleCompare(r.Header.Get("Authorization"), "Bearer "+s.opt.APIKey) ||
		subtleCompare(r.URL.Query().Get("api_key"), s.opt.APIKey)) {
		return true // machine client
	}
	if !s.opt.Auth.Valid(sessionToken(r)) {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		if r.Header.Get("X-Amad") != "1" {
			writeJSON(w, 403, map[string]string{"error": "forbidden"})
			return false
		}
	}
	return true
}

func securityHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	if strings.HasPrefix(r.URL.Path, "/gym/") || strings.HasPrefix(r.URL.Path, "/auth/") {
		h.Set("Cache-Control", "no-store")
	}
}
