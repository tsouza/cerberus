package chsql

import (
	"context"
	"errors"
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

// TestHistogramQuantileValueFrag_EmptyLadderGuardIsNoBranch pins the
// empty-ladder guard of every classic kernel as a factor of the value, never
// as the condition of a branch around it: ClickHouse filters every array
// column a row-dependent `if` branch reads down to that branch's rows, which
// raised a range quantile's peak memory several-fold (#4518). The kernel
// never tests length(BucketCounts) either, which answered nothing the
// observations test does not.
func TestHistogramQuantileValueFrag_EmptyLadderGuardIsNoBranch(t *testing.T) {
	t.Parallel()

	const factor = " * if(`_cerb_hqc_observations` = 0, nan, 1)"
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
			if !strings.HasSuffix(sql, factor) {
				t.Errorf("%s kernel (cumulative=%v) must end in the empty-ladder factor %q:\n%s", name, cumulative, factor, sql)
			}
			if strings.Contains(sql, "if(`_cerb_hqc_observations` = 0, nan, if") ||
				strings.Contains(sql, "length(`BucketCounts`) = 0") {
				t.Errorf("%s kernel (cumulative=%v) branches on an empty ladder:\n%s", name, cumulative, sql)
			}
		}
	}
}

// TestEmitHistogramQuantile_AnswerLeavesThroughOutputBoundary pins that both
// singular classic kernels hand their answer up through the output boundary
// (materializeHistogramOutput): the statement selects every output column
// from the boundary's tuple, and the boundary's singleton ARRAY JOIN reads
// the kernel's `Value`. Without it ClickHouse runs the kernel after the
// result's series-order sort, over the sort's output blocks, which raised a
// range quantile's peak several-fold (#4518).
func TestEmitHistogramQuantile_AnswerLeavesThroughOutputBoundary(t *testing.T) {
	t.Parallel()
	const (
		outer    = "SELECT `_cerb_histogram_output`.1 AS `AttrsAlias`, `_cerb_histogram_output`.2 AS `Value` FROM (SELECT arrayJoin([(`AttrsAlias`, `Value`)]) AS `_cerb_histogram_output` FROM (SELECT `Attributes` AS `AttrsAlias`, "
		answered = " AS `Value` FROM (SELECT *, arrayFirstIndex("
	)
	for name, native := range map[string]bool{"interpolating": false, "native": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := rankWalkNativePlan()
			h.UseNativeQuantileAggregate = native
			sql, _, err := Emit(context.Background(), h)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if !strings.HasPrefix(sql, outer) {
				t.Errorf("the quantile's answer must leave through the output boundary, want prefix %q\nSQL: %s", outer, sql)
			}
			if !native && !strings.Contains(sql, answered) {
				t.Errorf("the boundary must read the kernel's own Value\nSQL: %s", sql)
			}
		})
	}
}

// TestMaterializeHistogramOutput_RefusesAnUnnamedColumn pins that the output
// boundary refuses a row with a column it cannot read by name, rather than
// emitting a tuple that names an empty column.
func TestMaterializeHistogramOutput_RefusesAnUnnamedColumn(t *testing.T) {
	t.Parallel()
	row := chplan.Schema{Columns: []chplan.Column{{Name: "Attributes"}, {Name: ""}}}
	if _, err := materializeHistogramOutput(NewQuery().Select(Star()), row); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("materializeHistogramOutput over an unnamed column: err = %v, want ErrUnsupported", err)
	}
}
