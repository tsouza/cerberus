package promql

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// emptyAggGuardAlias mirrors the emitter's own guard-column alias
// (chsql.emitAggregateNoGroup). The SQL assertions in this file are what
// prove the chplan flag actually reaches the query.
const emptyAggGuardAlias = "_cerb_n"

// emptyAggGuardSQL is the one token the guard contributes to the emitted
// query — `WHERE `_cerb_n` > 0`. Counting it is how a test attributes
// guards to plan nodes without re-parsing the SQL.
const emptyAggGuardSQL = "`" + emptyAggGuardAlias + "` > 0"

// countGuardedAggregates returns how many Aggregate nodes in the plan
// satisfy the emitter's guard condition verbatim (see
// chsql/emit_node.go::emitAggregate): zero grouping keys AND
// DropEmptyOnNoGroup. This is the plan-side half of the invariant the
// SQL-side count is compared against.
func countGuardedAggregates(root chplan.Node) int {
	n := 0
	var walk func(chplan.Node)
	walk = func(node chplan.Node) {
		if node == nil {
			return
		}
		if a, ok := node.(*chplan.Aggregate); ok && len(a.GroupBy) == 0 && a.DropEmptyOnNoGroup {
			n++
		}
		for _, c := range node.Children() {
			walk(c)
		}
	}
	walk(root)
	return n
}

// histogramPlanShape counts the two collapse nodes a classic-histogram
// plan can use and reports whether the plan reached the histogram path
// at all. A "0 guards expected" case would pass vacuously against a plan
// that degenerated to an empty-result Filter — precisely how #1569
// presented — so every case asserts the shape before the guard count.
type histogramPlanShape struct {
	aggregates           int
	rangeBucketFanouts   int
	hasHistogramQuantile bool
}

// histogramCollapseKind names the node a classic-histogram plan uses to
// collapse its `le` group down to one value per output series. Only the
// Aggregate kind has a keyless mode, so only it can carry the guard.
type histogramCollapseKind string

const (
	// collapseAggregate is the instant-query shape: a chplan.Aggregate
	// whose GroupBy arity is exactly what this test reads.
	collapseAggregate histogramCollapseKind = "Aggregate"
	// collapseRangeBucketFanout is the range-query shape. The fanout
	// collapses per (series, anchor) structurally, has no
	// DropEmptyOnNoGroup concept, and therefore can never be guarded.
	collapseRangeBucketFanout histogramCollapseKind = "RangeBucketFanout"
)

func inspectHistogramPlan(root chplan.Node) histogramPlanShape {
	var shape histogramPlanShape
	var walk func(chplan.Node)
	walk = func(node chplan.Node) {
		if node == nil {
			return
		}
		switch node.(type) {
		case *chplan.Aggregate:
			shape.aggregates++
		case *chplan.RangeBucketFanout:
			shape.rangeBucketFanouts++
		case *chplan.HistogramQuantile:
			shape.hasHistogramQuantile = true
		}
		for _, c := range node.Children() {
			walk(c)
		}
	}
	walk(root)
	return shape
}

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
			if !strings.Contains(sql, emptyAggGuardSQL) {
				t.Errorf("emitted SQL for %q lacks the %s empty-input guard, so an empty scan would synthesise a default quantile row\nSQL: %s", query, emptyAggGuardSQL, sql)
			}
		})
	}
}

// TestHistogramQuantile_EmptyGuardTracksGroupingKeyArity pins the
// biconditional the classic-bucket layout merge made load-bearing: the
// empty-input guard appears in the emitted SQL **iff** an Aggregate has
// zero grouping keys. Not "usually", not "for the queries we happened to
// look at" — exactly.
//
// Before the layout merge, every classic-bucket Aggregate carried the
// bucket layout (`ExplicitBounds`) as a grouping key, so no histogram
// plan was ever keyless and this guard never appeared on that path.
// Dropping that key is what makes `sum by (le)` keyless, and the guard
// is what stops CH's 1-row-of-zeros for a keyless aggregate from
// surfacing as a synthesised quantile. Nothing but arity may move it: a
// later change that re-keys or de-keys one of these aggregates would
// otherwise only show up as a `_cerb_n` appearing in or vanishing from
// regenerated migration explain goldens, which reads as "regenerate
// them" rather than as a semantic change.
//
// The assertion is structural rather than textual. It counts the
// Aggregates the emitter would guard — `len(GroupBy) == 0 &&
// DropEmptyOnNoGroup`, the condition verbatim from
// chsql/emit_node.go::emitAggregate — and requires the emitted SQL to
// carry exactly that many guards. Each case additionally declares the
// count it expects, so the table spans BOTH sides of the biconditional:
// a mutation that suppresses the guard reddens the keyless rows, and one
// that emits it unconditionally reddens the keyed rows.
//
// The two guard-free shapes are distinct and not interchangeable:
//   - a user label survives the `le` drop (`sum by (le, job)`), so the
//     Aggregate is keyed;
//   - the query is a RANGE query, which collapses through
//     [chplan.RangeBucketFanout] — keyed by (series, anchor) — instead
//     of an Aggregate. `DropEmptyOnNoGroup` is an Aggregate-only
//     concept, so the guard cannot arise there at all. Note the fanout's
//     own `GroupBy` may legitimately be empty for `sum by (le)` (the
//     anchor key is structural, not a `GroupBy` entry); that emptiness
//     is #1584's subject and is deliberately not what this test reads.
func TestHistogramQuantile_EmptyGuardTracksGroupingKeyArity(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	// Any fixed instant/range works — the invariant is about plan
	// shape, not about time. Pinning them keeps the test deterministic.
	var (
		rangeStart = time.Unix(1_700_000_000, 0).UTC()
		rangeEnd   = rangeStart.Add(10 * time.Minute)
		rangeStep  = 30 * time.Second
	)

	cases := []struct {
		name string
		// query is a classic-histogram histogram_quantile in every
		// case; only the grouping and the instant/range posture move.
		query string
		// rangeQuery selects LowerAtRange over Lower.
		rangeQuery bool
		// wantCollapse is the node the plan must use to collapse the
		// `le` group. Asserting it first is what stops a "0 guards"
		// expectation from passing against a plan that never reached
		// the shape under test.
		wantCollapse histogramCollapseKind
		// wantGuarded is how many Aggregates should be keyless, and
		// therefore how many guards the SQL should carry.
		wantGuarded int
		// why documents the arity, so a future failure reads as a
		// semantic claim rather than as a number to re-baseline.
		why string
	}{
		{
			name:         "instant_sum_by_le_is_keyless",
			query:        `histogram_quantile(0.9, sum by (le) (rate(http_server_request_duration_bucket[5m])))`,
			wantCollapse: collapseAggregate,
			wantGuarded:  1,
			why:          "`le` is dropped (the distribution lives in the parallel arrays) and the layout is no longer a key, so nothing remains",
		},
		{
			name:         "instant_max_by_le_is_keyless",
			query:        `histogram_quantile(0.9, max by (le) (rate(http_server_request_duration_bucket[5m])))`,
			wantCollapse: collapseAggregate,
			wantGuarded:  1,
			why:          "the non-sum rung fold takes the same keyless shape as sum",
		},
		{
			name:         "instant_sum_by_le_job_keeps_the_user_label",
			query:        `histogram_quantile(0.9, sum by (le, job) (rate(http_server_request_duration_bucket[5m])))`,
			wantCollapse: collapseAggregate,
			wantGuarded:  0,
			why:          "`job` survives the `le` drop, so the aggregate is keyed and CH emits no synthetic row",
		},
		{
			name:         "range_sum_by_le_collapses_through_the_fanout",
			query:        `histogram_quantile(0.9, sum by (le) (rate(http_server_request_duration_bucket[5m])))`,
			rangeQuery:   true,
			wantCollapse: collapseRangeBucketFanout,
			wantGuarded:  0,
			why:          "the range path collapses through RangeBucketFanout, which has no Aggregate and therefore no keyless mode, so the Aggregate-only guard cannot apply at all",
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

			var plan chplan.Node
			if tc.rangeQuery {
				plan, err = LowerAtRange(context.Background(), expr, s, rangeStart, rangeEnd, rangeStep)
			} else {
				plan, err = Lower(context.Background(), expr, s)
			}
			if err != nil {
				t.Fatalf("lowering %q: %v", tc.query, err)
			}

			// Guard the guard: a plan that fell off the classic-bucket
			// path, or that collapses through the other node, would
			// satisfy a "0 guards" expectation for the wrong reason.
			shape := inspectHistogramPlan(plan)
			if !shape.hasHistogramQuantile {
				t.Fatalf("plan for %q has no HistogramQuantile node — it fell off the classic-bucket path, so any guard-count assertion below would be vacuous", tc.query)
			}
			switch tc.wantCollapse {
			case collapseAggregate:
				if shape.aggregates == 0 {
					t.Fatalf("plan for %q has no Aggregate node, so grouping-key arity is undefined and the guard assertion below would be vacuous", tc.query)
				}
				if shape.rangeBucketFanouts != 0 {
					t.Fatalf("plan for %q collapses through %d RangeBucketFanout(s), want %s — the case's guard expectation is written against the other shape", tc.query, shape.rangeBucketFanouts, collapseAggregate)
				}
			case collapseRangeBucketFanout:
				if shape.rangeBucketFanouts == 0 {
					t.Fatalf("plan for %q has no RangeBucketFanout node, want %s — the case's guard expectation is written against the other shape", tc.query, collapseRangeBucketFanout)
				}
				if shape.aggregates != 0 {
					t.Fatalf("plan for %q carries %d Aggregate node(s) alongside the RangeBucketFanout; the range path is expected to collapse structurally, so a keyless Aggregate appearing here would silently change what the guard count means", tc.query, shape.aggregates)
				}
			default:
				t.Fatalf("case %q declares no wantCollapse", tc.name)
			}

			gotGuarded := countGuardedAggregates(plan)
			if gotGuarded != tc.wantGuarded {
				t.Fatalf("plan for %q has %d guard-eligible (keyless + DropEmptyOnNoGroup) Aggregate(s), want %d — %s", tc.query, gotGuarded, tc.wantGuarded, tc.why)
			}

			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", tc.query, err)
			}
			gotGuards := strings.Count(sql, emptyAggGuardSQL)
			if gotGuards != gotGuarded {
				t.Errorf("emitted SQL for %q carries %d %s guard(s) but the plan has %d keyless Aggregate(s) — guard presence must track grouping-key arity and nothing else (%s)\nSQL: %s", tc.query, gotGuards, emptyAggGuardSQL, gotGuarded, tc.why, sql)
			}
		})
	}
}
