package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

const (
	compareWindowRange  = time.Minute
	compareWindowOffset = 2 * time.Minute
	// rootSeedMarker is the `TraceId IN (<windowed cohort>)` seed the compare
	// emitter pushes onto the root-lookup scan. Its presence is what the guards
	// under test decide.
	rootSeedMarker = "`TraceId` IN (SELECT `TraceId` FROM"
)

var (
	compareWindowStart = time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	compareWindowEnd   = time.Date(2026, 5, 12, 10, 3, 0, 0, time.UTC)
)

// compareRangeWindow wraps the root-lookup compare node in a RangeWindow with
// the given request window and offset.
func compareRangeWindow(start, end time.Time, offset time.Duration) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Input:           compareNodeWithRoot(),
		Range:           compareWindowRange,
		Step:            time.Minute,
		Start:           start,
		End:             end,
		Offset:          offset,
		TimestampColumn: "Timestamp",
	}
}

// TestEmitCompare_PartialWindowEmitsNoScanBound pins that the compare scan
// bound requires BOTH ends of the request window. The bound the emitter pushes
// into each MergeTree leg is the half-open interval (Start-range, End]; with
// only one endpoint known the other side would have to be invented, and an
// invented endpoint on a scan bound silently drops real spans rather than
// merely widening the read. So a half-specified window gets no bound at all and
// the legs stay honest.
//
// Kills metrics_compare.go's `!r.Start.IsZero() && !r.End.IsZero()`
// INVERT_LOGICAL (`||`), which builds a bound from a single endpoint.
func TestEmitCompare_PartialWindowEmitsNoScanBound(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), compareRangeWindow(compareWindowStart, time.Time{}, 0))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(sql, "WHERE `Timestamp` >") {
		t.Errorf("a window with no End must not bound the span scan:\n%s", sql)
	}
	if strings.Contains(sql, rootSeedMarker) {
		t.Errorf("a window with no End must not seed the root-lookup scan:\n%s", sql)
	}
}

// TestEmitCompare_NoWindowEmitsNoRootSeed pins the other conjunct of the
// root-seed guard: the seed is derived from the scan bound, so with no window
// there is nothing to seed from. The mutated form reaches the seed builder with
// a nil bound and derives its interval from the ZERO time, producing a cohort
// subquery bounded to year 1754 — which matches no span and silently empties
// the root-name/root-service enrichment for every trace.
//
// Kills metrics_compare.go's `bound != nil && m.RootLookup != nil`
// INVERT_LOGICAL (`||`).
func TestEmitCompare_NoWindowEmitsNoRootSeed(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), compareRangeWindow(time.Time{}, time.Time{}, 0))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(sql, rootSeedMarker) {
		t.Errorf("an unbounded request window must not seed the root-lookup scan:\n%s", sql)
	}
}

// TestEmitCompare_RootSeedWindowIsOffsetShifted pins the arithmetic of the root
// seed's cohort window. The seed must cover the SAME interval the span leg's own
// scan bound uses — (Start - offset - range, End - offset] — because a trace
// whose only matching span falls in the anchor lookback slice would otherwise
// satisfy the span leg and miss the seed, silently losing its root enrichment.
// The offset shift subtracts: a query with `offset 2m` reads spans two minutes
// OLDER than the request window, so both endpoints move backwards.
//
// Kills the four mutants on the seed's bound expression — ARITHMETIC_BASE and
// INVERT_NEGATIVES on each of `- offsetNS - rangeNS` and `- offsetNS` — every
// one of which shifts the cohort window the wrong way or by the wrong amount.
// A non-zero Offset is what makes them visible: at offset 0 the sign of a zero
// term is unobservable.
func TestEmitCompare_RootSeedWindowIsOffsetShifted(t *testing.T) {
	t.Parallel()

	sql, args, err := chsql.Emit(context.Background(),
		compareRangeWindow(compareWindowStart, compareWindowEnd, compareWindowOffset))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, rootSeedMarker) {
		t.Fatalf("a fully-specified window must seed the root-lookup scan:\n%s", sql)
	}

	wantLo := compareWindowStart.Add(-compareWindowOffset - compareWindowRange).UnixNano()
	wantHi := compareWindowEnd.Add(-compareWindowOffset).UnixNano()
	if !containsInt64Arg(args, wantLo) {
		t.Errorf("root seed lower bound must be Start-offset-range (%d), args=%v", wantLo, args)
	}
	if !containsInt64Arg(args, wantHi) {
		t.Errorf("root seed upper bound must be End-offset (%d), args=%v", wantHi, args)
	}
}
