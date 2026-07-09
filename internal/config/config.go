// Package config loads all middleware settings from the environment, optionally
// seeded from a .env file. Nothing is hardcoded: every tunable is an env var
// with a documented default, and Load validates the result.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	// Panel
	PanelAddr string        // host:port of the C3 panel
	PanelID   string        // logical id for this panel (labels events/metrics)
	IOTimeout time.Duration // per-command panel I/O timeout

	// Resilience
	ReconnectMin time.Duration // initial reconnect backoff
	ReconnectMax time.Duration // max reconnect backoff

	// HTTP API
	HTTPAddr          string
	ReadHeaderTimeout time.Duration
	ShutdownTimeout   time.Duration

	// Behavior
	DoorDefaultSeconds int           // default lock-relay pulse duration
	EventBufferSize    int           // per-SSE-subscriber buffer
	EventPollInterval  time.Duration // RTLog poll cadence
	EventReplayBacklog bool          // emit buffered panel events on connect

	// Auto-open (middleware-driven access): swipe at reader -> open mapped door.
	AutoOpenEnabled bool
	AutoOpenSeconds int
	AutoOpenCards   []string       // card numbers, or ["*"] for any card
	ReaderDoorMap   map[int]int    // reader/eventaddr -> door; empty = identity

	// Security
	APIKey        string   // bearer token; empty disables auth
	CORSOrigins   []string // allowed origins; empty disables CORS
	TrustedProxy  bool     // honor X-Forwarded-* for client IP/logging

	// Observability
	LogLevel  string // debug|info|warn|error
	LogFormat string // json|text
}

// Load reads .env (if present; path from ENV_FILE, default ".env"), then the
// process environment (which always wins over the file), and validates.
func Load() (Config, error) {
	if err := loadDotEnv(getenv("ENV_FILE", ".env")); err != nil {
		return Config{}, err
	}

	c := Config{
		PanelAddr:          getenv("PANEL_ADDR", ""),
		PanelID:            getenv("PANEL_ID", "panel-1"),
		IOTimeout:          getdur("IO_TIMEOUT", 5*time.Second),
		ReconnectMin:       getdur("RECONNECT_MIN", 1*time.Second),
		ReconnectMax:       getdur("RECONNECT_MAX", 30*time.Second),
		HTTPAddr:           getenv("HTTP_ADDR", ":8080"),
		ReadHeaderTimeout:  getdur("HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
		ShutdownTimeout:    getdur("HTTP_SHUTDOWN_TIMEOUT", 10*time.Second),
		DoorDefaultSeconds: getint("DOOR_DEFAULT_SECONDS", 5),
		EventBufferSize:    getint("EVENT_BUFFER_SIZE", 64),
		EventPollInterval:  getdur("EVENT_POLL_INTERVAL", time.Second),
		EventReplayBacklog: getbool("EVENT_REPLAY_BACKLOG", false),
		AutoOpenEnabled:    getbool("AUTO_OPEN_ENABLED", false),
		AutoOpenSeconds:    getint("AUTO_OPEN_SECONDS", getint("DOOR_DEFAULT_SECONDS", 5)),
		AutoOpenCards:      getlist("AUTO_OPEN_CARDS"),
		ReaderDoorMap:      parsePairs(getenv("READER_DOOR_MAP", "")),
		APIKey:             getenv("API_KEY", ""),
		CORSOrigins:        getlist("CORS_ORIGINS"),
		TrustedProxy:       getbool("TRUSTED_PROXY", false),
		LogLevel:           getenv("LOG_LEVEL", "info"),
		LogFormat:          getenv("LOG_FORMAT", "json"),
	}
	return c, c.validate()
}

func (c Config) validate() error {
	if c.PanelAddr == "" {
		return fmt.Errorf("config: PANEL_ADDR is required (host:port of the C3 panel)")
	}
	if !strings.Contains(c.PanelAddr, ":") {
		return fmt.Errorf("config: PANEL_ADDR must be host:port, got %q", c.PanelAddr)
	}
	if c.DoorDefaultSeconds <= 0 || c.DoorDefaultSeconds > 255 {
		return fmt.Errorf("config: DOOR_DEFAULT_SECONDS must be 1..255, got %d", c.DoorDefaultSeconds)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: LOG_LEVEL must be debug|info|warn|error, got %q", c.LogLevel)
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		return fmt.Errorf("config: LOG_FORMAT must be json|text, got %q", c.LogFormat)
	}
	if c.ReconnectMin <= 0 || c.ReconnectMax < c.ReconnectMin {
		return fmt.Errorf("config: RECONNECT_MIN>0 and RECONNECT_MAX>=RECONNECT_MIN required")
	}
	return nil
}

// ---- env helpers (no external deps) ----

func getenv(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func getint(k string, def int) int {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func getbool(k string, def bool) bool {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}

func getdur(k string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		v = strings.TrimSpace(v)
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	return def
}

// parsePairs parses "1:1,2:1,3:2,4:2" into {1:1,2:1,3:2,4:2}. Bad entries are
// skipped. Empty input yields nil (callers treat nil as identity mapping).
func parsePairs(s string) map[int]int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	m := map[int]int{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok {
			continue
		}
		ki, err1 := strconv.Atoi(strings.TrimSpace(k))
		vi, err2 := strconv.Atoi(strings.TrimSpace(v))
		if err1 == nil && err2 == nil {
			m[ki] = vi
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func getlist(k string) []string {
	v, ok := os.LookupEnv(k)
	if !ok || v == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadDotEnv parses a KEY=VALUE .env file. Existing process env is never
// overwritten (12-factor: real env wins). Missing file is not an error.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return fmt.Errorf("config: %s:%d: expected KEY=VALUE", path, line)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		// strip matching surrounding quotes
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if _, exists := os.LookupEnv(k); !exists {
			if err := os.Setenv(k, v); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}
