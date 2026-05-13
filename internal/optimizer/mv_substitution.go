package optimizer

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// MVSubstitution rewrites `RangeWindow(Scan(base))` to
// `RangeWindow(Scan(rollup))` when the operator has declared a
// pre-aggregated rollup table whose window, aggregation operator, and
// the outer query's step + range are all compatible.
//
// Lineage. [Promscale #152] is TimescaleDB's analogous problem
// (continuous-aggregate substitution). [Jindal et al., VLDB 2018]
// frames the general problem of selecting subexpressions to
// materialise — its §4 (subexpression matching) and §6 (cost model)
// are the parts that pertain here. Cerberus v1 ships a deliberately
// simple shape of that problem: a binary "rollup applies safely"
// decision with no cost comparison between overlapping candidates.
// The CostModel interface stub (see costModel) keeps the seam open
// for v2 to swap in a real estimator without changing this rule.
//
// Safety. Four conditions must hold for a substitution to preserve
// query semantics:
//
//  1. **Step ≥ Window.** The query's evaluation step must be at least
//     the rollup's bucket width. A 5-minute step over a 5-minute
//     rollup is safe; a 30-second step over a 5-minute rollup loses
//     resolution (the rollup has nothing finer to offer).
//  2. **Range ≥ Window AND Range is a multiple of Window.** The
//     `[range]` window the PromQL function evaluates over has to
//     cover at least one full rollup bucket, and has to align on a
//     bucket boundary. A 7-minute range over a 5-minute rollup would
//     ask the function to look at 1.4 buckets — meaningless for the
//     pre-aggregated path.
//  3. **Outer aggregate commutes with rollup AggOp.** The PromQL
//     `Func` on the RangeWindow tells us what the outer query will do
//     with each bucket. `sum_over_time(metric[1h])` over a sum-rollup
//     gives the same answer as the same call against raw samples.
//     `rate` / `increase` / `delta` look at per-sample deltas (the
//     first/last of the per-series array), which a sum-of-values
//     rollup does NOT preserve — those are deliberately excluded in
//     v1 even over a sum-rollup. `avg_over_time` does NOT compose
//     with an avg-rollup either, without per-bucket weights. The v1
//     commutativity table is the conservative subset that is
//     unambiguously safe regardless of the upstream exporter's
//     rollup-bucketing implementation.
//  4. **Rollup exists for the base table.** The Metrics registry
//     enumerates the rollups the operator has provisioned. An empty
//     list (the default if rollups are not enabled in the upstream
//     OTel exporter) makes the rule a no-op.
//
// Rewrite shape. When all four conditions hold, the rule:
//
//   - Replaces `Scan.Table` with `Rollup.RollupTable`.
//   - Rewrites `RangeWindow.ValueColumn` from the base table's value
//     column to the rollup's pre-aggregated value column
//     (`Rollup.ValueColumn`, typically "Sum"). The downstream
//     range-window emitter then references the pre-aggregated column
//     verbatim — no further codegen change required, because the
//     rollup table is expected to expose the same series-identity +
//     timestamp columns under the same names as the base table.
//
// The rule lives in its own batch (`optimizer.mv-substitution`,
// `FixedPoint`) that runs after `optimizer.predicate-pushdown` so
// predicate pushdown has a chance to surface a matching pattern by
// transposing filters under the RangeWindow.
//
// Cost model (v1). The rule iterates `Metrics.RollupsFor(base)` in
// registry order and substitutes the first rollup that satisfies all
// four safety conditions — the `firstApplicable` cost model. Because
// the default registry lists rollups coarsest-first
// (`otel_metrics_sum_1h` before `otel_metrics_sum_5m`), this naturally
// prefers the rollup that strips the most data per scan.
//
// [Promscale #152]: https://github.com/timescale/promscale/issues/152
// [Jindal et al., VLDB 2018]: http://www.vldb.org/pvldb/vol11/p800-jindal.pdf
func MVSubstitution(rollups []schema.Rollup, baseValueColumn string) Rule {
	return &mvSubstitutionRule{
		rollups:         rollups,
		baseValueColumn: baseValueColumn,
		cost:            firstApplicableCostModel{},
	}
}

// mvSubstitutionRule is the concrete Rule type. It is not exported
// because callers build it via MVSubstitution (or via Default()'s
// pre-wired pipeline).
type mvSubstitutionRule struct {
	// rollups is the operator-declared catalog of rollup tables.
	// Iterated in declared order.
	rollups []schema.Rollup
	// baseValueColumn is the column name on the base table that the
	// upstream RangeWindow lowering filled into ValueColumn. Used to
	// double-check we're rewriting a sample-shaped window (and not
	// e.g. a TraceQL matrix shape where ValueColumn is unused).
	baseValueColumn string
	// cost picks among candidate rollups. v1 is firstApplicable; v2
	// will plug in a real estimator.
	cost costModel
}

func (mvSubstitutionRule) Name() string { return "mv-substitution" }

func (r *mvSubstitutionRule) Apply(n chplan.Node) (chplan.Node, bool) {
	rw, ok := n.(*chplan.RangeWindow)
	if !ok {
		return n, false
	}
	scan, ok := rw.Input.(*chplan.Scan)
	if !ok {
		return n, false
	}
	if len(r.rollups) == 0 {
		return n, false
	}
	// Bail on shapes where ValueColumn is unused (TraceQL matrix path
	// — see chplan/range_window.go's doc comment about
	// MetricsAggregate input). Those windows route through a different
	// emitter and the rollup table doesn't help them.
	if rw.ValueColumn == "" || rw.ValueColumn != r.baseValueColumn {
		return n, false
	}

	candidates := make([]schema.Rollup, 0, len(r.rollups))
	for _, c := range r.rollups {
		if c.BaseTable != scan.Table {
			continue
		}
		if !rollupApplies(rw, c) {
			continue
		}
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return n, false
	}

	picked := r.cost.Pick(candidates)

	newScan := *scan
	newScan.Table = picked.RollupTable
	// The rollup table only exposes pre-aggregated columns; if the
	// caller has previously narrowed Scan.Columns to a base-table
	// subset (via ProjectionPushdown), clear it so the rewritten scan
	// re-projects `*` over the rollup. Subsequent ProjectionPushdown
	// passes will re-narrow against the rollup's actual columns.
	newScan.Columns = nil

	newRW := *rw
	newRW.Input = &newScan
	newRW.ValueColumn = picked.ValueColumn
	return &newRW, true
}

// rollupApplies checks the four safety conditions documented on
// MVSubstitution. Returns true only if all of them hold.
func rollupApplies(rw *chplan.RangeWindow, c schema.Rollup) bool {
	// (3) Outer aggregate commutes with the rollup's per-bucket
	// reducer. Checked first because it's the cheapest reject and the
	// one most likely to filter out a candidate.
	if !commutesWith(rw.Func, c.AggOp) {
		return false
	}
	// (1) Step ≥ Window. Step==0 means "instant query" (a single
	// anchor at End) — the rollup still applies as long as the range
	// covers at least one bucket; the instant anchor lines up with
	// the most recent bucket. Step > 0 requires Step ≥ Window so
	// adjacent anchors don't sample the same bucket twice.
	if rw.Step > 0 && rw.Step < c.Window {
		return false
	}
	// (2) Range ≥ Window AND Range is a multiple of Window.
	if rw.Range < c.Window {
		return false
	}
	if rw.Range%c.Window != 0 {
		return false
	}
	return true
}

// commutesWith reports whether the outer PromQL range function over
// rows in a rollup with the given AggOp yields the same answer as
// the same function over the raw samples.
//
// The mapping is deliberately conservative: only the PromQL functions
// that distribute cleanly over per-bucket pre-aggregations are
// allowed. Future entries (e.g. `count_over_time` with AggOp=count)
// add rows here as the registry grows.
func commutesWith(promFunc string, aggOp schema.RollupAggOp) bool {
	switch aggOp {
	case schema.RollupAggSum:
		// sum-of-sums == total sum. Only sum_over_time is allowed in
		// v1: rate / increase / delta look at per-sample deltas (the
		// (first, last) pair of the array), which is NOT preserved by
		// a sum-of-values rollup. If the operator's rollup were
		// instead a "last value per bucket" rollup we'd allow rate
		// here too — but that's a separate AggOp (not modelled in v1)
		// and pretending sum-rollup is rate-safe would silently
		// return wrong numbers.
		return promFunc == "sum_over_time"
	case schema.RollupAggCount:
		return promFunc == "count_over_time"
	case schema.RollupAggMin:
		return promFunc == "min_over_time"
	case schema.RollupAggMax:
		return promFunc == "max_over_time"
	}
	// avg_over_time does NOT commute with an avg-rollup without
	// per-bucket weights. The default registry has no avg rollup; if
	// an operator adds one and points avg_over_time at it, the v1
	// rule refuses rather than emit a subtly-wrong query.
	return false
}

// costModel picks one rollup from a slate of candidates that all
// pass the safety conditions on MVSubstitution.
//
// v1 ships exactly one implementation — firstApplicableCostModel —
// which returns the first candidate (and so requires callers to list
// rollups coarsest-first in the registry; the default schema does).
// v2 will introduce a real estimator that compares scan-cost / row
// cardinality across overlapping candidates; the interface exists so
// that swap is local to this file rather than rippling through the
// rule.
//
// Not exported. v1 callers don't construct one — they invoke
// MVSubstitution which wires firstApplicableCostModel itself. v2 will
// flip the visibility once a second impl exists.
type costModel interface {
	Pick(candidates []schema.Rollup) schema.Rollup
}

type firstApplicableCostModel struct{}

func (firstApplicableCostModel) Pick(candidates []schema.Rollup) schema.Rollup {
	return candidates[0]
}
