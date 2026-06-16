//go:build chdb

// Construct: setop_chain — left-assoc PromQL/LogQL vector set-op chain.
//
// Folds in the standalone setop_chain_scaling_chdb guard. This construct has
// outlived two emitter generations; the history matters because it explains
// why the wall axis is STILL quarantined after both.
//
//   - Gen #810 (superseded): the vector-set-op emitter inlined — and so
//     RE-RENDERED — the whole LEFT arm subplan twice per `or` level (UNION-ALL
//     left leg + the right leg's anti-join). A left-assoc chain
//     `m0 or m1 or m2 ...` lowers to `(((m0 or m1) or m2) ...)`, so the nested
//     LHS DOUBLED at every level: SQL TEXT and intermediate both EXPONENTIAL in
//     K, past CH's 256KB max_query_size by K~8. #810 hoisted the LHS into a
//     non-recursive `WITH _setop_lhs_<n>` CTE referenced by name — but CH
//     evaluates such CTEs INLINE at every reference, so the EXECUTION fan-out
//     survived even though the SQL text went linear. This harness caught that
//     (filed #88).
//   - Gen #814 (superseded for chains >2): replaced the per-arm CTE with a
//     single-pass `A UNION ALL B` tagging each row `0/1 AS _setop_side`, then
//     `max(_setop_side=0) OVER (PARTITION BY <sig>) AS _setop_has_left` and a
//     final `WHERE _setop_side = 0 OR _setop_has_left = 0`. Each BINARY set-op
//     now scans both arms EXACTLY ONCE — the data re-execution is gone, and the
//     peak intermediate is now a tiny bounded constant (3 -> 5 -> 9 rows across
//     K=2/4/8, the disjoint-arm row count, NOT a fan-out). The cardinality axis
//     is genuinely flat. But `m0 or m1 or m2 ...` still lowered LEFT-ASSOC into
//     K nested binary levels, each wrapping the prior level's whole relation in
//     ANOTHER `UNION ALL` + window-partition pass, so the COMPUTE (wall) tracked
//     K structurally (~2.6x/level) even though the intermediate stayed tiny.
//   - Gen #90 (current main): N-ARY LINEARISATION. The optimizer's
//     FlattenVectorSetOp rule collapses the left-assoc nested binary chain into
//     ONE chplan.NaryVectorSetOp, and the emitter renders it as a SINGLE
//     `UNION ALL` over all K arms with per-arm `_setop_side` tags + ONE
//     `min(_setop_side) OVER (PARTITION BY <sig>)` window over the combined
//     relation. The whole chain is now O(rows) in one scan — the K nested
//     window passes are gone — so the wall axis is genuinely (sub-)linear and
//     the quarantine is removed.
//
// THE REAL MULTIPLIER is the chain depth K. Param = K, swept 2 -> 4 -> 8.
//
// Because the harness now runs the chain through the optimizer (so the flatten
// rule fires), both axes — peak intermediate cardinality AND wall time — hard-
// gate. The static fan-out linter (#811) does not cover depth-of-nesting, so
// this harness's chDB sweep is the gate that proves the linearisation holds.
package scaling

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// setOpChainEvalTime is the fixed instant-eval anchor the chain lowers at;
// seeding rows one second before keeps every sample inside the 5-minute LWR
// staleness window so each arm's latest-per-series collapse yields its row.
var setOpChainEvalTime = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

func init() {
	const maxK = 8

	register(Construct{
		Name:        "setop_chain",
		Param:       "chain depth K",
		Why:         "vector set-op left-assoc K-level nesting (each `or` level wraps the prior relation in another single-pass)",
		ScanRowsSQL: "SELECT count() FROM otel_metrics_sum",
		// Disjoint arms -> the `or` chain materialises exactly k+1 rows; the
		// table itself holds maxK+1 rows. A bound of maxK+2 over the (tiny)
		// scan keeps the intermediate well within a small multiple while an
		// exponential re-materialisation regression blows straight past.
		CardinalityBound: float64(maxK + 2),
		SubLinearSlack:   0.9,
		// Post-#90 the chain flattens into ONE NaryVectorSetOp single-pass
		// (one UNION ALL over all K arms + one window partition), so both the
		// intermediate (3->9 rows, disjoint arms) AND the wall are (sub-)linear
		// in K. No quarantine — both axes hard-gate. See the package doc above.
		Seed: func() string {
			return setOpChainSeed(maxK)
		},
		Points: func(t *testing.T) []Point {
			ks := []int64{2, 4, 8}
			pts := make([]Point, 0, len(ks))
			for _, k := range ks {
				sqlText, args := emitSetOpChainSQL(t, "or", int(k))
				// Executability precondition: the chain must stay under CH's
				// 256KB parse limit at every depth (the pre-fix shape breached
				// it by K~8). A breach here means the exponential text
				// duplication regressed.
				const maxQuerySize = 256 * 1024
				if len(sqlText) >= maxQuerySize {
					t.Fatalf("or chain K=%d: emitted SQL is %d bytes, at/over CH's %d max_query_size — the "+
						"exponential set-op duplication regressed back in.", k, len(sqlText), maxQuerySize)
				}
				pts = append(pts, Point{
					Param: k,
					SQL:   sqlText,
					Args:  args,
					// The whole chain IS its own intermediate level — its
					// result-row count is the materialised set the fix bounds.
					LevelSQLs: []string{sqlText},
				})
			}
			return pts
		},
	})
}

// setOpChainMetricName is the OTel-dotted name arm i seeds + the underscore
// form the PromQL selector references.
func setOpChainMetricName(i int) (promName, otelName string) {
	return fmt.Sprintf("setop_chain_metric_%d", i), fmt.Sprintf("setop.chain.metric.%d", i)
}

// setOpChainSeed builds a single sum table seeding k+1 arms, one row per
// arm, each arm carrying a DISJOINT `arm` label so PromQL `or` reduces to
// the pure union of every arm — the chain materialises exactly k+1 rows.
func setOpChainSeed(k int) string {
	var b strings.Builder
	b.WriteString("DROP TABLE IF EXISTS otel_metrics_sum;")
	// ResourceAttributes mirrors the OTel-CH default schema: the rc.5 read
	// path projects mapUpdate(sanitize(ResourceAttributes), …), so the seed
	// table must carry the column (left empty via DEFAULT) or the chDB
	// round-trip 502s with UNKNOWN_IDENTIFIER.
	b.WriteString(`CREATE TABLE otel_metrics_sum (
	  MetricName String, Attributes Map(String,String),
	  ResourceAttributes Map(String,String) DEFAULT map(),
	  TimeUnix DateTime64(9), Value Float64
	) ENGINE = MergeTree() ORDER BY (MetricName, Attributes, TimeUnix);`)
	ts := setOpChainEvalTime.Add(-time.Second).UTC().Format("2006-01-02 15:04:05.000000000")
	b.WriteString("\nINSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value) VALUES\n")
	for i := 0; i <= k; i++ {
		_, otel := setOpChainMetricName(i)
		if i > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, "  ('%s', map('arm', '%d'), toDateTime64('%s', 9), %d.0)", otel, i, ts, i+1)
	}
	b.WriteString(";")
	return b.String()
}

// emitSetOpChainSQL lowers `m0 <op> m1 <op> ... <op> mk` at the fixed
// instant anchor through the real chain.
func emitSetOpChainSQL(t *testing.T, op string, k int) (string, []any) {
	t.Helper()
	parts := make([]string, k+1)
	for i := range parts {
		name, _ := setOpChainMetricName(i)
		parts[i] = name
	}
	q := strings.Join(parts, " "+op+" ")
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(),
		setOpChainEvalTime, setOpChainEvalTime)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", q, err)
	}
	// Run the optimizer so FlattenVectorSetOp (#90) linearises the left-assoc
	// nested binary chain into one NaryVectorSetOp single-pass — the whole
	// point of this harness post-#90. Mirrors the prom handler's pipeline
	// (LowerAt -> optimizer.Default().Run -> chsql.Emit).
	plan = optimizer.Default().Run(context.Background(), plan)
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", q, err)
	}
	return sqlText, args
}
