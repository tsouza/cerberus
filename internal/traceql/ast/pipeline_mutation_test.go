package ast

import "testing"

// Mutation-coverage tests for pipeline.go / metrics.go: the round-trip
// rendering of whole queries and the metrics-stage accessors.

// TestRootExprStringRoundTrip pins how a RootExpr renders the optional metrics
// first/second stages. The guards `if r.MetricsPipeline != nil` and
// `if r.MetricsSecondStage != nil` decide whether each ` | <stage>` segment is
// emitted; negating either drops a present stage or dereferences a nil one.
func TestRootExprStringRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query string
		want  string
	}{
		{`{ .x = 1 }`, "{ .x = 1 }"},
		{`{ .a > 1 } | by(.b)`, "{ .a > 1 } | by(.b)"},
		{`{} | rate()`, "{ true } | rate()"},
		{`{} | rate() by(.x)`, "{ true } | rate() by(.x)"},
		{`{} | count_over_time()`, "{ true } | count_over_time()"},
		{`{} | quantile_over_time(.x, 0.9)`, "{ true } | quantile_over_time(.x, 0.9)"},
		// A quantile that needs more than one significant digit pins the `-1`
		// (shortest) precision argument to FormatFloat: a mutant that turns it
		// into `1` would render this as `1`.
		{`{} | quantile_over_time(.x, 0.99)`, "{ true } | quantile_over_time(.x, 0.99)"},
		{`{} | quantile_over_time(.x, 0.95, 0.5)`, "{ true } | quantile_over_time(.x, 0.95, 0.5)"},
		// A `with(...)` hint clause is rendered only when present. Negating the
		// `r.Hints != nil` guard drops it.
		{`{} | rate() with(sample=true)`, "{ true } | rate() with(sample=true)"},
	}
	for _, c := range cases {
		if got := mustParse(t, c.query).String(); got != c.want {
			t.Errorf("Parse(%q).String() = %q; want %q", c.query, got, c.want)
		}
	}

	// A second stage must be rendered (guards the MetricsSecondStage branch:
	// negating it drops the topk segment, or panics on a nil deref for the
	// plain query above).
	if got := mustParse(t, `{} | rate() | topk(3)`).String(); got == "" || got[len(got)-len("topk(3)"):] != "topk(3)" {
		t.Errorf("metrics second stage String() = %q; want it to end with topk(3)", got)
	}
}

// TestMetricsAggregateAccessors pins the first-stage accessors used by
// lowering: the function op, the aggregated attribute, the group-by set, and
// the quantiles.
func TestMetricsAggregateAccessors(t *testing.T) {
	t.Parallel()

	agg, ok := mustParse(t, `{} | rate() by(.svc)`).MetricsPipeline.(*MetricsAggregate)
	if !ok {
		t.Fatalf("MetricsPipeline = %T; want *MetricsAggregate", mustParse(t, `{} | rate() by(.svc)`).MetricsPipeline)
	}
	if agg.Op() != MetricsAggregateRate {
		t.Errorf("Op() = %v; want rate", agg.Op())
	}
	if by := agg.GroupBy(); len(by) != 1 || by[0].Name != "svc" {
		t.Errorf("GroupBy() = %v; want [.svc]", by)
	}

	q, ok := mustParse(t, `{} | quantile_over_time(.dur, 0.5, 0.9)`).MetricsPipeline.(*MetricsAggregate)
	if !ok {
		t.Fatalf("quantile MetricsPipeline = %T; want *MetricsAggregate", q)
	}
	if q.Op() != MetricsAggregateQuantileOverTime {
		t.Errorf("Op() = %v; want quantile_over_time", q.Op())
	}
	if q.Attribute().Name != "dur" {
		t.Errorf("Attribute().Name = %q; want dur", q.Attribute().Name)
	}
}

// TestChainedSecondStageString pins that a chained second stage renders each
// element prefixed by its index-aligned separator. Negating the
// `i < len(c.separators)` guard drops every separator from the output.
func TestChainedSecondStageString(t *testing.T) {
	t.Parallel()
	c := newChainedSecondStage()
	c.Append(&TopKBottomK{op: OpTopK, limit: 3}, "A")
	c.Append(&TopKBottomK{op: OpBottomK, limit: 2}, "B")
	if got := c.String(); got != "Atopk(3)Bbottomk(2)" {
		t.Errorf("ChainedSecondStage.String() = %q; want Atopk(3)Bbottomk(2)", got)
	}
	if el := c.Elements(); len(el) != 2 {
		t.Errorf("Elements() len = %d; want 2", len(el))
	}
	if sep := c.Separators(); len(sep) != 2 || sep[0] != "A" || sep[1] != "B" {
		t.Errorf("Separators() = %v; want [A B]", sep)
	}
}

// TestTopKBottomKString pins the second-stage rendering.
func TestTopKBottomKString(t *testing.T) {
	t.Parallel()
	if got := (&TopKBottomK{op: OpTopK, limit: 4}).String(); got != "topk(4)" {
		t.Errorf("TopKBottomK.String() = %q; want topk(4)", got)
	}
	if got := (&TopKBottomK{op: OpBottomK, limit: 9}).String(); got != "bottomk(9)" {
		t.Errorf("TopKBottomK.String() = %q; want bottomk(9)", got)
	}
}
