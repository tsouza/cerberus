package optcorpus

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// TestExitStatus_String_CerberusSide pins the new cerberus-side enum tokens —
// the stable wire/DDL contract shared by both sinks, the CH Enum8, and the
// calibration SQL.
func TestExitStatus_String_CerberusSide(t *testing.T) {
	t.Parallel()
	for s, want := range map[ExitStatus]string{
		ExitSampleBudget: "sample_budget",
		ExitBreaker:      "breaker",
		ExitRejected:     "rejected",
	} {
		if got := s.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", s, got, want)
		}
	}
}

// TestParseExitStatus_RoundTrip pins that every cerberus-side token parses back
// to its ExitStatus, that CH-side tokens are NOT parseable here (they are
// query_log-derived, never passed through the in-process seams), and that an
// unknown token is rejected rather than silently mapped to ok.
func TestParseExitStatus_RoundTrip(t *testing.T) {
	t.Parallel()
	for _, s := range []ExitStatus{ExitSampleBudget, ExitBreaker, ExitRejected} {
		got, ok := parseExitStatus(s.String())
		if !ok || got != s {
			t.Errorf("parseExitStatus(%q) = (%v, %v), want (%v, true)", s.String(), got, ok, s)
		}
		if !s.cerberusSide() {
			t.Errorf("%v should be cerberusSide", s)
		}
	}
	for _, token := range []string{"ok", "oom", "timeout", "bogus", ""} {
		if _, ok := parseExitStatus(token); ok {
			t.Errorf("parseExitStatus(%q) accepted a non-cerberus-side token", token)
		}
	}
	for _, s := range []ExitStatus{ExitOK, ExitOOM, ExitTimeout} {
		if s.cerberusSide() {
			t.Errorf("%v must not be cerberusSide", s)
		}
	}
}

// TestObserveOutcome_SampleBudget_PrecedenceOverQueryLogOK is the headline of
// the gap fix: a dispatched query that the CH query_log reports as a clean
// finish (ok, with REAL cost), but for which cerberus returned the
// sample-budget 422 during the Go-side drain. The corpus row must keep the
// query_log COST while overriding exit_status to "sample_budget" — the richest
// calibration signal ("CH cost = X, but cerberus rejected: too big").
func TestObserveOutcome_SampleBudget_PrecedenceOverQueryLogOK(t *testing.T) {
	t.Parallel()

	src := newFakeSource()
	sink := &memSink{}
	r := New(src, sink, Options{RingCapacity: 8})

	// Dispatch record first (the at-dispatch seam), then the later cerberus-side
	// outcome — same FIFO order the channel preserves.
	r.ObserveQuery("qid-422", "cerb:agg;agg=2", []string{"x"}, "promql",
		true, "A", 241, 20, 300, 3600, 15, 0, "below-threshold")
	r.ObserveOutcome("qid-422", ExitTokenSampleBudget)
	r.drainIngest()

	// query_log says the CH query finished cleanly with real cost.
	src.seed(SourceRow{QueryID: "qid-422", ReadRows: 9_000_000, MemoryUsage: 4096, ExitStatus: ExitOK})

	r.reconcileOnce(context.Background())

	rows := sink.snapshot()
	if len(rows) != 1 {
		t.Fatalf("sink rows = %d; want 1", len(rows))
	}
	got := rows[0]
	if got.ExitStatus != "sample_budget" {
		t.Errorf("exit_status = %q; want sample_budget (cerberus outcome must win over query_log ok)", got.ExitStatus)
	}
	if got.ReadRows != 9_000_000 || got.MemoryUsage != 4096 {
		t.Errorf("CH cost must be retained on a sample-budget row: %+v", got)
	}
	if got.Route != "A" || got.NAnchors != 241 || got.DecisionReason != "below-threshold" {
		t.Errorf("routing read-out not joined: %+v", got)
	}
}

// TestObserveOutcome_MergePreservesDispatchMetadata pins that an outcome-update
// arriving after the dispatch record MERGES (keeps shape-id / route) rather than
// clobbering the slot with an otherwise-empty record.
func TestObserveOutcome_MergePreservesDispatchMetadata(t *testing.T) {
	t.Parallel()

	r := New(newFakeSource(), &memSink{}, Options{RingCapacity: 8})
	r.ObserveQuery("qid-m", "cerb:scan", []string{"opt"}, "logql",
		false, "", 0, 0, 0, 0, 0, 0, "")
	r.ObserveOutcome("qid-m", ExitTokenSampleBudget)
	r.drainIngest()

	rec, ok := r.recordFor("qid-m")
	if !ok {
		t.Fatal("dispatch record lost after outcome merge")
	}
	if rec.ShapeID != "cerb:scan" || rec.Language != "logql" || len(rec.Opts) != 1 {
		t.Errorf("outcome update clobbered dispatch metadata: %+v", rec)
	}
	if !rec.HasOutcome || rec.Outcome != ExitSampleBudget {
		t.Errorf("outcome not merged onto record: %+v", rec)
	}
}

// TestObserveOutcome_NoDispatchRecord_Dropped pins that an outcome-update for a
// query_id with no dispatch record (evicted / never observed) is dropped — there
// is nothing to join it to, so it must not create a phantom row.
func TestObserveOutcome_NoDispatchRecord_Dropped(t *testing.T) {
	t.Parallel()
	r := New(newFakeSource(), &memSink{}, Options{RingCapacity: 8})
	r.ObserveOutcome("ghost", ExitTokenSampleBudget)
	r.drainIngest()
	if ids := r.snapshotIDs(); len(ids) != 0 {
		t.Errorf("orphan outcome created a tracked id: %v", ids)
	}
}

// TestObserveOutcome_IgnoresNonCerberusSideToken pins that the seam only accepts
// cerberus-side tokens — a CH-side token (which would come from query_log, not
// the in-process seam) is ignored so it cannot overwrite the join-derived status.
func TestObserveOutcome_IgnoresNonCerberusSideToken(t *testing.T) {
	t.Parallel()
	r := New(newFakeSource(), &memSink{}, Options{RingCapacity: 8})
	r.ObserveQuery("qid-x", "cerb:scan", nil, "promql", false, "", 0, 0, 0, 0, 0, 0, "")
	r.ObserveOutcome("qid-x", "oom") // CH-side token — must be ignored
	r.ObserveOutcome("", ExitTokenSampleBudget)
	r.drainIngest()
	rec, ok := r.recordFor("qid-x")
	if !ok || rec.HasOutcome {
		t.Errorf("non-cerberus-side / empty-id outcome should not stamp: %+v ok=%v", rec, ok)
	}
}

// TestObserveRejection_Breaker_DecisionOnlyNoCost is the pre-CH-rejection case:
// the breaker fast-failed the request 503, so there is NO CH query and NO
// query_log row. The corpus must still emit a decision-only row carrying the
// routing read-out, exit_status="breaker", and ZERO cost.
func TestObserveRejection_Breaker_DecisionOnlyNoCost(t *testing.T) {
	t.Parallel()

	src := newFakeSource()
	sink := &memSink{}
	r := New(src, sink, Options{RingCapacity: 8})

	r.ObserveRejection("cerb:agg", []string{"o"}, "promql", ExitTokenBreaker,
		true, "B", 100, 10, 200, 1800, 18, 4, "routed")
	r.drainIngest()
	r.reconcileOnce(context.Background())

	rows := sink.snapshot()
	if len(rows) != 1 {
		t.Fatalf("sink rows = %d; want 1 decision-only row", len(rows))
	}
	got := rows[0]
	if got.ExitStatus != "breaker" {
		t.Errorf("exit_status = %q; want breaker", got.ExitStatus)
	}
	if got.ReadRows != 0 || got.ReadBytes != 0 || got.MemoryUsage != 0 || got.QueryDurationMS != 0 {
		t.Errorf("decision-only row must carry zero cost: %+v", got)
	}
	if got.Route != "B" || got.KShards != 4 || got.NAnchors != 100 || got.DecisionReason != "routed" {
		t.Errorf("routing read-out not joined onto rejection row: %+v", got)
	}
	// No query_id was ever tracked for a decision-only rejection.
	if ids := r.snapshotIDs(); len(ids) != 0 {
		t.Errorf("decision-only rejection leaked into the join ring: %v", ids)
	}
}

// TestObserveRejection_Cap_RejectedDecisionOnly pins the pre-parse cap
// rejection: no plan, no routing classification (routePresent=false), so the row
// is "rejected" with zero cost and empty routing columns.
func TestObserveRejection_Cap_RejectedDecisionOnly(t *testing.T) {
	t.Parallel()

	sink := &memSink{}
	r := New(newFakeSource(), sink, Options{RingCapacity: 8})

	r.ObserveRejection("", nil, "traceql", ExitTokenRejected,
		false, "", 0, 0, 0, 0, 0, 0, "")
	r.drainIngest()
	r.reconcileOnce(context.Background())

	rows := sink.snapshot()
	if len(rows) != 1 {
		t.Fatalf("sink rows = %d; want 1", len(rows))
	}
	got := rows[0]
	if got.ExitStatus != "rejected" || got.Language != "traceql" {
		t.Errorf("rejection row wrong: %+v", got)
	}
	if got.Route != "" || got.NAnchors != 0 {
		t.Errorf("cap rejection (no classify) must leave routing columns empty: %+v", got)
	}
}

// TestObserveRejection_IgnoresNonCerberusSideToken pins that a non-cerberus-side
// token never produces a decision-only row.
func TestObserveRejection_IgnoresNonCerberusSideToken(t *testing.T) {
	t.Parallel()
	sink := &memSink{}
	r := New(newFakeSource(), sink, Options{RingCapacity: 8})
	r.ObserveRejection("", nil, "promql", "oom", false, "", 0, 0, 0, 0, 0, 0, "")
	r.drainIngest()
	r.reconcileOnce(context.Background())
	if rows := sink.snapshot(); len(rows) != 0 {
		t.Errorf("non-cerberus-side rejection wrote a row: %+v", rows)
	}
}

// TestObserveDispatchedRejection_OOM_TerminalZeroCost is the headline of the
// memory-cap gap fix: a query that DID dispatch but was aborted by the per-query
// memory cap (CH code 241, token "oom"). The corpus must emit a TERMINAL row at
// the rejection site — exit_status "oom", routing read-out carried, ZERO cost —
// WITHOUT waiting for a system.query_log join (the row may never land or the id
// may be evicted first).
func TestObserveDispatchedRejection_OOM_TerminalZeroCost(t *testing.T) {
	t.Parallel()

	src := newFakeSource()
	sink := &memSink{}
	r := New(src, sink, Options{RingCapacity: 8})

	r.ObserveDispatchedRejection("qid-oom", "cerb:agg", []string{"o"}, "promql", ExitTokenOOM,
		true, "A", 500, 30, 600, 7200, 15, 0, "below-threshold")
	r.drainIngest()
	// No query_log row is seeded: the terminal row must be written regardless.
	r.reconcileOnce(context.Background())

	rows := sink.snapshot()
	if len(rows) != 1 {
		t.Fatalf("sink rows = %d; want 1 terminal oom row", len(rows))
	}
	got := rows[0]
	if got.ExitStatus != "oom" {
		t.Errorf("exit_status = %q; want oom", got.ExitStatus)
	}
	if got.ReadRows != 0 || got.ReadBytes != 0 || got.MemoryUsage != 0 || got.QueryDurationMS != 0 {
		t.Errorf("dispatched-rejection row must carry zero cost (unknowable at the abort site): %+v", got)
	}
	if got.Route != "A" || got.NAnchors != 500 || got.Fanout != 30 || got.DecisionReason != "below-threshold" {
		t.Errorf("routing read-out not joined onto oom row: %+v", got)
	}
}

// TestObserveDispatchedRejection_OOM_NoDoubleCountWithQueryLog pins the
// no-double-count contract: the dispatch query_id was registered via
// ObserveQuery, then the memory-cap abort records a terminal oom row carrying
// that id. The reconciler must FORGET the id so the later query_log join (which
// may also land an oom row for the same physical abort) cannot emit a second row.
func TestObserveDispatchedRejection_OOM_NoDoubleCountWithQueryLog(t *testing.T) {
	t.Parallel()

	src := newFakeSource()
	sink := &memSink{}
	r := New(src, sink, Options{RingCapacity: 8})

	// Dispatch seam first (id enters the join ring), then the terminal oom.
	r.ObserveQuery("qid-oom", "cerb:agg", []string{"o"}, "promql",
		true, "A", 500, 30, 600, 7200, 15, 0, "below-threshold")
	r.ObserveDispatchedRejection("qid-oom", "cerb:agg", []string{"o"}, "promql", ExitTokenOOM,
		true, "A", 500, 30, 600, 7200, 15, 0, "below-threshold")
	r.drainIngest()

	// The id must already be gone from the join index (forgotten by the terminal).
	if ids := r.snapshotIDs(); len(ids) != 0 {
		t.Errorf("dispatched oom must forget the join id; still tracked: %v", ids)
	}

	// Even if query_log later reports the same id as oom, the join writes nothing.
	src.seed(SourceRow{QueryID: "qid-oom", ReadRows: 9_000_000, ExitStatus: ExitOOM})
	r.reconcileOnce(context.Background())

	rows := sink.snapshot()
	if len(rows) != 1 {
		t.Fatalf("sink rows = %d; want exactly 1 (terminal row only, no join duplicate)", len(rows))
	}
	if rows[0].ExitStatus != "oom" || rows[0].ReadRows != 0 {
		t.Errorf("the single row must be the terminal zero-cost oom: %+v", rows[0])
	}
}

// TestObserveDispatchedRejection_IgnoresUnknownToken pins that an unrecognised
// token never produces a row.
func TestObserveDispatchedRejection_IgnoresUnknownToken(t *testing.T) {
	t.Parallel()
	sink := &memSink{}
	r := New(newFakeSource(), sink, Options{RingCapacity: 8})
	r.ObserveDispatchedRejection("qid", "cerb:scan", nil, "promql", "bogus",
		false, "", 0, 0, 0, 0, 0, 0, "")
	r.drainIngest()
	r.reconcileOnce(context.Background())
	if rows := sink.snapshot(); len(rows) != 0 {
		t.Errorf("unknown-token dispatched rejection wrote a row: %+v", rows)
	}
}

// TestParseTerminalRejectionStatus pins the terminal-at-rejection token set: the
// three cerberus-side tokens PLUS oom parse; the query_log-derived ok / timeout
// and an unknown token do not.
func TestParseTerminalRejectionStatus(t *testing.T) {
	t.Parallel()
	for token, want := range map[string]ExitStatus{
		ExitTokenSampleBudget: ExitSampleBudget,
		ExitTokenBreaker:      ExitBreaker,
		ExitTokenRejected:     ExitRejected,
		ExitTokenOOM:          ExitOOM,
	} {
		got, ok := parseTerminalRejectionStatus(token)
		if !ok || got != want {
			t.Errorf("parseTerminalRejectionStatus(%q) = (%v, %v); want (%v, true)", token, got, ok, want)
		}
	}
	for _, token := range []string{"ok", "timeout", "bogus", ""} {
		if _, ok := parseTerminalRejectionStatus(token); ok {
			t.Errorf("parseTerminalRejectionStatus(%q) accepted a non-terminal token", token)
		}
	}
}

// TestFlushRejections_SinkError_Rebuffers pins the failure-open contract for
// decision-only rows: a sink write failure re-buffers the rejections so the next
// interval retries them rather than dropping them.
func TestFlushRejections_SinkError_Rebuffers(t *testing.T) {
	t.Parallel()

	sink := &memSink{err: os.ErrPermission}
	r := New(newFakeSource(), sink, Options{RingCapacity: 8})
	r.ObserveRejection("", nil, "promql", ExitTokenBreaker, false, "", 0, 0, 0, 0, 0, 0, "")
	r.drainIngest()

	r.reconcileOnce(context.Background()) // sink fails
	if len(sink.snapshot()) != 0 {
		t.Fatal("rows written despite sink error")
	}

	// Heal the sink; the re-buffered rejection must flush next interval.
	sink.mu.Lock()
	sink.err = nil
	sink.mu.Unlock()
	r.reconcileOnce(context.Background())
	if n := len(sink.snapshot()); n != 1 {
		t.Errorf("re-buffered rejection not retried; sink rows = %d, want 1", n)
	}
}

// TestRow_JSONRoundTrip_CerberusSideExitStatus pins that a row carrying a new
// exit_status value survives the JSONL durability round-trip unchanged.
func TestRow_JSONRoundTrip_CerberusSideExitStatus(t *testing.T) {
	t.Parallel()
	want := Row{ShapeID: "cerb:scan", Language: "promql", ReadRows: 1, ExitStatus: "sample_budget"}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Row
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}
