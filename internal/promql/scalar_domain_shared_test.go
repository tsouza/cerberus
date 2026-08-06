package promql_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// sharedDomainCase is one scalar parameter whose value reference
// Prometheus rejects, written twice: once as a literal, once computed
// from a vector. `value` is what BOTH spellings evaluate to.
type sharedDomainCase struct {
	name string
	// literalQuery spells the parameter as a constant, so lowering folds
	// it and the domain runs before any SQL exists.
	literalQuery string
	// computedQuery spells the SAME parameter as `scalar(<vector>)`, so
	// its value exists only once ClickHouse has run the parameter's own
	// subtree and the domain runs from the registered guard.
	computedQuery string
	// value is what the parameter evaluates to in both spellings.
	value float64
	// wantError is reference Prometheus's message, verbatim.
	wantError string
}

// TestScalarParameterDomainsAreShared is epic #1455's definition of
// done: "add one test asserting the literal and computed paths share a
// domain check, so the asymmetry cannot reappear for a new function".
//
// For every scalar parameter with an evaluation-time domain, the test
// drives the same out-of-domain value down BOTH lowering paths and
// requires the two to answer with the identical message:
//
//   - the literal spelling fails lowering outright, because the folded
//     value is available before any SQL exists; and
//   - the computed spelling lowers cleanly and registers exactly one
//     [promql.ScalarGuard], whose Check — applied to the value series
//     the parameter evaluates to — produces the verdict.
//
// A function whose domain is wired into only one of the two paths fails
// here: the literal-only case lowers the computed spelling with no
// guard registered, and the computed-only case lowers the literal
// spelling without error.
func TestScalarParameterDomainsAreShared(t *testing.T) {
	cases := []sharedDomainCase{
		{
			name:          "aggregation K is NaN",
			literalQuery:  `topk(NaN, up)`,
			computedQuery: `topk(scalar(k_metric), up)`,
			value:         math.NaN(),
			wantError:     "Parameter value is NaN",
		},
		{
			name:          "aggregation K overflows int64",
			literalQuery:  `topk(1e300, up)`,
			computedQuery: `topk(scalar(k_metric), up)`,
			value:         1e300,
			wantError:     "Scalar value 1e+300 overflows int64",
		},
		{
			name:          "limit_ratio ratio is NaN",
			literalQuery:  `limit_ratio(NaN, up)`,
			computedQuery: `limit_ratio(scalar(r_metric), up)`,
			value:         math.NaN(),
			wantError:     "Ratio value is NaN",
		},
		{
			name:          "smoothing factor outside (0, 1)",
			literalQuery:  `double_exponential_smoothing(up[5m], 1.5, 0.3)`,
			computedQuery: `double_exponential_smoothing(up[5m], scalar(f_metric), 0.3)`,
			value:         1.5,
			wantError:     "invalid smoothing factor. Expected: 0 < sf < 1, got: 1.500000",
		},
		{
			name:          "trend factor outside (0, 1)",
			literalQuery:  `double_exponential_smoothing(up[5m], 0.3, 2)`,
			computedQuery: `double_exponential_smoothing(up[5m], 0.3, scalar(f_metric))`,
			value:         2,
			wantError:     "invalid trend factor. Expected: 0 < tf < 1, got: 2.000000",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			literal := lowerForDomain(t, tc.literalQuery, nil)
			if literal == nil {
				t.Fatalf("literal spelling %q lowered without error — its domain check is missing", tc.literalQuery)
			}
			if literal.Error() != tc.wantError {
				t.Errorf("literal spelling %q: error = %q, want %q (reference Prometheus's own wording)",
					tc.literalQuery, literal.Error(), tc.wantError)
			}

			var guards []promql.ScalarGuard
			if err := lowerForDomain(t, tc.computedQuery, &guards); err != nil {
				t.Fatalf("computed spelling %q failed lowering with %v — the value is not knowable at lowering, "+
					"so the domain must be deferred to a guard, not raised here", tc.computedQuery, err)
			}
			if len(guards) != 1 {
				t.Fatalf("computed spelling %q registered %d scalar guards, want exactly 1 — "+
					"without one the domain check never runs on this path", tc.computedQuery, len(guards))
			}
			computed := guards[0].Check([]float64{tc.value})
			if computed == nil {
				t.Fatalf("computed spelling %q: guard %q accepted %v, which the literal path rejects",
					tc.computedQuery, guards[0].Name, tc.value)
			}
			if computed.Error() != literal.Error() {
				t.Errorf("computed spelling %q: guard error = %q, literal error = %q — "+
					"the two paths must reach the SAME domain function",
					tc.computedQuery, computed.Error(), literal.Error())
			}
		})
	}
}

// lowerForDomain lowers query as a range query over a fixed window,
// collecting scalar guards into sink when it is non-nil, and returns the
// lowering error (nil on success).
func lowerForDomain(t *testing.T, query string, sink *[]promql.ScalarGuard) error {
	t.Helper()
	expr, err := experimentalParser().ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, lerr := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		start, start.Add(5*time.Minute), time.Minute, promql.LowerOpts{Guards: sink})
	return lerr
}
