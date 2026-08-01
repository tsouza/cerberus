package promql

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestHistogramQuantile_EmptyInput_DropsRow pins task #216's N6
// regression: when the underlying histogram has zero matching rows,
// `histogram_quantile(phi, sum by(le)(rate(<X>_bucket[r])))` must
// return EMPTY across the entire phi range — not a synthesised default
// quantile (the user-visible "4.75 with metric:{}" wire shape).
//
// CH synthesises a 1-row-of-zeros result for an aggregate with NO
// GROUP BY over empty input, and that row is what surfaced as the
// bogus quantile. The aggregated lowering (`lowerHistogramQuantileAgg`)
// forecloses it structurally: `classicBucketAggGroupBy` always appends
// the bucket-layout key, so the inner Aggregate carries at least one
// grouping key even for `sum(...)` / `sum by(le)(...)`, where the
// user's own clause contributes none. Zero input rows then yield zero
// GROUPS, hence zero output rows — no filter required, and nothing for
// a `count()`-companion guard to reject. This test pins that key's
// presence across a representative phi sweep — `phi ∈ {0.0, 0.1, 0.5,
// 0.95, 1.0}` covers the edge-case branches inside the
// quantile-interpolation expression (phi=0 → lowest bound, phi=1 →
// highest bound, mid-range → linear interp).
//
// Asserting the grouping key rather than `DropEmptyOnNoGroup` is
// deliberate: the emitter applies that flag only when GroupBy is empty
// (see emit_node.go), so with a layout key present the flag can no
// longer fail, and an assertion on it would pass regardless of whether
// the guarantee holds.
//
// The test runs at the Go-unit-test layer (no chDB build tag) so the
// guarantee holds on every CI run, not just the chDB-tagged workflow.
// The semantic round-trip is pinned separately by
// test/spec/promql/histogram_quantile_agg_empty.txtar (chdb-only).
func TestHistogramQuantile_EmptyInput_DropsRow(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	for _, phi := range []float64{0.0, 0.1, 0.5, 0.95, 1.0} {
		phi := phi
		t.Run(fmt.Sprintf("phi=%.2f", phi), func(t *testing.T) {
			t.Parallel()
			query := fmt.Sprintf(
				`histogram_quantile(%g, sum by (le) (rate(missing_metric_bucket[5m])))`,
				phi,
			)
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", query, err)
			}

			// Walk to the inner Aggregate and assert it groups.
			var agg *chplan.Aggregate
			var walk func(chplan.Node)
			walk = func(n chplan.Node) {
				if n == nil || agg != nil {
					return
				}
				if a, ok := n.(*chplan.Aggregate); ok {
					agg = a
					return
				}
				for _, c := range n.Children() {
					walk(c)
				}
			}
			walk(plan)
			if agg == nil {
				t.Fatalf("no Aggregate node in plan for %q", query)
			}
			if len(agg.GroupBy) == 0 {
				t.Fatalf("Aggregate.GroupBy is empty for %q; CH synthesises a 1-row-of-zeros result for a no-GROUP-BY aggregate over empty input, so histogram_quantile would emit a default row", query)
			}
			last, ok := agg.GroupBy[len(agg.GroupBy)-1].(*chplan.ColumnRef)
			if !ok || last.Name != s.ExplicitBoundsColumn {
				t.Fatalf("Aggregate.GroupBy for %q does not end in the %s layout key: %#v", query, s.ExplicitBoundsColumn, agg.GroupBy)
			}

			// The grouping must survive emission — a GROUP BY in the
			// SQL is what turns zero input rows into zero result rows.
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", query, err)
			}
			if !strings.Contains(sql, "GROUP BY `"+s.ExplicitBoundsColumn+"`") {
				t.Errorf("emitted SQL for %q lacks GROUP BY `%s`, so an empty scan would synthesise a default quantile row\nSQL: %s", query, s.ExplicitBoundsColumn, sql)
			}
		})
	}
}
