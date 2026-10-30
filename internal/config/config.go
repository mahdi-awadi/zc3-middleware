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
	PanelType string        // c3 (ZKTeco C3, default) | rosslare (Rosslare AC-215)
	PanelAddr string        // host:port of the C3 panel
	PanelID   string        // logical id for this panel (labels events/metrics)
	IOTimeout time.Duration // per-command panel I/O timeout

	// Rosslare AC-215 (PANEL_TYPE=rosslare): PANEL_ADDR is the RS-485/TCP adapter.
	RosslareAddress   int    // panel bus address, DIP switches 4-8 (1..32); 0 = find it on the bus
	RosslareCRC       bool   // CRC-16 check on normal commands (false = 1-byte sum)
	RosslareStatePath string // JSON record of enrolled cards + event cursor

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

	// Access enforcement (roles engine).
	AccessConfigPath string // JSON file with roles + policy; empty = in-memory defaults

	// Security
	APIKey       string   // bearer token; empty disables auth
	CORSOrigins  []string // allowed origins; empty disables CORS
	TrustedProxy bool     // honor X-Forwarded-* for client IP/logging
	AllowWrites  bool     // enable data-table writes (enroll/revoke/rules)

	// Observability & audit
	LogLevel      string // debug|info|warn|error
	LogFormat     string // json|text
	AuditLogPath  string // JSONL audit file; empty = memory-only
	AuditRingSize int    // in-memory history buffer size
}

// Load reads .env (if present; path from ENV_FILE, default ".env"), then the
// process environment (which always wins over the file), and validates.
func Load() (Config, error) {
	if err := loadDotEnv(getenv("ENV_FILE", ".env")); err != nil {
		return Config{}, err
	}

	c := Config{
		PanelType:          getenv("PANEL_TYPE", "c3"),
		PanelAddr:          getenv("PANEL_ADDR", ""),
		RosslareAddress:    getint("ROSSLARE_ADDRESS", 0),
		RosslareCRC:        getbool("ROSSLARE_CRC", true),
		RosslareStatePath:  getenv("ROSSLARE_STATE_PATH", "rosslare-state.json"),
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
		AccessConfigPath:   getenv("ACCESS_CONFIG_PATH", "access.json"),
		APIKey:             getenv("API_KEY", ""),
		CORSOrigins:        getlist("CORS_ORIGINS"),
		TrustedProxy:       getbool("TRUSTED_PROXY", false),
		AllowWrites:        getbool("ALLOW_WRITES", false),
		LogLevel:           getenv("LOG_LEVEL", "info"),
		LogFormat:          getenv("LOG_FORMAT", "json"),
		AuditLogPath:       getenv("AUDIT_LOG_PATH", "events.jsonl"),
		AuditRingSize:      getint("AUDIT_RING_SIZE", 10000),
	}
	return c, c.validate()
}

func (c Config) validate() error {
	if c.PanelAddr == "" && c.PanelType != "rosslare" { // a Rosslare panel can be added in the console
		return fmt.Errorf("config: PANEL_ADDR is required (host:port of the C3 panel)")
	}
	if c.PanelAddr != "" && !strings.Contains(c.PanelAddr, ":") {
		return fmt.Errorf("config: PANEL_ADDR must be host:port, got %q", c.PanelAddr)
	}
	switch c.PanelType {
	case "c3":
	case "rosslare":
		if c.RosslareAddress < 0 || c.RosslareAddress > 32 {
			return fmt.Errorf("config: ROSSLARE_ADDRESS must be 0 (find it) or 1..32, got %d", c.RosslareAddress)
		}
	default:
		return fmt.Errorf("config: PANEL_TYPE must be c3|rosslare, got %q", c.PanelType)
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
