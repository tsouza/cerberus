// Package config loads cerberus runtime configuration from environment
// variables (the seed source — YAML loading lands when there's an actual
// need for nested config).
package config

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// Config is the cerberus runtime configuration.
type Config struct {
	HTTPAddr   string
	ClickHouse chclient.Config
	Schema     schema.Metrics

	// AutoCreateSchema, when true, instructs cerberus to run the OTel
	// ClickHouse Exporter DDL (via internal/schema/ddl) against the
	// configured ClickHouse connection at startup, before HTTP serving
	// begins. Default is false — production deploys stay explicit and
	// keep the operator-runs-DDL contract. The DDL itself is idempotent
	// (every statement carries CREATE TABLE IF NOT EXISTS) so enabling
	// the flag on an already-populated ClickHouse is a no-op.
	AutoCreateSchema bool

	// Log configures cerberus's own structured logging (stdlib log/slog).
	// See LogConfig for the env-var contract.
	Log LogConfig
}

// LogConfig controls the slog setup applied at startup.
//
//   - Format is the slog handler kind. "text" produces a human-readable
//     stream suited to local development; "json" produces newline-delimited
//     JSON suited to log aggregators (Loki / ECS / GCP).
//   - Level is the minimum level recorded; lower-severity records are
//     dropped at the handler. Supported: "debug", "info" (default),
//     "warn", "error".
type LogConfig struct {
	Format string
	Level  slog.Level
}

// FromEnv reads configuration from environment variables, falling back to
// reasonable defaults for local development.
//
//	CERBERUS_HTTP_ADDR             default ":8080"
//	CERBERUS_CH_ADDR               default "localhost:9000"
//	CERBERUS_CH_DATABASE           default "otel"
//	CERBERUS_CH_USERNAME           default "default"
//	CERBERUS_CH_PASSWORD           default ""
//	CERBERUS_CH_DIAL_TIMEOUT       default "5s"
//	CERBERUS_AUTO_CREATE_SCHEMA    default "false"
//	CERBERUS_LOG_FORMAT            default "text"  ("text" | "json")
//	CERBERUS_LOG_LEVEL             default "info"  ("debug" | "info" | "warn" | "error")
func FromEnv() (Config, error) {
	dial, err := time.ParseDuration(envDefault("CERBERUS_CH_DIAL_TIMEOUT", "5s"))
	if err != nil {
		return Config{}, fmt.Errorf("CERBERUS_CH_DIAL_TIMEOUT: %w", err)
	}
	autoCreate, err := envBool("CERBERUS_AUTO_CREATE_SCHEMA", false)
	if err != nil {
		return Config{}, fmt.Errorf("CERBERUS_AUTO_CREATE_SCHEMA: %w", err)
	}
	logCfg, err := envLog()
	if err != nil {
		return Config{}, err
	}
	return Config{
		HTTPAddr: envDefault("CERBERUS_HTTP_ADDR", ":8080"),
		ClickHouse: chclient.Config{
			Addr:        envDefault("CERBERUS_CH_ADDR", "localhost:9000"),
			Database:    envDefault("CERBERUS_CH_DATABASE", "otel"),
			Username:    envDefault("CERBERUS_CH_USERNAME", "default"),
			Password:    envDefault("CERBERUS_CH_PASSWORD", ""),
			DialTimeout: dial,
		},
		Schema:           schema.DefaultOTelMetrics(),
		AutoCreateSchema: autoCreate,
		Log:              logCfg,
	}, nil
}

// NewLogger builds a *slog.Logger from a LogConfig writing to w. The
// caller is responsible for installing the result as the slog default
// (e.g. via slog.SetDefault) if global propagation is desired. Accepting
// io.Writer keeps the helper trivially testable (a *bytes.Buffer drops
// straight in).
func NewLogger(w io.Writer, cfg LogConfig) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.Level}
	var h slog.Handler
	switch cfg.Format {
	case "json":
		h = slog.NewJSONHandler(w, opts)
	default:
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool parses a boolean env var. Accepts the standard strconv.ParseBool
// vocabulary: "1"/"0", "t"/"f", "true"/"false", "TRUE"/"FALSE",
// case-insensitive. An unset or empty value returns def. Anything else is
// rejected with an error so misconfiguration fails fast at startup.
func envBool(key string, def bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("invalid boolean %q: %w", v, err)
	}
	return b, nil
}

// envLog parses CERBERUS_LOG_FORMAT + CERBERUS_LOG_LEVEL into a LogConfig.
// Unset values default to "text" / "info"; invalid values fail fast at
// startup so a typo never silently downgrades observability.
func envLog() (LogConfig, error) {
	format := strings.ToLower(strings.TrimSpace(envDefault("CERBERUS_LOG_FORMAT", "text")))
	switch format {
	case "text", "json":
	default:
		return LogConfig{}, fmt.Errorf("CERBERUS_LOG_FORMAT: invalid value %q (want \"text\" or \"json\")", format)
	}
	levelStr := strings.ToLower(strings.TrimSpace(envDefault("CERBERUS_LOG_LEVEL", "info")))
	var level slog.Level
	switch levelStr {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return LogConfig{}, fmt.Errorf("CERBERUS_LOG_LEVEL: invalid value %q (want \"debug\", \"info\", \"warn\", or \"error\")", levelStr)
	}
	return LogConfig{Format: format, Level: level}, nil
}
