package config

// Log records are written synchronously, and the store writes one per indexed
// file. The level is therefore a production setting: debug on a busy node is a
// throughput cost, and a log consumer that stops reading stalls the daemon.

import (
	"log/slog"
	"testing"
)

func TestDefaultLogLevelIsInfo(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.LogLevel != "info" || cfg.SlogLevel() != slog.LevelInfo {
		t.Fatalf("default log level is %q (%v), want info", cfg.LogLevel, cfg.SlogLevel())
	}
}

func TestLogLevelFromYAMLAndEnv(t *testing.T) {
	path := writeYAML(t, "node_id: n1\nlog_level: warn\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SlogLevel() != slog.LevelWarn {
		t.Fatalf("yaml level = %v, want warn", cfg.SlogLevel())
	}

	t.Setenv("BIRAK_LOG_LEVEL", "DEBUG")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("load with env: %v", err)
	}
	if cfg.SlogLevel() != slog.LevelDebug {
		t.Fatalf("env override = %v, want debug", cfg.SlogLevel())
	}
}

// A typo must not silently leave the daemon at a level nobody chose.
func TestUnknownLogLevelIsRejected(t *testing.T) {
	path := writeYAML(t, "node_id: n1\nlog_level: verbose\n")
	if _, err := Load(path); err == nil {
		t.Fatal("unknown log_level accepted")
	}

	t.Setenv("BIRAK_LOG_LEVEL", "trace")
	if _, err := Load(""); err == nil {
		t.Fatal("unknown BIRAK_LOG_LEVEL accepted")
	}
}
