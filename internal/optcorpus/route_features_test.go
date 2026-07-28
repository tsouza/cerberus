package optcorpus

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// TestRow_JSONRoundTrip_NewFields pins that the extended Row (routing features
// + exit_status) survives a JSON encode/decode unchanged — the JSONL sink's
// durability contract, and the stability the column-for-column CH-table sink
// swap depends on.
func TestRow_JSONRoundTrip_NewFields(t *testing.T) {
	t.Parallel()

	want := Row{
		ShapeID:             "cerb:agg;agg=2",
		Opts:                []string{"aggregation_in_order"},
		Language:            "promql",
		NormalizedQueryHash: 42,
		ReadRows:            1000,
		ReadBytes:           8000,
		QueryDurationMS:     12,
		MemoryUsage:         2048,
		ProfileEvents:       map[string]int64{"QueryConditionCacheHits": 3},
		NAnchors:            241,
		Fanout:              20,
		CumulativeD:         300,
		OuterRange:          3600,
		Step:                15,
		Route:               "B",
		KShards:             8,
		DecisionReason:      "routed",
		ExitStatus:          "oom",
	}

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

	// The new columns must be present under their corpus-schema JSON keys so
	// the CH-table sink (which maps these keys to columns) stays aligned.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	for _, key := range []string{
		"n_anchors", "fanout", "cumulative_d", "outer_range", "step",
		"route", "k_shards", "decision_reason", "exit_status",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("Row JSON missing key %q", key)
		}
	}
}

// TestReconcileOnce_JoinsRouteFeatures pins that the routing read-out captured
// at the dispatch seam lands on the reconciled Row alongside the joined cost.
func TestReconcileOnce_JoinsRouteFeatures(t *testing.T) {
	t.Parallel()

	src := newFakeSource()
	sink := &memSink{}
	r := New(src, sink, Options{RingCapacity: 8})

	r.Observe(Record{
		QueryID:  "qid-route",
		ShapeID:  "cerb:agg;agg=2",
		Language: "promql",
		Route: RouteFeatures{
			Present:        true,
			Route:          "B",
			NAnchors:       241,
			Fanout:         20,
			CumulativeD:    300,
			OuterRange:     3600,
			Step:           15,
			KShards:        8,
			DecisionReason: "routed",
		},
	})
	src.seed(SourceRow{QueryID: "qid-route", ReadRows: 5, ExitStatus: ExitOK})

	r.reconcileOnce(context.Background())

	rows := sink.snapshot()
	if len(rows) != 1 {
		t.Fatalf("sink rows = %d; want 1", len(rows))
	}
	got := rows[0]
	if got.Route != "B" || got.KShards != 8 || got.DecisionReason != "routed" {
		t.Errorf("route features not joined: %+v", got)
	}
	if got.NAnchors != 241 || got.Fanout != 20 || got.CumulativeD != 300 ||
		got.OuterRange != 3600 || got.Step != 15 {
		t.Errorf("cost-grid features not joined: %+v", got)
	}
	if got.ExitStatus != "ok" {
		t.Errorf("exit_status = %q, want ok", got.ExitStatus)
	}
}

// TestReconcileOnce_RouteAbsent_ZeroColumns pins that a dispatch with no
// routing classification (Solver off / unclassified head) leaves the routing
// columns empty rather than recording a fictitious route-A row.
func TestReconcileOnce_RouteAbsent_ZeroColumns(t *testing.T) {
	t.Parallel()

	src := newFakeSource()
	sink := &memSink{}
	r := New(src, sink, Options{RingCapacity: 8})

	r.Observe(Record{QueryID: "qid-noroute", ShapeID: "cerb:scan", Language: "logql"})
	src.seed(SourceRow{QueryID: "qid-noroute", ReadRows: 1, ExitStatus: ExitOK})

	r.reconcileOnce(context.Background())

	rows := sink.snapshot()
	if len(rows) != 1 {
		t.Fatalf("sink rows = %d; want 1", len(rows))
	}
	got := rows[0]
	if got.Route != "" || got.KShards != 0 || got.DecisionReason != "" ||
		got.NAnchors != 0 || got.Fanout != 0 {
		t.Errorf("absent route should leave columns zero: %+v", got)
	}
}

// unmappedExceptionCode is a ClickHouse error code the classifier does not
// name. It stands for the whole open tail of server error codes: whatever it
// is, the query FAILED, so the honest classification is ExitError — never the
// healthy population, and never a fabricated cause such as oom.
const unmappedExceptionCode int32 = 999

// TestExitStatusFor_DerivesFromFailureAndCode pins the terminal-outcome
// classification the reconciler keys the cost-distribution analysis on. Every
// named ClickHouse code is covered, plus the unmapped tail.
func TestExitStatusFor_DerivesFromFailureAndCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		failed bool
		code   int32
		want   ExitStatus
	}{
		{"clean finish", false, 0, ExitOK},
		{"finish ignores stray code", false, chErrMemoryLimitExceeded, ExitOK},
		{"oom", true, chErrMemoryLimitExceeded, ExitOOM},
		{"timeout", true, chErrTimeoutExceeded, ExitTimeout},
		{"too-slow folds to timeout", true, chErrTooSlow, ExitTimeout},
		{"read-after-eof is an abort", true, chErrAttemptToReadAfterEOF, ExitAborted},
		{"unknown client packet is an abort", true, chErrUnknownPacketFromClient, ExitAborted},
		{"network error is an abort", true, chErrNetworkError, ExitAborted},
		{"cancelled is an abort", true, chErrQueryWasCancelled, ExitAborted},
		{"cancelled by client is an abort", true, chErrQueryWasCancelledByClient, ExitAborted},
		{"unmapped exception is an error, never ok", true, unmappedExceptionCode, ExitError},
		{"missing exception code is still a failure", true, 0, ExitError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := exitStatusFor(tc.failed, tc.code); got != tc.want {
				t.Errorf("exitStatusFor(%v, %d) = %v, want %v", tc.failed, tc.code, got, tc.want)
			}
		})
	}
}

// TestExitStatus_String pins the enum token rendering used by both sinks, for
// every member of the iota — the tokens are the DDL and JSONL wire contract.
func TestExitStatus_String(t *testing.T) {
	t.Parallel()
	want := map[ExitStatus]string{
		ExitOK:           "ok",
		ExitOOM:          "oom",
		ExitTimeout:      "timeout",
		ExitSampleBudget: "sample_budget",
		ExitBreaker:      "breaker",
		ExitRejected:     "rejected",
		ExitAborted:      "aborted",
		ExitError:        "error",
	}
	if len(want) != len(exitStatuses) {
		t.Fatalf("this test pins %d tokens; exitStatuses has %d", len(want), len(exitStatuses))
	}
	for _, s := range exitStatuses {
		if got := s.String(); got != want[s] {
			t.Errorf("%d.String() = %q, want %q", s, got, want[s])
		}
	}
}
