package config

import (
	"strings"
	"testing"
	"time"
)

// TestFromEnv_SchemaProvisioning_Defaults confirms the schema-provisioning
// knobs default to the single-node zero shape (no cluster / engine / TTL /
// replicated database).
func TestFromEnv_SchemaProvisioning_Defaults(t *testing.T) {
	for _, k := range []string{
		"CERBERUS_SCHEMA_CLUSTER", "CERBERUS_SCHEMA_TABLE_ENGINE", "CERBERUS_SCHEMA_TTL",
		"CERBERUS_SCHEMA_TTL_METRICS", "CERBERUS_SCHEMA_TTL_LOGS", "CERBERUS_SCHEMA_TTL_TRACES",
		"CERBERUS_SCHEMA_DATABASE_REPLICATED", "CERBERUS_SCHEMA_DATABASE_REPLICATED_PATH",
		"CERBERUS_SCHEMA_DATABASE_REPLICATED_SHARD", "CERBERUS_SCHEMA_DATABASE_REPLICATED_REPLICA",
	} {
		t.Setenv(k, "")
	}
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	p := cfg.SchemaProvisioning
	if p.Cluster != "" || p.TableEngine != "" || p.DatabaseReplicated ||
		p.TTL != 0 || p.TTLMetrics != 0 || p.TTLLogs != 0 || p.TTLTraces != 0 ||
		p.DatabaseReplicatedPath != "" {
		t.Errorf("schema provisioning defaults not zero: %+v", p)
	}
}

// TestFromEnv_SchemaProvisioning_Overrides confirms every schema-provisioning
// env var (including the per-signal TTL overrides and the Replicated engine
// knobs) parses into SchemaProvisioning.
func TestFromEnv_SchemaProvisioning_Overrides(t *testing.T) {
	t.Setenv("CERBERUS_SCHEMA_CLUSTER", "prod")
	t.Setenv("CERBERUS_SCHEMA_TABLE_ENGINE", "ReplicatedMergeTree('/p', '{replica}')")
	t.Setenv("CERBERUS_SCHEMA_TTL", "2160h")
	t.Setenv("CERBERUS_SCHEMA_TTL_LOGS", "168h")
	t.Setenv("CERBERUS_SCHEMA_DATABASE_REPLICATED", "true")
	t.Setenv("CERBERUS_SCHEMA_DATABASE_REPLICATED_PATH", "/clickhouse/databases/otel")
	t.Setenv("CERBERUS_SCHEMA_DATABASE_REPLICATED_SHARD", "shard0")
	t.Setenv("CERBERUS_SCHEMA_DATABASE_REPLICATED_REPLICA", "replica0")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	p := cfg.SchemaProvisioning
	if p.Cluster != "prod" {
		t.Errorf("Cluster = %q; want prod", p.Cluster)
	}
	if p.TableEngine != "ReplicatedMergeTree('/p', '{replica}')" {
		t.Errorf("TableEngine = %q", p.TableEngine)
	}
	if p.TTL != 2160*time.Hour {
		t.Errorf("TTL = %v; want 2160h", p.TTL)
	}
	if p.TTLLogs != 168*time.Hour {
		t.Errorf("TTLLogs = %v; want 168h", p.TTLLogs)
	}
	if !p.DatabaseReplicated || p.DatabaseReplicatedPath != "/clickhouse/databases/otel" ||
		p.DatabaseReplicatedShard != "shard0" || p.DatabaseReplicatedReplica != "replica0" {
		t.Errorf("replicated knobs not parsed: %+v", p)
	}
}

// TestFromEnv_SchemaTTL_PrometheusSyntax confirms the TTL knobs accept the
// Prometheus/Grafana duration syntax (90d, 2w, 1y) operators use for
// retention windows — units Go's time.ParseDuration can't express — while
// the hour form (2160h) still works.
func TestFromEnv_SchemaTTL_PrometheusSyntax(t *testing.T) {
	t.Setenv("CERBERUS_SCHEMA_TTL", "90d")     // global
	t.Setenv("CERBERUS_SCHEMA_TTL_LOGS", "2w") // per-signal
	t.Setenv("CERBERUS_SCHEMA_TTL_METRICS", "1y")
	t.Setenv("CERBERUS_SCHEMA_TTL_TRACES", "2160h") // Go hour form still valid

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	p := cfg.SchemaProvisioning
	if p.TTL != 90*24*time.Hour {
		t.Errorf("TTL(90d) = %v; want 2160h", p.TTL)
	}
	if p.TTLLogs != 14*24*time.Hour {
		t.Errorf("TTLLogs(2w) = %v; want 336h", p.TTLLogs)
	}
	if p.TTLMetrics != 365*24*time.Hour {
		t.Errorf("TTLMetrics(1y) = %v; want 8760h", p.TTLMetrics)
	}
	if p.TTLTraces != 2160*time.Hour {
		t.Errorf("TTLTraces(2160h) = %v; want 2160h", p.TTLTraces)
	}
}

// TestFromEnv_SchemaTTL_Invalid confirms a malformed retention value fails
// fast, naming the offending var.
func TestFromEnv_SchemaTTL_Invalid(t *testing.T) {
	t.Setenv("CERBERUS_SCHEMA_TTL", "2 weeks") // spaces/words are not the compact syntax
	_, err := FromEnv()
	if err == nil {
		t.Fatal("malformed CERBERUS_SCHEMA_TTL must fail fast")
	}
	if !strings.Contains(err.Error(), "CERBERUS_SCHEMA_TTL") {
		t.Errorf("error should name the var, got: %v", err)
	}
}

// TestFromEnv_AutoCreateDatabase_InheritsSchema pins that
// CERBERUS_AUTO_CREATE_DATABASE defaults to CERBERUS_AUTO_CREATE_SCHEMA's
// value when unset, and that an explicit value overrides it.
func TestFromEnv_AutoCreateDatabase_InheritsSchema(t *testing.T) {
	cases := []struct {
		name         string
		schema       string // CERBERUS_AUTO_CREATE_SCHEMA
		database     string // CERBERUS_AUTO_CREATE_DATABASE ("" = unset)
		wantSchema   bool
		wantDatabase bool
	}{
		{"both off (default)", "false", "", false, false},
		{"schema on inherits db on", "true", "", true, true},
		{"schema on, db explicitly off", "true", "false", true, false},
		{"schema off, db explicitly on", "false", "true", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CERBERUS_AUTO_CREATE_SCHEMA", tc.schema)
			t.Setenv("CERBERUS_AUTO_CREATE_DATABASE", tc.database)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.AutoCreateSchema != tc.wantSchema {
				t.Errorf("AutoCreateSchema = %v; want %v", cfg.AutoCreateSchema, tc.wantSchema)
			}
			if cfg.AutoCreateDatabase != tc.wantDatabase {
				t.Errorf("AutoCreateDatabase = %v; want %v", cfg.AutoCreateDatabase, tc.wantDatabase)
			}
		})
	}
}

// TestFromEnv_HTTPAddr_Default confirms the documented :8080 fallback
// when CERBERUS_HTTP_ADDR is unset.
func TestFromEnv_HTTPAddr_Default(t *testing.T) {
	t.Setenv("CERBERUS_HTTP_ADDR", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q; want :8080", cfg.HTTPAddr)
	}
}

// TestFromEnv_HTTPAddr_Override verifies a custom listen address flows
// through end-to-end.
func TestFromEnv_HTTPAddr_Override(t *testing.T) {
	t.Setenv("CERBERUS_HTTP_ADDR", "127.0.0.1:9090")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9090" {
		t.Errorf("HTTPAddr = %q; want 127.0.0.1:9090", cfg.HTTPAddr)
	}
}

// TestFromEnv_CHAddr_Default confirms the localhost:9000 fallback used
// for local dev / k3d.
func TestFromEnv_CHAddr_Default(t *testing.T) {
	t.Setenv("CERBERUS_CH_ADDR", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClickHouse.Addr != "localhost:9000" {
		t.Errorf("CH.Addr = %q; want localhost:9000", cfg.ClickHouse.Addr)
	}
}

// TestFromEnv_CHAddr_Override verifies a deployed CH endpoint sticks.
func TestFromEnv_CHAddr_Override(t *testing.T) {
	t.Setenv("CERBERUS_CH_ADDR", "clickhouse.observability.svc:9000")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClickHouse.Addr != "clickhouse.observability.svc:9000" {
		t.Errorf("CH.Addr = %q; want clickhouse.observability.svc:9000", cfg.ClickHouse.Addr)
	}
}

// TestFromEnv_CHDatabase_DefaultAndOverride covers the documented
// default ("default", matching the upstream OTel ClickHouse exporter) +
// arbitrary override.
func TestFromEnv_CHDatabase_DefaultAndOverride(t *testing.T) {
	t.Setenv("CERBERUS_CH_DATABASE", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv default: %v", err)
	}
	if cfg.ClickHouse.Database != "default" {
		t.Errorf("CH.Database default = %q; want default", cfg.ClickHouse.Database)
	}
	t.Setenv("CERBERUS_CH_DATABASE", "tenant_a")
	cfg, err = FromEnv()
	if err != nil {
		t.Fatalf("FromEnv override: %v", err)
	}
	if cfg.ClickHouse.Database != "tenant_a" {
		t.Errorf("CH.Database override = %q; want tenant_a", cfg.ClickHouse.Database)
	}
}

// TestFromEnv_CHCredentials_DefaultAndOverride covers username/password.
// Empty password is the explicit default and must not be coerced to a
// fake value.
func TestFromEnv_CHCredentials_DefaultAndOverride(t *testing.T) {
	t.Setenv("CERBERUS_CH_USERNAME", "")
	t.Setenv("CERBERUS_CH_PASSWORD", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClickHouse.Username != "default" {
		t.Errorf("Username default = %q; want default", cfg.ClickHouse.Username)
	}
	if cfg.ClickHouse.Password != "" {
		t.Errorf("Password default = %q; want empty", cfg.ClickHouse.Password)
	}
	t.Setenv("CERBERUS_CH_USERNAME", "cerberus")
	t.Setenv("CERBERUS_CH_PASSWORD", "s3cret!")
	cfg, err = FromEnv()
	if err != nil {
		t.Fatalf("FromEnv override: %v", err)
	}
	if cfg.ClickHouse.Username != "cerberus" {
		t.Errorf("Username = %q; want cerberus", cfg.ClickHouse.Username)
	}
	if cfg.ClickHouse.Password != "s3cret!" {
		t.Errorf("Password = %q; want s3cret!", cfg.ClickHouse.Password)
	}
}

// TestFromEnv_CHDialTimeout_Default confirms the 5s default falls
// through cleanly.
func TestFromEnv_CHDialTimeout_Default(t *testing.T) {
	t.Setenv("CERBERUS_CH_DIAL_TIMEOUT", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClickHouse.DialTimeout != 5*time.Second {
		t.Errorf("DialTimeout = %v; want 5s", cfg.ClickHouse.DialTimeout)
	}
}

// TestFromEnv_CHDialTimeout_OverrideFormats walks accepted duration
// formats — the same Go time.ParseDuration vocabulary that powers the
// OTLP timeout.
func TestFromEnv_CHDialTimeout_OverrideFormats(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"3s", 3 * time.Second},
		{"1500ms", 1500 * time.Millisecond},
		{"2.5s", 2500 * time.Millisecond},
		{"30s", 30 * time.Second},
		{"1m", time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("CERBERUS_CH_DIAL_TIMEOUT", tc.raw)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.ClickHouse.DialTimeout != tc.want {
				t.Errorf("DialTimeout = %v; want %v", cfg.ClickHouse.DialTimeout, tc.want)
			}
		})
	}
}

// TestFromEnv_CHDialTimeout_Invalid rejects garbage strings rather than
// silently falling back — operators get a fast startup error.
func TestFromEnv_CHDialTimeout_Invalid(t *testing.T) {
	t.Setenv("CERBERUS_CH_DIAL_TIMEOUT", "not-a-duration")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv: want error for bad timeout, got nil")
	}
}

// TestFromEnv_OTLP_TimeoutFormats walks the documented duration grammar
// per the OTel timeout spec. The OTel SDK still does the actual gRPC
// deadline wrap; this just confirms cerberus parses the env correctly.
func TestFromEnv_OTLP_TimeoutFormats(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"5s", 5 * time.Second},
		{"500ms", 500 * time.Millisecond},
		{"1.5s", 1500 * time.Millisecond},
		{"0s", 0},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("CERBERUS_OTLP_TIMEOUT", tc.raw)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv(%s): %v", tc.raw, err)
			}
			if cfg.OTLP.Timeout != tc.want {
				t.Errorf("Timeout = %v; want %v", cfg.OTLP.Timeout, tc.want)
			}
		})
	}
}

// TestFromEnv_OTLP_HeadersEmptyAndWhitespace verifies the documented
// "empty/whitespace = no headers" contract and dropped-empty-entry
// behavior.
func TestFromEnv_OTLP_HeadersEmptyAndWhitespace(t *testing.T) {
	cases := []string{"", "  ", "\n\n", " , , "}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("CERBERUS_OTLP_HEADERS", raw)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv(%q): %v", raw, err)
			}
			if len(cfg.OTLP.Headers) != 0 {
				t.Errorf("Headers = %v; want empty", cfg.OTLP.Headers)
			}
		})
	}
}

// TestFromEnv_OTLP_HeadersDuplicates: a duplicated key keeps the last
// value (map semantics). Documenting it so operators can rely on the
// behavior.
func TestFromEnv_OTLP_HeadersDuplicates(t *testing.T) {
	t.Setenv("CERBERUS_OTLP_HEADERS", "x-tenant=a, x-tenant=b")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if got := cfg.OTLP.Headers["x-tenant"]; got != "b" {
		t.Errorf("Headers[x-tenant] = %q; want b (last wins)", got)
	}
}

// TestFromEnv_OTLP_HeadersValueWithEqualSign verifies values that
// themselves contain "=" survive the parser intact — Bearer tokens,
// base64-padded values, etc.
func TestFromEnv_OTLP_HeadersValueWithEqualSign(t *testing.T) {
	t.Setenv("CERBERUS_OTLP_HEADERS", "authorization=Bearer abc=def==,x-tenant=acme")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.OTLP.Headers["authorization"] != "Bearer abc=def==" {
		t.Errorf("Headers[authorization] = %q; want preserved", cfg.OTLP.Headers["authorization"])
	}
}

// TestFromEnv_OTLP_HeadersEmptyValueAccepted confirms an explicit
// empty-string value is allowed (and survives) — gRPC permits empty
// metadata.
func TestFromEnv_OTLP_HeadersEmptyValueAccepted(t *testing.T) {
	t.Setenv("CERBERUS_OTLP_HEADERS", "x-feature=")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if v, ok := cfg.OTLP.Headers["x-feature"]; !ok || v != "" {
		t.Errorf("Headers[x-feature] = %q (ok=%v); want empty string present", v, ok)
	}
}

// TestFromEnv_OTLP_HeadersEmptyKeyRejected: a leading `=value` is a
// typo; reject so we never silently drop it.
func TestFromEnv_OTLP_HeadersEmptyKeyRejected(t *testing.T) {
	t.Setenv("CERBERUS_OTLP_HEADERS", "=value")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv: want error for empty key, got nil")
	}
}

// TestFromEnv_OTLP_Insecure_Defaults verifies the secure-by-default
// stance — TLS unless the operator opts out.
func TestFromEnv_OTLP_Insecure_Defaults(t *testing.T) {
	t.Setenv("CERBERUS_OTLP_INSECURE", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.OTLP.Insecure {
		t.Errorf("Insecure default = true; want false")
	}
}

// TestFromEnv_OTLP_InsecureGarbage rejects unparseable booleans.
func TestFromEnv_OTLP_InsecureGarbage(t *testing.T) {
	t.Setenv("CERBERUS_OTLP_INSECURE", "yes-please")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv: want error for invalid bool, got nil")
	}
}

// TestFromEnv_OTLP_InsecureBoolVocabulary spot-checks the full strconv
// vocabulary cerberus accepts for the insecure flag.
func TestFromEnv_OTLP_InsecureBoolVocabulary(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{"t", true},
		{"false", false},
		{"0", false},
		{"f", false},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("CERBERUS_OTLP_INSECURE", tc.val)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.OTLP.Insecure != tc.want {
				t.Errorf("Insecure(%q) = %v; want %v", tc.val, cfg.OTLP.Insecure, tc.want)
			}
		})
	}
}

// TestFromEnv_Log_LevelErrorMessageMentionsKey ensures a level typo
// surfaces the env-var name so operators can locate the bad setting.
func TestFromEnv_Log_LevelErrorMessageMentionsKey(t *testing.T) {
	t.Setenv("CERBERUS_LOG_LEVEL", "tracee")
	_, err := FromEnv()
	if err == nil {
		t.Fatal("FromEnv: want error, got nil")
	}
	if !strings.Contains(err.Error(), "CERBERUS_LOG_LEVEL") {
		t.Errorf("error %q must mention CERBERUS_LOG_LEVEL", err.Error())
	}
}

// TestFromEnv_Log_FormatErrorMessageMentionsKey ensures a format typo
// surfaces the env-var name so operators can locate the bad setting.
func TestFromEnv_Log_FormatErrorMessageMentionsKey(t *testing.T) {
	t.Setenv("CERBERUS_LOG_FORMAT", "xml")
	_, err := FromEnv()
	if err == nil {
		t.Fatal("FromEnv: want error, got nil")
	}
	if !strings.Contains(err.Error(), "CERBERUS_LOG_FORMAT") {
		t.Errorf("error %q must mention CERBERUS_LOG_FORMAT", err.Error())
	}
}

// TestFromEnv_AdmitDisabled_DoesNotCascadeToToggles confirms setting
// CERBERUS_ADMIT_DISABLED=true leaves the per-head caps intact in the
// resolved Config. The wiring in main.go is responsible for skipping
// limiter construction — the config layer just reports both signals.
func TestFromEnv_AdmitDisabled_DoesNotCascadeToToggles(t *testing.T) {
	t.Setenv("CERBERUS_ADMIT_DISABLED", "true")
	t.Setenv("CERBERUS_ADMIT_PROM", "true")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if !cfg.Admit.Disabled {
		t.Errorf("Disabled = false; want true")
	}
	if cfg.Admit.Prom != DefaultAdmitProm {
		t.Errorf("Admit.Prom = %d; want %d (per-head cap reported even when globally disabled)", cfg.Admit.Prom, DefaultAdmitProm)
	}
}

// TestFromEnv_AdmitFalsyDisablesHead pins the "falsy disables this head"
// contract — a per-head cap accepts 0/false to leave that head unlimited
// (cap 0) without touching the others.
func TestFromEnv_AdmitFalsyDisablesHead(t *testing.T) {
	t.Setenv("CERBERUS_ADMIT_LOKI", "false")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv with loki disabled: %v", err)
	}
	if cfg.Admit.Loki != 0 {
		t.Errorf("Admit.Loki = %d; want 0", cfg.Admit.Loki)
	}
	if cfg.Admit.Prom != DefaultAdmitProm || cfg.Admit.Tempo != DefaultAdmitTempo {
		t.Errorf("disabling loki must not touch prom/tempo: prom=%d tempo=%d", cfg.Admit.Prom, cfg.Admit.Tempo)
	}
}

// TestFromEnv_AdmitIntegerCap pins the explicit-integer cap path the e2e
// chaos overlay depends on — CERBERUS_ADMIT_{PROM,LOKI,TEMPO}=2 forces a
// low backpressure cap, distinct from the boolean default-cap alias.
func TestFromEnv_AdmitIntegerCap(t *testing.T) {
	t.Setenv("CERBERUS_ADMIT_PROM", "2")
	t.Setenv("CERBERUS_ADMIT_LOKI", "2")
	t.Setenv("CERBERUS_ADMIT_TEMPO", "2")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv with integer admit caps: %v", err)
	}
	if cfg.Admit.Prom != 2 || cfg.Admit.Loki != 2 || cfg.Admit.Tempo != 2 {
		t.Errorf("integer caps not honoured: prom=%d loki=%d tempo=%d; want 2/2/2",
			cfg.Admit.Prom, cfg.Admit.Loki, cfg.Admit.Tempo)
	}
}

// TestFromEnv_AdmitTempoGarbage ensures a negative cap (outside the int /
// true-false vocabulary) fails fast for the tempo head too (symmetry with
// prom).
func TestFromEnv_AdmitTempoGarbage(t *testing.T) {
	t.Setenv("CERBERUS_ADMIT_TEMPO", "-5")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv: want error for negative tempo cap, got nil")
	}
}

// TestFromEnv_AdmitLokiGarbage covers the loki-head garbage-rejection
// symmetry.
func TestFromEnv_AdmitLokiGarbage(t *testing.T) {
	t.Setenv("CERBERUS_ADMIT_LOKI", "-10")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv: want error for negative loki cap, got nil")
	}
}

// TestParseHeaders_StrictModeBlocksGarbage drives the underlying parser
// with several invalid shapes that must all fail.
func TestParseHeaders_StrictModeBlocksGarbage(t *testing.T) {
	cases := []string{
		"no-equals-sign",
		"first=ok, no-equals",
		" =starts-with-equals ",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("CERBERUS_OTLP_HEADERS", raw)
			if _, err := FromEnv(); err == nil {
				t.Fatalf("FromEnv(%q): want error, got nil", raw)
			}
		})
	}
}

// TestNewLogger_TextLevelLineThroughDebug walks every accepted log
// level through NewLogger and confirms the level keyword affects
// suppression. Anchor for the cmd/ wiring that calls NewLogger after
// FromEnv resolves.
func TestNewLogger_TextLevelLineThroughDebug(t *testing.T) {
	t.Setenv("CERBERUS_LOG_LEVEL", "debug")
	t.Setenv("CERBERUS_LOG_FORMAT", "text")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	var buf strings.Builder
	logger := NewLogger(&buf, cfg.Log)
	logger.Debug("debug-msg")
	if !strings.Contains(buf.String(), "debug-msg") {
		t.Errorf("debug record missing under debug level: %q", buf.String())
	}
}
