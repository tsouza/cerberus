package optimizer_test

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// FilterRangeWindowTranspose's pattern captures the RangeWindow by kind
// alone — WithChildren checks the arity of the Filter it matched, not of
// the RangeWindow underneath it — so the rule sees every shape of
// RangeWindow, including the ones carrying an optional side-scan child.
// The tests below pin what the rule must do with each of those shapes.
//
// The window functions are `last_over_time`: downsampleTierMinSamples
// (internal/chsql/range_window_downsample_tier.go) accepts only irate,
// idelta and last_over_time, so anything else fails to emit and the
// wrong-answer assertion could not be made.

const (
	// tierTestRange and tierTestStep are a whole-bucket range and a grid
	// step for the synthetic tier plans below. Their exact values carry
	// no meaning beyond being a legal window.
	tierTestRange = 10 * time.Minute
	tierTestStep  = time.Minute
)

// downsampleTierRangeWindow builds `Filter(false, RangeWindow{...})`
// where the RangeWindow is in DownsampleTier mode: the emitter answers
// it from DownsampleTierInput and never reads Input.
//
// `Filter(false)` is not a contrivance — it is what `topk(0, …)` lowers
// to (internal/promql/lower.go's buildTopKLiteral), and it makes the
// wrong answer unambiguous: the correct result is the empty set.
func downsampleTierRangeWindow(downsampleTier bool) *chplan.Filter {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &chplan.Filter{
		Predicate: &chplan.LitBool{V: false},
		Input: &chplan.RangeWindow{
			Input:               &chplan.Scan{Table: "otel_metrics_gauge"},
			DownsampleTierInput: &chplan.Scan{Table: "otel_metrics_gauge_downsampled"},
			DownsampleTier:      downsampleTier,
			Func:                "last_over_time",
			Range:               tierTestRange,
			Step:                tierTestStep,
			Start:               start,
			End:                 start.Add(tierTestRange),
			TimestampColumn:     "TimeUnix",
			ValueColumn:         "Value",
			GroupBy:             []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		},
	}
}

// TestFilterRangeWindowTransposeDeclinesDownsampleTier pins the guard.
//
// Without it the rule fires, the predicate lands in RangeWindow.Input,
// and emitRangeWindow's DownsampleTier arm never reads Input — so the
// WHERE clause and its bound argument disappear from the SQL entirely
// and a query that must return nothing returns everything.
func TestFilterRangeWindowTransposeDeclinesDownsampleTier(t *testing.T) {
	t.Parallel()

	in := downsampleTierRangeWindow(true)
	// PatternRule.Apply hands back the node unchanged with ok=false when
	// the Transform declines, so the decline is the false, not a nil node.
	got, ok := optimizer.FilterRangeWindowTranspose().Apply(in)
	if ok {
		t.Fatalf("the rule transposed a Filter into a DownsampleTier RangeWindow's Input, "+
			"which the emitter never reads — the predicate is deleted, not pushed down.\n"+
			"got: %#v", got)
	}
	if got != chplan.Node(in) {
		t.Fatalf("a declining rule must return the node it was given, got %#v", got)
	}
}

// TestFilterRangeWindowTransposeDownsampleTierWouldDropThePredicate is
// the guard's justification, asserted rather than asserted-in-prose: it
// performs the transpose the rule now declines and shows the emitted SQL
// losing the predicate.
//
// Without this, TestFilterRangeWindowTransposeDeclinesDownsampleTier
// would pin a behaviour whose motivation lives only in a comment, and a
// later reader could "simplify" the guard away believing it cost
// nothing.
func TestFilterRangeWindowTransposeDownsampleTierWouldDropThePredicate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	held := downsampleTierRangeWindow(true)

	_, heldArgs, err := chsql.Emit(ctx, held)
	if err != nil {
		t.Fatalf("emit the un-transposed plan: %v", err)
	}
	if len(heldArgs) != 1 || heldArgs[0] != false {
		t.Fatalf("the un-transposed plan should bind the `false` predicate as a query argument; "+
			"args = %#v — the fixture no longer demonstrates what it claims", heldArgs)
	}

	// The rewrite the rule used to perform: predicate into Input, every
	// other field of the RangeWindow carried over unchanged.
	rw := held.Input.(*chplan.RangeWindow)
	pushed := *rw
	inner := *held
	inner.Input = rw.Input
	pushed.Input = &inner

	_, pushedArgs, err := chsql.Emit(ctx, &pushed)
	if err != nil {
		t.Fatalf("emit the transposed plan: %v", err)
	}
	if len(pushedArgs) != 0 {
		t.Fatalf("expected the transposed plan to have lost the predicate's bound argument, "+
			"demonstrating the wrong answer the guard prevents; args = %#v", pushedArgs)
	}
}

// TestFilterRangeWindowTransposeStillFiresWithDeltaPrefixSideScan pins
// the other half of the guard's doc: DeltaPrefixAggregateInput is NOT a
// reason to decline, because the emitter LEFT JOINs that side onto the
// Input-derived side keyed on the very group columns this rule restricts
// predicates to.
//
// Pinning it matters as much as the decline does. A future reader
// tempted to make the guard "safer" by declining on any populated
// side-scan would silently give up a sound pushdown, and nothing else in
// the suite would notice.
func TestFilterRangeWindowTransposeStillFiresWithDeltaPrefixSideScan(t *testing.T) {
	t.Parallel()

	in := downsampleTierRangeWindow(false)
	rw := in.Input.(*chplan.RangeWindow)
	rw.DownsampleTierInput = nil
	rw.DeltaPrefixAggregateInput = &chplan.Scan{Table: "otel_metrics_gauge_delta_prefix"}
	in.Predicate = &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "Attributes"},
		Right: &chplan.LitString{V: "api"},
	}

	got, ok := optimizer.FilterRangeWindowTranspose().Apply(in)
	if !ok || got == nil {
		t.Fatal("the rule declined a RangeWindow with a populated DeltaPrefixAggregateInput; " +
			"that side-scan joins from the filtered side and is safe to leave unfiltered")
	}

	out, isRangeWindow := got.(*chplan.RangeWindow)
	if !isRangeWindow {
		t.Fatalf("expected the transpose to hoist the RangeWindow, got %T", got)
	}
	if _, pushed := out.Input.(*chplan.Filter); !pushed {
		t.Fatalf("expected the Filter to end up in RangeWindow.Input, got %T", out.Input)
	}
	if out.DeltaPrefixAggregateInput == nil {
		t.Fatal("the transpose dropped DeltaPrefixAggregateInput; the shallow copy must carry " +
			"every arm of the node across")
	}
}
