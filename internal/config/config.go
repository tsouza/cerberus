// Package config loads cerberus runtime configuration from environment
// variables (the seed source — YAML loading lands when there's an actual
// need for nested config).
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// Config is the cerberus runtime configuration.
type Config struct {
	HTTPAddr   string
	ClickHouse chclient.Config
	Schema     schema.Metrics
}

// FromEnv reads configuration from environment variables, falling back to
// reasonable defaults for local development.
//
//	CERBERUS_HTTP_ADDR        default ":8080"
//	CERBERUS_CH_ADDR          default "localhost:9000"
//	CERBERUS_CH_DATABASE      default "otel"
//	CERBERUS_CH_USERNAME      default "default"
//	CERBERUS_CH_PASSWORD      default ""
//	CERBERUS_CH_DIAL_TIMEOUT  default "5s"
func FromEnv() (Config, error) {
	dial, err := time.ParseDuration(envDefault("CERBERUS_CH_DIAL_TIMEOUT", "5s"))
	if err != nil {
		return Config{}, fmt.Errorf("CERBERUS_CH_DIAL_TIMEOUT: %w", err)
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
		Schema: schema.DefaultOTelMetrics(),
	}, nil
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
