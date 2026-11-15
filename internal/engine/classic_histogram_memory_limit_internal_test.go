package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/solver"
)

// classicQuantileOver is a classic-histogram quantile over input.
func classicQuantileOver(input chplan.Node) *chplan.HistogramQuantile {
	return &chplan.HistogramQuantile{Input: input, Phi: 0.5, BucketCountsColumn: "BucketCounts", ExplicitBoundsColumn: "ExplicitBounds"}
}

// histogramScan is a bare scan of the classic-histogram table.
func histogramScan() chplan.Node { return &chplan.Scan{Table: "otel_metrics_histogram"} }

// TestInspectPlanShape_ClassicHistogramQuantileIsTheNode pins what sets the
// fact: a chplan.HistogramQuantile anywhere in the plan — at the root, under
// another node, inside a chplan.HistogramQuantiles (whose children skip it),
// or inside a scalar subquery — and neither the native quantile nor a plan
// that only reads the classic-histogram table.
func TestInspectPlanShape_ClassicHistogramQuantileIsTheNode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		plan chplan.Node
		want bool
	}{
		{"classic quantile", classicQuantileOver(histogramScan()), true},
		{"under a projection", &chplan.Project{Input: classicQuantileOver(histogramScan())}, true},
		{"inside histogram_quantiles", &chplan.HistogramQuantiles{
			Histogram: classicQuantileOver(histogramScan()),
			Levels:    []chplan.HistogramQuantileLevel{{Phi: 0.5}},
		}, true},
		{"inside a scalar subquery", &chplan.Filter{
			Input: histogramScan(),
			Predicate: &chplan.Binary{
				Op:    chplan.OpGt,
				Left:  &chplan.ScalarSubquery{Input: classicQuantileOver(histogramScan())},
				Right: &chplan.LitInt{V: 0},
			},
		}, true},
		{"native quantile", &chplan.HistogramQuantileNative{Input: histogramScan(), Phi: 0.5}, false},
		{"classic-histogram table without a quantile", histogramScan(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := inspectPlanShape(tc.plan, "").classicHistogramQuantile; got != tc.want {
				t.Fatalf("classicHistogramQuantile = %v, want %v", got, tc.want)
			}
		})
	}
}

// chMemoryLimitAbort is the error chain chclient surfaces for a code-241
// abort.
func chMemoryLimitAbort() error {
	return &chclient.MemoryLimitError{
		Limit: testQueryMemoryCap,
		Cause: &clickhouse.Exception{Code: 241, Name: "MEMORY_LIMIT_EXCEEDED"},
	}
}

// TestClassicHistogramMemoryLimit_ClassifiesAsTheUnwrappedAbort pins that
// the wrapping changes no engine-side classification: the route memo's
// outcome (route A and route B), the corpus outcome token and the failure
// log class read a wrapped abort exactly as the abort it wraps.
func TestClassicHistogramMemoryLimit_ClassifiesAsTheUnwrappedAbort(t *testing.T) {
	t.Parallel()
	raw := chMemoryLimitAbort()
	wrapped := wrapClassicHistogramMemoryLimit(raw, true)
	var classic *ClassicHistogramMemoryLimitError
	if !errors.As(wrapped, &classic) {
		t.Fatalf("wrapClassicHistogramMemoryLimit(abort, true) = %v, want a ClassicHistogramMemoryLimitError", wrapped)
	}
	if again := wrapClassicHistogramMemoryLimit(wrapped, true); again != wrapped {
		t.Fatalf("wrapping a wrapped abort = %v, want it unchanged", again)
	}
	for _, route := range []routememo.Route{routememo.RouteA, routememo.RouteB} {
		if got := classifyRouteOutcomeAfter(route, wrapped, time.Second); got != routememo.OutcomeResourceFailure {
			t.Fatalf("route %v outcome = %v, want OutcomeResourceFailure", route, got)
		}
	}
	if got, want := outcomeTokenForErr(wrapped), outcomeTokenForErr(raw); got != want || got != optcorpusExitOOM {
		t.Fatalf("outcome token = %q, want %q", got, optcorpusExitOOM)
	}
	if got := queryFailureClass(wrapped); got != optcorpusExitOOM {
		t.Fatalf("failure class = %q, want %q", got, optcorpusExitOOM)
	}
	if got := wrapClassicHistogramMemoryLimit(raw, false); got != raw {
		t.Fatalf("wrapClassicHistogramMemoryLimit(abort, false) = %v, want the abort unchanged", got)
	}
}

// composedFakeCursor is a chclient.ComposedCursor that fails its drain with
// a memory-limit abort.
type composedFakeCursor struct{ budget time.Duration }

func (composedFakeCursor) Next() bool                      { return false }
func (composedFakeCursor) Sample() chclient.Sample         { return chclient.Sample{} }
func (composedFakeCursor) Err() error                      { return chMemoryLimitAbort() }
func (composedFakeCursor) Close() error                    { return nil }
func (composedFakeCursor) Inspected() int64                { return 0 }
func (c composedFakeCursor) TeardownBudget() time.Duration { return c.budget }

// TestWrapClassicHistogramCursor_KeepsTheComposedTeardown pins that wrapping
// a route-B composed cursor keeps it a chclient.ComposedCursor with its own
// teardown budget, so chclient.CloseCursor bounds its teardown the same way.
func TestWrapClassicHistogramCursor_KeepsTheComposedTeardown(t *testing.T) {
	t.Parallel()
	const budget = 7 * time.Second
	wrapped := wrapClassicHistogramCursor(composedFakeCursor{budget: budget}, true)
	composed, ok := wrapped.(chclient.ComposedCursor)
	if !ok {
		t.Fatal("the wrapped composed cursor is no longer a chclient.ComposedCursor")
	}
	if got := composed.TeardownBudget(); got != budget {
		t.Fatalf("TeardownBudget = %v, want %v", got, budget)
	}
	var classic *ClassicHistogramMemoryLimitError
	if !errors.As(wrapped.Err(), &classic) {
		t.Fatalf("wrapped Err = %v, want a ClassicHistogramMemoryLimitError", wrapped.Err())
	}
	if _, ok := wrapClassicHistogramCursor(&perRungFakeCursor{}, true).(chclient.ComposedCursor); ok {
		t.Fatal("wrapping a plain cursor made it a chclient.ComposedCursor")
	}
}

// TestBuildRoutedCursorResult_WrapsAClassicStage pins the route-B memo-hit
// and retry dispatch seam: the cursor it returns for a classic-histogram
// plan reports a memory-limit drain abort as the quantile's, and the cursor for
// any other plan reports it unchanged.
func TestBuildRoutedCursorResult_WrapsAClassicStage(t *testing.T) {
	t.Parallel()
	eng := &Engine{}
	decision := &solver.Decision{K: 2}
	for _, tc := range []struct {
		name string
		plan chplan.Node
		want bool
	}{
		{"classic", classicQuantileOver(histogramScan()), true},
		{"other", histogramScan(), false},
	} {
		res := eng.buildRoutedCursorResult(Meta{}, tc.plan, "promql", decision, composedFakeCursor{}, nil, "retry")
		var classic *ClassicHistogramMemoryLimitError
		if got := errors.As(res.Cursor.Err(), &classic); got != tc.want {
			t.Fatalf("%s: drain abort wrapped = %v, want %v", tc.name, got, tc.want)
		}
	}
}
