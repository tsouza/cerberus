package traceql

import (
	"fmt"
	"testing"

	"github.com/tsouza/cerberus/test/property"
)

// testSpan is the compact per-span fixture shape these tests build
// datasets from. Mirrors gen/traceql.go's traceQLSpan, minus the
// timing fields the oracle doesn't read.
type testSpan struct {
	trace, id, parent string
	service, cluster  string
	method            string
	name              string
	status            string
	durationMs        int64
}

// buildDataset pivots testSpans into the property.Dataset shape
// gen/traceql.go's traceQLSpansToSeries produces, using the same
// reserved-label-key convention (see evaluator.go's spanViews).
func buildDataset(spans ...testSpan) property.Dataset {
	series := make([]property.SeriesData, 0, len(spans))
	for i, s := range spans {
		series = append(series, property.SeriesData{
			MetricName: s.name,
			Labels: map[string]string{
				"resource.service.name": s.service,
				"resource.cluster":      s.cluster,
				"span.http.method":      s.method,
				"__name__":              s.name,
				"__traceID__":           s.trace,
				"__spanID__":            s.id,
				"__parentSpanID__":      s.parent,
				"__status__":            s.status,
				"__duration_ns__":       fmt.Sprintf("%d", s.durationMs*nsPerMillisecond),
			},
			Points: []property.Point{{TimestampMs: int64(i), Value: float64(s.durationMs * nsPerMillisecond)}},
		})
	}
	return property.Dataset{Metrics: &property.MetricsModel{Series: series}}
}

// fixtureDataset is the shared trace shape every shape test below
// evaluates queries against:
//
//	t1: r1(api, east, GET, Ok, 50ms) -> c1(web, east, POST, Error, 100ms) -> g1(batch, west, GET, Unset, 300ms)
//	t2: r2(api, west, GET, Ok, 10ms)
//
// A linear 3-deep chain in t1 plus a lone root in t2 exercises both
// structural operators (`>`, `>>`) and gives count()/avg|min|max|sum
// a multi-span trace to aggregate over.
func fixtureDataset() property.Dataset {
	return buildDataset(
		testSpan{trace: "t1", id: "s1", parent: "0000000000000000", service: "api", cluster: "east", method: "GET", name: "r1", status: "Ok", durationMs: 50},
		testSpan{trace: "t1", id: "s2", parent: "s1", service: "web", cluster: "east", method: "POST", name: "c1", status: "Error", durationMs: 100},
		testSpan{trace: "t1", id: "s3", parent: "s2", service: "batch", cluster: "west", method: "GET", name: "g1", status: "Unset", durationMs: 300},
		testSpan{trace: "t2", id: "s10", parent: "0000000000000000", service: "api", cluster: "west", method: "GET", name: "r2", status: "Ok", durationMs: 10},
	)
}

func evalQ(t *testing.T, q string) property.Outcome {
	t.Helper()
	return Evaluate(fixtureDataset(), property.Query{String: q, EvalTs: 0})
}

// TestEvaluate_Shapes covers one representative query per generator
// shape (test/property/gen/traceql.go's TraceQLQuery doc), asserting
// the exact row count against the fixtureDataset by hand-derived
// expectation. Every case here failed with a parse or evaluation error
// before this change — the recognizer only understood the bare
// selector and `| count()` shapes.
func TestEvaluate_Shapes(t *testing.T) {
	cases := []struct {
		name      string
		query     string
		wantCount int
	}{
		{"shape0_bare_selector", `{ resource.service.name = "api" }`, 2},            // r1, r2
		{"shape0_bare_selector_no_match", `{ resource.service.name = "cache" }`, 0}, // no span uses this service

		{"shape1_count_satisfied", `{ resource.cluster = "east" } | count() = 2`, 1}, // t1 has 2 east-cluster spans
		{"shape1_count_unsatisfied", `{ resource.cluster = "east" } | count() = 3`, 0},

		{"shape2_attr_beyond_service", `{ resource.cluster = "west" }`, 2}, // g1, r2

		{"shape3_span_attr", `{ span.http.method = "POST" }`, 1}, // c1

		{"shape4_duration_gt", `{ duration > 100ms }`, 1}, // g1 (300ms)
		{"shape4_duration_le", `{ duration <= 10ms }`, 1}, // r2 (10ms)

		{"shape5_status_eq", `{ status = ok }`, 2},      // r1, r2
		{"shape5_status_neq", `{ status != error }`, 3}, // r1, g1, r2

		{"shape6_name_match", `{ name = "c1" }`, 1},
		{"shape6_name_no_match", `{ name = "nonexistent-span-name" }`, 0},

		{"shape7_regex_match", `{ resource.service.name =~ "^a.*" }`, 2},   // api, api
		{"shape7_regex_negated", `{ resource.service.name !~ "^a.*" }`, 2}, // web, batch

		{"shape8_negation", `{ resource.service.name != "api" }`, 2}, // web, batch

		{"shape9_multi_condition_match", `{ resource.service.name = "api" && status = ok }`, 2},
		{"shape9_multi_condition_narrows", `{ resource.cluster = "east" && span.http.method = "POST" }`, 1}, // c1 only, not r1

		{"shape10_structural_child_match", `{ resource.service.name = "api" } > { resource.service.name = "web" }`, 1},      // c1's parent r1 is api
		{"shape10_structural_child_no_match", `{ resource.service.name = "batch" } > { resource.service.name = "api" }`, 0}, // roots have no matching parent

		{"shape11_structural_descendant_match", `{ resource.service.name = "api" } >> { resource.service.name = "batch" }`, 1}, // g1's ancestor r1 is api
		{"shape11_structural_descendant_excludes_self", `{ resource.service.name = "web" } >> { resource.service.name = "web" }`, 0},

		{"shape12_avg", `{ resource.cluster = "east" } | avg(duration) > 60ms`, 1}, // (50+100)/2=75 > 60
		{"shape12_avg_unsatisfied", `{ resource.cluster = "east" } | avg(duration) > 80ms`, 0},
		{"shape12_min", `{ resource.cluster = "east" } | min(duration) <= 50ms`, 1},
		{"shape12_max", `{ resource.cluster = "east" } | max(duration) >= 100ms`, 1},
		{"shape12_sum", `{ resource.cluster = "east" } | sum(duration) = 150ms`, 1},

		{"shape13_select", `{ resource.service.name = "api" } | select(span.http.method)`, 2}, // same row count as the bare selector
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := evalQ(t, tc.query)
			if out.Err != nil {
				t.Fatalf("query %q: unexpected error: %v", tc.query, out.Err)
			}
			if len(out.Rows) != tc.wantCount {
				t.Fatalf("query %q: row count = %d, want %d", tc.query, len(out.Rows), tc.wantCount)
			}
		})
	}
}

// TestEvaluate_ParseErrors asserts the recognizer rejects shapes
// outside the generator's accept-set rather than silently
// misevaluating them.
func TestEvaluate_ParseErrors(t *testing.T) {
	for _, q := range []string{
		"not a traceql query at all",
		`{ resource.service.name = "api" `,              // unbalanced brace
		`{ span.unknown.attr = "x" }`,                   // attribute path outside the generator's pool
		`{ resource.service.name = "api" } | avg() > 1`, // avg() needs an argument
	} {
		out := evalQ(t, q)
		if out.Err == nil {
			t.Fatalf("query %q: expected a parse error, got rows=%d", q, len(out.Rows))
		}
	}
}
