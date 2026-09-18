//go:build chdb

// TopK generation for TestPropertyOptimizerSemanticEquivalence.
//
// A TopK's answer is a function of the row set alone only while no two
// rows of a partition tie on the sort key: the optimized plan is a
// different query, and ClickHouse may legitimately break a tie the
// other way, so the property would flake rather than report. The gauge
// seed ties (`up` carries 1.0 twice), which is why TopK ranks
// propertyJoinTable instead — every Value there is distinct, so every
// partition the generator can draw ranks without a tie.
package optimizer_test

import (
	"math/rand"

	"github.com/tsouza/cerberus/internal/chplan"
)

// propertyTopKMaxK bounds the K draw. The largest partition the
// generator can draw is a whole metric (four series), so a K up to
// three keeps the truncation observable in every partitioning.
const propertyTopKMaxK = 3

// propertyTopKMetrics are the metrics the input filter draws from —
// every metric in the join seed plus one no row carries, so a TopK over
// an empty input is drawn too.
var propertyTopKMetrics = []string{"requests", "errors", "capacity", "quota", "nope"}

// generateTopK draws one topk/bottomk over the distinct-valued seed.
// Every dimension the emitter branches on is drawn: the direction, the
// partitioning (none, the metric, a label), the K binding (a literal
// `LIMIT K BY`, or a computed K through the row_number() window), and
// whether the outer SELECT names its columns. limitk (`Unordered`) is
// never drawn: its K survivors are arbitrary by contract, so no seed can
// make its answer a function of the row set.
func generateTopK(rng *rand.Rand) chplan.Node {
	t := &chplan.TopK{
		Input:    topKInput(rng),
		SortExpr: &chplan.ColumnRef{Name: "Value"},
		Desc:     rng.Intn(2) == 0,
	}
	switch rng.Intn(3) {
	case 0:
		// Whole-result top-K, no partitioning.
	case 1:
		t.By = []chplan.Expr{&chplan.ColumnRef{Name: propertyGroupColumn}}
	default:
		t.By = []chplan.Expr{&chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: "Attributes"},
			Key: &chplan.LitString{V: "job"},
		}}
	}
	k := int64(1 + rng.Intn(propertyTopKMaxK))
	if rng.Intn(2) == 0 {
		t.K = k
	} else {
		t.KExpr = computedK(k)
	}
	if rng.Intn(2) == 0 {
		t.Columns = []string{propertyGroupColumn, "Attributes", "TimeUnix", "Value"}
	}
	return t
}

// topKInput is the Scan or Filter(Scan) a TopK ranks: the whole seed, or
// one metric of it.
func topKInput(rng *rand.Rand) chplan.Node {
	scan := &chplan.Scan{Table: propertyJoinTable}
	if rng.Intn(2) == 0 {
		return scan
	}
	return &chplan.Filter{
		Input: scan,
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: propertyGroupColumn},
			Right: &chplan.LitString{V: propertyTopKMetrics[rng.Intn(len(propertyTopKMetrics))]},
		},
	}
}

// computedK is the one-row scalar subtree a `topk(scalar(...), v)`
// lowers its K to: a single named RoleValue column, which the emitter
// resolves into `(SELECT toFloat64(Value) FROM (...) LIMIT 1)`.
func computedK(k int64) chplan.Node {
	return &chplan.Project{
		Input:       &chplan.OneRow{},
		Projections: []chplan.Projection{{Expr: &chplan.LitInt{V: k}, Alias: "Value"}},
		Roles:       []chplan.Column{{Name: "Value", Role: chplan.RoleValue}},
	}
}
