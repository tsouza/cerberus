package logql

import (
	"testing"
	"time"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerVectorAggregationRangeBucketedGate pins the
// `lc.rangeMode() && isMatrixRangeWindow(input)` gate inside
// [lowerVectorAggregation]. The flag controls whether the per-anchor
// bucket column is added to the Aggregate's GROUP BY so per-step rows
// survive the aggregation collapse.
//
// An INVERT_LOGICAL mutant flips `&&` to `||`, causing two divergent
// behaviours:
//
//   - range mode + non-matrix inner (e.g. an outer aggregation
//     wrapping a `vector(N)` literal): original keeps
//     rangeBucketed=false; mutant turns it on and references a
//     non-existent `anchor_ts` / `TimeUnix` column.
//   - instant mode + matrix inner: not constructible — matrix shape is
//     only produced under range mode.
//
// Pin the first case via `sum by (job) (vector(1))` lowered under
// [LowerAtRange]. `vector(...)` lowers to a Project over `chplan.OneRow`
// (see [lowerVector] / [syntheticLogScalar]) — a truly non-matrix
// shape that `isMatrixRangeWindow` will reject regardless of how
// many Aggregate / Project / Filter layers it walks through. The
// original lowering keeps the outer Aggregate at one GroupBy
// expression (the `by (job)` map-access); the mutant prepends the
// bucket and the GROUP BY length grows to two.
//
// Doubly-nested aggregations over a matrix (e.g. `sum by (job) (sum
// by (job) (rate(...)))`) DO surface a matrix RangeWindow through
// the inner Aggregate — see [TestIsMatrixRangeWindowWalksNestedAggregation]
// — so they're NOT suitable as the gate-isolation fixture; their
// outer aggregation correctly buckets per anchor.
func TestLowerVectorAggregationRangeBucketedGate(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()

	// Non-matrix inner: `vector(1)` lowers to Project(OneRow), which
	// bottoms out at a non-matrix node. `isMatrixRangeWindow` returns
	// false; the original keeps rangeBucketed=false; the mutant turns
	// it on.
	query := `sum by (job) (vector(1))`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}

	// Range-mode context: Step > 0 and a non-zero [Start, End] window.
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	lc := lowerCtx{Start: start, End: end, Step: time.Minute}

	plan, err := lower(expr, s, lc)
	if err != nil {
		t.Fatalf("lower(%q): %v", query, err)
	}

	// Walk past the outermost wrapper Project (the sample-shape
	// projection) to reach the outer Aggregate. The outer
	// Aggregate's GroupBy is the surface we inspect.
	outerProject, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("lower(%q) -> %T, want *chplan.Project (sample-shape wrapper)", query, plan)
	}
	outerAgg, ok := outerProject.Input.(*chplan.Aggregate)
	if !ok {
		t.Fatalf("lower(%q): Project.Input is %T, want *chplan.Aggregate", query, outerProject.Input)
	}

	// The outer Aggregate's inner is the `vector(1)` Project.
	if _, ok := outerAgg.Input.(*chplan.Project); !ok {
		t.Fatalf("outer Aggregate.Input is %T, want *chplan.Project (vector synthetic-scalar wrapper)", outerAgg.Input)
	}

	// Original: GroupBy carries one entry (the `by (job)` map-access
	// on ResourceAttributes). The mutant `||` would append the
	// bucket column → GroupBy length 2.
	if got, want := len(outerAgg.GroupBy), 1; got != want {
		t.Fatalf("outer Aggregate.GroupBy length = %d, want %d (rangeBucketed gate leaked through — bucket column was appended to a non-matrix inner)", got, want)
	}

	// Anchor the alias count to the same number — the mutant also
	// appends `bucket_ts` to GroupByAliases when it flips the gate.
	if got, want := len(outerAgg.GroupByAliases), 1; got != want {
		t.Fatalf("outer Aggregate.GroupByAliases length = %d, want %d (bucket_ts alias leaked through)", got, want)
	}
}

// TestLowerVectorAggregationRangeBucketedHonoursMatrixInner is the
// companion positive case: in range mode with a matrix-shape inner
// (the canonical `sum(rate(...))` lowering), `rangeBucketed` MUST be
// true so the per-anchor rows survive the GROUP BY collapse. This
// guards the OTHER direction of the `&&` gate — if a future change
// accidentally hard-codes `false` for the rangeBucketed gate, this
// test catches it.
func TestLowerVectorAggregationRangeBucketedHonoursMatrixInner(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()

	query := `sum by (job) (rate({app="api"}[5m]))`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}

	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	lc := lowerCtx{Start: start, End: end, Step: time.Minute}

	plan, err := lower(expr, s, lc)
	if err != nil {
		t.Fatalf("lower(%q): %v", query, err)
	}

	outerProject, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("lower(%q) -> %T, want *chplan.Project", query, plan)
	}
	outerAgg, ok := outerProject.Input.(*chplan.Aggregate)
	if !ok {
		t.Fatalf("lower(%q): Project.Input is %T, want *chplan.Aggregate", query, outerProject.Input)
	}

	// Range mode + matrix inner: the Aggregate's GroupBy MUST carry
	// the per-anchor column on top of the `by (job)` key — total 2.
	if got, want := len(outerAgg.GroupBy), 2; got != want {
		t.Fatalf("outer Aggregate.GroupBy length = %d, want %d (rangeBucketed should have appended `anchor_ts`)", got, want)
	}
	// The last entry is the per-anchor column reference.
	last, ok := outerAgg.GroupBy[len(outerAgg.GroupBy)-1].(*chplan.ColumnRef)
	if !ok {
		t.Fatalf("last GroupBy entry is %T, want *chplan.ColumnRef", outerAgg.GroupBy[len(outerAgg.GroupBy)-1])
	}
	if last.Name != "anchor_ts" {
		t.Fatalf("last GroupBy ColumnRef.Name = %q, want %q", last.Name, "anchor_ts")
	}
}
