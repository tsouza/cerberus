package config

import (
	"testing"
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
