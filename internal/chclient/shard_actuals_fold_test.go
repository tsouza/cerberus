package chclient

import (
	"context"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/actuals"
)

// simulateShardFlush mimics runShard's own per-shard sequence
// (internal/solver/executor.go): a SECOND WithProgressFor call on a
// descendant of outerCtx (the one WithShardActualsFold was wired onto),
// feeding it packet/ProfileEvents data, and flushing it exactly the way
// rowsCursor.Close does at end-of-shard.
func simulateShardFlush(outerCtx context.Context, rows, bytes, peakMemory uint64) {
	shardCtx := WithProgressFor(outerCtx, "promql")
	rec := recorderFromContext(shardCtx)
	rec.onProgress(&clickhouse.Progress{Rows: rows, Bytes: bytes})
	if peakMemory > 0 {
		rec.onProfileEvents([]clickhouse.ProfileEvent{
			{Name: profileEventMemoryTrackerPeakUsage, Value: int64(peakMemory)}, //nolint:gosec // G115 -- test fixture, always small positive values
		})
	}
	rec.flush()
}

// TestShardActualsFold_SumsRowsBytesMaxesPeakMemory pins the folding rule
// itself (issue #3033): ReadRows/ReadBytes SUM across shards, PeakMemory
// MAXES — and exactly ONE RecordActual reaches the tracker for the whole
// K-shard dispatch, not K.
func TestShardActualsFold_SumsRowsBytesMaxesPeakMemory(t *testing.T) {
	tracker := actuals.NewTracker(actuals.DefaultConfig())
	const shape = "cerb:agg;rw"
	tracker.RecordPredicted(shape, 1000)

	outer := WithProgressFor(context.Background(), "promql")
	outer = WithActualsCapture(outer, tracker, shape)
	outer = WithShardActualsFold(outer, 4)

	fold, ok := shardActualsFoldFromContext(outer)
	if !ok {
		t.Fatal("expected a fold to be wired for a 4-shard dispatch with actuals capture on")
	}

	// Each shard scanned a disjoint slice of the total; peak memory readings
	// are independent per-shard high-water marks.
	simulateShardFlush(outer, 250, 2500, 1_000)
	simulateShardFlush(outer, 300, 3000, 5_000) // the max
	simulateShardFlush(outer, 200, 2000, 2_000)
	simulateShardFlush(outer, 250, 2500, 3_000)

	if fold.rows != 1000 || fold.bytes != 10000 || fold.peakMemory != 5000 {
		t.Fatalf("expected SUM(rows)=1000 SUM(bytes)=10000 MAX(peakMemory)=5000, got rows=%d bytes=%d peakMemory=%d",
			fold.rows, fold.bytes, fold.peakMemory)
	}
	if fold.completed != 4 {
		t.Fatalf("expected all 4 shards to have folded in, got completed=%d", fold.completed)
	}

	report, ok := tracker.Snapshot(shape)
	if !ok {
		t.Fatal("expected the fold to have recorded exactly one actual into the tracker")
	}
	if report.Observations != 1 {
		t.Fatalf("expected exactly ONE RecordActual for the whole 4-shard dispatch, got Observations=%d", report.Observations)
	}
	if report.ActualEMARows != 1000 {
		t.Fatalf("expected the folded (summed) rows to reach the tracker, got ActualEMARows=%v", report.ActualEMARows)
	}
}

// TestShardActualsFold_KShardDispatchRecordsComparableActualsToSingleShard is
// the DoD test (issue #3033 / M4 exit criterion #1): dispatching the SAME
// logical plan once via a K-shard route-B fan-out and once via a
// single-shard dispatch must record COMPARABLE actuals — not off by a
// factor of K. Before the fold existed, each of the K shards' flush() called
// tracker.RecordActual directly with only ITS OWN fractional share, so the
// K-shard tracker's EMA converged toward roughly totalRows/K instead of
// totalRows; this test fails against that behavior and passes once shards
// fold into ONE observation.
func TestShardActualsFold_KShardDispatchRecordsComparableActualsToSingleShard(t *testing.T) {
	const totalRows, totalBytes, peakMemory = 1000, 10_000, 5_000
	const shapeSingle, shapeSharded = "cerb:single", "cerb:sharded"

	// Single-shard dispatch (route-A shape): one recorder sees the WHOLE
	// request in one flush, exactly as today.
	singleTracker := actuals.NewTracker(actuals.DefaultConfig())
	singleTracker.RecordPredicted(shapeSingle, totalRows)
	singleCtx := WithProgressFor(context.Background(), "promql")
	singleCtx = WithActualsCapture(singleCtx, singleTracker, shapeSingle)
	simulateShardFlush(singleCtx, totalRows, totalBytes, peakMemory)

	// K=4 route-B dispatch of the SAME total, split across 4 disjoint
	// shards (mirroring the executor's own shard-composition contract:
	// each shard scans a disjoint slice of the total).
	shardedTracker := actuals.NewTracker(actuals.DefaultConfig())
	shardedTracker.RecordPredicted(shapeSharded, totalRows)
	shardedOuter := WithProgressFor(context.Background(), "promql")
	shardedOuter = WithActualsCapture(shardedOuter, shardedTracker, shapeSharded)
	shardedOuter = WithShardActualsFold(shardedOuter, 4)

	shardRows := [4]uint64{250, 300, 200, 250}
	shardBytes := [4]uint64{2500, 3000, 2000, 2500}
	shardPeaks := [4]uint64{1000, 5000, 2000, 3000} // max is 5000, the peak the single dispatch itself saw
	for i := 0; i < 4; i++ {
		simulateShardFlush(shardedOuter, shardRows[i], shardBytes[i], shardPeaks[i])
	}

	singleReport, ok := singleTracker.Snapshot(shapeSingle)
	if !ok {
		t.Fatal("expected the single-shard dispatch to record an actual")
	}
	shardedReport, ok := shardedTracker.Snapshot(shapeSharded)
	if !ok {
		t.Fatal("expected the K-shard dispatch to record an actual")
	}

	if shardedReport.Observations != 1 {
		t.Fatalf("expected the K-shard dispatch to record exactly ONE observation, got %d (the un-folded bug records K)",
			shardedReport.Observations)
	}
	if singleReport.ActualEMARows != shardedReport.ActualEMARows {
		t.Fatalf("expected the K-shard dispatch's folded rows to be COMPARABLE to the single-shard dispatch's, got single=%v sharded=%v (ratio=%v)",
			singleReport.ActualEMARows, shardedReport.ActualEMARows, shardedReport.ActualEMARows/singleReport.ActualEMARows)
	}
}

// TestShardActualsFold_RouteAUnaffected pins that a plain route-A dispatch —
// no WithShardActualsFold call at all, exactly like every existing call site
// before issue #3033 — behaves byte-identically: flush() still calls
// tracker.RecordActual directly.
func TestShardActualsFold_RouteAUnaffected(t *testing.T) {
	tracker := actuals.NewTracker(actuals.DefaultConfig())
	const shape = "cerb:routeA"
	tracker.RecordPredicted(shape, 500)

	ctx := WithProgressFor(context.Background(), "promql")
	ctx = WithActualsCapture(ctx, tracker, shape)
	// No WithShardActualsFold call — route A never makes one.
	if _, ok := shardActualsFoldFromContext(ctx); ok {
		t.Fatal("expected no fold on a route-A ctx")
	}
	rec := recorderFromContext(ctx)
	rec.onProgress(&clickhouse.Progress{Rows: 500, Bytes: 5000})
	rec.flush()

	report, ok := tracker.Snapshot(shape)
	if !ok || report.ActualEMARows != 500 || report.Observations != 1 {
		t.Fatalf("expected route A's single flush to record directly into the tracker, got %+v (ok=%v)", report, ok)
	}
}

// TestWithShardActualsFold_NoOpWithoutActualsIntentOrSingleShard pins both
// no-op branches: no actualsIntent on ctx (feature off), and k < 2 (route A,
// or a degenerate single-shard route-B decision that already records
// correctly without folding).
func TestWithShardActualsFold_NoOpWithoutActualsIntentOrSingleShard(t *testing.T) {
	// No actuals capture at all.
	ctx := WithProgressFor(context.Background(), "promql")
	if got := WithShardActualsFold(ctx, 4); got != ctx {
		t.Fatal("expected WithShardActualsFold to no-op when actuals capture is off")
	}

	// Actuals capture on, but k < 2.
	tracker := actuals.NewTracker(actuals.DefaultConfig())
	ctx = WithActualsCapture(ctx, tracker, "cerb:agg;rw")
	if got := WithShardActualsFold(ctx, 1); got != ctx {
		t.Fatal("expected WithShardActualsFold to no-op for k < 2")
	}
	if _, ok := shardActualsFoldFromContext(WithShardActualsFold(ctx, 1)); ok {
		t.Fatal("expected no fold wired for k < 2")
	}
}
