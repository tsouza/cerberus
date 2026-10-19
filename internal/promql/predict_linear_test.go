package promql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_PredictLinear_OK covers the happy-path lowering: an
// instant `predict_linear(metric[range], t)` produces a RangeWindow
// with Func="predict_linear" and Scalars=[t].
func TestLower_PredictLinear_OK(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	expr, err := p.ParseExpr(`predict_linear(http_requests_total[5m], 3600)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	rw, ok := plan.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("expected *chplan.RangeWindow, got %T", plan)
	}
	if rw.Func != "predict_linear" {
		t.Fatalf("Func = %q, want predict_linear", rw.Func)
	}
	if len(rw.Scalars) != 1 || rw.Scalars[0] != 3600 {
		t.Fatalf("Scalars = %v, want [3600]", rw.Scalars)
	}

	// Sanity check that the SQL emitter produces a non-empty,
	// `simpleLinearRegression`-flavoured query.
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "simpleLinearRegression") {
		t.Fatalf("emitted SQL does not contain simpleLinearRegression:\n%s", sql)
	}
}

// TestLower_PredictLinear_Errors covers the rejected shapes so the
// error messages stay observable as the surface grows.
func TestLower_PredictLinear_Errors(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{
			name:    "missing second arg",
			query:   `predict_linear(http_requests_total[5m])`,
			wantErr: "expects 2 arguments",
		},
		{
			name:    "first arg must be range-vector",
			query:   `predict_linear(up, 60)`,
			wantErr: "must be a range-vector selector",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				// Some of these may fail at parse time (predict_linear
				// is a known function so the parser checks arity); treat
				// the parse error as the surfaced error.
				if !strings.Contains(err.Error(), tc.wantErr) &&
					!strings.Contains(err.Error(), "expected") {
					t.Fatalf("ParseExpr: %v", err)
				}
				return
			}
			_, err = promql.Lower(context.Background(), expr, s)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestLower_PredictLinear_ComputedHorizon pins the computed-horizon
// acceptance: `predict_linear(m[5m], scalar(x))` lowers into a
// RangeWindow whose ScalarExprs slot carries the computed t (a
// chplan.ScalarSubquery) instead of the literal Scalars slot —
// reference Prometheus evaluates the horizon per query, so the old
// scalar-literal-only rejection was a wrong rejection.
func TestLower_PredictLinear_ComputedHorizon(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	expr, err := p.ParseExpr(`predict_linear(http_requests_total[5m], scalar(other))`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	rw, ok := plan.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.RangeWindow", plan)
	}
	if len(rw.ScalarExprs) != 1 || len(rw.Scalars) != 0 {
		t.Fatalf("ScalarExprs=%d Scalars=%d, want 1 / 0", len(rw.ScalarExprs), len(rw.Scalars))
	}
	// The horizon is `assumeNotNull(<scalar subquery>)`: ClickHouse types
	// every scalar-subquery position Nullable, and the wrapper strips
	// that inference (see promql.scalarSubqueryValue). Assert both
	// layers, so dropping the wrapper is caught here rather than as a
	// TYPE_MISMATCH from a downstream consumer.
	wrap, ok := rw.ScalarExprs[0].(*chplan.FuncCall)
	if !ok || wrap.Fn != chplan.FnAssumeNotNull || len(wrap.Args) != 1 {
		t.Fatalf("ScalarExprs[0] = %#v, want assumeNotNull(<1 arg>)", rw.ScalarExprs[0])
	}
	if _, ok := wrap.Args[0].(*chplan.ScalarSubquery); !ok {
		t.Fatalf("assumeNotNull arg = %T, want *chplan.ScalarSubquery", wrap.Args[0])
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "simpleLinearRegression") {
		t.Fatalf("emitted SQL missing simpleLinearRegression:\n%s", sql)
	}
}

func TestLower_PredictLinear_PinnedRangeUsesGridEvalAnchor(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	expr, err := p.ParseExpr(`predict_linear(http_requests_total[5m] @ 1767225600, 600)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	plan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
	if err != nil {
		t.Fatalf("LowerAtRange: %v", err)
	}

	project, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.Project", plan)
	}
	joined, ok := project.Input.(*chplan.CrossJoin)
	if !ok {
		t.Fatalf("project input = %T, want *chplan.CrossJoin", project.Input)
	}
	if _, ok := joined.Left.(*chplan.StepGrid); !ok {
		t.Fatalf("cross-join left = %T, want *chplan.StepGrid", joined.Left)
	}
	rw, ok := joined.Right.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("cross-join right = %T, want *chplan.RangeWindow", joined.Right)
	}
	if rw.PredictLinearSlopeColumn == "" {
		t.Fatal("pinned predict_linear window does not expose its fitted slope")
	}
	var value chplan.Expr
	for _, projection := range project.Projections {
		if projection.Alias == s.ValueColumn {
			value = projection.Expr
			break
		}
	}
	if value == nil || !exprReadsColumn(value, chplan.RangeWindowAnchorColumn) ||
		!exprReadsColumn(value, rw.PredictLinearSlopeColumn) {
		t.Fatalf("broadcast value does not combine %q with %q", chplan.RangeWindowAnchorColumn, rw.PredictLinearSlopeColumn)
	}
}

// predictLinearWindows returns every predict_linear RangeWindow on plan's
// node tree.
func predictLinearWindows(plan chplan.Node) []*chplan.RangeWindow {
	var out []*chplan.RangeWindow
	chplan.Walk(plan, func(n chplan.Node) bool {
		if rw, ok := n.(*chplan.RangeWindow); ok && rw.Func == "predict_linear" {
			out = append(out, rw)
		}
		return true
	})
	return out
}

// TestLower_PredictLinear_HorizonStartsAtEvaluationInstant pins the horizon
// each lowering hands the regression. The emitter fits the regression at the
// window's right edge, the anchor minus the offset; reference Prometheus
// forecasts from the evaluation instant (`linearRegression(samples.Floats,
// enh.Ts)`). The lowering advances the horizon by the distance between the
// two, at millisecond precision, so `@` and `offset` choose only the samples.
func TestLower_PredictLinear_HorizonStartsAtEvaluationInstant(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	// eval is 00:46:10; the pins are 00:45:30.5 and 00:45:30.
	eval := time.Date(2026, 1, 1, 0, 46, 10, 0, time.UTC)
	cases := []struct {
		name  string
		query string
		step  time.Duration
		want  float64
	}{
		{"no modifier", `predict_linear(m[10m], 60)`, 0, 60},
		{"fractional pin", `predict_linear(m[10m] @ 1767228330.5, 60)`, 0, 60 + 39.5},
		{"whole pin", `predict_linear(m[10m] @ 1767228330, 60)`, 0, 60 + 40},
		{"fractional pin with offset", `predict_linear(m[10m] @ 1767228330.5 offset 30s, 60)`, 0, 60 + 69.5},
		{"pin at the evaluation instant", `predict_linear(m[10m] @ end(), 60)`, 0, 60},
		{"offset", `predict_linear(m[10m] offset 30s, 60)`, 0, 60 + 30},
		{"negative offset", `predict_linear(m[10m] offset -30s, 60)`, 0, 60 - 30},
		{"sub-second offset", `predict_linear(m[10m] offset 700ms, 60)`, 0, 60 + 0.7},
		{"offset on the range grid", `predict_linear(m[10m] offset 30s, 60)`, time.Minute, 60 + 30},
		{"subquery offset", `predict_linear(m[10m:1m] offset 30s, 60)`, 0, 60 + 30},
		{"subquery offset on the range grid", `predict_linear(m[10m:1m] offset 30s, 60)`, time.Minute, 60 + 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			start := eval
			if tc.step > 0 {
				start = eval.Add(-2 * tc.step)
			}
			plan, err := promql.LowerAtRange(context.Background(), expr, s, start, eval, tc.step)
			if err != nil {
				t.Fatalf("LowerAtRange: %v", err)
			}
			windows := predictLinearWindows(plan)
			if len(windows) != 1 {
				t.Fatalf("plan holds %d predict_linear windows, want 1", len(windows))
			}
			if got := windows[0].Scalars; len(got) != 1 || got[0] != tc.want {
				t.Fatalf("horizon Scalars = %v (ScalarExprs %v), want [%v]", got, windows[0].ScalarExprs, tc.want)
			}
		})
	}
}

// TestLower_PredictLinear_ComputedHorizonAdvancesToEvaluationInstant pins the
// computed-horizon arm: a pinned window's distance to the evaluation instant
// is added to the scalar-subquery horizon rather than folded.
func TestLower_PredictLinear_ComputedHorizonAdvancesToEvaluationInstant(t *testing.T) {
	t.Parallel()

	expr, err := parser.NewParser(parser.Options{}).ParseExpr(`predict_linear(m[10m] @ 1767228330.5, scalar(other))`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	eval := time.Date(2026, 1, 1, 0, 46, 10, 0, time.UTC)
	plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), eval, eval, 0)
	if err != nil {
		t.Fatalf("LowerAtRange: %v", err)
	}
	windows := predictLinearWindows(plan)
	if len(windows) != 1 || len(windows[0].ScalarExprs) != 1 || len(windows[0].Scalars) != 0 {
		t.Fatalf("want one predict_linear window with a single computed horizon, got %d windows", len(windows))
	}
	sum, ok := windows[0].ScalarExprs[0].(*chplan.Binary)
	if !ok || sum.Op != chplan.OpAdd {
		t.Fatalf("horizon = %#v, want <scalar(other)> + <shift>", windows[0].ScalarExprs[0])
	}
	if shift, ok := sum.Right.(*chplan.LitFloat); !ok || shift.V != 39.5 {
		t.Fatalf("horizon shift = %#v, want 39.5", sum.Right)
	}
}

// TestLower_PredictLinear_PinWithoutEvaluationInstantReadsNow pins the
// context-free [promql.Lower] arm: with no request instant the evaluation
// instant is ClickHouse's now64(9), so the shift is computed in SQL from the
// pinned window's right edge.
func TestLower_PredictLinear_PinWithoutEvaluationInstantReadsNow(t *testing.T) {
	t.Parallel()

	expr, err := parser.NewParser(parser.Options{}).ParseExpr(`predict_linear(m[10m] @ 1767228330.5, 60)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.Lower(context.Background(), expr, schema.DefaultOTelMetrics())
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	windows := predictLinearWindows(plan)
	if len(windows) != 1 || len(windows[0].ScalarExprs) != 1 {
		t.Fatalf("want one predict_linear window with a computed horizon, got %d windows", len(windows))
	}
	readsNow := false
	chplan.InspectExpr(windows[0].ScalarExprs[0], func(e chplan.Expr) bool {
		if call, ok := e.(*chplan.FuncCall); ok && call.Fn == chplan.FnNow64 {
			readsNow = true
		}
		return true
	})
	if !readsNow {
		t.Fatalf("horizon %#v does not read the evaluation instant now64(9)", windows[0].ScalarExprs[0])
	}
}

// TestLower_PredictLinear_PinnedSubqueryIsEmpty pins cerberus issue #3878:
// Prometheus wraps an `@`-pinned subquery argument of predict_linear in a
// StepInvariantExpr, finds no range vector among the call's arguments and
// answers no sample, so the lowering reads no regression at all.
func TestLower_PredictLinear_PinnedSubqueryIsEmpty(t *testing.T) {
	t.Parallel()

	eval := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	for _, query := range []string{
		`predict_linear(vector(time())[5m:1m] @ 1767225600, 60)`,
		`predict_linear((m[10m:1m] @ 1767225600), 60)`,
		`predict_linear(vector(time())[5m:1m] @ end(), 60)`,
	} {
		for _, step := range []time.Duration{0, time.Minute} {
			expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			start := eval
			if step > 0 {
				start = eval.Add(-5 * step)
			}
			plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, eval, step)
			if err != nil {
				t.Fatalf("LowerAtRange(%q, step %v): %v", query, step, err)
			}
			if windows := predictLinearWindows(plan); len(windows) != 0 {
				t.Fatalf("%q (step %v) lowers %d predict_linear windows; Prometheus answers empty", query, step, len(windows))
			}
		}
	}
}
