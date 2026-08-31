package chclient

import (
	"context"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/actuals"
)

func TestProgressRecorder_OnProgressLatchesMax(t *testing.T) {
	rec := &progressRecorder{ql: "promql", ctx: context.Background()}
	rec.onProgress(&clickhouse.Progress{Rows: 10, Bytes: 100})
	rec.onProgress(&clickhouse.Progress{Rows: 5, Bytes: 50})
	rec.onProgress(&clickhouse.Progress{Rows: 20, Bytes: 200})
	if rec.rows != 20 || rec.bytes != 200 {
		t.Fatalf("expected the latched snapshot to be the max seen, got rows=%d bytes=%d", rec.rows, rec.bytes)
	}
	// A nil packet must never panic.
	rec.onProgress(nil)
}

func TestProgressRecorder_OnProfileEventsLatchesPeakMemory(t *testing.T) {
	rec := &progressRecorder{ql: "promql", ctx: context.Background()}
	rec.onProfileEvents([]clickhouse.ProfileEvent{
		{Name: "Query", Value: 1},
		{Name: profileEventMemoryTrackerPeakUsage, Value: 100},
	})
	rec.onProfileEvents([]clickhouse.ProfileEvent{
		{Name: profileEventMemoryTrackerPeakUsage, Value: 50},
	})
	rec.onProfileEvents([]clickhouse.ProfileEvent{
		{Name: profileEventMemoryTrackerPeakUsage, Value: 301166},
	})
	if rec.peakMemory != 301166 {
		t.Fatalf("expected the latched peak memory to be the max seen, got %d", rec.peakMemory)
	}
	// A negative counter value (never seen in practice, per the driver's
	// own encoding, but defensively guarded) must never underflow into a
	// huge uint64.
	rec.onProfileEvents([]clickhouse.ProfileEvent{
		{Name: profileEventMemoryTrackerPeakUsage, Value: -1},
	})
	if rec.peakMemory != 301166 {
		t.Fatalf("a negative ProfileEvent value must be ignored, got %d", rec.peakMemory)
	}
}

// TestWithActualsCapture_StoresIntentEvenWithoutAnExistingRecorder pins the
// (unusual, but correct) ordering where WithActualsCapture is called BEFORE
// any WithProgressFor: there is no recorder yet to wire directly, but the
// intent must still be stored so the NEXT WithProgressFor call picks it up
// — see WithProgressFor's own doc for why this is a separate, non-recorder-
// bound context value. Every real call site in this codebase calls
// WithProgressFor first (this ordering never occurs in production), but the
// function must not assume that.
func TestWithActualsCapture_StoresIntentEvenWithoutAnExistingRecorder(t *testing.T) {
	tracker := actuals.NewTracker(actuals.DefaultConfig())
	const shape = "cerb:agg;rw"

	ctx := WithActualsCapture(context.Background(), tracker, shape) // no WithProgressFor yet
	if recorderFromContext(ctx) != nil {
		t.Fatal("expected no recorder to exist yet")
	}

	// A LATER WithProgressFor call must still pick up the intent.
	ctx = WithProgressFor(ctx, "promql")
	rec := recorderFromContext(ctx)
	if rec == nil || rec.shapeID != shape || rec.tracker != tracker {
		t.Fatalf("expected the later recorder to inherit the stored intent, got %+v", rec)
	}
}

func TestWithActualsCapture_NoOpWithNilTrackerOrEmptyShapeID(t *testing.T) {
	ctx := WithProgressFor(context.Background(), "promql")
	if got := WithActualsCapture(ctx, nil, "cerb:agg;rw"); got != ctx {
		t.Fatal("WithActualsCapture must be a no-op with a nil tracker")
	}
	tracker := actuals.NewTracker(actuals.DefaultConfig())
	if got := WithActualsCapture(ctx, tracker, ""); got != ctx {
		t.Fatal("WithActualsCapture must be a no-op with an empty shapeID")
	}
}

func TestWithActualsCapture_WiresRecorderAndFlushRecordsIntoTracker(t *testing.T) {
	tracker := actuals.NewTracker(actuals.DefaultConfig())
	const shape = "cerb:agg;rw"
	tracker.RecordPredicted(shape, 1000)

	ctx := WithProgressFor(context.Background(), "promql")
	ctx = WithActualsCapture(ctx, tracker, shape)

	rec := recorderFromContext(ctx)
	if rec == nil {
		t.Fatal("expected a recorder on ctx")
	}
	if rec.shapeID != shape || rec.tracker != tracker {
		t.Fatalf("expected WithActualsCapture to wire shapeID/tracker onto the SAME recorder, got shapeID=%q tracker=%v", rec.shapeID, rec.tracker)
	}

	rec.onProgress(&clickhouse.Progress{Rows: 900, Bytes: 9000})
	rec.onProfileEvents([]clickhouse.ProfileEvent{{Name: profileEventMemoryTrackerPeakUsage, Value: 12345}})
	rec.flush()

	report, ok := tracker.Snapshot(shape)
	if !ok {
		t.Fatal("expected flush to record an actual into the tracker")
	}
	if report.ActualEMARows != 900 || report.Observations != 1 {
		t.Fatalf("unexpected recorded actual: %+v", report)
	}
}

// TestWithProgressFor_SecondCallInheritsActualsIntent pins the route-B fix:
// internal/solver/executor.go's runShard calls WithProgressFor a SECOND
// time, on a descendant of the ctx routeBExecCtx already ran
// WithActualsCapture over — a fresh recorder must still inherit the
// (tracker, shapeID) pair, or the packet fast path silently does nothing
// for every route-B shard.
func TestWithProgressFor_SecondCallInheritsActualsIntent(t *testing.T) {
	tracker := actuals.NewTracker(actuals.DefaultConfig())
	const shape = "cerb:agg;rw"

	outer := WithProgressFor(context.Background(), "promql")
	outer = WithActualsCapture(outer, tracker, shape)
	outerRec := recorderFromContext(outer)

	// Simulate runShard's own second WithProgressFor call on a descendant
	// ctx — a NEW recorder, but the SAME intent.
	shardCtx := WithProgressFor(outer, "promql")
	shardRec := recorderFromContext(shardCtx)

	if shardRec == outerRec {
		t.Fatal("expected a FRESH recorder per WithProgressFor call (sharing one would corrupt concurrent shards' histograms)")
	}
	if shardRec.shapeID != shape || shardRec.tracker != tracker {
		t.Fatalf("expected the shard's fresh recorder to inherit the actuals intent, got shapeID=%q tracker=%v", shardRec.shapeID, shardRec.tracker)
	}

	// And it actually records into the tracker on flush.
	shardRec.onProgress(&clickhouse.Progress{Rows: 111, Bytes: 222})
	shardRec.flush()
	report, ok := tracker.Snapshot(shape)
	if !ok || report.ActualEMARows != 111 {
		t.Fatalf("expected the shard's flush to record into the tracker, got %+v (ok=%v)", report, ok)
	}
}

func TestWithProgressFor_NoIntentLeavesFreshRecorderInert(t *testing.T) {
	// A plain WithProgressFor call with no preceding WithActualsCapture
	// (every existing call site, and every dispatch with the feature off)
	// must produce a recorder with no ProfileEvents registration at all —
	// the kill-switch-off byte-unchanged contract.
	ctx := WithProgressFor(context.Background(), "promql")
	rec := recorderFromContext(ctx)
	if rec.shapeID != "" || rec.tracker != nil {
		t.Fatalf("expected an inert recorder with no prior actuals intent, got shapeID=%q tracker=%v", rec.shapeID, rec.tracker)
	}
}

func TestProgressRecorder_FlushWithoutActualsCaptureIsANoOpOnTracker(t *testing.T) {
	// flush() on a recorder that never had WithActualsCapture applied must
	// not touch any tracker — pins the kill-switch-off byte-unchanged
	// contract for a dispatch that never opted in.
	rec := &progressRecorder{ql: "promql", ctx: context.Background(), rows: 42, bytes: 4200}
	rec.flush() // must not panic even though rec.tracker is nil
}

func TestProgressRecorder_FlushNilReceiverIsSafe(t *testing.T) {
	var rec *progressRecorder
	rec.flush() // must not panic
}
