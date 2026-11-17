package traceql

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	tempoql "github.com/tsouza/cerberus/internal/traceql/ast"
)

// lowerWithSchema lowers query under ctx with schema s.
func lowerWithSchema(t *testing.T, ctx context.Context, query string, s schema.Traces) chplan.Node {
	t.Helper()
	expr, err := tempoql.Parse(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	plan, err := Lower(ctx, expr, s)
	if err != nil {
		t.Fatalf("lower %q: %v", query, err)
	}
	return plan
}

// TestDrainLowering pins the window and schema a WithSearchDrain lowering
// stamps: the request window widened by the maximum trace duration, with a
// zero maximum trace duration so per-trace subqueries read that window and no
// further — and the inputs unchanged without the drain context, without a
// window, or with a zero maximum trace duration.
func TestDrainLowering(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	lookback := s.MaxTraceDuration
	zero := s
	zero.MaxTraceDuration = 0
	drainCtx := WithSearchDrain(context.Background())
	for _, tc := range []struct {
		name               string
		ctx                context.Context
		start, end         time.Time
		s                  schema.Traces
		wantStart, wantEnd time.Time
		wantLookback       time.Duration
	}{
		{"no drain context", context.Background(), winStart, winEnd, s, winStart, winEnd, lookback},
		{"drain", drainCtx, winStart, winEnd, s, winStart.Add(-lookback), winEnd.Add(lookback), 0},
		{"drain, start only", drainCtx, winStart, time.Time{}, s, winStart.Add(-lookback), time.Time{}, 0},
		{"drain, no window", drainCtx, time.Time{}, time.Time{}, s, time.Time{}, time.Time{}, lookback},
		{"drain, zero maximum trace duration", drainCtx, winStart, winEnd, zero, winStart, winEnd, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			start, end, got := drainLowering(tc.ctx, tc.start, tc.end, tc.s)
			if !start.Equal(tc.wantStart) || !end.Equal(tc.wantEnd) {
				t.Errorf("window [%v, %v], want [%v, %v]", start, end, tc.wantStart, tc.wantEnd)
			}
			if got.MaxTraceDuration != tc.wantLookback {
				t.Errorf("MaxTraceDuration %v, want %v", got.MaxTraceDuration, tc.wantLookback)
			}
		})
	}
}

// TestSearchDrains pins when a windowed search reads its chosen traces over a
// wider window: a request window and a positive maximum trace duration.
func TestSearchDrains(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	zero := s
	zero.MaxTraceDuration = 0
	windowed := WithSearchWindow(context.Background(), winStart, winEnd)
	if !SearchDrains(windowed, s) {
		t.Error("windowed search with a maximum trace duration does not drain")
	}
	if SearchDrains(windowed, zero) {
		t.Error("windowed search with a zero maximum trace duration drains")
	}
	if SearchDrains(context.Background(), s) {
		t.Error("windowless search drains")
	}
}

// TestSearchDrainLoweringReadsTheDrainWindow pins a WithSearchDrain lowering
// of a spanset operation, as the search handler's phase B lowers it: every
// span row and every per-trace subquery reads the request window widened by
// the maximum trace duration — once, never widened again.
func TestSearchDrainLoweringReadsTheDrainWindow(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	ctx := WithSearchDrain(WithSearchWindow(WithSearchTraceLimit(context.Background(), 20), winStart, winEnd))
	plan := lowerWithSchema(t, ctx, `{ rootName = name || traceDuration > 2s } && { kind = server }`, s)
	var leaves []leafWindow
	collectLeafWindows(plan, false, s.TimestampColumn, &leaves)
	want := []int64{winStart.Add(-s.MaxTraceDuration).UnixNano(), winEnd.Add(s.MaxTraceDuration).UnixNano()}
	var perTrace, rows int
	for i, leaf := range leaves {
		if leaf.perTrace {
			perTrace++
		} else {
			rows++
		}
		if !slices.Equal(leaf.instants, want) {
			t.Errorf("leaf %d (per-trace %v) window instants %v, want %v", i, leaf.perTrace, leaf.instants, want)
		}
	}
	if perTrace == 0 || rows == 0 {
		t.Fatalf("%d per-trace and %d row leaves; the query must exercise both", perTrace, rows)
	}
}

// TestPlainSearchZeroLookbackIsTheRank pins the plain shape with a zero
// maximum trace duration: a single-input SearchTraceLimit, whose row source is
// the one the default setting ranks its traces over.
func TestPlainSearchZeroLookbackIsTheRank(t *testing.T) {
	t.Parallel()
	ctx := WithSearchWindow(WithSearchTraceLimit(context.Background(), 20), winStart, winEnd)
	const query = `{ resource.service.name = "a" && name = "b" }`
	zero := schema.DefaultOTelTraces()
	zero.MaxTraceDuration = 0
	single, ok := lowerWithSchema(t, ctx, query, zero).(*chplan.SearchTraceLimit)
	if !ok || single.Rank != nil {
		t.Fatalf("zero-lookback plain search = %#v, want a SearchTraceLimit without a Rank", single)
	}
	drained, ok := lowerWithSchema(t, ctx, query, schema.DefaultOTelTraces()).(*chplan.SearchTraceLimit)
	if !ok || drained.Rank == nil {
		t.Fatalf("plain search = %#v, want a SearchTraceLimit with a Rank", drained)
	}
	if !single.Input.Equal(drained.Rank) {
		t.Error("the zero-lookback row source differs from the row source the default setting ranks over")
	}
	if drained.Input.Equal(drained.Rank) {
		t.Error("the drain reads the same window as the rank")
	}
}
