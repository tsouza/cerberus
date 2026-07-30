package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestTSGridNativeRecollapse_StaysNativeToTheEngine pins that the
// deferred-label-shaping shape is invisible to both engine-level readers of the
// native node: the experimental-setting gate still fires, and the plan_shape_id
// still classifies the query as native ("rwn").
//
// Both detectors match on the node TYPE, so populating Recollapse cannot change
// their verdict — that is exactly the invariant worth pinning. The hoist adds
// two SQL levels inside the same node, so a future refactor that split the
// deferred shape into its own plan node would silently stop stamping
// allow_experimental_time_series_aggregate_functions (a production 502 on every
// hoisted query) and silently reclassify the shape id (breaking the
// observability series that tracks native-path adoption).
func TestTSGridNativeRecollapse_StaysNativeToTheEngine(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	native := func(recollapse []chplan.Projection) chplan.Node {
		return &chplan.Project{Input: &chplan.RangeWindowNative{
			Input:           &chplan.Scan{Table: "otel_metrics_sum"},
			Func:            "rate",
			Range:           5 * time.Minute,
			Step:            30 * time.Second,
			Start:           start,
			End:             end,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			Recollapse:      recollapse,
		}}
	}
	hoisted := native([]chplan.Projection{{
		Expr:  &chplan.FuncCall{Name: chplan.CanonicalMapFunc, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}}},
		Alias: "Attributes",
	}})

	if !planHasTSGridNative(hoisted) {
		t.Error("planHasTSGridNative == false for a hoisted plan — the experimental setting would NOT be stamped")
	}
	got := planShapeID(hoisted)
	if !strings.Contains(got, "rwn") {
		t.Errorf("planShapeID = %q; want it to carry the native modifier %q", got, "rwn")
	}
	// The shape id must be unchanged by the hoist: it is a COST-only rewrite of
	// the same query shape, so splitting the observability series in two would
	// misreport it as a different query.
	if want := planShapeID(native(nil)); got != want {
		t.Errorf("planShapeID(hoisted) = %q; want the pre-hoist id %q", got, want)
	}
}
