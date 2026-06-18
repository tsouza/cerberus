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

// TestFromEnv_DebugPProf confirms the pprof toggle defaults OFF and flips ON
// only when CERBERUS_DEBUG_PPROF is explicitly true — the profiling surface
// must never ship open by default.
func TestFromEnv_DebugPProf(t *testing.T) {
	t.Setenv("CERBERUS_DEBUG_PPROF", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.DebugPProf {
		t.Errorf("DebugPProf = true; want false (default off)")
	}

	t.Setenv("CERBERUS_DEBUG_PPROF", "true")
	cfg, err = FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if !cfg.DebugPProf {
		t.Errorf("DebugPProf = false; want true (CERBERUS_DEBUG_PPROF=true)")
	}
}

// TestFromEnv_RequirementsCheck_Default confirms the preflight knob defaults
// to true (ON) when CERBERUS_REQUIREMENTS_CHECK is unset.
func TestFromEnv_RequirementsCheck_Default(t *testing.T) {
	t.Setenv("CERBERUS_REQUIREMENTS_CHECK", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if !cfg.RequirementsCheck {
		t.Errorf("RequirementsCheck = false; want true (default ON)")
	}
}

// TestFromEnv_RequirementsCheck_Parsing covers the ParseBool vocabulary and
// confirms the knob can be turned off.
func TestFromEnv_RequirementsCheck_Parsing(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"true", true},
		{"1", true},
		{"false", false},
		{"FALSE", false},
		{"0", false},
		{"f", false},
		{"  false  ", false},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("CERBERUS_REQUIREMENTS_CHECK", tc.val)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.RequirementsCheck != tc.want {
				t.Errorf("RequirementsCheck = %v; want %v", cfg.RequirementsCheck, tc.want)
			}
		})
	}
}

// TestFromEnv_RequirementsCheck_Invalid confirms a bad boolean fails fast.
func TestFromEnv_RequirementsCheck_Invalid(t *testing.T) {
	t.Setenv("CERBERUS_REQUIREMENTS_CHECK", "maybe")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv: want error for invalid bool, got nil")
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

// TestFromEnv_Admit_Defaults verifies admission control is enabled on
// every head out of the box when no env vars are set.
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
	if cfg.Admit.Prom != DefaultAdmitProm {
		t.Errorf("Admit.Prom = %d; want %d (default cap)", cfg.Admit.Prom, DefaultAdmitProm)
	}
	if cfg.Admit.Loki != DefaultAdmitLoki {
		t.Errorf("Admit.Loki = %d; want %d (default cap)", cfg.Admit.Loki, DefaultAdmitLoki)
	}
	if cfg.Admit.Tempo != DefaultAdmitTempo {
		t.Errorf("Admit.Tempo = %d; want %d (default cap)", cfg.Admit.Tempo, DefaultAdmitTempo)
	}
}

// TestFromEnv_Admit_Overrides confirms env-var overrides flow through.
// The per-head ADMIT_* caps accept a falsy value ("false"/"0") to leave
// that head unlimited (cap 0) and a truthy value ("true") to enable at the
// head's default cap, independent of the CERBERUS_ADMIT_DISABLED master.
func TestFromEnv_Admit_Overrides(t *testing.T) {
	t.Setenv("CERBERUS_ADMIT_DISABLED", "true")
	t.Setenv("CERBERUS_ADMIT_PROM", "false")
	t.Setenv("CERBERUS_ADMIT_LOKI", "0")
	t.Setenv("CERBERUS_ADMIT_TEMPO", "true")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if !cfg.Admit.Disabled {
		t.Errorf("Admit.Disabled = false; want true")
	}
	if cfg.Admit.Prom != 0 {
		t.Errorf("Admit.Prom = %d; want 0 (false disables the head)", cfg.Admit.Prom)
	}
	if cfg.Admit.Loki != 0 {
		t.Errorf("Admit.Loki = %d; want 0 (\"0\" disables the head)", cfg.Admit.Loki)
	}
	if cfg.Admit.Tempo != DefaultAdmitTempo {
		t.Errorf("Admit.Tempo = %d; want %d (\"true\" = default cap)", cfg.Admit.Tempo, DefaultAdmitTempo)
	}
}

// TestFromEnv_Admit_AcceptsIntOrBool pins the per-head cap contract: an
// explicit integer caps the head at that value, while the boolean
// spellings act as aliases ("true" -> the head's default cap, "false"/"0"
// -> unlimited). The boolean path is also the end-to-end regression for
// the Helm-chart crash — a default `helm install` renders the YAML bool
// `true` into CERBERUS_ADMIT_PROM, which a strict integer parser would
// reject ("invalid integer \"true\"") and crash-loop cerberus. The
// explicit-integer path (e.g. "2") is what the e2e chaos overlay relies on
// to force a low backpressure cap.
func TestFromEnv_Admit_AcceptsIntOrBool(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"true", DefaultAdmitProm},
		{"t", DefaultAdmitProm},
		{"TRUE", DefaultAdmitProm},
		{"false", 0},
		{"False", 0},
		{"f", 0},
		{"0", 0},
		{"1", 1},
		{"2", 2},
		{"128", 128},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("CERBERUS_ADMIT_PROM", tc.raw)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv with CERBERUS_ADMIT_PROM=%q: unexpected error %v", tc.raw, err)
			}
			if cfg.Admit.Prom != tc.want {
				t.Errorf("Admit.Prom = %d; want %d for %q", cfg.Admit.Prom, tc.want, tc.raw)
			}
		})
	}
}

// TestFromEnv_Admit_RejectsGarbage ensures a value outside the int/bool
// vocabulary (and a negative cap) fails fast at startup rather than
// silently mis-parsing.
func TestFromEnv_Admit_RejectsGarbage(t *testing.T) {
	for _, raw := range []string{"maybe", "-1", "1.5", "2x"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("CERBERUS_ADMIT_PROM", raw)
			if _, err := FromEnv(); err == nil {
				t.Fatalf("FromEnv with CERBERUS_ADMIT_PROM=%q: want error, got nil", raw)
			}
		})
	}
}

// TestParseBool is the table-driven contract for the shared boolean
// parser every CERBERUS_* boolean knob routes through. It must accept
// the full strconv.ParseBool vocabulary (1/0/true/false, case-
// insensitive) and reject anything else.
func TestParseBool(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    bool
		wantErr bool
	}{
		{"1", true, false},
		{"0", false, false},
		{"true", true, false},
		{"false", false, false},
		{"TRUE", true, false},
		{"False", false, false},
		{"  true  ", true, false},
		{"maybe", false, true},
		{"2", false, true},
		{"", false, true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseBool(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseBool(%q): want error, got nil", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBool(%q): unexpected error %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("parseBool(%q) = %v; want %v", tc.raw, got, tc.want)
			}
		})
	}
}
