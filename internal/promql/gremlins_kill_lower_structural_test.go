package promql

import (
	"testing"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Structural recognisers and shape guards in lower.go that survived mutation
// on `phase4-promql-lower` (cerberus issues #2883 / #2913). Each is a pure
// function whose wrong answer is a wrong PLAN, not a slow one, and each
// survived for the same reason: the golden corpus only ever reaches it down
// the arm it accepts, so the rejections were never the reason for any pinned
// SQL.

// unionArmWindow is a range window standing in for the DELTA-temporality
// fan-out arm. Its contents are irrelevant to the structural recogniser under
// test — only its Go type is.
func unionArmWindow() *chplan.RangeWindow { return &chplan.RangeWindow{Func: "rate"} }

// unionArmNative is the CUMULATIVE native-grid arm's stand-in. Func is
// pinned to "rate" — one of the two functions
// [rateIncreaseTemporalityUnionArms] actually recognizes (cerberus issue
// #2803's Func guard, added once irate() started building a structurally
// identical union of its own) — rather than left at its zero value; every
// other field is irrelevant to the structural recogniser under test.
func unionArmNative() *chplan.RangeWindowGridNative {
	return &chplan.RangeWindowGridNative{Func: "rate"}
}

// temporalityUnion wraps arms in the Project-over-UnionAll shape
// [rateIncreaseTemporalityUnionArms] recognises.
func temporalityUnion(arms ...chplan.Node) chplan.Node {
	return &chplan.Project{Input: &chplan.UnionAll{Inputs: arms}}
}

// TestRateIncreaseTemporalityUnionArms_RecognisesOnlyTheExactTwoArmShape pins
// every structural clause of the recogniser independently.
//
// The recogniser's whole contract is that it identifies rate()/increase()'s
// OWN construction site's output, not merely A shape that happens to match
// positionally. Its caller folds an outer sum/min/max into the native arm
// and leaves the delta arm alone, so a false positive rewrites a plan the
// fold was never proven against: a third arm would be silently dropped, a
// mis-typed arm would be folded as if it were the native grid, and (since
// cerberus issue #2803) a native arm from a DIFFERENT function that merely
// builds the same node layout — irate(), via derivedIrateArm — would be
// folded as if its per-cell values composed the same way rate/increase's
// do, which was never verified. All three are answered here: the arm count
// and the two type assertions for the first two, the native arm's own Func
// field for the third. Every clause survived because the corpus only ever
// hands this function the exact shape [derivedRateArm] builds.
func TestRateIncreaseTemporalityUnionArms_RecognisesOnlyTheExactTwoArmShape(t *testing.T) {
	t.Parallel()

	// The baseline every rejection below is a perturbation of.
	native, delta, ok := rateIncreaseTemporalityUnionArms(temporalityUnion(unionArmNative(), unionArmWindow()))
	if !ok || native == nil || delta == nil {
		t.Fatalf("rateIncreaseTemporalityUnionArms(native, window) = (%v, %v, %v); want both arms and ok=true — the rejections below prove nothing against a broken accept path", native, delta, ok)
	}

	cases := []struct {
		name  string
		input chplan.Node
		why   string
	}{
		{
			name:  "not a Project",
			input: &chplan.UnionAll{Inputs: []chplan.Node{unionArmNative(), unionArmWindow()}},
			why:   "the recognised shape is the attributes Project over the union, not a bare union",
		},
		{
			name:  "Project over something other than a union",
			input: &chplan.Project{Input: unionArmWindow()},
			why:   "there are no arms to split without a UnionAll",
		},
		{
			name:  "three arms",
			input: temporalityUnion(unionArmNative(), unionArmWindow(), unionArmWindow()),
			why:   "the fold rewrites arm 0 and passes arm 1 through, so a third arm would be silently dropped",
		},
		{
			name:  "one arm",
			input: temporalityUnion(unionArmNative()),
			why:   "there is no delta arm to pass through",
		},
		{
			name:  "first arm is not the native grid",
			input: temporalityUnion(unionArmWindow(), unionArmWindow()),
			why:   "the fold folds the outer aggregation INTO the native grid arm; a fan-out window there is a different plan",
		},
		{
			name:  "second arm is not a fan-out window",
			input: temporalityUnion(unionArmNative(), unionArmNative()),
			why:   "the delta arm is the raw fan-out; a second native arm is not the shape derivedRateArm builds",
		},
		{
			name:  "native arm Func is irate, not rate/increase",
			input: temporalityUnion(&chplan.RangeWindowGridNative{Func: "irate"}, unionArmWindow()),
			why: "derivedIrateArm (cerberus issue #2803) builds a positionally identical " +
				"Project{UnionAll{RangeWindowGridNative, RangeWindow}} shape for irate(), whose " +
				"trailing-pair counter-reset correction was never verified to compose the same way " +
				"under the ts_grid_vector_agg fold — admitting it here would silently widen that fold",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if n, d, got := rateIncreaseTemporalityUnionArms(tc.input); got {
				t.Errorf("rateIncreaseTemporalityUnionArms(%s) = (%v, %v, true); want ok=false — %s", tc.name, n, d, tc.why)
			}
		})
	}
}

// TestResolveUnambiguousScanTable_RejectsAnUnpinnedNameOverAHistogramSchema
// pins the regex-name exclusion. An unpinned `__name__` matcher fans out to
// the multi-arm regex-histogram union, so there is no single table for the
// temporality read (or the downsample tier) to attach to — and answering with
// one anyway would route a union-shaped query onto a single-scan strategy.
//
// The exclusion is only OBSERVABLE on a schema whose unknown-name table set is
// already a singleton, because a schema with distinct Gauge and Sum tables
// hands the routing two candidates and the later `len(route.tables) != 1`
// check answers "" for its own reasons. Collapsing Sum onto Gauge — the shape
// [schema.Metrics.TablesForUnknownName] carries an explicit branch for — is
// what isolates this clause as the ONLY thing standing between a regex
// selector and a single-scan answer. The default schema's two-table case is
// asserted alongside it: the exclusion must hold there too.
func TestResolveUnambiguousScanTable_RejectsAnUnpinnedNameOverAHistogramSchema(t *testing.T) {
	t.Parallel()
	base := schema.DefaultOTelMetrics()
	if base.HistogramTable == "" {
		t.Fatalf("schema.DefaultOTelMetrics() has no HistogramTable; this test asserts the clause that reads it")
	}
	// A deployment whose plain-Sample gauges and sums share one physical
	// table: TablesForUnknownName then yields exactly one candidate.
	collapsed := base
	collapsed.SumTable = base.GaugeTable

	pinned := &promparser.VectorSelector{LabelMatchers: []*labels.Matcher{
		mustMatcher(t, labels.MatchEqual, model.MetricNameLabel, "http_requests_total"),
	}}
	if got := resolveUnambiguousScanTable(pinned, collapsed, lowerCtx{}); got != collapsed.GaugeTable {
		t.Fatalf("resolveUnambiguousScanTable(pinned name) = %q; want %q — the rejections below prove nothing if the accept path is already broken", got, collapsed.GaugeTable)
	}

	for _, tc := range []struct {
		name string
		s    schema.Metrics
	}{
		{"gauge and sum collapsed onto one table", collapsed},
		{"distinct gauge and sum tables", base},
	} {
		unpinned := &promparser.VectorSelector{LabelMatchers: []*labels.Matcher{
			mustMatcher(t, labels.MatchRegexp, model.MetricNameLabel, "http_.*"),
		}}
		if got := resolveUnambiguousScanTable(unpinned, tc.s, lowerCtx{}); got != "" {
			t.Errorf("resolveUnambiguousScanTable(regex name, %s) = %q; want \"\" — a regex name fans out to the multi-arm histogram union, which is not a single unambiguous scan", tc.name, got)
		}
	}
}

// TestAppendNameGroupKey_AppendsToAWindowGroupingOnNothing covers the
// degenerate grouping the corpus never produces: every fixture that reaches
// [appendNameGroupKey] already groups on the attributes column, so the
// append's own sizing was only ever exercised with a non-empty key list.
//
// The emitter projects EXACTLY the group keys, so the post-condition that
// matters is that the name key is present afterwards and appears once —
// asserted here for both the empty starting grouping and the idempotent
// re-application.
func TestAppendNameGroupKey_AppendsToAWindowGroupingOnNothing(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	rw := &chplan.RangeWindow{Func: "rate"}
	ref := appendNameGroupKey(rw, s)
	if ref == nil || ref.Name != s.MetricNameColumn {
		t.Fatalf("appendNameGroupKey returned %#v; want a ColumnRef for %q", ref, s.MetricNameColumn)
	}
	if len(rw.GroupBy) != 1 || !isIdentityColumnRef(rw.GroupBy[0], s.MetricNameColumn) {
		t.Fatalf("GroupBy after appending to an empty grouping = %#v; want exactly the name key", rw.GroupBy)
	}

	// Idempotent: a window that already groups on the name keeps one key.
	appendNameGroupKey(rw, s)
	if len(rw.GroupBy) != 1 {
		t.Errorf("GroupBy after a second append = %#v; want the single name key, not a duplicate", rw.GroupBy)
	}
}
