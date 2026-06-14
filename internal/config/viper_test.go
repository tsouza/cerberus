package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFromEnv_ViperDefaults_NoEnv asserts that with no CERBERUS_* env
// vars set the viper-backed loader resolves every var to its historical
// default — the contract the prior hand-rolled parser guaranteed. This
// is the "(a) defaults resolve correctly with no env set" case.
func TestFromEnv_ViperDefaults_NoEnv(t *testing.T) {
	// Clear every key the loader reads so a value leaking in from the
	// ambient shell can't mask a broken default.
	clearAllEnv(t)

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}

	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q; want :8080", cfg.HTTPAddr)
	}
	if cfg.ClickHouse.Addr != "localhost:9000" {
		t.Errorf("CH.Addr = %q; want localhost:9000", cfg.ClickHouse.Addr)
	}
	if cfg.ClickHouse.Database != "otel" {
		t.Errorf("CH.Database = %q; want otel", cfg.ClickHouse.Database)
	}
	if cfg.ClickHouse.Username != "default" {
		t.Errorf("CH.Username = %q; want default", cfg.ClickHouse.Username)
	}
	if cfg.ClickHouse.Password != "" {
		t.Errorf("CH.Password = %q; want empty", cfg.ClickHouse.Password)
	}
	if cfg.ClickHouse.DialTimeout != 5*time.Second {
		t.Errorf("DialTimeout = %v; want 5s", cfg.ClickHouse.DialTimeout)
	}
	if cfg.ClickHouse.MaxOpenConns != 10 {
		t.Errorf("MaxOpenConns = %d; want 10", cfg.ClickHouse.MaxOpenConns)
	}
	if cfg.ClickHouse.MaxIdleConns != 5 {
		t.Errorf("MaxIdleConns = %d; want 5", cfg.ClickHouse.MaxIdleConns)
	}
	if cfg.ClickHouse.ConnMaxLifetime != time.Hour {
		t.Errorf("ConnMaxLifetime = %v; want 1h", cfg.ClickHouse.ConnMaxLifetime)
	}
	if cfg.ClickHouse.MaxQuerySamples != 50_000_000 {
		t.Errorf("MaxQuerySamples = %d; want 50000000", cfg.ClickHouse.MaxQuerySamples)
	}
	if cfg.ClickHouse.MaxQueryMemoryBytes != 1_073_741_824 {
		t.Errorf("MaxQueryMemoryBytes = %d; want 1073741824", cfg.ClickHouse.MaxQueryMemoryBytes)
	}
	if cfg.ClickHouse.BreakerDisabled {
		t.Errorf("BreakerDisabled = true; want false")
	}
	if cfg.ClickHouse.BreakerThreshold != 5 {
		t.Errorf("BreakerThreshold = %d; want 5", cfg.ClickHouse.BreakerThreshold)
	}
	if cfg.ClickHouse.BreakerWindow != 10*time.Second {
		t.Errorf("BreakerWindow = %v; want 10s", cfg.ClickHouse.BreakerWindow)
	}
	if cfg.ClickHouse.BreakerOpenInterval != 5*time.Second {
		t.Errorf("BreakerOpenInterval = %v; want 5s", cfg.ClickHouse.BreakerOpenInterval)
	}
	if cfg.AutoCreateSchema {
		t.Errorf("AutoCreateSchema = true; want false")
	}
	if cfg.ExperimentalTSGridRange {
		t.Errorf("ExperimentalTSGridRange = true; want false")
	}
	if cfg.Log.Format != "text" {
		t.Errorf("Log.Format = %q; want text", cfg.Log.Format)
	}
	if cfg.OTLP.Endpoint != "" {
		t.Errorf("OTLP.Endpoint = %q; want empty", cfg.OTLP.Endpoint)
	}
	if cfg.OTLP.Insecure {
		t.Errorf("OTLP.Insecure = true; want false")
	}
	if cfg.OTLP.Timeout != 10*time.Second {
		t.Errorf("OTLP.Timeout = %v; want 10s", cfg.OTLP.Timeout)
	}
	if cfg.OTLP.ExportInterval != 10*time.Second {
		t.Errorf("OTLP.ExportInterval = %v; want 10s", cfg.OTLP.ExportInterval)
	}
	if cfg.Admit.Disabled {
		t.Errorf("Admit.Disabled = true; want false")
	}
	if cfg.Admit.MaxInflightProm != defaultAdmitProm {
		t.Errorf("MaxInflightProm = %d; want %d", cfg.Admit.MaxInflightProm, defaultAdmitProm)
	}
	if cfg.Admit.MaxInflightLoki != defaultAdmitLoki {
		t.Errorf("MaxInflightLoki = %d; want %d", cfg.Admit.MaxInflightLoki, defaultAdmitLoki)
	}
	if cfg.Admit.MaxInflightTempo != defaultAdmitTempo {
		t.Errorf("MaxInflightTempo = %d; want %d", cfg.Admit.MaxInflightTempo, defaultAdmitTempo)
	}
}

// TestFromEnv_ViperEnvBeatsDefault asserts a CERBERUS_<VAR> override
// beats the built-in default for one representative var of each type
// (string, int, int64, bool, duration). This is the "(b) a
// CERBERUS_<VAR> override beats the default" case.
func TestFromEnv_ViperEnvBeatsDefault(t *testing.T) {
	clearAllEnv(t)
	t.Setenv(envHTTPAddr, "127.0.0.1:7777") // string
	t.Setenv(envCHMaxOpenConns, "99")       // int
	t.Setenv(envQueryMaxSamples, "123")     // int64
	t.Setenv(envAutoCreateSchema, "true")   // bool
	t.Setenv(envCHDialTimeout, "12s")       // duration

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.HTTPAddr != "127.0.0.1:7777" {
		t.Errorf("HTTPAddr = %q; want 127.0.0.1:7777 (env beats default)", cfg.HTTPAddr)
	}
	if cfg.ClickHouse.MaxOpenConns != 99 {
		t.Errorf("MaxOpenConns = %d; want 99 (env beats default)", cfg.ClickHouse.MaxOpenConns)
	}
	if cfg.ClickHouse.MaxQuerySamples != 123 {
		t.Errorf("MaxQuerySamples = %d; want 123 (env beats default)", cfg.ClickHouse.MaxQuerySamples)
	}
	if !cfg.AutoCreateSchema {
		t.Errorf("AutoCreateSchema = false; want true (env beats default)")
	}
	if cfg.ClickHouse.DialTimeout != 12*time.Second {
		t.Errorf("DialTimeout = %v; want 12s (env beats default)", cfg.ClickHouse.DialTimeout)
	}
}

// TestFromEnv_ViperEnvBeatsConfigFile asserts the precedence ordering
// that makes config-file support safe: an environment variable always
// wins over a value supplied by cerberus.yaml, while the file is still
// honoured when the env var is absent. This is the "(c) env beats
// config-file" case. The test runs from a temp working directory holding
// a cerberus.yaml so the loader's AddConfigPath(".") finds it.
func TestFromEnv_ViperEnvBeatsConfigFile(t *testing.T) {
	clearAllEnv(t)
	dir := t.TempDir()
	yaml := "" +
		"CERBERUS_HTTP_ADDR: \"fromfile:1111\"\n" +
		"CERBERUS_CH_DATABASE: \"file_db\"\n"
	if err := os.WriteFile(filepath.Join(dir, "cerberus.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatalf("write cerberus.yaml: %v", err)
	}
	chdir(t, dir)

	// (1) env unset → the file value is used.
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv (file only): %v", err)
	}
	if cfg.HTTPAddr != "fromfile:1111" {
		t.Errorf("HTTPAddr = %q; want fromfile:1111 (config file honoured when env unset)", cfg.HTTPAddr)
	}
	if cfg.ClickHouse.Database != "file_db" {
		t.Errorf("CH.Database = %q; want file_db (config file honoured)", cfg.ClickHouse.Database)
	}

	// (2) env set → env wins over the file.
	t.Setenv(envHTTPAddr, "fromenv:2222")
	cfg, err = FromEnv()
	if err != nil {
		t.Fatalf("FromEnv (env over file): %v", err)
	}
	if cfg.HTTPAddr != "fromenv:2222" {
		t.Errorf("HTTPAddr = %q; want fromenv:2222 (env beats config file)", cfg.HTTPAddr)
	}
	// The non-overridden file value still comes through.
	if cfg.ClickHouse.Database != "file_db" {
		t.Errorf("CH.Database = %q; want file_db (untouched file value)", cfg.ClickHouse.Database)
	}
}

// clearAllEnv unsets every CERBERUS_* var the loader reads so a test
// observes pristine defaults regardless of the ambient environment.
func clearAllEnv(t *testing.T) {
	t.Helper()
	for _, key := range allEnvKeys {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

// chdir changes the working directory to dir for the duration of the
// test, restoring the original on cleanup. Used by the config-file test
// so the loader's relative AddConfigPath(".") resolves the temp yaml.
func chdir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}
