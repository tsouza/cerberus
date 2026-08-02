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
// bogus quantile. `sum by(le)` legitimately leaves the inner Aggregate
// with no grouping keys — `le` is dropped (the distribution lives in
// the parallel arrays, not in Attributes) and the user asked for
// nothing else — so the guarantee cannot come from a grouping key. It
// comes from `DropEmptyOnNoGroup`, which the aggregated lowering
// (`lowerHistogramQuantileAgg`) always sets: the emitter renders the
// keyless aggregate with a companion row-count and rejects the
// synthesised row when the count is zero. This test pins the flag AND
// the emitted guard across a representative phi sweep — `phi ∈ {0.0,
// 0.1, 0.5, 0.95, 1.0}` covers the edge-case branches inside the
// quantile-interpolation expression (phi=0 → lowest bound, phi=1 →
// highest bound, mid-range → linear interp).
//
// The flag is only honoured when GroupBy is empty (see emit_node.go),
// so the test asserts emptiness first — a non-empty GroupBy would make
// the flag inert and the assertion vacuous.
//
// The test runs at the Go-unit-test layer (no chDB build tag) so the
// guarantee holds on every CI run, not just the chDB-tagged workflow.
// The semantic round-trip is pinned separately by
// test/spec/promql/histogram_quantile_agg_empty.txtar (chdb-only).
func TestHistogramQuantile_EmptyInput_DropsRow(t *testing.T) {
	t.Parallel()

	// emptyAggGuardAlias mirrors the emitter's own guard-column alias
	// (emitAggregateNoGroup); the SQL assertion below is what proves the
	// chplan flag actually reaches the query.
	const emptyAggGuardAlias = "_cerb_n"

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
			if len(agg.GroupBy) != 0 {
				t.Fatalf("Aggregate.GroupBy for %q = %#v, want no keys — `le` is dropped and nothing else was asked for, so any key here (a bucket layout above all) would split the one requested series", query, agg.GroupBy)
			}
			if !agg.DropEmptyOnNoGroup {
				t.Fatalf("Aggregate.DropEmptyOnNoGroup = false for %q; CH synthesises a 1-row-of-zeros result for a keyless aggregate over empty input, so histogram_quantile would emit a default row", query)
			}

			// The flag must survive emission — the count guard in the
			// SQL is what turns zero input rows into zero result rows.
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", query, err)
			}
			if !strings.Contains(sql, "`"+emptyAggGuardAlias+"` > 0") {
				t.Errorf("emitted SQL for %q lacks the `%s` > 0 empty-input guard, so an empty scan would synthesise a default quantile row\nSQL: %s", query, emptyAggGuardAlias, sql)
			}
		})
	}
}
