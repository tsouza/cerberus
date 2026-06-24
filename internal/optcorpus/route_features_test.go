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

// TestExitStatusFor_DerivesFromTypeAndCode pins the query_log type+exception
// mapping the reconciler keys the cost-distribution analysis on.
func TestExitStatusFor_DerivesFromTypeAndCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		typ  string
		code int32
		want ExitStatus
	}{
		{"clean finish", "QueryFinish", 0, ExitOK},
		{"finish ignores stray code", "QueryFinish", chErrMemoryLimitExceeded, ExitOK},
		{"oom", "QueryExceptionWhileProcessing", chErrMemoryLimitExceeded, ExitOOM},
		{"timeout", "QueryExceptionWhileProcessing", chErrTimeoutExceeded, ExitTimeout},
		{"too-slow folds to timeout", "QueryExceptionWhileProcessing", chErrTooSlow, ExitTimeout},
		{"other exception -> ok (never fake oom)", "QueryExceptionWhileProcessing", 999, ExitOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := exitStatusFor(tc.typ, tc.code); got != tc.want {
				t.Errorf("exitStatusFor(%q, %d) = %v, want %v", tc.typ, tc.code, got, tc.want)
			}
		})
	}
}

// TestExitStatus_String pins the enum token rendering used by both sinks.
func TestExitStatus_String(t *testing.T) {
	t.Parallel()
	for s, want := range map[ExitStatus]string{ExitOK: "ok", ExitOOM: "oom", ExitTimeout: "timeout"} {
		if got := s.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", s, got, want)
		}
	}
}
