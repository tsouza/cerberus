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

// TestLower_HoltWinters_Supported pins that the PromQL head lowers
// `double_exponential_smoothing` cleanly. The function is now implemented
// (the maintainer flipped it from gated to supported): the chsql emitter
// renders the Holt-Winters double-exponential recurrence as an arrayFold
// over the windowed value array, verified reference-exact against
// prometheus/promql/functions.go::funcDoubleExponentialSmoothing.
//
// `double_exponential_smoothing` is the only spelling the upstream parser
// recognises (the legacy `holt_winters` name was removed upstream and is
// rejected at parse time — itself parity-correct). The lowering case still
// keys on both names so the IR alias is covered if upstream ever re-adds
// the old spelling.
func TestLower_HoltWinters_Supported(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	const q = `double_exponential_smoothing(http_requests_total[10m], 0.5, 0.1)`
	expr, err := p.ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	plan, err := promql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("double_exponential_smoothing should lower cleanly, got: %v", err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "arrayFold") {
		t.Fatalf("emitted SQL does not contain arrayFold:\n%s", sql)
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
