package config

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestFromEnv_AutoCreateSchema_Default confirms the new flag defaults to
// false when CERBERUS_AUTO_CREATE_SCHEMA is unset — production deploys
// keep the operator-runs-DDL contract.
func TestFromEnv_AutoCreateSchema_Default(t *testing.T) {
	t.Setenv("CERBERUS_AUTO_CREATE_SCHEMA", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.AutoCreateSchema {
		t.Errorf("AutoCreateSchema = true; want false (default)")
	}
}

// TestFromEnv_AutoCreateSchema_Parsing covers the strconv.ParseBool
// vocabulary cerberus accepts for the flag — true/false/1/0/etc.
func TestFromEnv_AutoCreateSchema_Parsing(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"1", true},
		{"t", true},
		{"false", false},
		{"FALSE", false},
		{"0", false},
		{"f", false},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("CERBERUS_AUTO_CREATE_SCHEMA", tc.val)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.AutoCreateSchema != tc.want {
				t.Errorf("AutoCreateSchema = %v; want %v", cfg.AutoCreateSchema, tc.want)
			}
		})
	}
}

// TestFromEnv_AutoCreateSchema_Invalid confirms a bad boolean string
// surfaces as a startup error rather than silently defaulting — fail-fast
// on misconfiguration.
func TestFromEnv_AutoCreateSchema_Invalid(t *testing.T) {
	t.Setenv("CERBERUS_AUTO_CREATE_SCHEMA", "yes-please")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv: want error for invalid bool, got nil")
	}
}

// TestFromEnv_AutoCreateSchema_Whitespace confirms surrounding whitespace
// is trimmed before parsing (operators often paste values with newlines).
func TestFromEnv_AutoCreateSchema_Whitespace(t *testing.T) {
	t.Setenv("CERBERUS_AUTO_CREATE_SCHEMA", "  true  ")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if !cfg.AutoCreateSchema {
		t.Errorf("AutoCreateSchema = false; want true (trimmed)")
	}
}

// TestFromEnv_ExperimentalTSGridRange_Default confirms the experimental
// native-rate flag defaults OFF when unset — the default behaviour
// (arrayJoin fan-out) is preserved and the compose / e2e / compatibility
// lanes (ClickHouse 25.8; the native path stays experimental even though
// the function now exists) stay green.
func TestFromEnv_ExperimentalTSGridRange_Default(t *testing.T) {
	t.Setenv("CERBERUS_EXPERIMENTAL_TS_GRID_RANGE", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ExperimentalTSGridRange {
		t.Errorf("ExperimentalTSGridRange = true; want false (default)")
	}
}

// TestFromEnv_ExperimentalTSGridRange_Parsing covers the strconv.ParseBool
// vocabulary the flag accepts, with the load-bearing assertion that
// CERBERUS_EXPERIMENTAL_TS_GRID_RANGE=true flips it on.
func TestFromEnv_ExperimentalTSGridRange_Parsing(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"true", true},
		{"1", true},
		{"false", false},
		{"0", false},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("CERBERUS_EXPERIMENTAL_TS_GRID_RANGE", tc.val)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.ExperimentalTSGridRange != tc.want {
				t.Errorf("ExperimentalTSGridRange = %v; want %v", cfg.ExperimentalTSGridRange, tc.want)
			}
		})
	}
}

// TestFromEnv_ExperimentalTSGridRange_Invalid confirms a bad boolean
// string fails fast at startup rather than silently defaulting.
func TestFromEnv_ExperimentalTSGridRange_Invalid(t *testing.T) {
	t.Setenv("CERBERUS_EXPERIMENTAL_TS_GRID_RANGE", "maybe")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv: want error for invalid bool, got nil")
	}
}

// TestFromEnv_OTLP_Default confirms the disabled-by-default contract:
// no env vars set → empty endpoint, default timeout, no headers. The
// empty endpoint is what installs noop providers in main.
func TestFromEnv_OTLP_Default(t *testing.T) {
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.OTLP.Endpoint != "" {
		t.Errorf("OTLP.Endpoint = %q; want empty", cfg.OTLP.Endpoint)
	}
	if cfg.OTLP.Insecure {
		t.Errorf("OTLP.Insecure = true; want false")
	}
	if got, want := cfg.OTLP.Timeout, 10*time.Second; got != want {
		t.Errorf("OTLP.Timeout = %v; want %v", got, want)
	}
	if cfg.OTLP.Headers != nil {
		t.Errorf("OTLP.Headers = %v; want nil", cfg.OTLP.Headers)
	}
	if got, want := cfg.OTLP.ExportInterval, 10*time.Second; got != want {
		t.Errorf("OTLP.ExportInterval = %v; want %v", got, want)
	}
}

// TestFromEnv_OTLP_ExportIntervalOverride covers the operator-facing
// knob that raises the metric flush cadence above the 10s quickstart
// default when collector load matters more than time-to-visibility.
func TestFromEnv_OTLP_ExportIntervalOverride(t *testing.T) {
	t.Setenv("CERBERUS_OTLP_EXPORT_INTERVAL", "45s")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if got, want := cfg.OTLP.ExportInterval, 45*time.Second; got != want {
		t.Errorf("OTLP.ExportInterval = %v; want %v", got, want)
	}
}

// TestFromEnv_OTLP_InvalidExportInterval rejects a malformed duration so
// the operator sees a startup failure rather than a silent fallback.
func TestFromEnv_OTLP_InvalidExportInterval(t *testing.T) {
	t.Setenv("CERBERUS_OTLP_EXPORT_INTERVAL", "not-a-duration")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv: want error for bad export interval, got nil")
	}
}

// TestFromEnv_OTLP_Populated walks every OTLP env var through the
// parser. Endpoint + insecure flag + custom timeout + headers map.
func TestFromEnv_OTLP_Populated(t *testing.T) {
	t.Setenv("CERBERUS_OTLP_ENDPOINT", "otel-collector.observability.svc:4317")
	t.Setenv("CERBERUS_OTLP_INSECURE", "true")
	t.Setenv("CERBERUS_OTLP_TIMEOUT", "3s")
	t.Setenv("CERBERUS_OTLP_HEADERS", "authorization=Bearer abc, x-tenant=acme")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if got, want := cfg.OTLP.Endpoint, "otel-collector.observability.svc:4317"; got != want {
		t.Errorf("Endpoint = %q; want %q", got, want)
	}
	if !cfg.OTLP.Insecure {
		t.Errorf("Insecure = false; want true")
	}
	if got, want := cfg.OTLP.Timeout, 3*time.Second; got != want {
		t.Errorf("Timeout = %v; want %v", got, want)
	}
	if got, want := cfg.OTLP.Headers["authorization"], "Bearer abc"; got != want {
		t.Errorf("Headers[authorization] = %q; want %q", got, want)
	}
	if got, want := cfg.OTLP.Headers["x-tenant"], "acme"; got != want {
		t.Errorf("Headers[x-tenant] = %q; want %q", got, want)
	}
}

// TestFromEnv_OTLP_InvalidHeaders rejects a header entry without "=" so
// a typo never silently drops an auth token.
func TestFromEnv_OTLP_InvalidHeaders(t *testing.T) {
	t.Setenv("CERBERUS_OTLP_HEADERS", "authorization Bearer abc")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv: want error for headers missing '=', got nil")
	}
}

// TestFromEnv_OTLP_InvalidTimeout surfaces a bad duration as a startup
// error rather than silently falling back to the default.
func TestFromEnv_OTLP_InvalidTimeout(t *testing.T) {
	t.Setenv("CERBERUS_OTLP_TIMEOUT", "not-a-duration")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv: want error for bad timeout, got nil")
	}
}

// TestFromEnv_OTLP_EndpointTrimmed trims whitespace from the endpoint
// value — operators sometimes paste with stray newlines / spaces.
func TestFromEnv_OTLP_EndpointTrimmed(t *testing.T) {
	t.Setenv("CERBERUS_OTLP_ENDPOINT", "  collector:4317\n")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if got, want := cfg.OTLP.Endpoint, "collector:4317"; got != want {
		t.Errorf("Endpoint = %q; want %q", got, want)
	}
}

// TestFromEnv_SchemaDefaults confirms that with no schema env vars set
// the resolved Config.Schema / Logs / Traces match the defaults-only
// factories exactly. The override path is additive — a deploy that
// touches nothing keeps the upstream OTel CH layout.
func TestFromEnv_SchemaDefaults(t *testing.T) {
	for _, key := range []string{
		schema.EnvMetricsGaugeTable,
		schema.EnvMetricsSumTable,
		schema.EnvMetricsHistogramTable,
		schema.EnvMetricsExpHistogramTable,
		schema.EnvMetricsSummaryTable,
		schema.EnvLogsTable,
		schema.EnvTracesTable,
	} {
		t.Setenv(key, "")
	}
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Schema.SumTable != schema.DefaultOTelMetrics().SumTable {
		t.Errorf("Schema.SumTable: got %q, want default", cfg.Schema.SumTable)
	}
	if cfg.Logs.LogsTable != schema.DefaultOTelLogs().LogsTable {
		t.Errorf("Logs.LogsTable: got %q, want default", cfg.Logs.LogsTable)
	}
	if cfg.Traces.SpansTable != schema.DefaultOTelTraces().SpansTable {
		t.Errorf("Traces.SpansTable: got %q, want default", cfg.Traces.SpansTable)
	}
}

// TestFromEnv_SchemaOverrides confirms env-var overrides reach the
// resolved Config struct. Covers one knob per signal — the per-field
// override matrix is exhaustively covered in internal/schema/env_test.go.
func TestFromEnv_SchemaOverrides(t *testing.T) {
	t.Setenv(schema.EnvMetricsSumTable, "custom_metrics_sum")
	t.Setenv(schema.EnvLogsTable, "custom_logs")
	t.Setenv(schema.EnvTracesTable, "custom_spans")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Schema.SumTable != "custom_metrics_sum" {
		t.Errorf("Schema.SumTable: got %q, want %q", cfg.Schema.SumTable, "custom_metrics_sum")
	}
	if cfg.Logs.LogsTable != "custom_logs" {
		t.Errorf("Logs.LogsTable: got %q, want %q", cfg.Logs.LogsTable, "custom_logs")
	}
	if cfg.Traces.SpansTable != "custom_spans" {
		t.Errorf("Traces.SpansTable: got %q, want %q", cfg.Traces.SpansTable, "custom_spans")
	}
	// Sanity: non-overridden fields stay defaulted (column names are not
	// part of the override surface in this milestone).
	if cfg.Schema.GaugeTable != schema.DefaultOTelMetrics().GaugeTable {
		t.Errorf("Schema.GaugeTable should be unchanged: got %q", cfg.Schema.GaugeTable)
	}
}

// TestFromEnv_Admit_Defaults verifies the conservative defaults for
// the per-handler concurrency caps when no env vars are set.
func TestFromEnv_Admit_Defaults(t *testing.T) {
	t.Setenv("CERBERUS_ADMIT_DISABLED", "")
	t.Setenv("CERBERUS_ADMIT_PROM", "")
	t.Setenv("CERBERUS_ADMIT_LOKI", "")
	t.Setenv("CERBERUS_ADMIT_TEMPO", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
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

// TestFromEnv_Admit_Overrides confirms env-var overrides flow through.
func TestFromEnv_Admit_Overrides(t *testing.T) {
	t.Setenv("CERBERUS_ADMIT_DISABLED", "true")
	t.Setenv("CERBERUS_ADMIT_PROM", "128")
	t.Setenv("CERBERUS_ADMIT_LOKI", "16")
	t.Setenv("CERBERUS_ADMIT_TEMPO", "8")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if !cfg.Admit.Disabled {
		t.Errorf("Admit.Disabled = false; want true")
	}
	if cfg.Admit.MaxInflightProm != 128 {
		t.Errorf("MaxInflightProm = %d; want 128", cfg.Admit.MaxInflightProm)
	}
	if cfg.Admit.MaxInflightLoki != 16 {
		t.Errorf("MaxInflightLoki = %d; want 16", cfg.Admit.MaxInflightLoki)
	}
	if cfg.Admit.MaxInflightTempo != 8 {
		t.Errorf("MaxInflightTempo = %d; want 8", cfg.Admit.MaxInflightTempo)
	}
}

// TestFromEnv_Admit_RejectsNegative ensures a negative cap fails fast
// at startup rather than silently disabling admission control.
func TestFromEnv_Admit_RejectsNegative(t *testing.T) {
	t.Setenv("CERBERUS_ADMIT_PROM", "-1")
	if _, err := FromEnv(); err == nil {
		t.Fatalf("FromEnv with CERBERUS_ADMIT_PROM=-1: want error, got nil")
	}
}

// TestFromEnv_Admit_RejectsGarbage ensures a non-integer value also
// fails fast.
func TestFromEnv_Admit_RejectsGarbage(t *testing.T) {
	t.Setenv("CERBERUS_ADMIT_PROM", "not-a-number")
	if _, err := FromEnv(); err == nil {
		t.Fatalf("FromEnv with CERBERUS_ADMIT_PROM=not-a-number: want error, got nil")
	}
}
