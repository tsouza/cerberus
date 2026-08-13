package main

import (
	"testing"
	"time"
)

// envOr is the seeder's entire environment contract: every endpoint flag
// takes its DEFAULT from an env var, so the compose tier can point the
// seeder at its published ports without rewriting the command line. An
// empty variable must fall through to the compiled default rather than
// dialling the empty string, which fails as a confusing connection error
// far from its cause.
func TestEnvOrFallsBackOnUnsetAndEmpty(t *testing.T) {
	const key = "CERBERUS_MIGRATION_SEED_TEST_ADDR"

	if got := envOr(key, defaultCHAddr); got != defaultCHAddr {
		t.Fatalf("unset %s = %q, want the compiled default %q", key, got, defaultCHAddr)
	}

	t.Setenv(key, "")
	if got := envOr(key, defaultCHAddr); got != defaultCHAddr {
		t.Fatalf("empty %s = %q, want the compiled default %q", key, got, defaultCHAddr)
	}

	t.Setenv(key, "clickhouse:9000")
	if got := envOr(key, defaultCHAddr); got != "clickhouse:9000" {
		t.Fatalf("set %s = %q, want the environment's value", key, got)
	}
}

// Every budget here is a deadline on a poll loop that FAILS the seed when
// it expires, so a sub-budget wider than the run budget is a deadline that
// can never be reached — the run dies first, reporting the wrong cause.
func TestEveryWaitFitsInsideTheRunBudget(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		wait time.Duration
	}{
		{"schemaWait", schemaWait},
		{"referenceReadyWait", referenceReadyWait},
		{"settleWait", settleWait},
	} {
		if tc.wait <= 0 {
			t.Fatalf("%s = %v, want a positive deadline", tc.name, tc.wait)
		}
		if tc.wait >= runBudget {
			t.Fatalf("%s = %v is not inside the %v run budget: the run dies before the deadline fires",
				tc.name, tc.wait, runBudget)
		}
	}
}
