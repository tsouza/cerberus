package chsql

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Unit pins for the emitted-SQL statement-size bound (cerberus issue #2733,
// emit_size_bound.go). The end-to-end proof — issue #2728's level-2
// composition still emits, its level-3 sibling is refused — lives next to the
// shapes themselves in internal/promql; what this file pins is the guard's own
// arithmetic, its fallbacks, and the composition summary its message carries,
// each of which is a one-line decision no end-to-end assertion would localise.

// TestRequireEmittedSQLBounded_Boundary pins the ceiling EXACTLY: a statement
// of precisely the bound's length is admitted, one byte more is refused. The
// bound is a `<=`, and the whole value of naming ClickHouse's own
// max_query_size as the default rests on that comparison being inclusive —
// a statement of exactly 262144 bytes is one ClickHouse parses (its own check
// fires at position 262145), so rejecting it would be cerberus refusing a
// query the server would have run.
func TestRequireEmittedSQLBounded_Boundary(t *testing.T) {
	t.Parallel()

	e := &emitter{}
	if got := e.emittedSQLByteBound(); got != maxEmittedSQLBytes {
		t.Fatalf("emittedSQLByteBound() = %d on a zero emitter, want the compiled-in default %d", got, maxEmittedSQLBytes)
	}

	atBound := strings.Repeat("x", int(maxEmittedSQLBytes))
	if err := e.requireEmittedSQLBounded(nil, atBound); err != nil {
		t.Errorf("a statement of exactly %d bytes was refused: %v — the bound must be inclusive, "+
			"since ClickHouse's own max_query_size check fires at position %d",
			maxEmittedSQLBytes, err, maxEmittedSQLBytes+1)
	}

	overBound := atBound + "x"
	err := e.requireEmittedSQLBounded(nil, overBound)
	if err == nil {
		t.Fatalf("a statement of %d bytes was admitted, want a rejection at %d + 1", len(overBound), maxEmittedSQLBytes)
	}
	if !errors.Is(err, ErrEmittedSQLTooLarge) {
		t.Errorf("error %v does not wrap ErrEmittedSQLTooLarge", err)
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("error %v does not wrap ErrUnsupported — the HTTP error mapping keys on that", err)
	}
	for _, want := range []string{"262145", "262144", "max_query_size", "CERBERUS_CH_MAX_EMITTED_SQL_BYTES"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("rejection message %q does not mention %q", err.Error(), want)
		}
	}
}

// TestEmittedSQLByteBound_Override pins both directions of the operator knob:
// a threaded override replaces the default, and a bound that would be read as
// the Go zero value falls back to the default rather than rejecting every
// statement. The fallback is what protects the many tests in this package that
// build &emitter{} directly, bypassing Emit's ctx seeding.
func TestEmittedSQLByteBound_Override(t *testing.T) {
	t.Parallel()

	const override int64 = 4096
	ctx := WithMaxEmittedSQLBytes(context.Background(), override)
	if got := maxEmittedSQLBytesFromCtx(ctx); got != override {
		t.Errorf("maxEmittedSQLBytesFromCtx = %d, want the threaded override %d", got, override)
	}
	if got := maxEmittedSQLBytesFromCtx(context.Background()); got != maxEmittedSQLBytes {
		t.Errorf("maxEmittedSQLBytesFromCtx = %d on an unthreaded ctx, want the default %d", got, maxEmittedSQLBytes)
	}

	e := &emitter{emittedSQLMaxBytes: override}
	if got := e.emittedSQLByteBound(); got != override {
		t.Errorf("emittedSQLByteBound() = %d, want the seeded %d", got, override)
	}
	if err := e.requireEmittedSQLBounded(nil, strings.Repeat("x", int(override)+1)); err == nil {
		t.Error("a statement past the OVERRIDE was admitted — the override is not being read")
	}
	if err := e.requireEmittedSQLBounded(nil, strings.Repeat("x", int(override))); err != nil {
		t.Errorf("a statement at exactly the override was refused: %v", err)
	}
}

// sizeBoundRangeWindow builds one grid-carrying level over input. RangeWindow
// is a chplan.GridCarrier, which is what gridCarrierNesting counts.
func sizeBoundRangeWindow(input chplan.Node) *chplan.RangeWindow {
	return &chplan.RangeWindow{Input: input, Func: "rate", TimestampColumn: "TimeUnix", ValueColumn: "Value"}
}

// TestGridCarrierNesting counts the LONGEST chain of grid carriers, sees the
// carriers hidden inside an Expr slot, and reports a Mixed set op wherever it
// sits. Each of the three is a separate way the summary could understate a
// composition and so name the wrong shape in a rejection.
func TestGridCarrierNesting(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}

	t.Run("chain length", func(t *testing.T) {
		t.Parallel()
		if levels, mixed := gridCarrierNesting(scan); levels != 0 || mixed {
			t.Errorf("bare scan = (%d, %v), want (0, false)", levels, mixed)
		}
		one := sizeBoundRangeWindow(scan)
		if levels, _ := gridCarrierNesting(one); levels != 1 {
			t.Errorf("one carrier = %d levels, want 1", levels)
		}
		three := sizeBoundRangeWindow(sizeBoundRangeWindow(one))
		if levels, _ := gridCarrierNesting(three); levels != 3 {
			t.Errorf("three stacked carriers = %d levels, want 3", levels)
		}
		// A non-carrier between two carriers does not break the chain: what
		// costs SQL is how many grids are stacked, not how they are spelled.
		spaced := sizeBoundRangeWindow(&chplan.Filter{Input: one, Predicate: &chplan.LitBool{V: true}})
		if levels, _ := gridCarrierNesting(spaced); levels != 2 {
			t.Errorf("two carriers with a Filter between = %d levels, want 2", levels)
		}
	})

	t.Run("longest branch wins over a shared DAG child", func(t *testing.T) {
		t.Parallel()
		// splitMixedRelByDiscriminator hands the SAME relation to both halves,
		// so the plan is a DAG. The count is a DEPTH, not a total: two arms
		// over one shared two-level child is still three levels, not five.
		shared := sizeBoundRangeWindow(sizeBoundRangeWindow(scan))
		setOp := &chplan.VectorSetOp{
			Left:  sizeBoundRangeWindow(shared),
			Right: shared,
			Op:    chplan.VectorSetOr,
			Mixed: true,
		}
		levels, mixed := gridCarrierNesting(setOp)
		if levels != 3 {
			t.Errorf("shared-child DAG = %d levels, want 3 (the deepest branch)", levels)
		}
		if !mixed {
			t.Error("mixed = false, want true — the set op is Mixed")
		}
	})

	t.Run("carriers inside an Expr slot", func(t *testing.T) {
		t.Parallel()
		// A composition nested in a scalar subquery renders just as much SQL
		// as one on the spine, so Node.Children alone is not enough reach.
		hidden := &chplan.Filter{
			Input: scan,
			Predicate: &chplan.Binary{
				Op:    chplan.OpGt,
				Left:  &chplan.ColumnRef{Name: "Value"},
				Right: &chplan.ScalarSubquery{Input: sizeBoundRangeWindow(sizeBoundRangeWindow(scan))},
			},
		}
		if levels, _ := gridCarrierNesting(hidden); levels != 2 {
			t.Errorf("carriers reachable only through an Expr slot = %d levels, want 2", levels)
		}
	})
}

// TestPlanCompositionSummary pins all four arms of the sentence a rejection
// carries. The message is the entire deliverable of this bound — the query
// already failed before it existed, just with ClickHouse's own wording — so a
// summary that stopped naming the composition would silently undo the fix.
func TestPlanCompositionSummary(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	mixedOver := func(n chplan.Node) chplan.Node {
		return &chplan.VectorSetOp{Left: n, Right: scan, Op: chplan.VectorSetOr, Mixed: true}
	}

	cases := []struct {
		name string
		plan chplan.Node
		want string
	}{
		{
			name: "stacked levels over a mixed relation",
			plan: sizeBoundRangeWindow(sizeBoundRangeWindow(mixedOver(scan))),
			want: "a plan stacking 2 range-vector levels over a mixed float/histogram relation",
		},
		{
			name: "stacked levels, no mixed relation",
			plan: sizeBoundRangeWindow(sizeBoundRangeWindow(scan)),
			want: "a plan stacking 2 range-vector levels",
		},
		{
			name: "mixed relation, one level",
			plan: sizeBoundRangeWindow(mixedOver(scan)),
			want: "a plan over a mixed float/histogram relation",
		},
		{
			name: "neither",
			plan: scan,
			want: "this query's plan",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := planCompositionSummary(tc.plan); got != tc.want {
				t.Errorf("planCompositionSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRequireEmittedSQLBounded_NamesTheRootPlan pins which plan the message
// describes when a SUB-statement is what crossed the ceiling: the whole query's
// shape, not the fragment's. A user can only act on the shape they wrote, and
// "at least" is what keeps the byte figure truthful in that case.
func TestRequireEmittedSQLBounded_NamesTheRootPlan(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	fragment := sizeBoundRangeWindow(scan)
	root := sizeBoundRangeWindow(sizeBoundRangeWindow(fragment))

	e := &emitter{rootPlan: root}
	err := e.requireEmittedSQLBounded(fragment, strings.Repeat("x", int(maxEmittedSQLBytes)+1))
	if err == nil {
		t.Fatal("an over-bound sub-statement was admitted")
	}
	if want := "a plan stacking 3 range-vector levels"; !strings.Contains(err.Error(), want) {
		t.Errorf("message %q does not describe the ROOT plan (%q) — it named the fragment instead", err.Error(), want)
	}
	if !strings.Contains(err.Error(), "at least") {
		t.Errorf("message %q drops the \"at least\" qualifier, which is what makes a sub-statement's "+
			"byte count an honest figure for the whole statement", err.Error())
	}

	// With no root stamped (EmitCompareRootLeg, and this package's own
	// round-trip tests), the node actually handed to the guard is described.
	bare := &emitter{}
	err = bare.requireEmittedSQLBounded(fragment, strings.Repeat("x", int(maxEmittedSQLBytes)+1))
	if err == nil {
		t.Fatal("an over-bound statement was admitted by a rootless emitter")
	}
	if want := "a plan over"; strings.Contains(err.Error(), want) {
		t.Errorf("message %q describes a mixed relation the fragment does not carry", err.Error())
	}
	if !strings.Contains(err.Error(), "this query's plan") {
		t.Errorf("message %q does not fall back to describing the node it was handed", err.Error())
	}
}

// TestEmitRejectsAnOversizeStatement drives the guard through the real
// chsql.Emit chokepoint rather than the helper, so the wiring — the ctx
// seeding, the emitter field, the call site — is proven, not just the
// arithmetic. A deliberately tiny override stands in for a 588KB composition
// so the test stays cheap; the real shape's rejection is pinned in
// internal/promql.
func TestEmitRejectsAnOversizeStatement(t *testing.T) {
	t.Parallel()

	plan := &chplan.Project{
		Input:       &chplan.Scan{Table: "otel_metrics_gauge"},
		Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"}},
	}

	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit under the default bound: %v", err)
	}
	if len(sql) == 0 {
		t.Fatal("Emit returned empty SQL")
	}

	// One byte under what this plan actually renders: the smallest override
	// that must reject it, so the assertion cannot pass on a guard that only
	// fires for absurd values.
	tight := WithMaxEmittedSQLBytes(context.Background(), int64(len(sql)-1))
	if _, _, err := Emit(tight, plan); !errors.Is(err, ErrEmittedSQLTooLarge) {
		t.Errorf("Emit with a %d-byte bound over %d bytes of SQL returned %v, want ErrEmittedSQLTooLarge",
			len(sql)-1, len(sql), err)
	}
	// Exactly what it renders: admitted, the inclusive-bound half of the pin.
	exact := WithMaxEmittedSQLBytes(context.Background(), int64(len(sql)))
	if _, _, err := Emit(exact, plan); err != nil {
		t.Errorf("Emit with a bound of exactly the rendered %d bytes returned %v, want success", len(sql), err)
	}
}
