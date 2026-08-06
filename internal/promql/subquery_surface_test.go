package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// surfaceEvalStart / surfaceEvalEnd / surfaceEvalStep pin a deterministic
// query window for the range-mode lowerings below; the assertions are on
// plan SHAPE, so the exact instants only need to be stable.
var (
	surfaceEvalStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	surfaceEvalEnd   = surfaceEvalStart.Add(10 * time.Minute)
)

const surfaceEvalStep = time.Minute

// findNode returns the first node of type T in a pre-order walk of the
// plan, and whether one was found.
func findNode[T chplan.Node](n chplan.Node) (T, bool) {
	var zero T
	if n == nil {
		return zero, false
	}
	if hit, ok := n.(T); ok {
		return hit, true
	}
	for _, kid := range n.Children() {
		if hit, ok := findNode[T](kid); ok {
			return hit, true
		}
	}
	return zero, false
}

func lowerSurfaceQuery(t *testing.T, query string, rangeMode bool) chplan.Node {
	t.Helper()
	expr, err := experimentalParser().ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	s := schema.DefaultOTelMetrics()
	if rangeMode {
		plan, err := promql.LowerAtRange(context.Background(), expr, s,
			surfaceEvalStart, surfaceEvalEnd, surfaceEvalStep)
		if err != nil {
			t.Fatalf("LowerAtRange(%q): %v", query, err)
		}
		return plan
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, surfaceEvalStart, surfaceEvalEnd)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	return plan
}

// TestLowerSubqueryArg_RangeVectorFns pins the range-vector functions that
// accept a subquery argument. The four `ts_of_*_over_time` reducers ride
// the same RangeWindow emitter case as the `*_over_time` family, so the
// assertion is that each lowers to a RangeWindow carrying its own name
// over the subquery's matrix (issue #1837).
func TestLowerSubqueryArg_RangeVectorFns(t *testing.T) {
	t.Parallel()

	for _, fn := range []string{
		"ts_of_first_over_time",
		"ts_of_last_over_time",
		"ts_of_max_over_time",
		"ts_of_min_over_time",
	} {
		t.Run(fn, func(t *testing.T) {
			t.Parallel()
			query := fn + `(rate(http_requests_total[1m])[5m:1m])`
			for _, rangeMode := range []bool{false, true} {
				plan := lowerSurfaceQuery(t, query, rangeMode)
				rw, ok := findNode[*chplan.RangeWindow](plan)
				if !ok {
					t.Fatalf("rangeMode=%v: no RangeWindow in the lowered plan for %q", rangeMode, query)
				}
				if rw.Func != fn {
					t.Fatalf("rangeMode=%v: outer RangeWindow.Func = %q, want %q", rangeMode, rw.Func, fn)
				}
				// The outer reducer must sit ON the subquery's matrix,
				// i.e. read the per-anchor column the inner emits.
				if rw.TimestampColumn != chplan.RangeWindowAnchorColumn {
					t.Fatalf("rangeMode=%v: outer RangeWindow reads %q, want %q",
						rangeMode, rw.TimestampColumn, chplan.RangeWindowAnchorColumn)
				}
			}
		})
	}
}

// TestLowerSubqueryArg_AbsentOverTime pins `absent_over_time(<subquery>)`
// onto the AbsentOverTime plan node reading the subquery's matrix rather
// than a scan, with the subquery's own range as the absence window and no
// synthesised labels (upstream's createLabelsForAbsentFunction returns
// EmptyLabels for a SubqueryExpr argument) — issue #1837.
func TestLowerSubqueryArg_AbsentOverTime(t *testing.T) {
	t.Parallel()

	const query = `absent_over_time(rate(http_requests_total[1m])[5m:1m])`

	t.Run("instant", func(t *testing.T) {
		t.Parallel()
		plan := lowerSurfaceQuery(t, query, false)
		a, ok := findNode[*chplan.AbsentOverTime](plan)
		if !ok {
			t.Fatalf("no AbsentOverTime node in the lowered plan for %q", query)
		}
		if a.Range != 5*time.Minute {
			t.Fatalf("AbsentOverTime.Range = %v, want 5m (the subquery range)", a.Range)
		}
		if len(a.SynthLabels) != 0 {
			t.Fatalf("AbsentOverTime.SynthLabels = %v, want none for a subquery argument", a.SynthLabels)
		}
		if a.Step != 0 {
			t.Fatalf("instant AbsentOverTime.Step = %v, want 0", a.Step)
		}
		if _, ok := findNode[*chplan.RangeWindow](a.Input); !ok {
			t.Fatal("AbsentOverTime.Input does not contain the subquery's matrix RangeWindow")
		}
	})

	t.Run("range", func(t *testing.T) {
		t.Parallel()
		plan := lowerSurfaceQuery(t, query, true)
		a, ok := findNode[*chplan.AbsentOverTime](plan)
		if !ok {
			t.Fatalf("no AbsentOverTime node in the lowered plan for %q", query)
		}
		if a.Step != surfaceEvalStep {
			t.Fatalf("range AbsentOverTime.Step = %v, want %v", a.Step, surfaceEvalStep)
		}
		if !a.Start.Equal(surfaceEvalStart) || !a.End.Equal(surfaceEvalEnd) {
			t.Fatalf("range AbsentOverTime grid = [%v, %v], want [%v, %v]",
				a.Start, a.End, surfaceEvalStart, surfaceEvalEnd)
		}
		// The inner matrix must reach back one full subquery range before
		// the query start, or the leading anchors have nothing to read.
		rw, ok := findNode[*chplan.RangeWindow](a.Input)
		if !ok {
			t.Fatal("AbsentOverTime.Input does not contain the subquery's matrix RangeWindow")
		}
		wantInnerStart := surfaceEvalStart.Add(-5 * time.Minute)
		if rw.Start.After(wantInnerStart) {
			t.Fatalf("inner matrix grid starts at %v, want no later than %v", rw.Start, wantInnerStart)
		}
	})
}

// TestLowerSubqueryInner_InstantTransforms pins the parts of the
// instantTransformFns membership rule that are not per-name: composition
// (an admitted call over another admitted call) and the exclusions the
// rule deliberately keeps out (issue #1838). The per-name half — every
// admitted name lowers under a subquery, and no name is admitted without
// a probe — is exhaustive in
// TestInstantTransformFnsAreAllLowerable, which reads the table directly.
func TestLowerSubqueryInner_InstantTransforms(t *testing.T) {
	t.Parallel()

	accepted := []string{
		// Nesting: an admitted call over another admitted call.
		`max_over_time(abs(sort(http_requests_total))[5m:1m])`,
		`max_over_time(deg(atan(http_requests_total))[5m:1m])`,
		`max_over_time(sort_by_label(clamp_min(http_requests_total, 0), "job")[5m:1m])`,
	}
	for _, query := range accepted {
		t.Run("accept/"+query, func(t *testing.T) {
			t.Parallel()
			for _, rangeMode := range []bool{false, true} {
				plan := lowerSurfaceQuery(t, query, rangeMode)
				if plan == nil {
					t.Fatalf("rangeMode=%v: nil plan for %q", rangeMode, query)
				}
			}
		})
	}

	// The exclusions. Each names a family from the membership rule:
	// anchor-synthesising (zero-arg date form, vector, time) and
	// cross-sample (histogram_quantile's `le` fan-in, info's join).
	rejected := []string{
		`max_over_time(hour()[5m:1m])`,
		`max_over_time(vector(1)[5m:1m])`,
		`max_over_time(histogram_quantile(0.9, http_request_duration_seconds_bucket)[5m:1m])`,
		`max_over_time(info(http_requests_total)[5m:1m])`,
	}
	for _, query := range rejected {
		t.Run("reject/"+query, func(t *testing.T) {
			t.Parallel()
			expr, err := experimentalParser().ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			_, err = promql.LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(),
				surfaceEvalStart, surfaceEvalEnd)
			if err == nil {
				t.Fatalf("Lower(%q): expected an error, got nil", query)
			}
		})
	}
}

// TestLowerComputedK pins the general "scalar-valued PromQL expression as
// K" lowering for topk / bottomk / limitk, in both instant and
// subquery-inner position (issue #1840). Each shape must reach the
// computed-K slot (TopK.KExpr) rather than being rejected.
func TestLowerComputedK(t *testing.T) {
	t.Parallel()

	queries := []string{
		`topk(scalar(up), http_requests_total)`,
		`topk(scalar(up) * 2, http_requests_total)`,
		`topk(2 + scalar(up), http_requests_total)`,
		`topk((scalar(up) + 1) / 2, http_requests_total)`,
		`bottomk(scalar(up) - 1, http_requests_total)`,
		`topk(time() % 5, http_requests_total)`,
		`limitk(scalar(up), http_requests_total)`,
		`limitk(scalar(up) + 1, http_requests_total)`,
		`max_over_time(topk(scalar(up), rate(http_requests_total[1m]))[5m:1m])`,
		`max_over_time(bottomk(scalar(up) * 2, rate(http_requests_total[1m]))[5m:1m])`,
		`max_over_time(limitk(scalar(up), rate(http_requests_total[1m]))[5m:1m])`,
	}
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			for _, rangeMode := range []bool{false, true} {
				plan := lowerSurfaceQuery(t, query, rangeMode)
				topk, ok := findNode[*chplan.TopK](plan)
				if !ok {
					t.Fatalf("rangeMode=%v: no TopK node in the lowered plan for %q", rangeMode, query)
				}
				if topk.KExpr == nil {
					t.Fatalf("rangeMode=%v: TopK.KExpr is nil for %q — K did not bind as computed", rangeMode, query)
				}
				if topk.K != 0 {
					t.Fatalf("rangeMode=%v: TopK carries both KExpr and literal K=%d for %q", rangeMode, topk.K, query)
				}
			}
		})
	}
}
