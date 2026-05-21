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
// The aggregated lowering (`lowerHistogramQuantileAgg`) sets
// `DropEmptyOnNoGroup: true` on the inner Aggregate so CH's default
// "1-row-of-zeros over empty input" for aggregates without GROUP BY
// is suppressed. The emit-time guard is `WHERE _cerb_n > 0` over a
// synthesised `count()` companion column. This test pins the guard's
// presence in the emitted SQL across a representative phi sweep —
// `phi ∈ {0.0, 0.1, 0.5, 0.95, 1.0}` covers the edge-case branches
// inside the quantile-interpolation expression (phi=0 → lowest bound,
// phi=1 → highest bound, mid-range → linear interp).
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

			// Walk to the inner Aggregate and assert DropEmptyOnNoGroup.
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
			if !agg.DropEmptyOnNoGroup {
				t.Fatalf("Aggregate.DropEmptyOnNoGroup = false for %q; histogram_quantile over empty input would emit a default row", query)
			}

			// The emitted SQL must guard the no-group aggregate with
			// `_cerb_n > 0` so CH's "1-row-of-zeros" behaviour can't
			// produce a synthesised quantile out of empty input.
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", query, err)
			}
			if !strings.Contains(sql, "`_cerb_n` > 0") {
				t.Errorf("emitted SQL for %q lacks the empty-input guard `_cerb_n > 0`\nSQL: %s", query, sql)
			}
		})
	}
}
