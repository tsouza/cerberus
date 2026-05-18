package promql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerSubquery_DirectNested exercises
// `lowerSubqueryOverSubquery`, the defensive branch for a
// programmatically-constructed AST where `SubqueryExpr.Expr` is itself
// a `*parser.SubqueryExpr`. PromQL's parser type system forbids this
// shape — a SubqueryExpr produces a range vector, and a SubqueryExpr's
// body must be an instant vector — so this branch is unreachable
// through parsed PromQL. The test constructs the AST directly so the
// lowering path stays exercised in case an optimizer rewrite or future
// parser change produces this shape.
func TestLowerSubquery_DirectNested(t *testing.T) {
	t.Parallel()

	// Build `(rate(m[1m])[5m:30s])[1h:5m]` directly, skipping the
	// parser's range-vector-on-subquery check.
	innerMatrix := &parser.MatrixSelector{
		VectorSelector: &parser.VectorSelector{
			Name:          "m",
			LabelMatchers: []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "__name__", "m")},
		},
		Range: time.Minute,
	}
	rate := &parser.Call{
		Func: parser.MustGetFunction("rate"),
		Args: parser.Expressions{innerMatrix},
	}
	innerSub := &parser.SubqueryExpr{
		Expr:  rate,
		Range: 5 * time.Minute,
		Step:  30 * time.Second,
	}
	outerSub := &parser.SubqueryExpr{
		Expr:  innerSub,
		Range: time.Hour,
		Step:  5 * time.Minute,
	}

	plan, err := promql.Lower(context.Background(), outerSub, schema.DefaultOTelMetrics())
	if err != nil {
		t.Fatalf("Lower(direct-nested SubqueryExpr): %v", err)
	}

	// Outer is an Identity-mode RangeWindow with Step=5m and
	// OuterRange=1h. Inner widens to outer.Range + inner.Range = 1h5m
	// so every outer anchor's lookback finds inner anchors.
	outer, ok := plan.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("outer plan = %T; want *chplan.RangeWindow", plan)
	}
	if !outer.Identity {
		t.Errorf("outer.Identity = false; want true (no reducer for bare nested subquery)")
	}
	if outer.OuterRange != time.Hour {
		t.Errorf("outer.OuterRange = %v; want 1h", outer.OuterRange)
	}
	if outer.Step != 5*time.Minute {
		t.Errorf("outer.Step = %v; want 5m", outer.Step)
	}
	if outer.TimestampColumn != "anchor_ts" {
		t.Errorf("outer.TimestampColumn = %q; want anchor_ts (consumes inner matrix grid)", outer.TimestampColumn)
	}

	innerRW, ok := outer.Input.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("inner plan = %T; want *chplan.RangeWindow", outer.Input)
	}
	if innerRW.Func != "rate" {
		t.Errorf("inner.Func = %q; want rate", innerRW.Func)
	}
	if innerRW.OuterRange != 65*time.Minute {
		t.Errorf("inner.OuterRange = %v; want 65m (sub.Range + innerSub.Range widening)", innerRW.OuterRange)
	}
	if innerRW.Step != 30*time.Second {
		t.Errorf("inner.Step = %v; want 30s", innerRW.Step)
	}
}

// TestLowerSubquery_DirectNested_ZeroRange pins the zero-range
// rejection in `lowerSubqueryOverSubquery` — the inner subquery's
// range must be positive, mirroring `lowerSubqueryOverCallSubquery`.
func TestLowerSubquery_DirectNested_ZeroRange(t *testing.T) {
	t.Parallel()

	vs := &parser.VectorSelector{
		Name:          "m",
		LabelMatchers: []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "__name__", "m")},
	}
	innerSub := &parser.SubqueryExpr{Expr: vs, Range: 0, Step: 30 * time.Second}
	outerSub := &parser.SubqueryExpr{Expr: innerSub, Range: time.Hour, Step: 5 * time.Minute}

	_, err := promql.Lower(context.Background(), outerSub, schema.DefaultOTelMetrics())
	if err == nil {
		t.Fatal("Lower(direct-nested zero-range): want error, got nil")
	}
	if !strings.Contains(err.Error(), "inner subquery range must be positive") {
		t.Errorf("Lower error = %q; want 'inner subquery range must be positive'", err)
	}
}
