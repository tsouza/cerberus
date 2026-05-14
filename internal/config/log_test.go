package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// TestFromEnv_Log_Defaults confirms the slog format/level defaults match
// the documented contract — text + info when neither env var is set, so
// dev shells produce a human-readable stream and prod overrides flip to
// json.
func TestFromEnv_Log_Defaults(t *testing.T) {
	t.Setenv("CERBERUS_LOG_FORMAT", "")
	t.Setenv("CERBERUS_LOG_LEVEL", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Log.Format != "text" {
		t.Errorf("Log.Format = %q; want %q", cfg.Log.Format, "text")
	}
	if cfg.Log.Level != slog.LevelInfo {
		t.Errorf("Log.Level = %v; want %v", cfg.Log.Level, slog.LevelInfo)
	}
}

// TestFromEnv_Log_FormatParsing covers the accepted format vocabulary
// (case-insensitive, whitespace trimmed) and the reject path.
func TestFromEnv_Log_FormatParsing(t *testing.T) {
	good := []struct {
		val  string
		want string
	}{
		{"text", "text"},
		{"json", "json"},
		{"TEXT", "text"},
		{"JSON", "json"},
		{"  json  ", "json"},
	}
	for _, tc := range good {
		t.Run("ok/"+tc.val, func(t *testing.T) {
			t.Setenv("CERBERUS_LOG_FORMAT", tc.val)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.Log.Format != tc.want {
				t.Errorf("Log.Format = %q; want %q", cfg.Log.Format, tc.want)
			}
		})
	}
	t.Run("reject/yaml", func(t *testing.T) {
		t.Setenv("CERBERUS_LOG_FORMAT", "yaml")
		if _, err := FromEnv(); err == nil {
			t.Fatal("FromEnv: want error for invalid format, got nil")
		}
	})
}

// TestFromEnv_Log_LevelParsing covers every accepted level keyword + the
// reject path. The "warning" alias is accepted because slog itself prints
// the level as "WARN" — operators sometimes type the full word.
func TestFromEnv_Log_LevelParsing(t *testing.T) {
	good := []struct {
		val  string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"  ERROR  ", slog.LevelError},
	}
	for _, tc := range good {
		t.Run("ok/"+tc.val, func(t *testing.T) {
			t.Setenv("CERBERUS_LOG_LEVEL", tc.val)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.Log.Level != tc.want {
				t.Errorf("Log.Level = %v; want %v", cfg.Log.Level, tc.want)
			}
		})
	}
	t.Run("reject/trace", func(t *testing.T) {
		t.Setenv("CERBERUS_LOG_LEVEL", "trace")
		if _, err := FromEnv(); err == nil {
			t.Fatal("FromEnv: want error for invalid level, got nil")
		}
	})
}

// TestNewLogger_JSON confirms the json format emits one valid JSON
// object per record so log aggregators can parse the stream — this is
// the load-bearing property of the format flag.
func TestNewLogger_JSON(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, LogConfig{Format: "json", Level: slog.LevelInfo})
	logger.Info("hello", "k", "v", "n", 7)

	line := strings.TrimSpace(buf.String())
	var out map[string]any
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", line, err)
	}
	if got := out["msg"]; got != "hello" {
		t.Errorf("msg = %v; want %q", got, "hello")
	}
	if got := out["k"]; got != "v" {
		t.Errorf("k = %v; want %q", got, "v")
	}
	if got, ok := out["n"].(float64); !ok || got != 7 {
		t.Errorf("n = %v; want 7", out["n"])
	}
	if got := out["level"]; got != "INFO" {
		t.Errorf("level = %v; want %q", got, "INFO")
	}
}

// TestNewLogger_Text confirms the text handler produces a human-readable
// "key=value" stream — the default for local dev.
func TestNewLogger_Text(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, LogConfig{Format: "text", Level: slog.LevelInfo})
	logger.Info("hello", "k", "v")
	if !strings.Contains(buf.String(), "msg=hello") || !strings.Contains(buf.String(), "k=v") {
		t.Errorf("text output missing expected fields: %q", buf.String())
	}
}

// TestNewLogger_LevelFilter confirms records below the configured level
// are dropped at the handler — operators flip the level to silence debug
// in prod and trust nothing slips through.
func TestNewLogger_LevelFilter(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, LogConfig{Format: "text", Level: slog.LevelWarn})
	logger.Info("dropped")
	logger.Warn("kept")
	out := buf.String()
	if strings.Contains(out, "dropped") {
		t.Errorf("info record leaked under warn level: %q", out)
	}
	if !strings.Contains(out, "kept") {
		t.Errorf("warn record missing under warn level: %q", out)
	}
}

// TestNewLogger_UnknownFormatFallsBackToText guards against a future
// caller passing an unvalidated LogConfig — NewLogger should never panic
// or return nil. Unknown formats fall through to the text handler.
func TestNewLogger_UnknownFormatFallsBackToText(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, LogConfig{Format: "yaml", Level: slog.LevelInfo})
	logger.Info("hi")
	if !strings.Contains(buf.String(), "msg=hi") {
		t.Errorf("unknown format did not fall back to text: %q", buf.String())
	}
}
