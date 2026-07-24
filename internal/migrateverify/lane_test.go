package migrateverify

import (
	"strings"
	"testing"
)

// TestRouteQuery pins the offline lane decision for every shape the router must
// distinguish.
//
// Each out-of-scope row asserts the Reason SUBSTRINGS, not merely that the entry
// was not replayed: a router that returns Replayable=false with an empty reason
// would satisfy a boolean-only assertion while leaving the operator unable to see
// which of their queries the gate declined to judge, or why. That accounting is
// the deliverable, so it is what the test checks.
func TestRouteQuery(t *testing.T) {
	cases := []struct {
		name         string
		lang         string
		expr         string
		wantHead     string
		wantReplay   bool
		wantKind     string
		reasonHasAll []string
	}{
		{
			name: "promql", lang: "promql", expr: "up",
			wantHead: HeadProm, wantReplay: true,
		},
		{
			// The pre-three-headed corpus default: an untagged entry is PromQL.
			name: "empty lang defaults to prom", lang: "", expr: "up",
			wantHead: HeadProm, wantReplay: true,
		},
		{
			name: "logql metric", lang: "logql", expr: `sum(rate({job="api"}[5m]))`,
			wantHead: HeadLoki, wantReplay: true,
		},
		{
			name: "logql range aggregation", lang: "logql", expr: `count_over_time({job="api"}[1m])`,
			wantHead: HeadLoki, wantReplay: true,
		},
		{
			// The dotted-label regression: OTel resource attributes are dotted, and
			// without the normaliser this real Grafana panel misroutes to
			// "unparseable" even though cerberus serves it.
			name: "logql metric with OTel dotted label", lang: "logql", expr: `sum(rate({service.name="api"}[5m]))`,
			wantHead: HeadLoki, wantReplay: true,
		},
		{
			// LiteralExpr and VectorExpr both implement SampleExpr, exactly as
			// logql.IsMetricQuery treats them.
			name: "logql literal", lang: "logql", expr: "1+1",
			wantHead: HeadLoki, wantReplay: true,
		},
		{
			name: "logql vector", lang: "logql", expr: "vector(1)",
			wantHead: HeadLoki, wantReplay: true,
		},
		{
			name: "logql log stream", lang: "logql", expr: `{job="api"}`,
			wantHead: HeadLoki, wantKind: KindLogStream,
			reasonHasAll: []string{"lang=logql", "kind=log-stream", "metric-lane gate"},
		},
		{
			name: "logql log stream with pipeline", lang: "logql", expr: `{job="api"} |= "boom" | json`,
			wantHead: HeadLoki, wantKind: KindLogStream,
			reasonHasAll: []string{"lang=logql", "kind=log-stream"},
		},
		{
			name: "logql unparseable", lang: "logql", expr: `{job=`,
			wantHead: HeadLoki, wantKind: KindUnparseable,
			reasonHasAll: []string{"lang=logql", "kind=unparseable", "parse"},
		},
		{
			name: "traceql rate", lang: "traceql", expr: "{} | rate()",
			wantHead: HeadTempo, wantReplay: true,
		},
		{
			name: "traceql count_over_time", lang: "traceql", expr: "{} | count_over_time()",
			wantHead: HeadTempo, wantReplay: true,
		},
		{
			name: "traceql quantile_over_time", lang: "traceql", expr: "{} | quantile_over_time(duration, .99)",
			wantHead: HeadTempo, wantReplay: true,
		},
		{
			name: "traceql histogram_over_time", lang: "traceql", expr: "{} | histogram_over_time(duration)",
			wantHead: HeadTempo, wantReplay: true,
		},
		{
			name: "traceql topk second stage", lang: "traceql", expr: "{} | rate() | topk(10)",
			wantHead: HeadTempo, wantReplay: true,
		},
		{
			name: "traceql trace search", lang: "traceql", expr: `{ span.foo = "bar" }`,
			wantHead: HeadTempo, wantKind: KindTraceSearch,
			reasonHasAll: []string{"lang=traceql", "kind=trace-search", "metric-lane gate"},
		},
		{
			name: "traceql compare", lang: "traceql", expr: `{} | compare({status=error})`,
			wantHead: HeadTempo, wantKind: KindMetricsCompare,
			reasonHasAll: []string{"lang=traceql", "kind=metrics-compare", "compare()"},
		},
		{
			name: "traceql unparseable", lang: "traceql", expr: "{{{",
			wantHead: HeadTempo, wantKind: KindUnparseable,
			reasonHasAll: []string{"lang=traceql", "kind=unparseable", "parse"},
		},
		{
			name: "unknown language", lang: "cypher", expr: "MATCH (n) RETURN n",
			wantKind: KindUnknownLang, reasonHasAll: []string{"cypher", "no metric lane"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RouteQuery(tc.lang, tc.expr)
			if got.Head != tc.wantHead {
				t.Errorf("Head = %q, want %q", got.Head, tc.wantHead)
			}
			if got.Replayable != tc.wantReplay {
				t.Fatalf("Replayable = %v, want %v (routing = %+v)", got.Replayable, tc.wantReplay, got)
			}
			if tc.wantReplay {
				if got.Kind != "" || got.Reason != "" {
					t.Errorf("a replayable routing must carry no out-of-scope kind/reason, got %+v", got)
				}
				return
			}
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if got.Reason == "" {
				t.Fatal("an out-of-scope routing MUST carry a reason: an operator has to see how many queries the gate did not judge and why")
			}
			for _, want := range tc.reasonHasAll {
				if !strings.Contains(got.Reason, want) {
					t.Errorf("Reason = %q, want it to contain %q", got.Reason, want)
				}
			}
		})
	}
}

// TestRouteQuery_UnparseableCarriesTheParserError pins that a rejected expression
// is reported with the parser's OWN message, so the operator can fix the query
// rather than guessing at an opaque "unparseable".
func TestRouteQuery_UnparseableCarriesTheParserError(t *testing.T) {
	logql := RouteQuery("logql", `{job=`)
	if !strings.Contains(logql.Reason, "parse error") {
		t.Errorf("logql unparseable reason = %q, want the parser's verbatim error text", logql.Reason)
	}
	traceql := RouteQuery("traceql", "{{{")
	if !strings.Contains(traceql.Reason, "parse error") {
		t.Errorf("traceql unparseable reason = %q, want the parser's verbatim error text", traceql.Reason)
	}
}
