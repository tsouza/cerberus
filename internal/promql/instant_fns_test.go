package promql_test

import (
	"context"
	"testing"

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
// Filter (empty vector) instead of a 422 — and instead of reassembling a
// bucket ladder from rows that cannot hold one. An argument that MAY carry
// `le` (`up + up`: any series can) is not folded; the spec corpus pins its
// answer against Prometheus (histogram_quantile_non_bucket_arithmetic_empty).
func TestLower_HistogramQuantile_NonBucketInputFoldsEmpty(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	for q, wantFold := range map[string]bool{
		`histogram_quantile(0.9, sum(up))`:               true,
		`histogram_quantile(0.9, vector(1))`:             true,
		`histogram_quantile(0.9, sum by (job) (up) * 2)`: true,
		`histogram_quantile(0.9, up + up)`:               false,
		`histogram_quantile(0.9, sum by (le) (up))`:      false,
	} {
		expr, err := p.ParseExpr(q)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", q, err)
		}
		plan, err := promql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower(%q): %v", q, err)
		}
		f, isFilter := plan.(*chplan.Filter)
		folded := false
		if isFilter {
			lit, ok := f.Predicate.(*chplan.LitBool)
			folded = ok && !lit.V
		}
		if folded != wantFold {
			t.Fatalf("Lower(%q) folded to constant-false = %v, want %v (root %T)", q, folded, wantFold, plan)
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
