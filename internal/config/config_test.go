package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func clearEnv(keys ...string) {
	for _, k := range keys {
		os.Unsetenv(k)
	}
}

func TestLoadPanelAddrOptional(t *testing.T) {
	// PANEL_ADDR may be empty: gates added from the console take over as the
	// connection target. A malformed value must still be rejected.
	clearEnv("PANEL_ADDR", "ENV_FILE")
	os.Setenv("ENV_FILE", "/nonexistent")
	defer clearEnv("ENV_FILE")
	cfg, err := Load()
	if err != nil || cfg.PanelAddr != "" {
		t.Fatalf("empty PANEL_ADDR should load: err=%v addr=%q", err, cfg.PanelAddr)
	}
	os.Setenv("PANEL_ADDR", "no-port-here")
	defer clearEnv("PANEL_ADDR")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for malformed PANEL_ADDR")
	}
}

func TestLoadDefaultsAndValidation(t *testing.T) {
	clearEnv("ENV_FILE")
	os.Setenv("ENV_FILE", "/nonexistent")
	os.Setenv("PANEL_ADDR", "1.2.3.4:4370")
	defer clearEnv("PANEL_ADDR", "ENV_FILE")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8000" {
		t.Fatalf("default HTTP_ADDR=%q", cfg.HTTPAddr)
	}
	if cfg.IOTimeout != 5*time.Second {
		t.Fatalf("default IO_TIMEOUT=%v", cfg.IOTimeout)
	}
	if cfg.DoorDefaultSeconds != 5 {
		t.Fatalf("default door secs=%d", cfg.DoorDefaultSeconds)
	}
}

func TestLoadRejectsBadLogLevel(t *testing.T) {
	clearEnv("ENV_FILE")
	os.Setenv("ENV_FILE", "/nonexistent")
	os.Setenv("PANEL_ADDR", "1.2.3.4:4370")
	os.Setenv("LOG_LEVEL", "loud")
	defer clearEnv("PANEL_ADDR", "LOG_LEVEL", "ENV_FILE")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for bad LOG_LEVEL")
	}
}

func TestParsePairs(t *testing.T) {
	m := parsePairs("1:1,2:1,3:2,4:2")
	if len(m) != 4 || m[3] != 2 || m[1] != 1 {
		t.Fatalf("parsePairs = %v", m)
	}
	if parsePairs("") != nil {
		t.Fatal("empty should be nil")
	}
}

func TestDotEnvLoading(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	os.WriteFile(envFile, []byte("PANEL_ADDR=9.9.9.9:4370\nDOOR_DEFAULT_SECONDS=7\n# comment\n"), 0o600)
	clearEnv("PANEL_ADDR", "DOOR_DEFAULT_SECONDS")
	os.Setenv("ENV_FILE", envFile)
	defer clearEnv("ENV_FILE", "PANEL_ADDR", "DOOR_DEFAULT_SECONDS")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PanelAddr != "9.9.9.9:4370" {
		t.Fatalf(".env PANEL_ADDR=%q", cfg.PanelAddr)
	}
	if cfg.DoorDefaultSeconds != 7 {
		t.Fatalf(".env DOOR_DEFAULT_SECONDS=%d", cfg.DoorDefaultSeconds)
	}
}
