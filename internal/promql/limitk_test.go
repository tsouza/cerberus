package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// experimentalParser returns a PromQL parser with the experimental
// function set enabled so `limitk` / `limit_ratio` parse. The default
// parser used elsewhere in the package keeps experimental functions
// OFF, mirroring the production handler's pre-parse experimental gate.
func experimentalParser() parser.Parser {
	return parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
}

// TestLower_LimitK_Shape pins the lowered chplan + emitted SQL shape for
// PromQL's experimental `limitk(K, v)` aggregator. limitk returns up to
// K arbitrary series per aggregation group with their samples unchanged
// — no ranking. The lowering reuses chplan.TopK with Unordered=true, so
// the emitter renders `LIMIT K BY <partition>` with NO ORDER BY (the
// "arbitrary K-per-group" contract).
func TestLower_LimitK_Shape(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := experimentalParser()

	cases := []struct {
		name        string
		query       string
		wantK       int64
		wantLimitBy string // substring the emitted SQL must contain
	}{
		{
			name:        "by job",
			query:       `limitk(3, up) by (job)`,
			wantK:       3,
			wantLimitBy: "LIMIT 3 BY",
		},
		{
			name:        "without instance",
			query:       `limitk(2, up) without (instance)`,
			wantK:       2,
			wantLimitBy: "LIMIT 2 BY mapFilter",
		},
		{
			name:        "bare (no grouping)",
			query:       `limitk(5, up)`,
			wantK:       5,
			wantLimitBy: "LIMIT 5",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}
			tk, ok := plan.(*chplan.TopK)
			if !ok {
				t.Fatalf("Lower(%q) = %T, want *chplan.TopK", tc.query, plan)
			}
			if !tk.Unordered {
				t.Fatalf("Lower(%q): TopK.Unordered = false, want true (limitk has no ranking)", tc.query)
			}
			if tk.SortExpr != nil {
				t.Fatalf("Lower(%q): TopK.SortExpr = %#v, want nil (limitk does not rank)", tc.query, tk.SortExpr)
			}
			if tk.K != tc.wantK {
				t.Fatalf("Lower(%q): K = %d, want %d", tc.query, tk.K, tc.wantK)
			}

			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", tc.query, err)
			}
			if strings.Contains(sql, "ORDER BY") {
				t.Errorf("Emit(%q): SQL must not contain ORDER BY (limitk is unranked).\nSQL: %s", tc.query, sql)
			}
			if !strings.Contains(sql, tc.wantLimitBy) {
				t.Errorf("Emit(%q): SQL missing %q.\nSQL: %s", tc.query, tc.wantLimitBy, sql)
			}
		})
	}
}

// TestLower_LimitK_KDomain pins the reference-faithful K parameter
// domain for limitk — shared verbatim with topk/bottomk (topKDomain):
//
//   - K < 1 (0, negatives, sub-1 fractions) → an EMPTY result, not an
//     error: the lowering folds to a constant-false Filter over the
//     lowered input so the canonical column shape survives.
//   - Fractional K >= 1 truncates toward zero (`int64(fParam)`).
//   - NaN / int64-overflow K are rejected exactly where the reference
//     engine rejects them.
func TestLower_LimitK_KDomain(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := experimentalParser()

	t.Run("K below one folds to constant-false filter", func(t *testing.T) {
		t.Parallel()
		for _, q := range []string{
			`limitk(0, up)`,
			`limitk(-1, up)`,
			`limitk(0.5, up)`,
			`limitk(0, up) by (job)`,
		} {
			expr, err := p.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", q, err)
			}
			plan, err := promql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", q, err)
			}
			f, ok := plan.(*chplan.Filter)
			if !ok {
				t.Fatalf("Lower(%q) = %T, want *chplan.Filter", q, plan)
			}
			lit, ok := f.Predicate.(*chplan.LitBool)
			if !ok || lit.V {
				t.Fatalf("Lower(%q) predicate = %#v, want LitBool{false}", q, f.Predicate)
			}
		}
	})

	t.Run("fractional K truncates toward zero", func(t *testing.T) {
		t.Parallel()
		expr, err := p.ParseExpr(`limitk(2.9, up)`)
		if err != nil {
			t.Fatalf("ParseExpr: %v", err)
		}
		plan, err := promql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower: %v", err)
		}
		tk, ok := plan.(*chplan.TopK)
		if !ok {
			t.Fatalf("Lower = %T, want *chplan.TopK", plan)
		}
		if tk.K != 2 {
			t.Fatalf("K = %d, want 2 (int64 truncation of 2.9)", tk.K)
		}
	})
}

// TestLower_LimitK_Errors covers limitk's observable error contract:
// NaN / overflow K (shared with topKDomain) and the computed-K
// rejection (limitk(scalar(<vector>), v) is unsupported — CH's LIMIT
// needs a constant and limitk's arbitrary-selection gives no natural
// row_number() ordering to filter on).
func TestLower_LimitK_Errors(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := experimentalParser()

	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{
			name:    "NaN K rejected",
			query:   `limitk(NaN, up)`,
			wantErr: "K must not be NaN",
		},
		{
			name:    "overflow K rejected",
			query:   `limitk(1e300, up)`,
			wantErr: "overflows int64",
		},
		{
			name:    "computed K rejected",
			query:   `limitk(scalar(up), up)`,
			wantErr: "computed-K",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			_, err = promql.Lower(context.Background(), expr, s)
			if err == nil {
				t.Fatalf("Lower(%q): expected error containing %q, got nil", tc.query, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Lower(%q): error %q does not contain %q", tc.query, err.Error(), tc.wantErr)
			}
		})
	}
}
