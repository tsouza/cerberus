package prom

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// matrixWindowUnder builds the plan spine a label rewrite over a matrix
// range vector produces below the guard: a matrix RangeWindow, then the
// Project that swaps Attributes while forwarding `anchor_ts` and the
// schema timestamp.
func matrixWindowUnder(s schema.Metrics, offset time.Duration) *chplan.Project {
	return &chplan.Project{
		Input: &chplan.RangeWindow{
			Input:      &chplan.Scan{Table: "otel_metrics_gauge"},
			OuterRange: time.Hour,
			Range:      5 * time.Minute,
			Step:       time.Minute,
			Offset:     offset,
			GroupBy:    []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
		},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn}},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}},
		},
	}
}

// guardOverMatrixWindow mirrors the Aggregate promql's
// guardLabelRewriteCollision plants over that spine: keyed on the label
// set, the grid anchor and the timestamp, with the row-count HAVING.
func guardOverMatrixWindow(s schema.Metrics, offset time.Duration) *chplan.Aggregate {
	inner := matrixWindowUnder(s, offset)
	return &chplan.Aggregate{
		Input: inner,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.AttributesColumn},
			&chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn},
			&chplan.ColumnRef{Name: s.TimestampColumn},
		},
		GroupByAliases: []string{
			s.AttributesColumn,
			chplan.RangeWindowAnchorColumn,
			s.TimestampColumn,
		},
		AggFuncs: []chplan.AggFunc{{
			Name:  "any",
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}},
			Alias: s.ValueColumn,
		}},
	}
}

// foldingAggregateOverMatrixWindow mirrors what `sum by (job)
// (rate(m[5m]))` lowers to: the label set reduced to one extracted key
// under a synthetic alias, and the step axis republished as `bucket_ts`.
func foldingAggregateOverMatrixWindow(s schema.Metrics) *chplan.Aggregate {
	return &chplan.Aggregate{
		Input: matrixWindowUnder(s, 0),
		GroupBy: []chplan.Expr{
			&chplan.MapAccess{
				Map: &chplan.ColumnRef{Name: s.AttributesColumn},
				Key: &chplan.LitString{V: "job"},
			},
			&chplan.ColumnRef{Name: s.TimestampColumn},
		},
		GroupByAliases: []string{"gkey_0", "bucket_ts"},
		AggFuncs: []chplan.AggFunc{{
			Name:  "sum",
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}},
			Alias: s.ValueColumn,
		}},
		DropEmptyOnNoGroup: true,
	}
}

// TestMatrixWalks_CrossShapePreservingAggregate pins that both spine walks
// see through the duplicate-labelset guard's Aggregate.
//
// The two walks are asserted together because they must agree: the first
// decides `anchor_ts` is the timestamp source, the second decides how that
// column is relabeled. One crossing a node the other stops at emits an
// offset-relabel over a column nobody selected — or, in the direction this
// test guards, reclassifies a guarded matrix query as instant and answers
// an EMPTY matrix at status 200, which no status assertion notices.
func TestMatrixWalks_CrossShapePreservingAggregate(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	cols := sampleColumns(s)

	guarded := guardOverMatrixWindow(s, 0)
	if !isMatrixRangeWindow(guarded, cols) {
		t.Fatal("a guard Aggregate keyed on (Attributes, anchor_ts) republishes the grid it " +
			"was handed; not crossing it answers an empty matrix at 200")
	}

	// The relabel decision has to survive the crossing too: a matrix window
	// with a non-zero offset reports on the UNSHIFTED request grid, and the
	// guard does not change that.
	const offset = 10 * time.Minute
	shifted := guardOverMatrixWindow(s, offset)
	gotOffset, relabel := matrixWindowOffset(shifted, cols)
	if !relabel {
		t.Fatal("matrixWindowOffset must reach the offset window under the guard Aggregate")
	}
	if gotOffset != offset {
		t.Errorf("matrixWindowOffset offset = %v, want %v", gotOffset, offset)
	}

	// The wrapper is what consumes both answers, so pin the column it
	// actually projects into the timestamp slot.
	wrapped, ok := wrapWithSampleProjection(guarded, s).(*chplan.Project)
	if !ok {
		t.Fatalf("wrapWithSampleProjection returned %T, want *chplan.Project", wrapped)
	}
	ts := wrapped.Projections[2]
	if ts.Alias != s.TimestampColumn {
		t.Fatalf("projection[2] alias = %q, want %q", ts.Alias, s.TimestampColumn)
	}
	ref, ok := ts.Expr.(*chplan.ColumnRef)
	if !ok {
		t.Fatalf("timestamp projection is %T, want *chplan.ColumnRef{anchor_ts}; "+
			"a now64() synthesis here is the empty-matrix bug", ts.Expr)
	}
	if ref.Name != chplan.RangeWindowAnchorColumn {
		t.Errorf("timestamp projection reads %q, want %q", ref.Name, chplan.RangeWindowAnchorColumn)
	}
}

// TestMatrixWalks_StopAtFoldingAggregate is the other direction, and it is
// the assertion that a blanket `case *chplan.Aggregate` arm would fail.
//
// `sum by (job) (rate(m[5m]))` is also an Aggregate over a matrix
// RangeWindow, but it reports on the step axis through its own `bucket_ts`
// alias and never exposes `anchor_ts`. Crossing it would make the wrapper
// select an identifier the aggregate's output scope does not carry.
func TestMatrixWalks_StopAtFoldingAggregate(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	cols := sampleColumns(s)

	folding := foldingAggregateOverMatrixWindow(s)
	if isMatrixRangeWindow(folding, cols) {
		t.Error("a PromQL aggregation folds the grid and republishes the step axis under its " +
			"own alias; crossing it selects `anchor_ts` from a scope that has none")
	}
	if _, relabel := matrixWindowOffset(folding, cols); relabel {
		t.Error("matrixWindowOffset must not reach past a folding Aggregate; the grid-keyed " +
			"path applies its own offset handling")
	}
}
