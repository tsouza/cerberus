package promql_test

import (
	"context"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// stepGridAnchorAlias is the column a per-step fan-out names each
// evaluation anchor with, mirrored from the lowering's own constant. A
// test that read the constant through the package under test could not
// catch the constant itself changing out from under the emitted SQL.
const stepGridAnchorAlias = "anchor_ts"

// TestHistogramQuantileComputedPhiKeysOnItsOwnScope pins the column a
// computed phi's per-step lookup is subscripted by against the column the
// scope it lands in actually exposes.
//
// A computed phi (`histogram_quantile(scalar(x), …)`) lowers, in range
// mode, to a per-step map subscripted by the current evaluation step. The
// range fan-out lowerings evaluate above a per-anchor fan-out whose scope
// names the step `anchor_ts` and carries no Sample timestamp column at
// all, so a lookup keyed on the Sample timestamp column names an
// identifier that does not exist there. ClickHouse 24.8 — the declared
// minimum base — rejects that outright; later versions resolve it from an
// enclosing scope and answer, which is why this has to be asserted on the
// plan rather than left to a roundtrip.
func TestHistogramQuantileComputedPhiKeysOnItsOwnScope(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()

	cases := []struct {
		name  string
		query string
		// wantRange is the column the phi lookup must key on when the
		// query is evaluated over a step grid.
		wantRange string
	}{
		{
			name:      "classic agg idiom",
			query:     `histogram_quantile(scalar(vector(0.9)), sum by(le) (rate(http_duration_bucket[1m])))`,
			wantRange: stepGridAnchorAlias,
		},
		{
			name:      "classic rate without aggregation",
			query:     `histogram_quantile(scalar(vector(0.9)), rate(http_duration_bucket[1m]))`,
			wantRange: stepGridAnchorAlias,
		},
		{
			name:      "classic bare selector",
			query:     `histogram_quantile(scalar(vector(0.9)), http_duration_bucket)`,
			wantRange: stepGridAnchorAlias,
		},
		{
			name:      "native bare selector",
			query:     `histogram_quantile(scalar(vector(0.9)), http_duration_exp_hist)`,
			wantRange: stepGridAnchorAlias,
		},
		{
			name:      "native agg idiom",
			query:     `histogram_quantile(scalar(vector(0.9)), sum(rate(http_duration_exp_hist[1m])))`,
			wantRange: stepGridAnchorAlias,
		},
		{
			// A shaping aggregation has no per-`le` rung fold, so the
			// argument goes through the ordinary sample pipeline and phi
			// lands in a row position that carries the Sample timestamp
			// column.
			name:      "shaping aggregation falls back to float buckets",
			query:     `histogram_quantile(scalar(vector(0.9)), topk(2, rate(http_duration_bucket[1m])))`,
			wantRange: s.TimestampColumn,
		},
		{
			// An absolute `@` pins one window for the whole query, so the
			// quantile is evaluated once by the instant lowering and
			// broadcast across the grid. Reference Prometheus still
			// re-evaluates the scalar argument at every step, so phi stays
			// per-step — keyed on the instant scope's own Sample timestamp
			// column.
			name:      "absolute @ pin evaluates once and broadcasts",
			query:     `histogram_quantile(scalar(vector(0.9)), rate(http_duration_bucket[1m] @ 1000))`,
			wantRange: s.TimestampColumn,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			t.Run("range", func(t *testing.T) {
				t.Parallel()
				assertPhiScopeColumns(t, tc.query, true, s, []string{tc.wantRange})
			})
			t.Run("instant", func(t *testing.T) {
				t.Parallel()
				// A single evaluation binds phi once for the whole
				// statement, so there is no per-step lookup and phi reads
				// nothing from the scope it lands in.
				assertPhiScopeColumns(t, tc.query, false, s, nil)
			})
		})
	}
}

// perStepLookupKey matches every step-key rendering in the emitted SQL,
// capturing the column that names the evaluation step and, in the second
// group, the alias clause that marks the one rendering which is NOT a
// lookup: the key column the per-step map is BUILT from, inside the
// scalar subquery's own scope.
var perStepLookupKey = regexp.MustCompile("toUnixTimestamp64Nano\\(`?([A-Za-z_][A-Za-z0-9_]*)`?\\)( AS )?")

// TestHistogramQuantileRungFoldScalarKeysOnTheAnchor covers the OTHER
// computed scalar a histogram_quantile call can carry: the parameter of a
// `quantile by(le)(...)` rung fold, which is embedded in the same
// anchor-keyed fan-out scope as phi. Asserting on the emitted SQL rather
// than on one plan field catches every per-step binding that reaches that
// scope, whichever argument put it there.
func TestHistogramQuantileRungFoldScalarKeysOnTheAnchor(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	const query = `histogram_quantile(0.9, quantile by(le) (scalar(vector(0.5)), rate(http_duration_bucket[1m])))`

	plan := lowerHistogramQuery(t, query, true, s)
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	var lookups int
	for _, k := range perStepLookupKey.FindAllStringSubmatch(sql, -1) {
		if k[2] != "" {
			continue
		}
		lookups++
		if k[1] != stepGridAnchorAlias {
			t.Errorf("%q: per-step lookup keyed on %q, want %q — the rung fold is evaluated in the "+
				"fan-out's anchor-keyed scope, which exposes no Sample timestamp column",
				query, k[1], stepGridAnchorAlias)
		}
	}
	if lookups == 0 {
		t.Fatalf("%q emitted no per-step scalar lookup — the computed rung-fold parameter "+
			"stopped binding per step, so this test asserts nothing", query)
	}
}

// assertPhiScopeColumns lowers query and asserts that the set of columns
// the computed phi expression reads from its enclosing scope is exactly
// want.
func assertPhiScopeColumns(t *testing.T, query string, rangeMode bool, s schema.Metrics, want []string) {
	t.Helper()

	plan := lowerHistogramQuery(t, query, rangeMode, s)

	phis := collectPhiExprs(plan)
	if len(phis) == 0 {
		t.Fatalf("%q lowered to a plan with no computed phi expression — "+
			"the lowering stopped exercising the computed-phi path, so this case asserts nothing", query)
	}
	for _, phi := range phis {
		got := scopeColumnsOf(phi)
		if !slices.Equal(got, want) {
			t.Errorf("%q (range=%v): phi reads %v from its enclosing scope, want %v — "+
				"a phi keyed on a column its own scope does not expose is an unknown identifier "+
				"on the minimum supported ClickHouse base",
				query, rangeMode, got, want)
		}
	}
}

func lowerHistogramQuery(t *testing.T, query string, rangeMode bool, s schema.Metrics) chplan.Node {
	t.Helper()

	expr, err := experimentalParser().ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !rangeMode {
		plan, err := promql.LowerAt(context.Background(), expr, s, start, start)
		if err != nil {
			t.Fatalf("LowerAt(%q): %v", query, err)
		}
		return plan
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s,
		start, start.Add(5*time.Minute), time.Minute, promql.LowerOpts{})
	if err != nil {
		t.Fatalf("LowerAtRangeOpts(%q): %v", query, err)
	}
	return plan
}

// collectPhiExprs returns every runtime-computed phi expression in plan,
// from both the classic and the native quantile nodes.
func collectPhiExprs(plan chplan.Node) []chplan.Expr {
	var out []chplan.Expr
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		switch hq := n.(type) {
		case *chplan.HistogramQuantile:
			if hq.PhiExpr != nil {
				out = append(out, hq.PhiExpr)
			}
		case *chplan.HistogramQuantileNative:
			if hq.PhiExpr != nil {
				out = append(out, hq.PhiExpr)
			}
		}
		return true
	})
	return out
}

// scopeColumnsOf returns the sorted, deduplicated set of columns e reads
// from the scope it is embedded in. A ScalarSubquery opens a scope of its
// own, so its body is deliberately not descended into.
func scopeColumnsOf(e chplan.Expr) []string {
	seen := map[string]struct{}{}
	var walk func(chplan.Expr)
	walk = func(e chplan.Expr) {
		switch v := e.(type) {
		case *chplan.ColumnRef:
			seen[v.Name] = struct{}{}
		case *chplan.FuncCall:
			for _, a := range v.Args {
				walk(a)
			}
		case *chplan.ScalarSubquery:
			// A separate scope — its column references resolve against
			// its own input, not against the scope phi lands in.
		}
	}
	walk(e)

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}
