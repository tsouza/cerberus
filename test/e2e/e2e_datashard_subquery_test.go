//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"
)

// Live execution of docs/operations.md's "Known ClickHouse risk: predicate
// pushdown through subqueries against Distributed (ClickHouse#29332)" test
// plan (cerberus issue #3079, epic #3074). That doc enumerates six concrete
// subquery/derived-table shapes `internal/chsql`/`internal/chplan` builds
// and names #3079 as the issue that should execute them against a real
// multi-shard cluster. These tests are UNCONDITIONAL — they run in every
// `just e2e-run` invocation (the standard single-shard lane, the bwc lane,
// AND the datashard lane), which is exactly how this suite already proves
// "results match a single-shard reference run" everywhere else: the SAME
// pinned assertions running byte-identically regardless of which lane
// executes them. Two of the six shapes already have dedicated coverage
// elsewhere in this package and are deliberately NOT duplicated here:
//   - shape 3 (plain TraceQL search over a Distributed spans table) —
//     TestTempoSearch (e2e_tempo_test.go).
//   - shape 5 (structural join's recursive CTE) —
//     TestTempoSearch_StructuralChild (e2e_tempo_extra_test.go).
//
// Every query below is deliberately loose on VALUES (matching this
// package's own established idiom — TestPromQueryRangeRate and its
// siblings assert "status=success + non-empty", never a numeric pin) since
// the point is that the shape survives the Distributed target /
// legacy-analyzer combination at all, not a specific number — see the doc's
// own "that equality, not a specific number, is the pass criterion" line.

// TestPromQueryHistogramQuantileNativeAggregate — shape 1 (priority 1):
// `histogram_quantile(phi, sum(rate(<native histogram>[5m])))` reaches
// internal/chplan/histogram_quantile_native.go's ScalarSubquery under
// applyNativeHistogramAnalyzerFix's forced `enable_analyzer=0` — the exact
// legacy-pipeline + subquery-over-Distributed combination #29332 named.
// showcase_latency_exp_hist is the deterministic exponential-histogram
// fixture every e2e seed (standard, bwc, datashard) inserts.
func TestPromQueryHistogramQuantileNativeAggregate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	q := url.QueryEscape("histogram_quantile(0.9, sum(rate(showcase_latency_exp_hist[5m])))")
	resp := getJSON(ctx, t, "/api/v1/query?query="+q)
	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []any  `json:"result"`
		} `json:"data"`
	}
	mustDecode(t, resp, &parsed)
	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success", parsed.Status)
	}
	if len(parsed.Data.Result) == 0 {
		t.Fatalf("expected at least one series for histogram_quantile(sum(rate(...))); got 0")
	}
}

// TestPromQueryHistogramQuantileNativeSelector — shape 1's second form: the
// plain-selector `histogram_quantile(phi, <native histogram>)`, the other
// ScalarSubquery-building path histogram_quantile_native.go documents.
func TestPromQueryHistogramQuantileNativeSelector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	q := url.QueryEscape("histogram_quantile(0.9, showcase_latency_exp_hist)")
	resp := getJSON(ctx, t, "/api/v1/query?query="+q)
	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []any  `json:"result"`
		} `json:"data"`
	}
	mustDecode(t, resp, &parsed)
	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success", parsed.Status)
	}
	if len(parsed.Data.Result) == 0 {
		t.Fatalf("expected at least one series for histogram_quantile(<selector>); got 0")
	}
}

// TestPromQueryScalarOfVector — shape 2: `scalar(<vector>)` under the
// DEFAULT analyzer (enable_analyzer=1, never forced legacy), a
// baseline-regression control proving the new-analyzer fix genuinely holds
// on a real multi-shard Distributed table and not merely ClickHouse's own
// single-node test suite.
func TestPromQueryScalarOfVector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	q := url.QueryEscape("scalar(sum(http_server_request_duration_count))")
	resp := getJSON(ctx, t, "/api/v1/query?query="+q)
	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     [2]any `json:"result"`
		} `json:"data"`
	}
	mustDecode(t, resp, &parsed)
	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success", parsed.Status)
	}
	if parsed.Data.ResultType != "scalar" {
		t.Fatalf("resultType: got %q, want scalar", parsed.Data.ResultType)
	}
	if parsed.Data.Result[1] == nil {
		t.Fatalf("scalar(sum(...)) returned a nil value")
	}
}

// TestPromQueryAbsentOverTimeGapDetection — shape 6:
// internal/chsql/absent_over_time.go's NotInSubquery covered-anchor
// exclusion, exercised against a Distributed metrics table. A metric name
// that deterministically never exists in ANY e2e seed fixture makes the
// expected answer unambiguous (present=1) rather than depending on the
// seed's own churn.
func TestPromQueryAbsentOverTimeGapDetection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	q := url.QueryEscape(`absent_over_time(cerberus_e2e_datashard_nonexistent_metric_3079[5m])`)
	resp := getJSON(ctx, t, "/api/v1/query?query="+q)
	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	mustDecode(t, resp, &parsed)
	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success", parsed.Status)
	}
	if len(parsed.Data.Result) != 1 {
		t.Fatalf("absent_over_time(<nonexistent metric>) expected exactly 1 result; got %d", len(parsed.Data.Result))
	}
	if got := fmt.Sprintf("%v", parsed.Data.Result[0].Value[1]); got != "1" {
		t.Fatalf("absent_over_time(<nonexistent metric>) value: got %q, want \"1\"", got)
	}
}

// TestTempoSearchStructureTabTopN — shape 4: BoundedTraceScope's
// structure-tab top-N gate (internal/chplan/bounded_trace_scope_bind.go),
// another `TraceId IN (<subquery>)` shape with an additional row-count
// bound. `select(nestedSetLeft, nestedSetParent, nestedSetRight)` is the
// exact projection Grafana's Traces Drilldown structure tab requests
// (internal/api/tempo/handler.go's own "Drilldown structure tab hard-fails
// without nestedSetLeft/nestedSetParent/nestedSetRight" comment) and is
// what activates the BoundedTraceScope binding at all.
func TestTempoSearchStructureTabTopN(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	v := url.Values{}
	v.Set("q", `{ resource.service.name = "frontend" } | select(nestedSetLeft, nestedSetParent, nestedSetRight)`)
	resp := getJSON(ctx, t, "/api/search?"+v.Encode())
	var parsed struct {
		Traces []any `json:"traces"`
	}
	mustDecode(t, resp, &parsed)
	if len(parsed.Traces) == 0 {
		t.Fatalf("expected at least one structure-tab result; got 0")
	}
}

// TestTempoMetricsCompareRootScope — the remainder of shape 3: `compare()`
// nests an InSubquery root-lookup inside an additional bounded root leg
// (internal/chsql/metrics_compare.go's cohortPred/
// bindRootLookupTraceIDTsEnvelope), served by GET /api/metrics/query_range
// (internal/api/tempo/metrics_query_range.go's own doc: `q`, `start`, `end`
// required; `step` optional). This assertion is deliberately STRUCTURAL
// (a well-formed series envelope came back) rather than a non-empty-series
// claim: the seed fixtures do not guarantee an error-status span exists to
// populate compare()'s comparison cohort, so an empty selection is a valid
// answer here — the shape reaching a 200 at all against the Distributed
// target under compare()'s root-scope InSubquery is what this test exists
// to prove.
func TestTempoMetricsCompareRootScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	now := time.Now().Unix()
	start := now - 3*60*60
	v := url.Values{}
	v.Set("q", `{ resource.service.name = "frontend" } | compare({ status = error })`)
	v.Set("start", fmt.Sprintf("%d", start))
	v.Set("end", fmt.Sprintf("%d", now))

	resp := getJSON(ctx, t, "/api/metrics/query_range?"+v.Encode())
	var parsed struct {
		Series []any `json:"series"`
	}
	mustDecode(t, resp, &parsed)
	if parsed.Series == nil {
		t.Fatalf("compare() response carried no 'series' field at all (want at least an empty array)")
	}
}
