package config

import (
	"strings"
	"testing"
)

// TestFromEnv_QueryMaxSamples_Default pins the default-on per-query
// sample budget at 5,000,000 (the backstop for the matrixFromCursor
// OOM class — see defaultQueryMaxSamples). A regression that re-raised
// the default toward Prometheus's 50M (effectively no cap on a ~2Gi
// pod) fails here.
func TestFromEnv_QueryMaxSamples_Default(t *testing.T) {
	t.Setenv("CERBERUS_QUERY_MAX_SAMPLES", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClickHouse.MaxQuerySamples != 5_000_000 {
		t.Errorf("MaxQuerySamples = %d; want 5000000 (default-on OOM backstop)",
			cfg.ClickHouse.MaxQuerySamples)
	}
}

// TestQueryMaxSamples_DefaultIsEnforcedAndSane guards the two ways the
// default-on budget can silently stop protecting the pod: regressing to
// 0 (disabled — the runaway drain OOMs again) or ballooning back toward
// Prometheus's 50M (effectively no cap on a ~2Gi heap, which is exactly
// the prod state that caused the matrixFromCursor OOM). The lower bound
// is the documented safe floor; the upper bound is below the old 50M
// default so a revert to it fails here. The chclient cursor only
// enforces a STRICTLY POSITIVE budget (maxSamples <= 0 means
// "unlimited"), so a positive default is what makes the abort fire at
// all.
func TestQueryMaxSamples_DefaultIsEnforcedAndSane(t *testing.T) {
	const (
		// safeBudgetFloor is the bottom of the empirically-safe 2Gi-pod
		// range recorded for the OOM-bug class; a default below it would
		// start turning legitimate large Grafana queries into 422s.
		safeBudgetFloor int64 = 2_000_000
		// noRealCapCeiling is the threshold above which the budget stops
		// bounding a ~2Gi heap on cerberus's label-carrying rows — the old
		// 50M Prometheus-parity default sat here and protected nothing.
		noRealCapCeiling int64 = 10_000_000
	)
	if defaultQueryMaxSamples <= 0 {
		t.Fatalf("defaultQueryMaxSamples = %d; a non-positive default disables the budget (cursor treats <=0 as unlimited), removing the OOM backstop",
			defaultQueryMaxSamples)
	}
	if defaultQueryMaxSamples < safeBudgetFloor {
		t.Errorf("defaultQueryMaxSamples = %d; below the safe floor %d — would 422 realistic large queries",
			defaultQueryMaxSamples, safeBudgetFloor)
	}
	if defaultQueryMaxSamples > noRealCapCeiling {
		t.Errorf("defaultQueryMaxSamples = %d; above %d is no real cap on a ~2Gi pod (the pre-fix OOM state)",
			defaultQueryMaxSamples, noRealCapCeiling)
	}
}

// TestFromEnv_QueryMaxSamples_Override confirms the env var threads
// through to chclient.Config, including the documented 0 = disabled
// opt-out.
func TestFromEnv_QueryMaxSamples_Override(t *testing.T) {
	cases := []struct {
		val  string
		want int64
	}{
		{"5000000", 5_000_000},
		{"0", 0},
		{"1", 1},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("CERBERUS_QUERY_MAX_SAMPLES", tc.val)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.ClickHouse.MaxQuerySamples != tc.want {
				t.Errorf("MaxQuerySamples = %d; want %d", cfg.ClickHouse.MaxQuerySamples, tc.want)
			}
		})
	}
}

// TestFromEnv_QueryMaxSamples_Invalid confirms non-integer and negative
// values fail fast at startup rather than silently defaulting.
func TestFromEnv_QueryMaxSamples_Invalid(t *testing.T) {
	for _, val := range []string{"lots", "1.5", "-1"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("CERBERUS_QUERY_MAX_SAMPLES", val)
			_, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv accepted %q; want error", val)
			}
			if !strings.Contains(err.Error(), "CERBERUS_QUERY_MAX_SAMPLES") {
				t.Errorf("error %q does not name the env var", err)
			}
		})
	}
}
