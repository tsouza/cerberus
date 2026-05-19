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
// [lowerVectorAggregation]. The flag controls whether `anchor_ts` is
// added to the Aggregate's GROUP BY so per-step rows survive the
// aggregation collapse.
//
// An INVERT_LOGICAL mutant flips `&&` to `||`, causing two divergent
// behaviours:
//
//   - range mode + non-matrix inner (e.g. an outer aggregation wrapping
//     an inner aggregation): original keeps rangeBucketed=false;
//     mutant turns it on and references a non-existent `anchor_ts`
//     column.
//   - instant mode + matrix inner: not constructible — matrix shape is
//     only produced under range mode.
//
// Pin the first case: lower a doubly-nested `sum by (job) (sum by
// (job) (rate(...)))` under [LowerAtRange] and inspect the OUTER
// Aggregate's GROUP BY. The original lowering leaves it with a single
// expression (the by-key access); the mutant prepends `anchor_ts` and
// the GROUP BY length grows to two.
func TestLowerVectorAggregationRangeBucketedGate(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()

	// Nested aggregation: the outer `sum by (job)` sees an inner that
	// is a *chplan.Project over *chplan.Aggregate — not a matrix
	// RangeWindow. `isMatrixRangeWindow` returns false; the original
	// keeps rangeBucketed=false; the mutant turns it on.
	query := `sum by (job) (sum by (job) (rate({app="api"}[5m])))`
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

	// The outer Aggregate's inner is the inner sum's Project. Confirm
	// — this anchors the test fixture against future changes.
	if _, ok := outerAgg.Input.(*chplan.Project); !ok {
		t.Fatalf("outer Aggregate.Input is %T, want *chplan.Project (inner sample-shape wrapper)", outerAgg.Input)
	}

	// Original: GroupBy carries one entry (the `by (job)` map-access
	// on ResourceAttributes). The mutant `||` would append `anchor_ts`
	// → GroupBy length 2.
	if got, want := len(outerAgg.GroupBy), 1; got != want {
		t.Fatalf("outer Aggregate.GroupBy length = %d, want %d (rangeBucketed gate leaked through — `anchor_ts` was appended to a non-matrix inner)", got, want)
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
