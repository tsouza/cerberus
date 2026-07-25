package migrateverify

import (
	"strings"
	"testing"
)

// TestRouteQuery pins the offline lane decision for every shape the router must
// distinguish.
//
// Every row asserts the routed KIND, replayable or not: the kind is what selects
// the comparator, so a router that replays a log stream through the matrix
// comparator would satisfy a Replayable-only assertion while diffing a streams
// body against a matrix baseline.
//
// Each out-of-scope row also asserts the Reason SUBSTRINGS, not merely that the
// entry was not replayed: a router that returns Replayable=false with an empty
// reason would satisfy a boolean-only assertion while leaving the operator unable
// to see which of their queries the gate declined to judge, or why. That
// accounting is the deliverable, so it is what the test checks.
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
			wantHead: HeadProm, wantReplay: true, wantKind: KindMetricMatrix,
		},
		{
			// The pre-three-headed corpus default: an untagged entry is PromQL.
			name: "empty lang defaults to prom", lang: "", expr: "up",
			wantHead: HeadProm, wantReplay: true, wantKind: KindMetricMatrix,
		},
		{
			name: "logql metric", lang: "logql", expr: `sum(rate({job="api"}[5m]))`,
			wantHead: HeadLoki, wantReplay: true, wantKind: KindMetricMatrix,
		},
		{
			name: "logql range aggregation", lang: "logql", expr: `count_over_time({job="api"}[1m])`,
			wantHead: HeadLoki, wantReplay: true, wantKind: KindMetricMatrix,
		},
		{
			// The dotted-label regression: OTel resource attributes are dotted, and
			// without the normaliser this real Grafana panel misroutes to
			// "unparseable" even though cerberus serves it.
			name: "logql metric with OTel dotted label", lang: "logql", expr: `sum(rate({service.name="api"}[5m]))`,
			wantHead: HeadLoki, wantReplay: true, wantKind: KindMetricMatrix,
		},
		{
			// LiteralExpr and VectorExpr both implement SampleExpr, exactly as
			// logql.IsMetricQuery treats them.
			name: "logql literal", lang: "logql", expr: "1+1",
			wantHead: HeadLoki, wantReplay: true, wantKind: KindMetricMatrix,
		},
		{
			name: "logql vector", lang: "logql", expr: "vector(1)",
			wantHead: HeadLoki, wantReplay: true, wantKind: KindMetricMatrix,
		},
		{
			name: "logql log stream", lang: "logql", expr: `{job="api"}`,
			wantHead: HeadLoki, wantReplay: true, wantKind: KindLogStream,
		},
		{
			// Pipeline stages filter, parse and reformat; none of them adds or
			// removes an entry, so the result is still a comparable log stream.
			name: "logql log stream with pipeline", lang: "logql", expr: `{job="api"} |= "boom" | json`,
			wantHead: HeadLoki, wantReplay: true, wantKind: KindLogStream,
		},
		{
			name: "logql log stream with line_format", lang: "logql", expr: `{job="api"} | json | line_format "{{.msg}}"`,
			wantHead: HeadLoki, wantReplay: true, wantKind: KindLogStream,
		},
		{
			name: "logql unparseable", lang: "logql", expr: `{job=`,
			wantHead: HeadLoki, wantKind: KindUnparseable,
			reasonHasAll: []string{"lang=logql", "kind=unparseable", "parse"},
		},
		{
			name: "traceql rate", lang: "traceql", expr: "{} | rate()",
			wantHead: HeadTempo, wantReplay: true, wantKind: KindMetricMatrix,
		},
		{
			name: "traceql count_over_time", lang: "traceql", expr: "{} | count_over_time()",
			wantHead: HeadTempo, wantReplay: true, wantKind: KindMetricMatrix,
		},
		{
			name: "traceql quantile_over_time", lang: "traceql", expr: "{} | quantile_over_time(duration, .99)",
			wantHead: HeadTempo, wantReplay: true, wantKind: KindMetricMatrix,
		},
		{
			name: "traceql histogram_over_time", lang: "traceql", expr: "{} | histogram_over_time(duration)",
			wantHead: HeadTempo, wantReplay: true, wantKind: KindMetricMatrix,
		},
		{
			name: "traceql topk second stage", lang: "traceql", expr: "{} | rate() | topk(10)",
			wantHead: HeadTempo, wantReplay: true, wantKind: KindMetricMatrix,
		},
		{
			// A spanset filter with no metrics pipeline returns trace summaries, which
			// the trace-search comparator judges on trace identity plus field equality.
			name: "traceql trace search", lang: "traceql", expr: `{ span.foo = "bar" }`,
			wantHead: HeadTempo, wantReplay: true, wantKind: KindTraceSearch,
		},
		{
			name: "traceql trace search with structural operator", lang: "traceql", expr: `{ span.foo = "bar" } >> { span.baz = "qux" }`,
			wantHead: HeadTempo, wantReplay: true, wantKind: KindTraceSearch,
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
			wantKind: KindUnknownLang, reasonHasAll: []string{"cypher", "no parity lane", "no comparator can be selected"},
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
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if tc.wantReplay {
				if got.Reason != "" {
					t.Errorf("a replayable routing has a comparator, so it must carry no out-of-scope reason, got %+v", got)
				}
				return
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
