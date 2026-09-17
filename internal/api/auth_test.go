package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/awadi/zk-c3-middleware/internal/auth"
	"github.com/awadi/zk-c3-middleware/internal/gym"
)

const apiTestCode = "AMAD-APIT0-00000-00000-00000"

func authServer(t *testing.T) http.Handler {
	t.Helper()
	auth.AllowCodeForTests(apiTestCode)
	guard, err := auth.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(&fakePanel{}, Options{Auth: guard, APIKey: "machine-key"})
	store, _ := gym.Open("")
	s.SetGym(store, gym.NewEngine(store, &fakePanel{}, 3, 0, nil))
	return s.Routes()
}

type call struct {
	method, path, body, remote string
	cookie                     *http.Cookie
	headers                    map[string]string
}

func do(h http.Handler, c call) *httptest.ResponseRecorder {
	req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
	if c.remote != "" {
		req.RemoteAddr = c.remote
	}
	if c.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookie {
			return c
		}
	}
	return nil
}

func TestConsoleAuthFlow(t *testing.T) {
	h := authServer(t)
	local := "127.0.0.1:5000"

	// Before activation: console shell loads, data does not.
	if rec := do(h, call{method: "GET", path: "/"}); rec.Code != 200 {
		t.Fatalf("console shell: %d", rec.Code)
	}
	if rec := do(h, call{method: "GET", path: "/gym/members"}); rec.Code != 403 || !strings.Contains(rec.Body.String(), "not_activated") {
		t.Fatalf("data before activation: %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, call{method: "GET", path: "/gym/members", headers: map[string]string{"Authorization": "Bearer machine-key"}}); rec.Code != 403 {
		t.Fatalf("API key before activation: %d", rec.Code)
	}

	setup := `{"code":"` + apiTestCode + `","username":"admin","password":"s3cret-pass"}`
	if rec := do(h, call{method: "POST", path: "/auth/setup", body: setup, remote: "192.168.0.50:4000"}); rec.Code != 403 {
		t.Fatalf("setup from another device: %d", rec.Code)
	}
	rec := do(h, call{method: "POST", path: "/auth/setup", body: setup, remote: local})
	ck := sessionCookie(rec)
	if rec.Code != 200 || ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode {
		t.Fatalf("setup: %d %s cookie=%+v", rec.Code, rec.Body, ck)
	}

	// Signed-in reads work; anonymous reads do not.
	if rec := do(h, call{method: "GET", path: "/gym/members", cookie: ck}); rec.Code != 200 {
		t.Fatalf("signed-in read: %d", rec.Code)
	}
	if rec := do(h, call{method: "GET", path: "/gym/members"}); rec.Code != 401 {
		t.Fatalf("anonymous read: %d", rec.Code)
	}
	if rec := do(h, call{method: "GET", path: "/config"}); rec.Code != 401 {
		t.Fatalf("anonymous /config: %d", rec.Code)
	}
	// Writes need the X-Amad header (cross-site requests cannot add it).
	member := `{"name":"علي","plan":{"months":1,"access":"gym"}}`
	if rec := do(h, call{method: "POST", path: "/gym/members", body: member, cookie: ck}); rec.Code != 403 {
		t.Fatalf("write without X-Amad: %d", rec.Code)
	}
	if rec := do(h, call{method: "POST", path: "/gym/members", body: member, cookie: ck, headers: map[string]string{"X-Amad": "1"}}); rec.Code != 201 {
		t.Fatalf("write with X-Amad: %d %s", rec.Code, rec.Body)
	}
	// The machine API key still works for integrations.
	if rec := do(h, call{method: "GET", path: "/gym/members", headers: map[string]string{"Authorization": "Bearer machine-key"}}); rec.Code != 200 {
		t.Fatalf("API key: %d", rec.Code)
	}

	// Logout, bad login, good login.
	do(h, call{method: "POST", path: "/auth/logout", cookie: ck})
	if rec := do(h, call{method: "GET", path: "/gym/members", cookie: ck}); rec.Code != 401 {
		t.Fatalf("after logout: %d", rec.Code)
	}
	if rec := do(h, call{method: "POST", path: "/auth/login", body: `{"username":"admin","password":"nope-nope"}`, remote: local}); rec.Code != 401 {
		t.Fatalf("bad login: %d", rec.Code)
	}
	rec = do(h, call{method: "POST", path: "/auth/login", body: `{"username":"admin","password":"s3cret-pass"}`, remote: local})
	var st auth.Status
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if rec.Code != 200 || sessionCookie(rec) == nil || !st.Authenticated || st.Username != "admin" {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("security headers missing: %v", rec.Header())
	}
}

func TestLoginThrottled(t *testing.T) {
	h := authServer(t)
	local := "127.0.0.1:5000"
	setup := `{"code":"` + apiTestCode + `","username":"admin","password":"s3cret-pass"}`
	do(h, call{method: "POST", path: "/auth/setup", body: setup, remote: local})
	var last *httptest.ResponseRecorder
	for i := 0; i < 5; i++ {
		last = do(h, call{method: "POST", path: "/auth/login", body: `{"username":"admin","password":"guess-guess"}`, remote: "10.0.0.9:1"})
	}
	if last.Code != 429 || last.Header().Get("Retry-After") == "" {
		t.Fatalf("5th failure: %d retry-after=%q", last.Code, last.Header().Get("Retry-After"))
	}
}
