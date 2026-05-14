package config

import (
	"strings"
	"testing"
	"time"
)

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
// otel default + arbitrary override.
func TestFromEnv_CHDatabase_DefaultAndOverride(t *testing.T) {
	t.Setenv("CERBERUS_CH_DATABASE", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv default: %v", err)
	}
	if cfg.ClickHouse.Database != "otel" {
		t.Errorf("CH.Database default = %q; want otel", cfg.ClickHouse.Database)
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

// TestFromEnv_AdmitDisabled_DoesNotCascadeToCaps confirms setting
// CERBERUS_ADMIT_DISABLED=true leaves the per-head caps intact in the
// resolved Config. The wiring in main.go is responsible for skipping
// limiter construction — the config layer just reports both signals.
func TestFromEnv_AdmitDisabled_DoesNotCascadeToCaps(t *testing.T) {
	t.Setenv("CERBERUS_ADMIT_DISABLED", "true")
	t.Setenv("CERBERUS_ADMIT_PROM", "256")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if !cfg.Admit.Disabled {
		t.Errorf("Disabled = false; want true")
	}
	if cfg.Admit.MaxInflightProm != 256 {
		t.Errorf("MaxInflightProm = %d; want 256 (caps reported even when disabled)", cfg.Admit.MaxInflightProm)
	}
}

// TestFromEnv_AdmitZeroIsAllowed pins the "zero disables this head"
// contract — the config layer accepts 0 (only negative values fail).
func TestFromEnv_AdmitZeroIsAllowed(t *testing.T) {
	t.Setenv("CERBERUS_ADMIT_LOKI", "0")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv with zero loki: %v", err)
	}
	if cfg.Admit.MaxInflightLoki != 0 {
		t.Errorf("MaxInflightLoki = %d; want 0", cfg.Admit.MaxInflightLoki)
	}
}

// TestFromEnv_AdmitLargeValueRoundtrip confirms large but valid ints
// survive the parser (no premature overflow check).
func TestFromEnv_AdmitLargeValueRoundtrip(t *testing.T) {
	t.Setenv("CERBERUS_ADMIT_TEMPO", "100000")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Admit.MaxInflightTempo != 100000 {
		t.Errorf("MaxInflightTempo = %d; want 100000", cfg.Admit.MaxInflightTempo)
	}
}

// TestFromEnv_AdmitTempoNegative ensures every per-head cap reports
// negative values consistently. (The existing Prom-specific test
// confirms the symmetric path.)
func TestFromEnv_AdmitTempoNegative(t *testing.T) {
	t.Setenv("CERBERUS_ADMIT_TEMPO", "-5")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv: want error for negative tempo, got nil")
	}
}

// TestFromEnv_AdmitLokiNegative covers the loki-cap negative-path
// symmetry.
func TestFromEnv_AdmitLokiNegative(t *testing.T) {
	t.Setenv("CERBERUS_ADMIT_LOKI", "-10")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv: want error for negative loki, got nil")
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
