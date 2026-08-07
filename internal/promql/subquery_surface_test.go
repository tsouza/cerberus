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
		// `info` rewrites Attributes per sample and leaves TimeUnix /
		// Value untouched, so it rides the Identity wrap exactly as the
		// other label rewrites do.
		`max_over_time(info(http_requests_total)[5m:1m])`,
	}
	for _, query := range accepted {
		t.Run("accept/"+query, func(t *testing.T) {
			t.Parallel()
			for _, rangeMode := range []bool{false, true} {
				plan := lowerSurfaceQuery(t, query, rangeMode)
				if plan == nil {
					t.Fatalf("rangeMode=%v: nil plan for %q", rangeMode, query)
				}
				// The other half of the membership rule: an admitted call
				// rides the Identity wrap. Asserting it here is what keeps
				// the per-anchor cases below from passing vacuously — the
				// same probe must come back true here and false there.
				if !hasIdentityRangeWindow(plan) {
					t.Fatalf("rangeMode=%v: %q did not lower through the Identity RangeWindow",
						rangeMode, query)
				}
			}
		})
	}

	// The exclusions. Each names a family from the membership rule:
	// anchor-synthesising (zero-arg date form, vector, time) and
	// cross-sample (histogram_quantile's `le` fan-in). Being excluded from
	// the table is a statement about the LOWERING, not about
	// acceptance — reference Prometheus answers all three by re-evaluating
	// the inner call at every subquery anchor, and so does cerberus
	// (issue #1866). What the exclusion buys is that they must NOT ride
	// the Identity wrap, whose "carry the latest stored sample forward"
	// contract none of them satisfies; they take the per-anchor grid path
	// instead, which emits one freshly-evaluated row per anchor.
	perAnchor := []string{
		`max_over_time(hour()[5m:1m])`,
		`max_over_time(vector(1)[5m:1m])`,
		`max_over_time(histogram_quantile(0.9, http_request_duration_seconds_bucket)[5m:1m])`,
	}
	for _, query := range perAnchor {
		t.Run("per-anchor/"+query, func(t *testing.T) {
			t.Parallel()
			for _, rangeMode := range []bool{false, true} {
				plan := lowerSurfaceQuery(t, query, rangeMode)
				if plan == nil {
					t.Fatalf("rangeMode=%v: nil plan for %q", rangeMode, query)
				}
				if hasIdentityRangeWindow(plan) {
					t.Fatalf("rangeMode=%v: %q lowered through an Identity RangeWindow; "+
						"the instant-transform wrap cannot model this inner", rangeMode, query)
				}
			}
		})
	}
}

// hasIdentityRangeWindow reports whether any node in the plan is the
// Identity ("carry the latest stored sample in the window forward")
// RangeWindow that [instantTransformFns] members lower through.
func hasIdentityRangeWindow(n chplan.Node) bool {
	if n == nil {
		return false
	}
	if rw, ok := n.(*chplan.RangeWindow); ok && rw.Identity {
		return true
	}
	for _, kid := range n.Children() {
		if hasIdentityRangeWindow(kid) {
			return true
		}
	}
	return false
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

// TestLowerSubqueryInner_PerAnchorInstantCalls pins the subquery-inner
// calls that are neither sample-preserving nor RangeWindow reducers over
// a range vector (issue #1866). Each used to be rejected by
// `subqueryInnerRangeFnShape`'s reducer contract — on arity, because the
// shape table reports 1 for every name it does not list, or on "must
// wrap a MatrixSelector", because the argument in the matrix position is
// a literal or an aggregation. Reference Prometheus evaluates all of
// them, one instant query per subquery anchor, so each must now lower to
// the matrix shape an enclosing reducer reads: the outer RangeWindow
// sits on the subquery's per-anchor column rather than on a raw scan's
// timestamp.
func TestLowerSubqueryInner_PerAnchorInstantCalls(t *testing.T) {
	t.Parallel()

	queries := []string{
		// Arity: the shape table claimed 1 argument for both.
		`max_over_time(histogram_quantile(0.9, http_request_duration_seconds_bucket)[5m:1m])`,
		`max_over_time(day_of_month()[5m:1m])`,
		`max_over_time(days_in_month()[5m:1m])`,
		// Matrix position: a literal and an aggregation, neither a
		// MatrixSelector.
		`max_over_time(vector(1)[5m:1m])`,
		`max_over_time(sort(http_requests_total or vector(0))[5m:1m])`,
		`sum_over_time(sort(sum(http_requests_total))[5m:1m])`,
		// An admitted transform whose ARGUMENT subtree is not
		// sample-preserving: subqueryInstantSafe rejects the Identity
		// wrap, and the per-anchor path is the fallback that keeps the
		// query answerable.
		`max_over_time(abs(rate(http_requests_total[1m]))[5m:1m])`,
	}
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			for _, rangeMode := range []bool{false, true} {
				plan := lowerSurfaceQuery(t, query, rangeMode)
				rw, ok := findNode[*chplan.RangeWindow](plan)
				if !ok {
					t.Fatalf("rangeMode=%v: no RangeWindow in the lowered plan for %q", rangeMode, query)
				}
				if rw.TimestampColumn != chplan.RangeWindowAnchorColumn {
					t.Fatalf("rangeMode=%v: outer reducer reads %q, want %q — "+
						"the inner did not lower to the subquery matrix shape",
						rangeMode, rw.TimestampColumn, chplan.RangeWindowAnchorColumn)
				}
			}
		})
	}
}

// TestLowerSubqueryInner_AbsentOverTime pins `absent_over_time` in
// subquery-inner position (issue #1867). It is the one range-vector
// function outside `rangeVectorFn` — it lowers to its own
// chplan.AbsentOverTime node rather than to a RangeWindow — so reading
// it through the reducer table produced a RangeWindow the emitter has no
// case for (the plain-matrix argument) or an outright "does not accept a
// subquery argument" rejection (the nested-subquery argument). Both
// argument shapes must reach the AbsentOverTime node, carrying the
// absence window from the INNER range and the anchor grid from the OUTER
// subquery's step.
func TestLowerSubqueryInner_AbsentOverTime(t *testing.T) {
	t.Parallel()

	// Both spellings of the argument: a plain matrix selector and a
	// nested subquery. `Range` is the inner absence window in each.
	queries := []string{
		`max_over_time(absent_over_time(http_requests_total[5m])[10m:1m])`,
		`max_over_time(absent_over_time(http_requests_total[5m:1m])[10m:1m])`,
	}
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			for _, rangeMode := range []bool{false, true} {
				plan := lowerSurfaceQuery(t, query, rangeMode)
				a, ok := findNode[*chplan.AbsentOverTime](plan)
				if !ok {
					t.Fatalf("rangeMode=%v: no AbsentOverTime node in the lowered plan for %q",
						rangeMode, query)
				}
				if a.Range != 5*time.Minute {
					t.Fatalf("rangeMode=%v: AbsentOverTime.Range = %v, want 5m (the inner absence window)",
						rangeMode, a.Range)
				}
				// The verdict is recomputed at every anchor of the OUTER
				// subquery's grid, so the node fans out at its step.
				if a.Step != time.Minute {
					t.Fatalf("rangeMode=%v: AbsentOverTime.Step = %v, want 1m (the outer subquery step)",
						rangeMode, a.Step)
				}
				rw, ok := findNode[*chplan.RangeWindow](plan)
				if !ok {
					t.Fatalf("rangeMode=%v: no outer RangeWindow in the lowered plan for %q",
						rangeMode, query)
				}
				if rw.Func != "max_over_time" {
					t.Fatalf("rangeMode=%v: outer RangeWindow.Func = %q, want max_over_time",
						rangeMode, rw.Func)
				}
				if rw.TimestampColumn != chplan.RangeWindowAnchorColumn {
					t.Fatalf("rangeMode=%v: outer reducer reads %q, want %q",
						rangeMode, rw.TimestampColumn, chplan.RangeWindowAnchorColumn)
				}
			}
		})
	}
}
