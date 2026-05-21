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

// TestLowerVectorAggregationByTopLevelColumn pins the fix for task #218:
// `sum by (SeverityText) (rate({}[5m]))` (and the same pattern for any
// top-level OTel-CH scalar column the schema names) must produce one
// output series per distinct column value rather than collapsing every
// row into a single `{SeverityText:""}` series.
//
// Pre-fix flow: `levelAwareGroupKey("SeverityText", s)` returned
// `attributeLookupColumn(ResourceAttributes, "SeverityText")` — a Map
// access against a key that is never present (SeverityText is a
// top-level otel_logs column, not a key inside ResourceAttributes), so
// the outer Aggregate's GROUP BY column was always the empty string.
//
// Post-fix flow: the inner range Project's `withDetectedLevelAndColumns`
// wrap inflates the augmented identity map with a synthesised
// `SeverityText` key carrying `toString(SeverityText)`, AND the outer
// Aggregate's GROUP BY reads `ResourceAttributes['SeverityText']` from
// that map. The two layers agree on the key, so distinct severities
// produce distinct rows.
//
// This test walks the lowered plan and asserts the augmented-identity
// map carries the `SeverityText` synthesised key. The TXTAR fixture
// `test/spec/logql/agg_by_severity.txtar` exercises the end-to-end
// chDB round-trip (4 distinct severity rows survive the Aggregate).
func TestLowerVectorAggregationByTopLevelColumn(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	const query = `sum by (SeverityText) (rate({job="api"}[5m]))`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}

	plan, err := lower(expr, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lower(%q): %v", query, err)
	}

	// Walk down to the inner range Project to find the augmented map.
	// Plan shape: Project -> Aggregate -> RangeWindow -> Project (identity wrap).
	outerProj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.Project", plan)
	}
	agg, ok := outerProj.Input.(*chplan.Aggregate)
	if !ok {
		t.Fatalf("outer.Input = %T, want *chplan.Aggregate", outerProj.Input)
	}
	rw, ok := agg.Input.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("Aggregate.Input = %T, want *chplan.RangeWindow", agg.Input)
	}
	innerProj, ok := rw.Input.(*chplan.Project)
	if !ok {
		t.Fatalf("RangeWindow.Input = %T, want *chplan.Project", rw.Input)
	}

	// The first projection is the identity wrap (aliased to
	// ResourceAttributes). It must be a mapConcat whose synthesised
	// inner map literal carries a `SeverityText` key.
	if len(innerProj.Projections) == 0 {
		t.Fatalf("inner Project has no projections")
	}
	wrap, ok := innerProj.Projections[0].Expr.(*chplan.FuncCall)
	if !ok || wrap.Name != "mapConcat" {
		t.Fatalf("inner identity projection is %T (name %q), want *chplan.FuncCall(mapConcat)", innerProj.Projections[0].Expr, funcName(innerProj.Projections[0].Expr))
	}
	if len(wrap.Args) < 2 {
		t.Fatalf("mapConcat has %d args, want >= 2", len(wrap.Args))
	}
	mapFilter, ok := wrap.Args[1].(*chplan.FuncCall)
	if !ok || mapFilter.Name != "mapFilter" {
		t.Fatalf("mapConcat.Args[1] = %T (%q), want *chplan.FuncCall(mapFilter)", wrap.Args[1], funcName(wrap.Args[1]))
	}
	if len(mapFilter.Args) < 2 {
		t.Fatalf("mapFilter has %d args, want >= 2", len(mapFilter.Args))
	}
	synthMap, ok := mapFilter.Args[1].(*chplan.FuncCall)
	if !ok || synthMap.Name != "map" {
		t.Fatalf("mapFilter.Args[1] = %T (%q), want *chplan.FuncCall(map)", mapFilter.Args[1], funcName(mapFilter.Args[1]))
	}

	// Scan the synthesised map's args for a `SeverityText` key. The
	// map literal alternates key/value, so we walk even indices.
	var foundSeverityText bool
	for i := 0; i+1 < len(synthMap.Args); i += 2 {
		key, ok := synthMap.Args[i].(*chplan.LitString)
		if !ok {
			continue
		}
		if key.V == s.SeverityColumn {
			foundSeverityText = true
			break
		}
	}
	if !foundSeverityText {
		t.Fatalf("synthesised identity map missing %q key — outer by-clause cannot resolve to the column value (task #218 regression)", s.SeverityColumn)
	}

	// Outer Aggregate's GROUP BY reads from ResourceAttributes via
	// the synthesised key.
	if len(agg.GroupBy) != 1 {
		t.Fatalf("outer Aggregate.GroupBy length = %d, want 1", len(agg.GroupBy))
	}
	gkey, ok := agg.GroupBy[0].(*chplan.MapAccess)
	if !ok {
		t.Fatalf("outer Aggregate.GroupBy[0] = %T, want *chplan.MapAccess", agg.GroupBy[0])
	}
	key, ok := gkey.Key.(*chplan.LitString)
	if !ok || key.V != s.SeverityColumn {
		t.Fatalf("outer Aggregate.GroupBy[0].Key = %v, want LitString(%q)", gkey.Key, s.SeverityColumn)
	}
}

// funcName extracts the Name field from a FuncCall expression for
// diagnostic messages. Returns the empty string when expr isn't a
// FuncCall.
func funcName(expr chplan.Expr) string {
	if fc, ok := expr.(*chplan.FuncCall); ok {
		return fc.Name
	}
	return ""
}
