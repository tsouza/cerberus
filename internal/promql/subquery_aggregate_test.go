package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerSubquery_Aggregate_Errors covers the error contracts on the
// subquery-over-aggregation paths: computed K / phi / label-name shapes
// are rejected the same way they are in instant mode (see
// aggregate_test.go) so callers see a clear error rather than a SQL
// runtime failure.
func TestLowerSubquery_Aggregate_Errors(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{
			// Reference Prometheus errors on a NaN K; K < 1 shapes are
			// NOT errors — they return an empty result (covered by
			// TestLowerSubqueryTopK_KDomain). Same domain as instant
			// mode via the shared topKDomain helper.
			name:    "topk K must not be NaN",
			query:   `max_over_time(topk(NaN, rate(http_requests_total[1m]))[1h:30s])`,
			wantErr: "Parameter value is NaN",
		},
		{
			name:    "topk K must not overflow int64",
			query:   `max_over_time(topk(1e300, rate(http_requests_total[1m]))[1h:30s])`,
			wantErr: "overflows int64",
		},
		{
			name:    "count_values requires non-empty label",
			query:   `max_over_time(count_values("", up)[5m:1m])`,
			wantErr: "non-empty label name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			_, err = promql.Lower(context.Background(), expr, s)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestLowerSubqueryTopK_KDomain pins the subquery-over-topk variant of
// the reference K domain: K < 1 folds to a constant-false Filter over
// the inner matrix (empty result, canonical 3-column shape preserved
// for the wrapping reducer), and fractional K >= 1 truncates toward
// zero — the same topKDomain contract the instant path pins in
// TestLowerTopK_KDomain.
func TestLowerSubqueryTopK_KDomain(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	for _, q := range []string{
		`max_over_time(topk(0, rate(http_requests_total[1m]))[1h:30s])`,
		`max_over_time(topk(1.5, rate(http_requests_total[1m]))[1h:30s])`,
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

// TestLowerSubquery_Aggregate_ComputedPhi pins the computed-phi
// acceptance on the subquery-over-quantile path — the parity-corpus
// shape `max_over_time(quantile(scalar(up), up)[5m:1m])` lowers (and
// emits) instead of 422ing; reference Prometheus evaluates phi per
// step and a NaN phi is a NaN result, not an error.
func TestLowerSubquery_Aggregate_ComputedPhi(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	expr, err := p.ParseExpr(`max_over_time(quantile(scalar(up), up)[5m:1m])`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	if _, err := promql.Lower(context.Background(), expr, s); err != nil {
		t.Fatalf("Lower: %v", err)
	}
}

// TestLowerSubqueryTopK_ComputedK pins the computed-K acceptance on the
// subquery-over-topk/bottomk/limitk paths. Reference Prometheus
// type-checks K as scalar-valued and answers these; cerberus routes
// them through chplan.TopK's KExpr slot (rank filter), so the lowered
// tree is a TopK with KExpr set and no literal K.
func TestLowerSubqueryTopK_ComputedK(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	// limitk is experimental, so this case needs the experimental parser;
	// topk / bottomk parse under either.
	p := experimentalParser()

	for _, q := range []string{
		`max_over_time(topk(scalar(up), rate(http_requests_total[1m]))[1h:30s])`,
		`max_over_time(bottomk(scalar(up), rate(http_requests_total[1m]))[1h:30s])`,
		`max_over_time(limitk(scalar(up), rate(http_requests_total[1m]))[1h:30s])`,
	} {
		expr, err := p.ParseExpr(q)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", q, err)
		}
		node, err := promql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower(%q): %v", q, err)
		}
		topk := findTopK(node)
		if topk == nil {
			t.Fatalf("Lower(%q): no chplan.TopK in the lowered tree", q)
		}
		if topk.KExpr == nil {
			t.Fatalf("Lower(%q): TopK.KExpr is nil, want the computed-K subtree", q)
		}
		if topk.K != 0 {
			t.Fatalf("Lower(%q): TopK.K = %d, want 0 (KExpr and K are exclusive)", q, topk.K)
		}
	}
}

// findTopK walks a lowered plan tree and returns the first chplan.TopK
// it finds, or nil.
func findTopK(n chplan.Node) *chplan.TopK {
	var found *chplan.TopK
	chplan.Walk(n, func(node chplan.Node) bool {
		if found != nil {
			return false
		}
		if t, ok := node.(*chplan.TopK); ok {
			found = t
			return false
		}
		return true
	})
	return found
}
