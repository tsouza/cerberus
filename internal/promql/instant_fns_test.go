package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_HistogramQuantile_NonBucketInputFoldsEmpty pins the
// non-selector histogram_quantile fallback for arguments whose label
// algebra proves they carry no `le`: reference Prometheus accepts ANY
// instant-vector second argument and skips float samples without an `le`
// label (info annotation, empty result), so these fold to a constant-false
// Filter (empty vector) instead of a 422 — and instead of reading the whole
// argument to reassemble a bucket ladder from rows that cannot hold one. A
// selector pinning a name without the `_bucket` suffix and with no `le`
// matcher carries no `le`, so `up * 2`, `abs(up)` and `info(up)` fold, in
// instant and range mode alike. An argument that MAY carry `le` (a
// `_bucket` selector, an `le` matcher, an unpinned name, a minted `le`, an
// info selector matching on `le`) is not folded. The spec
// corpus pins the `_bucket` and `le`-matcher answers against Prometheus
// (histogram_quantile_classic_bucket_arithmetic,
// histogram_quantile_le_matcher_unparseable_empty).
func TestLower_HistogramQuantile_NonBucketInputFoldsEmpty(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	for q, wantFold := range map[string]bool{
		`histogram_quantile(0.9, info(up))`:                             true,
		`histogram_quantile(0.9, info(up, {k8s_cluster_name=~".+"}))`:   true,
		`histogram_quantile(0.9, info(up, {le=~".+"}))`:                 false,
		`histogram_quantile(0.9, sum(up))`:                              true,
		`histogram_quantile(0.9, vector(1))`:                            true,
		`histogram_quantile(0.9, sum by (job) (up) * 2)`:                true,
		`histogram_quantile(0.9, up + up)`:                              true,
		`histogram_quantile(0.9, up * 2)`:                               true,
		`histogram_quantile(0.9, abs(up))`:                              true,
		`histogram_quantile(0.9, rate(requests_total[5m]) * 2)`:         true,
		`histogram_quantile(0.9, sum by (le) (up))`:                     true,
		`histogram_quantile(0.9, max_over_time((up * 2)[5m:1m]))`:       true,
		`histogram_quantile(0.9, request_latency_bucket * 2)`:           false,
		`histogram_quantile(0.9, abs(request_latency_bucket))`:          false,
		`histogram_quantile(0.9, up{le="1"} * 2)`:                       false,
		`histogram_quantile(0.9, {__name__=~"up.*"} * 2)`:               false,
		`histogram_quantile(0.9, label_replace(up, "le", "1", "", ""))`: false,
	} {
		expr, err := p.ParseExpr(q)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", q, err)
		}
		instant, err := promql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower(%q): %v", q, err)
		}
		rangeEnd := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		ranged, err := promql.LowerAtRange(context.Background(), expr, s, rangeEnd.Add(-time.Hour), rangeEnd, time.Minute)
		if err != nil {
			t.Fatalf("LowerAtRange(%q): %v", q, err)
		}
		for mode, plan := range map[string]chplan.Node{"instant": instant, "range": ranged} {
			f, isFilter := plan.(*chplan.Filter)
			folded := false
			if isFilter {
				lit, ok := f.Predicate.(*chplan.LitBool)
				folded = ok && !lit.V
			}
			if folded != wantFold {
				t.Errorf("%s Lower(%q) folded to constant-false = %v, want %v (root %T)", mode, q, folded, wantFold, plan)
			}
		}
	}
}

// TestLower_InstantFn_ComputedScalarArgs pins the computed-scalar
// acceptance for the instant-fn family: `scalar(<vector>)` (and any
// scalar-typed composition) is a valid bound / to_nearest / phi
// argument — reference Prometheus evaluates these per query, so a
// lowering-time rejection was a wrong rejection (rejection-parity
// catalogue: clamp / clamp_min / round / histogram_quantile entries).
// The bound rides a chplan.ScalarSubquery; the chdb round-trip
// fixtures (clamp_min_scalar_bound.txtar & friends) pin the values.
func TestLower_InstantFn_ComputedScalarArgs(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	for _, q := range []string{
		`clamp_max(up, scalar(other))`,
		`clamp_min(up, scalar(other))`,
		`clamp(up, scalar(other), 1)`,
		`clamp(up, 0, scalar(other) * 2)`,
		`round(temperature, scalar(other))`,
		`histogram_quantile(scalar(other), foo_bucket)`,
		`vector(scalar(up))`,
		`quantile(scalar(up), up)`,
	} {
		expr, err := p.ParseExpr(q)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", q, err)
		}
		if _, err := promql.Lower(context.Background(), expr, s); err != nil {
			t.Fatalf("Lower(%q): %v", q, err)
		}
	}
}
