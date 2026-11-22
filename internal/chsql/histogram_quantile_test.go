package chsql

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

func TestHistogramQuantileValueFrag_DividesRankBeforeWidth(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	histogramQuantileValueFrag(&chplan.HistogramQuantile{
		Phi:                  0.5,
		BucketCountsColumn:   "BucketCounts",
		ExplicitBoundsColumn: "ExplicitBounds",
	}, hqClassicHelperColumns{})(b)

	if !strings.Contains(b.String(), " * (((0.5 *") {
		t.Fatalf("interpolation must divide the rank offset before multiplying by the bucket width:\n%s", b.String())
	}
}

// TestHistogramQuantileValueFrag_PhiExprGating kills the two
// CONDITIONALS_NEGATION mutants that gate the computed-phi path: the
// `h.PhiExpr != nil` inside the arrayFirstIndex predicate built at
// histogram_quantile.go:`w.idx = func() Frag`, and
// histogram_quantile.go:`h.PhiExpr == nil`, the early return before the
// isNaN wrapper.
//
// The first citation names the assignment that opens the closure rather
// than the guard itself: `h.PhiExpr != nil` is spelled identically at two
// code lines in this file, so no substring of the guard singles one out,
// while the closure it lives in is unique.
//
// The literal-Phi path (h.PhiExpr == nil) must render the BARE `c >=
// <target>` comparison as the arrayFirstIndex predicate and must NOT
// wrap the whole expression in an `isNaN(phi) → nan` guard (h.Phi is a
// query-shape constant, never NaN). The computed-PhiExpr path is the
// mirror: CH 24.8 rejects a scalar subquery inside arrayFirstIndex's
// filter unless the comparison is re-folded through `if(...)`, so the
// predicate must render as `(if(c >= <target>, 1, 0) = 1)`, and the
// runtime phi CAN be NaN, so the isNaN guard must wrap the whole
// expression.
//
// Negating the predicate guard alone would apply the wrapped
// `if(...)  = 1)` predicate to the literal-Phi path (and the bare
// predicate to the PhiExpr path); negating the early return alone would
// apply (or skip) the isNaN wrapper on the wrong path. Asserting both
// markers on both cases pins each independently.
func TestHistogramQuantileValueFrag_PhiExprGating(t *testing.T) {
	t.Parallel()

	t.Run("literal phi: bare predicate, no isNaN guard", func(t *testing.T) {
		t.Parallel()
		b := NewBuilder()
		histogramQuantileValueFrag(&chplan.HistogramQuantile{
			Phi:                  0.5,
			BucketCountsColumn:   "BucketCounts",
			ExplicitBoundsColumn: "ExplicitBounds",
		}, hqClassicHelperColumns{})(b)
		sql := b.String()
		if !strings.Contains(sql, "arrayFirstIndex(c -> c >=") {
			t.Errorf("literal phi must use the bare `c >= target` predicate (histogram_quantile.go:`w.idx = func() Frag`'s guard flipped?):\n%s", sql)
		}
		if strings.Contains(sql, "arrayFirstIndex(c -> (if(") {
			t.Errorf("literal phi must NOT wrap the predicate in if(...)=1 (histogram_quantile.go:`w.idx = func() Frag`'s guard flipped?):\n%s", sql)
		}
		if strings.Contains(sql, "isNaN(") {
			t.Errorf("literal phi must NOT carry the isNaN guard (histogram_quantile.go:`h.PhiExpr == nil` flipped?):\n%s", sql)
		}
	})

	t.Run("computed phi: wrapped predicate and isNaN guard", func(t *testing.T) {
		t.Parallel()
		b := NewBuilder()
		histogramQuantileValueFrag(&chplan.HistogramQuantile{
			PhiExpr:              &chplan.ColumnRef{Name: "PhiCol"},
			BucketCountsColumn:   "BucketCounts",
			ExplicitBoundsColumn: "ExplicitBounds",
		}, hqClassicHelperColumns{})(b)
		sql := b.String()
		if !strings.Contains(sql, "arrayFirstIndex(c -> (if(c >=") {
			t.Errorf("computed phi must wrap the predicate as (if(c >= ..., 1, 0) = 1) (histogram_quantile.go:`w.idx = func() Frag`'s guard flipped?):\n%s", sql)
		}
		if !strings.Contains(sql, "= 1),") {
			t.Errorf("computed phi predicate must close with `= 1)` "+
				"(histogram_quantile.go:`w.idx = func() Frag` flipped?):\n%s", sql)
		}
		if !strings.HasPrefix(sql, "if(isNaN(") {
			t.Errorf("computed phi must lead with the isNaN(phi) guard "+
				"(histogram_quantile.go:`h.PhiExpr == nil` flipped?):\n%s", sql)
		}
	})
}

func TestEmitHistogramQuantile_LegacyColumnsDoNotDriveInput(t *testing.T) {
	t.Parallel()
	base := func() *chplan.HistogramQuantile {
		return &chplan.HistogramQuantile{
			Input:                classicQuantileInput(),
			Phi:                  0.5,
			BucketCountsColumn:   "BucketCounts",
			ExplicitBoundsColumn: "ExplicitBounds",
		}
	}
	cases := []struct {
		name   string
		mutate func(*chplan.HistogramQuantile)
	}{
		{"BucketCountsColumn empty", func(h *chplan.HistogramQuantile) { h.BucketCountsColumn = "" }},
		{"ExplicitBoundsColumn empty", func(h *chplan.HistogramQuantile) { h.ExplicitBoundsColumn = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := base()
			c.mutate(h)
			_, _, err := Emit(context.Background(), h)
			if err != nil {
				t.Errorf("Emit: %v", err)
			}
		})
	}
}

// TestEmitHistogramQuantile_GroupByAliasFallback kills both the
// CONDITIONALS_BOUNDARY (`<` -> `<=`) and CONDITIONALS_NEGATION (`<` ->
// `>=`) mutants at histogram_quantile.go:`i < len(h.GroupByAliases)`.
// Placing the alias slice ONE ENTRY SHORT of GroupBy hits the exact boundary index (i == len(h.GroupByAliases)):
// under either mutant that index tries h.GroupByAliases[i], which is out
// of range and panics; under the original code it falls back to an
// unaliased projection.
func TestEmitHistogramQuantile_GroupByAliasFallback(t *testing.T) {
	t.Parallel()
	h := &chplan.HistogramQuantile{
		Input:                classicQuantileInput(),
		Phi:                  0.5,
		BucketCountsColumn:   "BucketCounts",
		ExplicitBoundsColumn: "ExplicitBounds",
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: "ServiceName"},
			&chplan.ColumnRef{Name: "SpanName"},
		},
		// One alias only — the second group key must render unaliased
		// without panicking.
		GroupByAliases: []string{"service"},
	}
	sql, _, err := Emit(context.Background(), h)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "`ServiceName` AS `service`") {
		t.Errorf("expected `ServiceName AS service`, SQL=%s", sql)
	}
	if !strings.Contains(sql, "`SpanName`") {
		t.Errorf("expected bare `SpanName` group key, SQL=%s", sql)
	}
}

// TestHistogramQuantileValueFrag_EmptyLadderGuardFollowsTheLadder pins the
// empty-ladder guard of both singular classic kernels by the ladder they
// read. Over a cumulative ladder the guard is a factor of the value, never
// the condition of a branch around it: ClickHouse filters every array column
// a row-dependent `if` branch reads down to that branch's rows, which raised
// the per-series range quantile of a rate several-fold (#4518). Over
// per-bucket counts it is the branch chain
// `if(length(BucketCounts) = 0, nan, if(observations = 0, nan, …))`, whose
// statement the factor does not improve on (#4529).
func TestHistogramQuantileValueFrag_EmptyLadderGuardFollowsTheLadder(t *testing.T) {
	t.Parallel()

	const (
		factor   = " * if(`_cerb_hqc_observations` = 0, nan, 1)"
		branches = "if(length(`BucketCounts`) = 0, nan, if(`_cerb_hqc_observations` = 0, nan, if("
	)
	helpers := hqClassicHelperColumns{observations: hqClassicObservationsColumn}
	for _, cumulative := range []bool{false, true} {
		h := &chplan.HistogramQuantile{
			Phi:                    0.5,
			BucketCountsColumn:     "BucketCounts",
			ExplicitBoundsColumn:   "ExplicitBounds",
			BucketCountsCumulative: cumulative,
		}
		for name, frag := range map[string]Frag{
			"interpolating": histogramQuantileValueFrag(h, helpers),
			"native":        histogramQuantileRankWalkNativeValueFrag(h, helpers),
		} {
			sql := renderFragToSQL(frag)
			if cumulative {
				if !strings.HasSuffix(sql, factor) {
					t.Errorf("%s kernel over a cumulative ladder must end in the empty-ladder factor %q:\n%s", name, factor, sql)
				}
				if strings.Contains(sql, "if(`_cerb_hqc_observations` = 0, nan, if") ||
					strings.Contains(sql, "length(`BucketCounts`) = 0") {
					t.Errorf("%s kernel over a cumulative ladder branches on an empty ladder:\n%s", name, sql)
				}
				continue
			}
			if !strings.HasPrefix(sql, branches) {
				t.Errorf("%s kernel over per-bucket counts must open with the empty-ladder branches %q:\n%s", name, branches, sql)
			}
			if strings.Contains(sql, factor) {
				t.Errorf("%s kernel over per-bucket counts carries the empty-ladder factor:\n%s", name, sql)
			}
		}
	}
}

// TestEmitHistogramQuantile_AnswerIsTheKernelSelect pins that both singular
// classic kernels, over either ladder, answer straight from the SELECT that
// computes `Value`: no further stage, such as a singleton ARRAY JOIN over the
// answer row, sits above it. Such a stage keeps the kernel beneath the range
// result's series-order sort, which cost the range quantile over a bare
// bucket selector CPU time on 26.8 and every small statement a fixed
// 0.1-0.5 MiB, and bounded no memory the empty-ladder factor had not
// already bounded (#4529).
func TestEmitHistogramQuantile_AnswerIsTheKernelSelect(t *testing.T) {
	t.Parallel()
	const (
		outer    = "SELECT `Attributes` AS `AttrsAlias`, "
		answered = " AS `Value` FROM (SELECT *, "
	)
	for _, native := range []bool{false, true} {
		for _, cumulative := range []bool{false, true} {
			h := rankWalkNativePlan()
			h.UseNativeQuantileAggregate = native
			h.BucketCountsCumulative = cumulative
			sql, _, err := Emit(context.Background(), h)
			if err != nil {
				t.Fatalf("native=%v cumulative=%v: Emit: %v", native, cumulative, err)
			}
			if !strings.HasPrefix(sql, outer) {
				t.Errorf("native=%v cumulative=%v: the statement must be the kernel's own SELECT, want prefix %q\nSQL: %s", native, cumulative, outer, sql)
			}
			if !native && !strings.Contains(sql, answered) {
				t.Errorf("native=%v cumulative=%v: the kernel's SELECT must compute Value over its staged helpers\nSQL: %s", native, cumulative, sql)
			}
			if strings.Contains(sql, "arrayJoin([(`AttrsAlias`") {
				t.Errorf("native=%v cumulative=%v: the answer row passes through a singleton ARRAY JOIN\nSQL: %s", native, cumulative, sql)
			}
		}
	}
}
