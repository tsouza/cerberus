package chsql

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// hqNativeMutationPlan returns a HistogramQuantileNative over the default
// OTel-CH exp-histogram column names, parameterised only by the literal phi —
// phi is what selects the rank-walk direction at emit time, which is the axis
// every test in this file varies.
func hqNativeMutationPlan(phi float64) *chplan.HistogramQuantileNative {
	return &chplan.HistogramQuantileNative{
		Input:                      &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
		Phi:                        phi,
		ScaleColumn:                "Scale",
		ZeroCountColumn:            "ZeroCount",
		PositiveOffsetColumn:       "PositiveOffset",
		PositiveBucketCountsColumn: "PositiveBucketCounts",
		NegativeOffsetColumn:       "NegativeOffset",
		NegativeBucketCountsColumn: "NegativeBucketCounts",
		CountColumn:                "Count",
		SumColumn:                  "Sum",
	}
}

// The exhausted-walk branch: `idx = 0` (the index functions' not-found
// sentinel), Sum not NaN, and the iterator yielded at least one bucket. What
// follows this prefix is armSelectFiniteSum's choice, and it is the ONLY place
// in the emitted chain where that helper's output lands — so anchoring on the
// prefix makes the assertions below read the arm selection and nothing else.
const hqExhaustedFiniteSumHead = "if(`_cerb_hq_idx` = 0, if(isNaN(`Sum`), nan, " +
	"if(length(`NegativeBucketCounts`) = 0 AND length(`PositiveBucketCounts`) = 0 AND `ZeroCount` = 0, 0., "

// The forward arm's fallback edge: the upper edge at w.lastIterated, the top
// of the walk order.
const hqLastIteratedEdge = "if(if(length(`PositiveBucketCounts`) > 0, " +
	"length(`NegativeBucketCounts`) + 1 + length(`PositiveBucketCounts`), "

// The backward arm's fallback edge: the upper edge at w.firstIterated, the
// bottom of the walk order.
const hqFirstIteratedEdge = "if(if(length(`NegativeBucketCounts`) > 0 OR `ZeroCount` > 0, 1, 2) <= "

// TestMutation_HQNativeArmSelectFiniteSum_ForwardBelowReverseWalkPhi pins the
// exhausted-walk fallback for a literal phi BELOW reverseWalkPhi: reference
// Prometheus walks forward there, so the bucket its iterator is left sitting
// on is the LAST one the walk order yields, and the arm must be resolved at
// emit time — a literal phi is query shape, not per-row data, so no runtime
// branch belongs in the SQL.
//
// Kills, in
// histogram_quantile_native.go:`w.armSelectFiniteSum = func(fwd, rev Frag) Frag`,
// the CONDITIONALS_NEGATION on `h.Phi < reverseWalkPhi` (-> `>=`), which picks
// the backward arm's firstIterated edge for phi = 0.25, and the
// CONDITIONALS_NEGATION on `h.PhiExpr == nil` (-> `!= nil`), which drops
// through to the computed-phi form and emits a runtime `if(0.25 < 0.5, …)`
// over two constant-folded arms.
func TestMutation_HQNativeArmSelectFiniteSum_ForwardBelowReverseWalkPhi(t *testing.T) {
	t.Parallel()

	sql := emitNodeSQL(t, hqNativeMutationPlan(0.25))
	if !strings.Contains(sql, hqExhaustedFiniteSumHead+hqLastIteratedEdge) {
		t.Fatalf("a literal phi below reverseWalkPhi must resolve the exhausted-walk fallback to the "+
			"forward arm's lastIterated edge at emit time, got %q", sql)
	}
}

// TestMutation_HQNativeArmSelectFiniteSum_BackwardAtReverseWalkPhi pins the
// BOUNDARY of the same choice. reverseWalkPhi is reference Prometheus's own
// `q < 0.5` test, so phi EQUAL to it belongs to the backward arm — its
// fallback edge is the FIRST position the walk order yields.
//
// Kills, in the same
// histogram_quantile_native.go:`w.armSelectFiniteSum = func(fwd, rev Frag) Frag`,
// the CONDITIONALS_BOUNDARY on `h.Phi < reverseWalkPhi` (-> `<=`), the only
// mutant that changes behaviour at exactly phi == 0.5, and again its
// CONDITIONALS_NEGATION (`>=`, also forward here) and the
// CONDITIONALS_NEGATION on `h.PhiExpr == nil` (which emits the runtime
// `if(0.5 < 0.5, …)` form instead).
func TestMutation_HQNativeArmSelectFiniteSum_BackwardAtReverseWalkPhi(t *testing.T) {
	t.Parallel()

	sql := emitNodeSQL(t, hqNativeMutationPlan(reverseWalkPhi))
	if !strings.Contains(sql, hqExhaustedFiniteSumHead+hqFirstIteratedEdge) {
		t.Fatalf("a literal phi AT reverseWalkPhi must resolve the exhausted-walk fallback to the "+
			"backward arm's firstIterated edge at emit time, got %q", sql)
	}
}

// TestMutation_HQNativeHelperColumns_MaterializedOnceAndReadBack pins the
// helper-column protocol that emitHistogramQuantileNative's staged pipeline
// runs on. Each stage builds its writers with the helper names produced SO FAR:
// an EMPTY name means "this stage is the one that computes the quantity, so
// expand the expression", and a non-empty one means "an enclosing stage already
// projected it, read the column". Inverting that test makes every writer do
// exactly the wrong thing — the defining stage emits `Col("")`, which renders
// as an empty backtick-quoted identifier, and every consumer re-expands the
// full array walk per use, which is the unbounded-cost shape the staging exists
// to avoid.
//
// Kills the three surviving CONDITIONALS_NEGATION mutants of that test:
// histogram_quantile_native.go:`helpers.revCum != ""`,
// histogram_quantile_native.go:`helpers.valueIdx != ""` and
// histogram_quantile_native.go:`helperCol != ""` (shared by firstPopulated
// and lastPopulated).
//
// phi is at reverseWalkPhi so reachesReverseArm holds and the revCum stage is
// actually projected.
func TestMutation_HQNativeHelperColumns_MaterializedOnceAndReadBack(t *testing.T) {
	t.Parallel()

	sql := emitNodeSQL(t, hqNativeMutationPlan(reverseWalkPhi))

	// Each helper is DEFINED once, by expansion, in its own stage …
	for _, want := range []string{
		"arrayReverse(arrayCumSum(arrayReverse(`_cerb_hq_buckets`))) AS `_cerb_hq_revcum`",
		"`_cerb_hq_idx`) AS `_cerb_hq_value_idx`",
		"arrayFirstIndex(c -> c > 0, `_cerb_hq_buckets`) AS `_cerb_hq_first_populated`",
		"arrayLastIndex(c -> c > 0, `_cerb_hq_buckets`) AS `_cerb_hq_last_populated`",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("helper column must be materialized by expansion: expected %q in\n%s", want, sql)
		}
	}
	// … and READ BACK as a column everywhere above it.
	for _, want := range []string{
		"`_cerb_hq_revcum`[`_cerb_hq_idx`]",
		"if(`_cerb_hq_value_idx` <= length(`NegativeBucketCounts`)",
		"if(`_cerb_hq_first_populated` <= length(`NegativeBucketCounts`)",
		"if(`_cerb_hq_last_populated` <= length(`NegativeBucketCounts`)",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("materialized helper must be read back as a column: expected %q in\n%s", want, sql)
		}
	}
	// The negated guard's signature: Col("") renders as a bare pair of
	// backticks, which no legitimate identifier in this query produces.
	if strings.Contains(sql, "``") {
		t.Fatalf("emitted SQL contains an empty backtick-quoted identifier:\n%s", sql)
	}
}
