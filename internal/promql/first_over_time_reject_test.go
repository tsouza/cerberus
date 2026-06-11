package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_FirstOverTime_RejectedByPromQLHead pins the cross-head gate:
// `first_over_time` is an experimental PromQL function the reference
// backend (prom/prometheus:v3.11.3, started without the experimental-
// functions feature flag in the compatibility harness) rejects. Cerberus's
// parser enables experimental functions for the deliberately-supported
// subset, so it parses `first_over_time`, but the *shared* chsql over-time
// emitter gained `first_over_time` for the LogQL head (`first_over_time(...
// | unwrap v)`). Without a PromQL-lowering gate, the RangeWindow would
// reach that emitter and execute — silently turning a parity rejection into
// a wrong acceptance and breaking the showcase-promql bidirectional pin.
//
// The error message must contain "unsupported: range function" so the
// showcase-promql parity-rejection contract substring still matches.
func TestLower_FirstOverTime_RejectedByPromQLHead(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	expr, err := p.ParseExpr(`first_over_time(up[5m])`)
	if err != nil {
		t.Fatalf("ParseExpr(first_over_time): %v", err)
	}
	_, err = promql.Lower(context.Background(), expr, s)
	if err == nil {
		t.Fatal("expected first_over_time to be rejected by the PromQL head, got nil error")
	}
	if want := "unsupported: range function"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain showcase contract substring %q", err.Error(), want)
	}
}

// TestLower_LastOverTime_StillSupported guards the gate's blast radius:
// `last_over_time` is non-experimental and reference-supported, so it must
// keep lowering cleanly — the gate is `first_over_time`-only.
func TestLower_LastOverTime_StillSupported(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	expr, err := p.ParseExpr(`last_over_time(up[5m])`)
	if err != nil {
		t.Fatalf("ParseExpr(last_over_time): %v", err)
	}
	if _, err := promql.Lower(context.Background(), expr, s); err != nil {
		t.Fatalf("last_over_time should lower cleanly, got: %v", err)
	}
}
