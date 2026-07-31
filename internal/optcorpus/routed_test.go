package optcorpus

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"testing"
	"time"
)

// errSinkRefused is the sink-side failure the reconciler's retry contract is
// defined against: the rows are kept and re-delivered on the next interval.
var errSinkRefused = errors.New("optcorpus test: sink refused the write")

// shardIDs mints n distinct shard query_ids under prefix, the shape a routed
// dispatch derives from its request's trace context.
func shardIDs(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = prefix + "-" + strconv.Itoa(i)
	}
	return out
}

// observeRoutedAtP calls ObserveRoutedQuery with the routing read-out a real
// routed dispatch always carries (route B, reason "routed") at an explicit
// effective shard concurrency, keeping the call sites below readable. Every
// test that asserts on the folded cost columns or on the stored parallelism
// column spells P here rather than taking the shorthand, because a
// helper-supplied constant would let such an assertion pass without the value
// ever crossing the seam.
func observeRoutedAtP(r *Reconciler, shardIDs []string, parallelism int, shapeID string, kShards uint8) {
	r.ObserveRoutedQuery(shardIDs, parallelism, shapeID, nil, "promql", true, "B", 240, 4, 900, 3600, 15, kShards, "routed")
}

// observeRouted is observeRoutedAtP for the fully parallel fan-out, where every
// shard overlaps every other and both schedule-dependent folds collapse to the
// plain sum/max.
func observeRouted(r *Reconciler, shardIDs []string, shapeID string, kShards uint8) {
	observeRoutedAtP(r, shardIDs, len(shardIDs), shapeID, kShards)
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
		t.Errorf("memory_usage = %d; want 60 (all three peaks coexist at P=3)", got.MemoryUsage)
	}
	if got.QueryDurationMS != 70 {
		t.Errorf("query_duration_ms = %d; want 70 (150ms of work on 3 workers cannot beat the slowest shard)", got.QueryDurationMS)
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

// TestObserveRoutedQuery_ClampedConcurrencyFoldsTheWholeFanOut pins the fold
// against the shape the Executor actually runs: K shards at an effective
// concurrency P that is routinely BELOW K (the configured P, further clamped by
// the admission top-up and the shard gate, down to sequential). Only P peaks
// coexist and only P durations overlap, so the wall-clock and memory columns
// are not the plain max and sum.
//
// Drop Record.Parallelism and fold as if the fan-out were fully parallel and
// this fails on both columns at once: the three shards below take 150ms of
// ClickHouse time that one worker cannot compress below 150ms, but a plain max
// reports 70 — a route-B latency understated by more than half against the
// route-A row it is compared with — while a plain sum reports memory three
// shards never held at the same time.
func TestObserveRoutedQuery_ClampedConcurrencyFoldsTheWholeFanOut(t *testing.T) {
	type want struct {
		memory   uint64
		duration uint64
	}
	// 150ms of work and peaks of 10/20/30 across three shards. P=1 serialises
	// them (sum the durations, only the largest peak is ever held); P=2 runs two
	// waves (75ms beats the 70ms slowest shard, and the two largest peaks
	// coexist); P=3 is the fully parallel case.
	cases := map[int]want{
		1: {memory: 30, duration: 150},
		2: {memory: 50, duration: 75},
		3: {memory: 60, duration: 70},
	}
	for p, w := range cases {
		t.Run("P="+strconv.Itoa(p), func(t *testing.T) {
			src := newFakeSource()
			sink := &memSink{}
			r := New(src, sink, Options{RingCapacity: 8, ObserveBuffer: 16})

			ids := []string{
				"trace-p" + strconv.Itoa(p) + "-1",
				"trace-p" + strconv.Itoa(p) + "-2",
				"trace-p" + strconv.Itoa(p) + "-3",
			}
			src.seed(SourceRow{QueryID: ids[0], ReadRows: 100, MemoryUsage: 10, QueryDurationMS: 30})
			src.seed(SourceRow{QueryID: ids[1], ReadRows: 200, MemoryUsage: 20, QueryDurationMS: 70})
			src.seed(SourceRow{QueryID: ids[2], ReadRows: 300, MemoryUsage: 30, QueryDurationMS: 50})

			observeRoutedAtP(r, ids, p, "cerb:rangewindow", uint8(len(ids)))
			r.drainIngest()
			r.reconcileOnce(context.Background())

			rows := sink.snapshot()
			if len(rows) != 1 {
				t.Fatalf("routed dispatch wrote %d rows; want exactly 1", len(rows))
			}
			got := rows[0]
			if got.MemoryUsage != w.memory {
				t.Errorf("memory_usage = %d; want %d (the %d largest peaks, the most that coexist)", got.MemoryUsage, w.memory, p)
			}
			if got.QueryDurationMS != w.duration {
				t.Errorf("query_duration_ms = %d; want %d (150ms of work on %d workers, no faster than the slowest shard)",
					got.QueryDurationMS, w.duration, p)
			}
			// The work columns are concurrency-independent: the fan-out reads
			// what it reads however it is scheduled.
			if got.ReadRows != 600 {
				t.Errorf("read_rows = %d; want 600 regardless of concurrency", got.ReadRows)
			}
		})
	}
}

// TestObserveRoutedQuery_WaitsForEveryShard pins the deferred-write rule: a
// COMPLETE routed row is written only once every shard's query_log row is
// visible. Writing early would produce a row whose cost columns count part of
// the fan-out while still claiming shards_observed == k_shards — an
// under-counted route-B row the calibration cannot tell apart from a genuinely
// cheap dispatch.
//
// Waiting is not the same as forgetting: the shards that DID land are folded
// into the parked accumulator and their ids leave the join index immediately, so
// the next interval's IN(...) asks only about the ones still missing. The
// partial-harvest tests below cover what happens when they never arrive.
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
	// Only the shard that has not landed is still asked about: re-reading the two
	// that already folded would either double-count them or force the fold to be
	// rebuilt from scratch every interval.
	got := r.snapshotIDs()
	if len(got) != 1 || got[0] != ids[2] {
		t.Fatalf("join index holds %v after the deferred reconcile; want only the un-landed shard %q", got, ids[2])
	}

	// The last shard lands: now the whole fan-out reconciles, in one row, whose
	// cost columns still carry the two shards folded an interval earlier.
	src.seed(SourceRow{QueryID: ids[2], ReadRows: 300})
	r.reconcileOnce(context.Background())

	rows := sink.snapshot()
	if len(rows) != 1 {
		t.Fatalf("wrote %d rows once every shard landed; want 1", len(rows))
	}
	if rows[0].ReadRows != 600 {
		t.Errorf("read_rows = %d; want 600 (all three shards, across two intervals)", rows[0].ReadRows)
	}
	if rows[0].ShardsObserved != uint8(len(ids)) {
		t.Errorf("shards_observed = %d; want %d (the fan-out completed)", rows[0].ShardsObserved, len(ids))
	}
	if n := len(r.snapshotIDs()); n != 0 {
		t.Errorf("join index still holds %d ids after the completed row was written; want 0", n)
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

// TestExitSeverityOrderIsPinned pins the RANK of every status, not merely that
// each is ranked somewhere. The fold keeps the most severe shard status, so the
// order IS the behaviour: swap ExitOOM below ExitAborted and every OOMed fan-out
// files as an abort, emptying the population the memory watermarks are learnt
// from — while a membership-only assertion stays green throughout.
//
// The order is restated here as a literal rather than derived from exitSeverity,
// because a test that reads the ordering off the thing it is checking pins
// nothing. Each neighbouring pair carries the reason it sits where it does:
//
// The three load-bearing boundaries, in the order exitSeverity's own comment
// justifies them:
//
//   - ok < aborted: a shard cancelled by its sibling's failure did terminate
//     abnormally, so it must outrank a clean finish — but only barely, since the
//     abort is the CONSEQUENCE of another shard's cause and must never mask it.
//   - aborted < every cerberus-side refusal (sample_budget, rejected, breaker):
//     a real refusal must survive the fold against a cascade of cancellations.
//     Their order among themselves is fixed here so a reshuffle is a decision
//     rather than a drift; no fold outcome depends on which of the three wins,
//     because a fan-out refused by one of them is refused as a whole request.
//   - every refusal < error < timeout < oom: the ClickHouse-side verdicts rank
//     highest, and the two resource ones highest of all. They are what the
//     calibration learns its watermarks from, so losing one to a sibling's
//     plain error would silently shrink that population.
func TestExitSeverityOrderIsPinned(t *testing.T) {
	want := []ExitStatus{
		ExitOK,
		ExitAborted,
		ExitSampleBudget,
		ExitRejected,
		ExitBreaker,
		ExitError,
		ExitTimeout,
		ExitOOM,
	}
	if len(exitSeverity) != len(want) {
		t.Fatalf("exitSeverity has %d entries; the pinned order has %d", len(exitSeverity), len(want))
	}
	for rank, s := range want {
		if got := exitSeverityOf(s); got != rank {
			t.Errorf("exitSeverityOf(%q) = %d; want rank %d", s, got, rank)
		}
	}
	// The ranks are what the fold compares, so assert the comparison itself is
	// strictly increasing across the pinned order — a duplicated rank would let
	// whichever shard the join happened to visit first win the tie.
	for i := 1; i < len(want); i++ {
		if exitSeverityOf(want[i-1]) >= exitSeverityOf(want[i]) {
			t.Errorf("%q must rank strictly below %q", want[i-1], want[i])
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

// TestObserveRoutedQuery_PartialFanOutIsPublishedOnTTLExpiry pins the rule that
// a fan-out only PART of which reached query_log is reconciled from the shards
// that DID land, marked partial, rather than dropped whole.
//
// This is not a rare edge. The executor runs the K shards under an errgroup with
// first-error-wins cancellation and a residency cap of Parallelism, so a failure
// in the first wave cancels the siblings and the remaining waves are never
// dispatched at all — no ClickHouse query, therefore no query_log row, ever. A
// reconciler that waits for all K waits forever, and the record leaves through
// the TTL with nothing written.
//
// The rows lost that way are the expensive ones. Route B's failures are exactly
// the population docs/router-calibration.sql learns its memory and duration
// watermarks from, so silently dropping them teaches the calibration that route
// B never fails.
func TestObserveRoutedQuery_PartialFanOutIsPublishedOnTTLExpiry(t *testing.T) {
	const (
		kShards      = 8 // the solver's structural fan-out
		parallelism  = 3 // the executor's concurrent-shard cap: one wave of 3
		landedShards = 3 // the first wave; shards 4..8 were never dispatched
		ttl          = time.Hour
	)

	src := newFakeSource()
	sink := &memSink{}
	r := New(src, sink, Options{RingCapacity: 8, ObserveBuffer: 16, TTL: ttl})
	clock := time.Unix(1_700_000_000, 0).UTC()
	r.now = func() time.Time { return clock }

	ids := shardIDs("trace-partial", kShards)
	src.seed(SourceRow{
		QueryID: ids[0], NormalizedQueryHash: 77,
		ReadRows: 100, ReadBytes: 1_000, MemoryUsage: 10, QueryDurationMS: 40,
		ExitStatus: ExitOK,
	})
	src.seed(SourceRow{
		QueryID: ids[1], NormalizedQueryHash: 77,
		ReadRows: 200, ReadBytes: 2_000, MemoryUsage: 20, QueryDurationMS: 90,
		ExitStatus: ExitOOM, // the shard that killed the fan-out
	})
	src.seed(SourceRow{
		QueryID: ids[2], NormalizedQueryHash: 77,
		ReadRows: 300, ReadBytes: 3_000, MemoryUsage: 30, QueryDurationMS: 60,
		ExitStatus: ExitAborted, // cancelled by its sibling's failure
	})

	observeRoutedAtP(r, ids, parallelism, "cerb:rangewindow", kShards)
	r.drainIngest()
	r.reconcileOnce(context.Background())

	// Nothing yet: the un-dispatched shards are still inside the lookback window,
	// so the reconciler cannot yet distinguish "never ran" from "not visible yet".
	if rows := sink.snapshot(); len(rows) != 0 {
		t.Fatalf("wrote %d rows while the missing shards were still inside the lookback window; want 0", len(rows))
	}

	// Past the TTL a missing shard can no longer match any row the source can
	// still see, so the dispatch is as finished as the corpus will ever know.
	clock = clock.Add(ttl + 30*time.Minute)
	r.reconcileOnce(context.Background())

	rows := sink.snapshot()
	if len(rows) != 1 {
		t.Fatalf("wrote %d rows for the expired partial fan-out; want exactly 1 (reconcile what landed, do not drop the record)", len(rows))
	}
	got := rows[0]
	if got.ShardsObserved != landedShards {
		t.Errorf("shards_observed = %d; want %d — the row must declare how much of the fan-out it describes",
			got.ShardsObserved, landedShards)
	}
	if got.KShards != kShards {
		t.Errorf("k_shards = %d; want %d — the DISPATCHED width, not the observed one", got.KShards, kShards)
	}
	if got.Parallelism != parallelism {
		t.Errorf("parallelism = %d; want %d", got.Parallelism, parallelism)
	}
	if got.ReadRows != 600 || got.ReadBytes != 6_000 {
		t.Errorf("read_rows/read_bytes = %d/%d; want 600/6000 (the three shards that ran)", got.ReadRows, got.ReadBytes)
	}
	// One wave of P=3 landed, so the concurrency the fold applies is 3 and both
	// schedule-dependent columns collapse to their fully-parallel readings over
	// the shards that ran: the sum of all three peaks, and the slowest of them
	// (which beats the 190/3 per-worker term).
	if got.MemoryUsage != 60 {
		t.Errorf("memory_usage = %d; want 60 (the P largest peaks over the shards that ran, P = all 3 of them)", got.MemoryUsage)
	}
	if got.QueryDurationMS != 90 {
		t.Errorf("query_duration_ms = %d; want 90 (the makespan bound over the shards that ran)", got.QueryDurationMS)
	}
	if got.ExitStatus != ExitOOM.String() {
		t.Errorf("exit_status = %q; want %q — the OOM is the whole point of keeping this row", got.ExitStatus, ExitOOM)
	}
	if got.NormalizedQueryHash != 77 {
		t.Errorf("normalized_query_hash = %d; want 77", got.NormalizedQueryHash)
	}
	if got.Route != "B" || got.DecisionReason != "routed" || got.ShapeID != "cerb:rangewindow" {
		t.Errorf("routing read-out = (route=%q reason=%q shape=%q); want (B, routed, cerb:rangewindow)",
			got.Route, got.DecisionReason, got.ShapeID)
	}
	if n := len(r.snapshotIDs()); n != 0 {
		t.Errorf("join index still holds %d ids after the partial row was published; want 0", n)
	}

	// The harvest is one-shot: a further interval must not republish the same
	// dispatch, which would double-count it in every route-B percentile.
	r.reconcileOnce(context.Background())
	if n := len(sink.snapshot()); n != 1 {
		t.Errorf("sink holds %d rows after a second interval; want the 1 partial row, published once", n)
	}
}

// TestObserveRoutedQuery_PartialFanOutFoldsAtTheClampedConcurrency pins the
// intersection of the two halves: a PARTIAL row's schedule-dependent columns
// must still fold at the executor's effective P, not at the shard count that
// happened to land. The TTL case above has P equal to the landed count, where
// both folds are the identity and a naive sum/max would pass it unchanged; this
// one lands two waves, so the clamp has to bind for the row to be right.
//
// Fold naively instead and both columns move at once: memory reads 100 (four
// peaks that never coexisted, P=2) and duration reads 90 (one shard's, when
// 200ms of work on two workers cannot finish inside 100ms).
func TestObserveRoutedQuery_PartialFanOutFoldsAtTheClampedConcurrency(t *testing.T) {
	const (
		kShards      = 8 // the solver's structural fan-out
		parallelism  = 2 // the executor's concurrent-shard cap
		landedShards = 4 // two full waves; shards 5..8 were never dispatched
		ttl          = time.Hour
	)

	src := newFakeSource()
	sink := &memSink{}
	r := New(src, sink, Options{RingCapacity: 8, ObserveBuffer: 16, TTL: ttl})
	clock := time.Unix(1_700_000_000, 0).UTC()
	r.now = func() time.Time { return clock }

	ids := shardIDs("trace-partial-clamped", kShards)
	// Peaks of 10/20/30/40 and 200ms of ClickHouse time across the four shards
	// that ran, the slowest of them 90ms.
	src.seed(SourceRow{QueryID: ids[0], MemoryUsage: 10, QueryDurationMS: 40})
	src.seed(SourceRow{QueryID: ids[1], MemoryUsage: 20, QueryDurationMS: 90})
	src.seed(SourceRow{QueryID: ids[2], MemoryUsage: 30, QueryDurationMS: 60})
	src.seed(SourceRow{QueryID: ids[3], MemoryUsage: 40, QueryDurationMS: 10})

	observeRoutedAtP(r, ids, parallelism, "cerb:rangewindow", kShards)
	r.drainIngest()
	r.reconcileOnce(context.Background())
	clock = clock.Add(ttl + 30*time.Minute)
	r.reconcileOnce(context.Background())

	rows := sink.snapshot()
	if len(rows) != 1 {
		t.Fatalf("wrote %d rows for the expired partial fan-out; want exactly 1", len(rows))
	}
	got := rows[0]
	if got.ShardsObserved != landedShards {
		t.Errorf("shards_observed = %d; want %d", got.ShardsObserved, landedShards)
	}
	if got.MemoryUsage != 70 {
		t.Errorf("memory_usage = %d; want 70 (the P=2 largest of the four peaks that landed)", got.MemoryUsage)
	}
	if got.QueryDurationMS != 100 {
		t.Errorf("query_duration_ms = %d; want 100 (200ms of work on P=2 workers, above the 90ms slowest shard)",
			got.QueryDurationMS)
	}
}

// TestObserveRoutedQuery_PartialFanOutIsPublishedOnRingSlotReuse pins the other
// harvest trigger, and it is the one that decides whether the fix is real.
//
// The TTL is max(2*interval, 1h) — the same lookback the query_log SELECT uses.
// The ring, by contrast, wraps in however long RingCapacity dispatches take,
// which on any deployment busy enough to route is far inside that hour. Harvest
// on expiry ALONE would therefore almost never fire in production: the slot is
// reused first, and with it the fold would vanish. TTL is deliberately left
// unset here so the reuse path is proved on its own.
func TestObserveRoutedQuery_PartialFanOutIsPublishedOnRingSlotReuse(t *testing.T) {
	const (
		ringCap      = 2
		kShards      = 4
		parallelism  = 2
		landedShards = 2
	)

	src := newFakeSource()
	sink := &memSink{}
	r := New(src, sink, Options{RingCapacity: ringCap, ObserveBuffer: 16})

	ids := shardIDs("trace-reuse", kShards)
	src.seed(SourceRow{QueryID: ids[0], ReadRows: 100, MemoryUsage: 10, ExitStatus: ExitOK})
	src.seed(SourceRow{QueryID: ids[1], ReadRows: 200, MemoryUsage: 20, ExitStatus: ExitTimeout})

	observeRoutedAtP(r, ids, parallelism, "cerb:rangewindow", kShards)
	r.drainIngest()
	r.reconcileOnce(context.Background())
	if rows := sink.snapshot(); len(rows) != 0 {
		t.Fatalf("wrote %d rows before the fan-out ended; want 0", len(rows))
	}

	// Enough later dispatches to wrap the ring back onto the routed record's slot.
	for i := 0; i < ringCap; i++ {
		observeNoRoute(r, "single-"+strconv.Itoa(i), "cerb:scan", nil, "promql")
	}
	r.drainIngest()
	r.reconcileOnce(context.Background())

	rows := sink.snapshot()
	if len(rows) != 1 {
		t.Fatalf("wrote %d rows after the routed record's slot was reused; want 1 (the partial, harvested before the slot is overwritten)", len(rows))
	}
	got := rows[0]
	if got.ShardsObserved != landedShards || got.KShards != kShards {
		t.Errorf("(shards_observed, k_shards) = (%d, %d); want (%d, %d)",
			got.ShardsObserved, got.KShards, landedShards, kShards)
	}
	if got.ReadRows != 300 {
		t.Errorf("read_rows = %d; want 300 (the two shards that landed)", got.ReadRows)
	}
	if got.ExitStatus != ExitTimeout.String() {
		t.Errorf("exit_status = %q; want %q", got.ExitStatus, ExitTimeout)
	}
	if got.Parallelism != parallelism {
		t.Errorf("parallelism = %d; want %d", got.Parallelism, parallelism)
	}
}

// TestEvictExpired_RoutedRecordExpiresEveryShardIDAndPublishesNothing pins TTL
// eviction over a ROUTED record — the garbage collector for a fan-out that never
// completes — in the case where NOT ONE shard reached query_log.
//
// Two things must hold at once. Every one of the K ids leaves the index (a
// leftover would later resolve to whatever dispatch reuses the slot and
// contaminate its row), and nothing is written: a fan-out with no observed shard
// has no measured cost, and publishing a zero-cost route-B row would feed the
// calibration a data point that never happened.
func TestEvictExpired_RoutedRecordExpiresEveryShardIDAndPublishesNothing(t *testing.T) {
	const (
		kShards     = 6
		parallelism = 3
		ttl         = time.Hour
	)

	sink := &memSink{}
	r := New(newFakeSource(), sink, Options{RingCapacity: 8, ObserveBuffer: 16, TTL: ttl})
	clock := time.Unix(1_800_000_000, 0).UTC()
	r.now = func() time.Time { return clock }

	ids := shardIDs("trace-expired", kShards)
	observeRoutedAtP(r, ids, parallelism, "cerb:rangewindow", kShards)
	r.drainIngest()
	if n := len(r.snapshotIDs()); n != kShards {
		t.Fatalf("join index holds %d ids after the routed dispatch; want %d", n, kShards)
	}

	clock = clock.Add(ttl + time.Minute)
	if n := r.evictExpired(); n != kShards {
		t.Fatalf("evictExpired = %d; want %d (every shard id of the record expires together)", n, kShards)
	}
	for _, id := range ids {
		if _, _, ok := r.recordFor(id); ok {
			t.Errorf("shard id %q still resolves after the record expired", id)
		}
	}

	r.reconcileOnce(context.Background())
	if rows := sink.snapshot(); len(rows) != 0 {
		t.Fatalf("published %d rows for a fan-out no shard of which reached query_log; want 0 (a row with no observed cost is a fabricated data point)", len(rows))
	}
}

// TestObserveRoutedQuery_SinkFailureDoesNotDoubleCountRefoldedShards pins why
// the fold's add is idempotent per query_id rather than merely defensive.
//
// On a sink-write failure reconcileOnce deliberately keeps the reconciled ids
// AND the completed fold, so the retry re-reads the very same query_log rows.
// Fold them in a second time and the retried row reports double the work — which
// is worse than the dropped row the retry exists to avoid, because it is
// plausible.
func TestObserveRoutedQuery_SinkFailureDoesNotDoubleCountRefoldedShards(t *testing.T) {
	const (
		kShards     = 3
		parallelism = 2
	)

	src := newFakeSource()
	sink := &memSink{err: errSinkRefused}
	r := New(src, sink, Options{RingCapacity: 8, ObserveBuffer: 16})

	ids := shardIDs("trace-retry", kShards)
	src.seed(SourceRow{QueryID: ids[0], ReadRows: 100, MemoryUsage: 10})
	src.seed(SourceRow{QueryID: ids[1], ReadRows: 200, MemoryUsage: 20})
	src.seed(SourceRow{QueryID: ids[2], ReadRows: 300, MemoryUsage: 30})

	observeRoutedAtP(r, ids, parallelism, "cerb:rangewindow", kShards)
	r.drainIngest()
	r.reconcileOnce(context.Background())

	if rows := sink.snapshot(); len(rows) != 0 {
		t.Fatalf("sink recorded %d rows despite refusing the write; want 0", len(rows))
	}
	if n := len(r.snapshotIDs()); n != kShards {
		t.Fatalf("join index holds %d ids after the refused write; want %d (all retried next interval)", n, kShards)
	}

	sink.setErr(nil)
	r.reconcileOnce(context.Background())

	rows := sink.snapshot()
	if len(rows) != 1 {
		t.Fatalf("wrote %d rows on the retry; want 1", len(rows))
	}
	// memory_usage is the sum of the P=2 largest peaks of (10, 20, 30) = 50.
	// A refold appends every peak a second time, so P=2 then selects 30+30 = 60
	// — the double-count this test exists to catch moves the column.
	if rows[0].ReadRows != 600 || rows[0].MemoryUsage != 50 {
		t.Errorf("retried row = (read_rows=%d memory_usage=%d); want (600, 50) — the re-read shards must not fold in twice",
			rows[0].ReadRows, rows[0].MemoryUsage)
	}
	if rows[0].ShardsObserved != kShards {
		t.Errorf("shards_observed = %d; want %d (a complete fan-out, published once)", rows[0].ShardsObserved, kShards)
	}
}
