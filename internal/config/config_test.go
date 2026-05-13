package config

import (
	"testing"
	"time"
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
