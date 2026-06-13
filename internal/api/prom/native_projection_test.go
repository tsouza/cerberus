package prom

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestWrapSampleProjection_NativeRangeWindowIsDerivedMatrix pins the
// classification of chplan.RangeWindowNative inside the HTTP-layer sample
// projection wrapper.
//
// Regression for the compose-smoke 502 surfaced by enabling the
// experimental native rate-range path on a real ClickHouse (#104): the
// native node's output schema is the SAME derived (group-keys…, anchor_ts,
// value) shape the fan-out matrix RangeWindow produces — MetricName never
// exists in that scope. Before the fix, isDerivedShape / isMatrixRangeWindow
// lacked a *chplan.RangeWindowNative case, so wrapWithSampleProjection took
// the canonical branch and emitted a bare `MetricName` column reference
// against the native subquery, which ClickHouse rejects with
// `code 47, Unknown expression identifier 'MetricName'`.
//
// The wrapper must instead synthesise `” AS MetricName` (derived branch)
// and source TimeUnix from the per-row `anchor_ts` column (matrix branch),
// exactly as it does for the fan-out RangeWindow.
func TestWrapSampleProjection_NativeRangeWindowIsDerivedMatrix(t *testing.T) {
	s := schema.DefaultOTelMetrics()

	native := &chplan.RangeWindowNative{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Func:            "rate",
		Range:           5 * time.Minute,
		Step:            time.Minute,
		Start:           time.Unix(1000, 0).UTC(),
		End:             time.Unix(4600, 0).UTC(),
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}

	if !isDerivedShape(native, s) {
		t.Fatal("RangeWindowNative must be a derived shape (MetricName absent from its scope)")
	}
	if !isMatrixRangeWindow(native) {
		t.Fatal("RangeWindowNative must be matrix-shape (exposes per-row anchor_ts)")
	}

	wrapped, ok := wrapWithSampleProjection(native, s).(*chplan.Project)
	if !ok {
		t.Fatalf("wrapWithSampleProjection returned %T, want *chplan.Project", wrapped)
	}
	if len(wrapped.Projections) != 4 {
		t.Fatalf("got %d projections, want 4 (MetricName, Attributes, TimeUnix, Value)", len(wrapped.Projections))
	}

	// MetricName must be the synthesised empty-string literal, NOT a bare
	// column reference into the native subquery (which would 502).
	mn := wrapped.Projections[0]
	if mn.Alias != s.MetricNameColumn {
		t.Fatalf("projection[0] alias = %q, want %q", mn.Alias, s.MetricNameColumn)
	}
	lit, ok := mn.Expr.(*chplan.LitString)
	if !ok {
		t.Fatalf("MetricName projection is %T, want *chplan.LitString (synthesised); a ColumnRef here is the 502 bug", mn.Expr)
	}
	if lit.V != "" {
		t.Fatalf("MetricName literal = %q, want empty string", lit.V)
	}

	// TimeUnix must source from the per-row anchor_ts column (matrix
	// branch), not the now64() instant synthesis.
	ts := wrapped.Projections[2]
	if ts.Alias != s.TimestampColumn {
		t.Fatalf("projection[2] alias = %q, want %q", ts.Alias, s.TimestampColumn)
	}
	col, ok := ts.Expr.(*chplan.ColumnRef)
	if !ok || col.Name != "anchor_ts" {
		t.Fatalf("TimeUnix projection = %#v, want ColumnRef{anchor_ts}", ts.Expr)
	}
}
