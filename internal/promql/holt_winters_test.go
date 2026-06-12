package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_HoltWinters_RejectedByPromQLHead pins the cross-head parity
// gate: `double_exponential_smoothing` (and its legacy `holt_winters`
// alias) is an experimental PromQL function the reference backend
// (prom/prometheus:v3.11.3, started WITHOUT
// `--enable-feature=promql-experimental-functions` in the compatibility
// harness) rejects. Cerberus's parser enables experimental functions for
// the deliberately-supported extension subset (`@start()`/`@end()`,
// `predict_linear`), so it parses the call, but the lowering dispatch now
// rejects it to keep the PromQL head at strict reference parity (the
// #64 surface-parity prober flagged it as the lone PromQL wrong-accept).
//
// The error message must contain "unsupported: range function" so the
// showcase-promql parity-rejection contract substring still matches.
func TestLower_HoltWinters_RejectedByPromQLHead(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	// `double_exponential_smoothing` is the only spelling the upstream
	// parser recognises (the legacy `holt_winters` name was renamed
	// upstream and is rejected at parse time — itself parity-correct).
	// The lowering case still keys on both names so the IR alias is
	// covered if upstream ever re-adds the old spelling.
	const q = `double_exponential_smoothing(http_requests_total[10m], 0.5, 0.1)`
	expr, err := p.ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	_, err = promql.Lower(context.Background(), expr, s)
	if err == nil {
		t.Fatalf("expected %q to be rejected by the PromQL head, got nil error", q)
	}
	if want := "unsupported: range function"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain showcase contract substring %q", err.Error(), want)
	}
}

// TestEmit_HoltWintersIR_StillRenders guards the gate's blast radius on
// the emitter side: the PromQL lowering dispatch no longer produces a
// holt_winters RangeWindow, but the chsql emitter's holt_winters branch
// is shared IR and must keep rendering the double-exponential recurrence
// (arrayFold) when handed the IR directly. Constructing the RangeWindow
// here keeps that emitter branch covered without routing through the
// now-gated PromQL dispatch.
func TestEmit_HoltWintersIR_StillRenders(t *testing.T) {
	t.Parallel()

	plan := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "holt_winters",
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		Scalars:         []float64{0.5, 0.1},
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "arrayFold") {
		t.Fatalf("emitted SQL does not contain arrayFold:\n%s", sql)
	}
}
