// Package config loads cerberus runtime configuration from environment
// variables (the seed source — YAML loading lands when there's an actual
// need for nested config).
package config

import (
	"fmt"
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
func FromEnv() (Config, error) {
	dial, err := time.ParseDuration(envDefault("CERBERUS_CH_DIAL_TIMEOUT", "5s"))
	if err != nil {
		return Config{}, fmt.Errorf("CERBERUS_CH_DIAL_TIMEOUT: %w", err)
	}
	autoCreate, err := envBool("CERBERUS_AUTO_CREATE_SCHEMA", false)
	if err != nil {
		return Config{}, fmt.Errorf("CERBERUS_AUTO_CREATE_SCHEMA: %w", err)
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
	}, nil
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
