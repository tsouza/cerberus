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
// CONDITIONALS_NEGATION mutants at histogram_quantile.go:457
// (`h.PhiExpr != nil`, inside the arrayFirstIndex predicate) and :266
// (`h.PhiExpr == nil`, the early return before the isNaN wrapper).
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
// Negating 217 alone would apply the wrapped `if(...)  = 1)` predicate
// to the literal-Phi path (and the bare predicate to the PhiExpr path);
// negating 318 alone would apply (or skip) the isNaN wrapper on the
// wrong path. Asserting both markers on both cases pins each
// independently.
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
			t.Errorf("literal phi must use the bare `c >= target` predicate (line 457 flipped?):\n%s", sql)
		}
		if strings.Contains(sql, "arrayFirstIndex(c -> (if(") {
			t.Errorf("literal phi must NOT wrap the predicate in if(...)=1 (line 457 flipped?):\n%s", sql)
		}
		if strings.Contains(sql, "isNaN(") {
			t.Errorf("literal phi must NOT carry the isNaN guard (line 266 flipped?):\n%s", sql)
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
			t.Errorf("computed phi must wrap the predicate as (if(c >= ..., 1, 0) = 1) (line 457 flipped?):\n%s", sql)
		}
		if !strings.Contains(sql, "= 1),") {
			t.Errorf("computed phi predicate must close with `= 1)` (line 457 flipped?):\n%s", sql)
		}
		if !strings.HasPrefix(sql, "if(isNaN(") {
			t.Errorf("computed phi must lead with the isNaN(phi) guard (line 266 flipped?):\n%s", sql)
		}
	})
}

// TestEmitHistogramQuantile_RequiredColumns kills the INVERT_LOGICAL mutant
// at histogram_quantile.go:92:32 (`h.BucketCountsColumn == "" ||
// h.ExplicitBoundsColumn == ""` -> `&&`). Each column empty ALONE must
// still error; an `&&` mutant would require BOTH to be empty
// simultaneously, letting a plan missing just one of the two through.
func TestEmitHistogramQuantile_RequiredColumns(t *testing.T) {
	t.Parallel()
	base := func() *chplan.HistogramQuantile {
		return &chplan.HistogramQuantile{
			Input:                &chplan.Scan{Table: "otel_metrics_histogram"},
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
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := base()
			c.mutate(h)
			_, _, err := Emit(context.Background(), h)
			if !errors.Is(err, ErrUnsupported) {
				t.Errorf("expected ErrUnsupported, got %v", err)
			}
		})
	}
}

// TestEmitHistogramQuantile_GroupByAliasFallback kills both the
// CONDITIONALS_BOUNDARY (`<` -> `<=`) and CONDITIONALS_NEGATION (`<` ->
// `>=`) mutants at histogram_quantile.go:152:8 (`i <
// len(h.GroupByAliases)`). Placing the alias slice ONE ENTRY SHORT of
// GroupBy hits the exact boundary index (i == len(h.GroupByAliases)):
// under either mutant that index tries h.GroupByAliases[i], which is out
// of range and panics; under the original code it falls back to an
// unaliased projection.
func TestEmitHistogramQuantile_GroupByAliasFallback(t *testing.T) {
	t.Parallel()
	h := &chplan.HistogramQuantile{
		Input:                &chplan.Scan{Table: "otel_metrics_histogram"},
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
