package optcorpus

import (
	"context"
	"sort"
	"strconv"
	"testing"
)

// observeRouted calls ObserveRoutedQuery with the routing read-out a real
// routed dispatch always carries (route B, reason "routed"), keeping the call
// sites below readable.
func observeRouted(r *Reconciler, shardIDs []string, shapeID string, kShards uint8) {
	r.ObserveRoutedQuery(shardIDs, shapeID, nil, "promql", true, "B", 240, 4, 900, 3600, 15, kShards, "routed")
}

// TestObserveRoutedQuery_FoldsShardRowsIntoOneRow pins the whole point of the
// routed seam: a route-B dispatch is ONE corpus row whose cost columns describe
// the whole fan-out, not K rows each describing a fraction of it.
//
// Revert the seam (or fold the shards per-row instead of per-request) and this
// fails two ways at once: the row count stops being 1, and read_rows stops
// being the sum of what the K shards actually read — which is exactly the
// comparison against a route-A row the corpus exists to make.
func TestObserveRoutedQuery_FoldsShardRowsIntoOneRow(t *testing.T) {
	src := newFakeSource()
	sink := &memSink{}
	r := New(src, sink, Options{RingCapacity: 8, ObserveBuffer: 16})

	ids := []string{"trace-a-1", "trace-a-2", "trace-a-3"}
	src.seed(SourceRow{
		QueryID: ids[0], NormalizedQueryHash: 99,
		ReadRows: 100, ReadBytes: 1_000, MemoryUsage: 10, QueryDurationMS: 30,
		ProfileEvents: map[string]int64{"SelectedMarks": 2},
	})
	src.seed(SourceRow{
		QueryID: ids[1], NormalizedQueryHash: 99,
		ReadRows: 200, ReadBytes: 2_000, MemoryUsage: 20, QueryDurationMS: 70,
		ProfileEvents: map[string]int64{"SelectedMarks": 3},
	})
	src.seed(SourceRow{
		QueryID: ids[2], NormalizedQueryHash: 99,
		ReadRows: 300, ReadBytes: 3_000, MemoryUsage: 30, QueryDurationMS: 50,
		ProfileEvents: map[string]int64{"SelectedRanges": 5},
	})

	observeRouted(r, ids, "cerb:rangewindow", uint8(len(ids)))
	r.drainIngest()
	r.reconcileOnce(context.Background())

	rows := sink.snapshot()
	if len(rows) != 1 {
		t.Fatalf("routed dispatch wrote %d rows; want exactly 1 (the corpus unit is the request)", len(rows))
	}
	got := rows[0]
	if got.ReadRows != 600 {
		t.Errorf("read_rows = %d; want 600 (sum across shards)", got.ReadRows)
	}
	if got.ReadBytes != 6_000 {
		t.Errorf("read_bytes = %d; want 6000 (sum across shards)", got.ReadBytes)
	}
	if got.MemoryUsage != 60 {
		t.Errorf("memory_usage = %d; want 60 (sum of the concurrent shards' peaks)", got.MemoryUsage)
	}
	if got.QueryDurationMS != 70 {
		t.Errorf("query_duration_ms = %d; want 70 (max: the shards run concurrently)", got.QueryDurationMS)
	}
	if got.NormalizedQueryHash != 99 {
		t.Errorf("normalized_query_hash = %d; want 99", got.NormalizedQueryHash)
	}
	if got.ProfileEvents["SelectedMarks"] != 5 || got.ProfileEvents["SelectedRanges"] != 5 {
		t.Errorf("profile_events = %v; want SelectedMarks=5 (2+3) and SelectedRanges=5", got.ProfileEvents)
	}
	if got.Route != "B" || got.DecisionReason != "routed" || got.KShards != uint8(len(ids)) {
		t.Errorf("routing read-out = (route=%q reason=%q k=%d); want (B, routed, %d)",
			got.Route, got.DecisionReason, got.KShards, len(ids))
	}
	if got.ShapeID != "cerb:rangewindow" {
		t.Errorf("shape_id = %q; want cerb:rangewindow (the WHOLE plan's shape, so A and B rows join)", got.ShapeID)
	}

	// Every shard id is retired from the join index once the row is written, so
	// the next interval cannot emit a second row for the same dispatch.
	if n := len(r.snapshotIDs()); n != 0 {
		t.Errorf("join index still holds %d ids after the routed row was written; want 0", n)
	}
}

// TestObserveRoutedQuery_WaitsForEveryShard pins the partial-join rule: a
// routed row is written only once every shard's query_log row is visible.
// Reverting it produces a row whose cost columns count part of the fan-out —
// an under-counted route-B row, which is worse than a missing one because the
// calibration cannot tell it apart from a genuinely cheap dispatch.
func TestObserveRoutedQuery_WaitsForEveryShard(t *testing.T) {
	src := newFakeSource()
	sink := &memSink{}
	r := New(src, sink, Options{RingCapacity: 8, ObserveBuffer: 16})

	ids := []string{"trace-b-1", "trace-b-2", "trace-b-3"}
	src.seed(SourceRow{QueryID: ids[0], ReadRows: 100})
	src.seed(SourceRow{QueryID: ids[1], ReadRows: 200})

	observeRouted(r, ids, "cerb:rangewindow", uint8(len(ids)))
	r.drainIngest()
	r.reconcileOnce(context.Background())

	if rows := sink.snapshot(); len(rows) != 0 {
		t.Fatalf("wrote %d rows with a shard still missing from query_log; want 0 (wait, do not under-count)", len(rows))
	}
	if n := len(r.snapshotIDs()); n != len(ids) {
		t.Fatalf("join index holds %d ids after the deferred reconcile; want %d (all retried next interval)", n, len(ids))
	}

	// The last shard lands: now the whole fan-out reconciles, in one row.
	src.seed(SourceRow{QueryID: ids[2], ReadRows: 300})
	r.reconcileOnce(context.Background())

	rows := sink.snapshot()
	if len(rows) != 1 {
		t.Fatalf("wrote %d rows once every shard landed; want 1", len(rows))
	}
	if rows[0].ReadRows != 600 {
		t.Errorf("read_rows = %d; want 600 (all three shards)", rows[0].ReadRows)
	}
}

// TestObserveRoutedQuery_MostSevereShardExitWins pins the exit-status fold: a
// fan-out that OOMed one shard is an OOM for the request, even though its
// siblings finished and the cancelled ones report an abort. Reverting it (say,
// to "first shard wins") reports the request as clean and silently removes it
// from the population the memory watermarks are learnt from.
func TestObserveRoutedQuery_MostSevereShardExitWins(t *testing.T) {
	src := newFakeSource()
	sink := &memSink{}
	r := New(src, sink, Options{RingCapacity: 8, ObserveBuffer: 16})

	ids := []string{"trace-c-1", "trace-c-2", "trace-c-3"}
	src.seed(SourceRow{QueryID: ids[0], ExitStatus: ExitOK})
	src.seed(SourceRow{QueryID: ids[1], ExitStatus: ExitAborted}) // cancelled by its sibling's failure
	src.seed(SourceRow{QueryID: ids[2], ExitStatus: ExitOOM})

	observeRouted(r, ids, "cerb:rangewindow", uint8(len(ids)))
	r.drainIngest()
	r.reconcileOnce(context.Background())

	rows := sink.snapshot()
	if len(rows) != 1 {
		t.Fatalf("wrote %d rows; want 1", len(rows))
	}
	if rows[0].ExitStatus != ExitOOM.String() {
		t.Errorf("exit_status = %q; want %q (the most severe shard outcome)", rows[0].ExitStatus, ExitOOM.String())
	}
}

// TestExitSeverityRanksEveryExitStatus pins the severity ranking against the
// enum it ranks. A new ExitStatus member that never reaches exitSeverity would
// otherwise fold as "more severe than everything" forever, silently relabelling
// whole fan-outs; this test turns that into a compile-time-adjacent failure.
func TestExitSeverityRanksEveryExitStatus(t *testing.T) {
	if len(exitSeverity) != len(exitStatuses) {
		t.Fatalf("exitSeverity ranks %d statuses; exitStatuses has %d", len(exitSeverity), len(exitStatuses))
	}
	ranked := make(map[ExitStatus]bool, len(exitSeverity))
	for _, s := range exitSeverity {
		if ranked[s] {
			t.Errorf("exitSeverity lists %q twice", s)
		}
		ranked[s] = true
	}
	for _, s := range exitStatuses {
		if !ranked[s] {
			t.Errorf("exitSeverity is missing %q — it would outrank every ranked status", s)
		}
	}
}

// TestObserveRoutedQuery_NonBlocking_DropsWhenBufferFull pins the seam's
// data-plane contract, identical to ObserveQuery's: recording a routed dispatch
// must never block the query that is being recorded. If it blocked, this test
// would hang rather than fail.
func TestObserveRoutedQuery_NonBlocking_DropsWhenBufferFull(t *testing.T) {
	r := New(newFakeSource(), &memSink{}, Options{RingCapacity: 8, ObserveBuffer: 2})
	for i := 0; i < 1000; i++ {
		observeRouted(r, []string{"q" + strconv.Itoa(i) + "-a", "q" + strconv.Itoa(i) + "-b"}, "cerb:scan", 2)
	}
	if n := len(r.snapshotIDs()); n != 0 {
		t.Errorf("ring populated without a drain; got %d", n)
	}
	if r.dropped.Load() == 0 {
		t.Error("expected dropped count > 0 when the ingest buffer overflowed")
	}
}

// TestObserveRoutedQuery_EmptyShardIDsRecordNothing pins the untraced-request
// contract: the shard ids are minted from the request's trace, so a dispatch
// with no trace context yields empty ids. Those cannot join to anything, and a
// record with no join key would sit in the ring until the TTL evicted it.
func TestObserveRoutedQuery_EmptyShardIDsRecordNothing(t *testing.T) {
	r := New(newFakeSource(), &memSink{}, Options{RingCapacity: 8, ObserveBuffer: 16})
	observeRouted(r, []string{"", ""}, "cerb:scan", 2)
	r.drainIngest()
	if n := len(r.snapshotIDs()); n != 0 {
		t.Errorf("ring holds %d records for an untraced routed dispatch; want 0", n)
	}
}

// TestObserveRoutedQuery_AllShardsIndexOneRecord pins the join-index shape the
// fold depends on: every shard id must resolve to the SAME ring slot, so K
// query_log rows reconcile into one record instead of K competing ones.
func TestObserveRoutedQuery_AllShardsIndexOneRecord(t *testing.T) {
	r := New(newFakeSource(), &memSink{}, Options{RingCapacity: 8, ObserveBuffer: 16})
	ids := []string{"trace-d-1", "trace-d-2", "trace-d-3"}
	observeRouted(r, ids, "cerb:rangewindow", uint8(len(ids)))
	r.drainIngest()

	slots := map[int]struct{}{}
	for _, id := range ids {
		rec, slot, ok := r.recordFor(id)
		if !ok {
			t.Fatalf("shard id %q does not resolve to a record", id)
		}
		if rec.ShapeID != "cerb:rangewindow" {
			t.Errorf("shard id %q resolves to shape %q; want cerb:rangewindow", id, rec.ShapeID)
		}
		slots[slot] = struct{}{}
	}
	if len(slots) != 1 {
		t.Errorf("the %d shard ids resolve to %d ring slots; want 1", len(ids), len(slots))
	}

	got := r.snapshotIDs()
	sort.Strings(got)
	if len(got) != len(ids) {
		t.Fatalf("join index holds %v; want all %d shard ids", got, len(ids))
	}
}

// TestObserveRoutedQuery_RingEvictionDropsEveryShardID pins the eviction half of
// the same shape: when a routed record's slot is reused, ALL K of its ids must
// leave the index. A leftover id would later resolve to whatever record now
// occupies the slot and contaminate that dispatch's row.
func TestObserveRoutedQuery_RingEvictionDropsEveryShardID(t *testing.T) {
	const ringCap = 2
	r := New(newFakeSource(), &memSink{}, Options{RingCapacity: ringCap, ObserveBuffer: 16})
	routedIDs := []string{"trace-e-1", "trace-e-2", "trace-e-3"}
	observeRouted(r, routedIDs, "cerb:routed", uint8(len(routedIDs)))
	r.drainIngest()

	// Fill the ring past the routed record's slot.
	for i := 0; i < ringCap; i++ {
		observeNoRoute(r, "single-"+strconv.Itoa(i), "cerb:scan", nil, "promql")
	}
	r.drainIngest()

	for _, id := range routedIDs {
		if _, _, ok := r.recordFor(id); ok {
			t.Errorf("shard id %q still resolves after its record was evicted", id)
		}
	}
}
