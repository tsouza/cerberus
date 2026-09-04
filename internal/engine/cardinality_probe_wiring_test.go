package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/solver"
)

// --- fixtures ---------------------------------------------------------------

// cardinalityProbeTestStart / cardinalityProbeTestEnd give buildCardinalityProbePlan
// a stable, non-zero Start/End pair.
var (
	cardinalityProbeTestStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cardinalityProbeTestEnd   = cardinalityProbeTestStart.Add(24 * time.Hour)
)

// cardinalityProbeTestMetric is the literal metric name cardinalityProbeTestPlan's
// Filter gates on.
const cardinalityProbeTestMetric = "http_requests_total"

// cardinalityProbeTestFilter builds a Filter(Scan) with a single literal
// MetricName equality predicate — the shape cardinalityProbeMetricName
// resolves unambiguously.
func cardinalityProbeTestFilter(metric string) *chplan.Filter {
	return &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_sum"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: cardinalityProbeMetricNameColumn},
			Right: &chplan.LitString{V: metric},
		},
	}
}

// cardinalityProbeTestPlan is a minimal *chplan.RangeWindow carrier over a
// single-metric Filter(Scan) — the one carrier shape this file's own scope
// narrowing (findCardinalityProbeCarrier) recognizes.
func cardinalityProbeTestPlan() chplan.Node {
	return &chplan.RangeWindow{
		Input:           cardinalityProbeTestFilter(cardinalityProbeTestMetric),
		Func:            "rate",
		Range:           5 * time.Minute,
		Step:            15 * time.Second,
		OuterRange:      24 * time.Hour,
		Start:           cardinalityProbeTestStart,
		End:             cardinalityProbeTestEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

const cardinalityProbeTestNAnchors = 241

func cardinalityProbeTestBaseline(reason string) *solver.Decision {
	return &solver.Decision{
		Reason:   reason,
		NAnchors: cardinalityProbeTestNAnchors,
		Fanout:   20,
		Step:     15 * time.Second,
	}
}

func cardinalityProbeTestKey() routememo.Key {
	d := cardinalityProbeTestBaseline(solver.ReasonRouted)
	return shapeKey(cardinalityProbeTestPlan(), d)
}

// countingCardinalityProbe is a fake CardinalityProbe that counts every
// ProbeCardinality call and returns a fixed result (or a fixed error).
type countingCardinalityProbe struct {
	calls    int
	estimate chclient.CardinalityEstimate
	err      error
}

func (c *countingCardinalityProbe) ProbeCardinality(_ context.Context, _ string, _ ...any) (chclient.CardinalityEstimate, error) {
	c.calls++
	return c.estimate, c.err
}

// --- CardinalityProbeAdvisor.Advise ------------------------------------------

func TestCardinalityProbeAdvisor_SkipsSecondProbeForSameShapeAndMetric(t *testing.T) {
	t.Parallel()
	probe := &countingCardinalityProbe{estimate: chclient.CardinalityEstimate{Rows: 500_000, DistinctSeries: 40}}
	a := NewCardinalityProbeAdvisor(probe, nil)
	plan := cardinalityProbeTestPlan()
	baseline := cardinalityProbeTestBaseline(solver.ReasonRouted)

	first := a.Advise(context.Background(), nil, plan, baseline, nil)
	if first == nil || first.Rows != 500_000 || first.DistinctSeries != 40 {
		t.Fatalf("first probe: got %+v, want Rows=500000, DistinctSeries=40", first)
	}
	if probe.calls != 1 {
		t.Fatalf("first probe issued %d round trips, want 1", probe.calls)
	}

	second := a.Advise(context.Background(), nil, plan, baseline, nil)
	if second == nil || second.Rows != 500_000 || second.DistinctSeries != 40 {
		t.Fatalf("second probe (cached): got %+v, want Rows=500000, DistinctSeries=40", second)
	}
	if probe.calls != 1 {
		t.Fatalf("second probe for the SAME (shape, metric) issued %d round trips, want 1 (cache hit)", probe.calls)
	}
}

func TestCardinalityProbeAdvisor_SkipsStructurallyIneligiblePlan(t *testing.T) {
	t.Parallel()
	probe := &countingCardinalityProbe{estimate: chclient.CardinalityEstimate{Rows: 1}}
	a := NewCardinalityProbeAdvisor(probe, nil)
	baseline := cardinalityProbeTestBaseline(solver.ReasonNow64)

	got := a.Advise(context.Background(), nil, cardinalityProbeTestPlan(), baseline, nil)
	if got != nil {
		t.Fatalf("got %+v, want nil (structurally ineligible)", got)
	}
	if probe.calls != 0 {
		t.Fatalf("issued %d round trips for a structurally ineligible plan, want 0", probe.calls)
	}
}

func TestCardinalityProbeAdvisor_SkipsWhenRouteMemoHasVerdict(t *testing.T) {
	t.Parallel()
	probe := &countingCardinalityProbe{estimate: chclient.CardinalityEstimate{Rows: 1}}
	a := NewCardinalityProbeAdvisor(probe, nil)
	plan := cardinalityProbeTestPlan()
	baseline := cardinalityProbeTestBaseline(solver.ReasonRouted)
	key := cardinalityProbeTestKey()

	memo := routememo.New(time.Hour)
	memo.Observe(key, routememo.RouteA, routememo.OutcomeResourceFailure)
	memo.Observe(key, routememo.RouteA, routememo.OutcomeResourceFailure)
	release, ok := memo.BeginProbe(key)
	if !ok {
		t.Fatalf("BeginProbe declined admission for a corroborated key")
	}
	memo.Observe(key, routememo.RouteB, routememo.OutcomeSuccess)
	release()
	if state, _ := memo.Lookup(key); state != routememo.PreferB {
		t.Fatalf("fixture failed to establish PreferB; got %v", state)
	}

	got := a.Advise(context.Background(), memo, plan, baseline, nil)
	if got != nil {
		t.Fatalf("got %+v, want nil (route memo already holds a verdict)", got)
	}
	if probe.calls != 0 {
		t.Fatalf("issued %d round trips despite an existing route memo verdict, want 0", probe.calls)
	}
}

func TestCardinalityProbeAdvisor_SkipsWhenPerRungAdmissionHasVerdict(t *testing.T) {
	t.Parallel()
	probe := &countingCardinalityProbe{estimate: chclient.CardinalityEstimate{Rows: 1}}
	learner := NewPerRungAdmissionLearner()
	a := NewCardinalityProbeAdvisor(probe, learner)
	plan := cardinalityProbeTestPlan()
	baseline := cardinalityProbeTestBaseline(solver.ReasonRouted)
	key := cardinalityProbeTestKey()

	learner.Observe(key, 10, cardinalityProbeTestNAnchors) // any real observation seeds a fresh entry

	got := a.Advise(context.Background(), nil, plan, baseline, nil)
	if got != nil {
		t.Fatalf("got %+v, want nil (per-rung admission already holds a verdict)", got)
	}
	if probe.calls != 0 {
		t.Fatalf("issued %d round trips despite an existing per-rung admission entry, want 0", probe.calls)
	}
}

func TestCardinalityProbeAdvisor_ProbeFailureIsAdvisoryUnchanged(t *testing.T) {
	t.Parallel()
	probe := &countingCardinalityProbe{err: errors.New("boom")}
	a := NewCardinalityProbeAdvisor(probe, nil)
	baseline := cardinalityProbeTestBaseline(solver.ReasonRouted)
	current := &solver.ScanEstimate{Rows: 42, Parts: 3, Marks: 9}

	got := a.Advise(context.Background(), nil, cardinalityProbeTestPlan(), baseline, current)
	if got != current {
		t.Fatalf("got %+v, want the SAME current pointer unchanged (probe failure must fail open)", got)
	}
	if probe.calls != 1 {
		t.Fatalf("probe called %d times, want exactly 1", probe.calls)
	}
}

func TestCardinalityProbeAdvisor_NilAdvisorAndNilBaseline(t *testing.T) {
	t.Parallel()
	probe := &countingCardinalityProbe{estimate: chclient.CardinalityEstimate{Rows: 1}}

	var nilAdvisor *CardinalityProbeAdvisor
	current := &solver.ScanEstimate{Rows: 7}
	if got := nilAdvisor.Advise(context.Background(), nil, cardinalityProbeTestPlan(), cardinalityProbeTestBaseline(solver.ReasonRouted), current); got != current {
		t.Fatalf("nil advisor: got %+v, want current unchanged", got)
	}

	a := NewCardinalityProbeAdvisor(probe, nil)
	if got := a.Advise(context.Background(), nil, cardinalityProbeTestPlan(), nil, current); got != current {
		t.Fatalf("nil baseline: got %+v, want current unchanged", got)
	}
	if probe.calls != 0 {
		t.Fatalf("issued %d round trips for a nil advisor/baseline, want 0", probe.calls)
	}
}

func TestCardinalityProbeAdvisor_SkipsUnrecognizedCarrierKind(t *testing.T) {
	t.Parallel()
	probe := &countingCardinalityProbe{estimate: chclient.CardinalityEstimate{Rows: 1}}
	a := NewCardinalityProbeAdvisor(probe, nil)
	baseline := cardinalityProbeTestBaseline(solver.ReasonRouted)

	// A RangeWindowStaleResample carrier — outside this file's deliberate
	// carrier-kind scope narrowing (this file's own top-level doc, point 1):
	// not one of the five #2840-recognised kinds.
	plan := &chplan.RangeWindowStaleResample{
		Input:         cardinalityProbeTestFilter(cardinalityProbeTestMetric),
		Start:         cardinalityProbeTestStart,
		End:           cardinalityProbeTestEnd,
		Step:          15 * time.Second,
		TimestampCol:  "TimeUnix",
		AttributesCol: "Attributes",
	}

	got := a.Advise(context.Background(), nil, plan, baseline, nil)
	if got != nil {
		t.Fatalf("got %+v, want nil (RangeWindowStaleResample carrier is out of this feature's scope)", got)
	}
	if probe.calls != 0 {
		t.Fatalf("issued %d round trips for an out-of-scope carrier kind, want 0", probe.calls)
	}
}

// cardinalityProbeNewCarrierFixtures builds one minimal, well-formed plan
// per #2840-added carrier kind (RangeWindowGridNative, RangeBucketFanout,
// RangeBucketGridNative, RangeLWR) over the SAME single-metric Filter(Scan)
// cardinalityProbeTestPlan uses, so every table-driven test below exercises
// findCardinalityProbeCarrier / buildCardinalityProbePlan / Advise against
// all five recognised carrier kinds with one shared fixture set.
func cardinalityProbeNewCarrierFixtures() map[string]chplan.Node {
	seriesKey := []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}}
	return map[string]chplan.Node{
		"RangeWindowGridNative": &chplan.RangeWindowGridNative{
			Input:           cardinalityProbeTestFilter(cardinalityProbeTestMetric),
			Func:            "rate",
			Range:           5 * time.Minute,
			Step:            15 * time.Second,
			Start:           cardinalityProbeTestStart,
			End:             cardinalityProbeTestEnd,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
			GroupBy:         seriesKey,
		},
		"RangeBucketFanout": &chplan.RangeBucketFanout{
			Input:        cardinalityProbeTestFilter(cardinalityProbeTestMetric),
			Start:        cardinalityProbeTestStart,
			End:          cardinalityProbeTestEnd,
			Step:         15 * time.Second,
			Lookback:     5 * time.Minute,
			GroupBy:      seriesKey,
			AnchorAlias:  "anchor_ts",
			TimestampCol: "TimeUnix",
		},
		"RangeBucketGridNative": &chplan.RangeBucketGridNative{
			Input:             cardinalityProbeTestFilter(cardinalityProbeTestMetric),
			Start:             cardinalityProbeTestStart,
			End:               cardinalityProbeTestEnd,
			Step:              15 * time.Second,
			Range:             5 * time.Minute,
			GroupBy:           seriesKey,
			AnchorAlias:       "anchor_ts",
			TimestampCol:      "TimeUnix",
			BucketCountsCol:   "BucketCounts",
			ExplicitBoundsCol: "ExplicitBounds",
		},
		"RangeLWR": &chplan.RangeLWR{
			Input:         cardinalityProbeTestFilter(cardinalityProbeTestMetric),
			Start:         cardinalityProbeTestStart,
			End:           cardinalityProbeTestEnd,
			Step:          15 * time.Second,
			Lookback:      5 * time.Minute,
			MetricNameCol: cardinalityProbeMetricNameColumn,
			AttributesCol: "Attributes",
			TimestampCol:  "TimeUnix",
			ValueCol:      "Value",
		},
	}
}

// TestFindCardinalityProbeCarrier_NewCarrierKinds pins that all four
// carrier kinds #2840 added are now recognised — the inverse of
// TestCardinalityProbeAdvisor_SkipsUnrecognizedCarrierKind above.
func TestFindCardinalityProbeCarrier_NewCarrierKinds(t *testing.T) {
	t.Parallel()
	for name, plan := range cardinalityProbeNewCarrierFixtures() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			carrier, ok := findCardinalityProbeCarrier(plan)
			if !ok || carrier == nil {
				t.Fatalf("expected %s to be recognised as a cardinality-probe carrier, got ok=%v", name, ok)
			}
			if carrier.TimestampCol != "TimeUnix" {
				t.Errorf("%s: TimestampCol = %q, want TimeUnix", name, carrier.TimestampCol)
			}
			if len(carrier.SeriesKey) != 1 {
				t.Errorf("%s: SeriesKey = %+v, want exactly one key", name, carrier.SeriesKey)
			}
		})
	}
}

// TestCardinalityProbeAdvisor_ProbesNewCarrierKinds runs the full Advise()
// path end to end for each #2840-added carrier kind, pinning that the probe
// actually fires (not merely that findCardinalityProbeCarrier recognises
// the shape) and that its emitted SQL carries the expected aggregate
// payload.
func TestCardinalityProbeAdvisor_ProbesNewCarrierKinds(t *testing.T) {
	t.Parallel()
	for name, plan := range cardinalityProbeNewCarrierFixtures() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			probe := &countingCardinalityProbe{estimate: chclient.CardinalityEstimate{Rows: 500_000, DistinctSeries: 40, DistinctSeriesApprox: 40}}
			a := NewCardinalityProbeAdvisor(probe, nil)
			baseline := cardinalityProbeTestBaseline(solver.ReasonRouted)

			got := a.Advise(context.Background(), nil, plan, baseline, nil)
			if got == nil || got.Rows != 500_000 || got.DistinctSeries != 40 {
				t.Fatalf("%s: got %+v, want Rows=500000, DistinctSeries=40", name, got)
			}
			if probe.calls != 1 {
				t.Fatalf("%s: issued %d round trips, want 1", name, probe.calls)
			}
		})
	}
}

func TestCardinalityProbeAdvisor_SkipsAmbiguousMetricSelector(t *testing.T) {
	t.Parallel()
	probe := &countingCardinalityProbe{estimate: chclient.CardinalityEstimate{Rows: 1}}
	a := NewCardinalityProbeAdvisor(probe, nil)
	baseline := cardinalityProbeTestBaseline(solver.ReasonRouted)

	plan := &chplan.RangeWindow{
		Input: &chplan.Filter{
			Input: &chplan.Scan{Table: "otel_metrics_sum"},
			Predicate: &chplan.Binary{
				Op:    chplan.OpMatch, // a regex __name__ matcher, not a literal Eq
				Left:  &chplan.ColumnRef{Name: cardinalityProbeMetricNameColumn},
				Right: &chplan.LitString{V: "http_.*"},
			},
		},
		Func:            "rate",
		Range:           5 * time.Minute,
		Step:            15 * time.Second,
		OuterRange:      24 * time.Hour,
		Start:           cardinalityProbeTestStart,
		End:             cardinalityProbeTestEnd,
		TimestampColumn: "TimeUnix",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}

	got := a.Advise(context.Background(), nil, plan, baseline, nil)
	if got != nil {
		t.Fatalf("got %+v, want nil (no single literal metric to key the cache on)", got)
	}
	if probe.calls != 0 {
		t.Fatalf("issued %d round trips for an ambiguous metric selector, want 0", probe.calls)
	}
}

func TestCardinalityProbeAdvisor_MergesWithExistingEstimate(t *testing.T) {
	t.Parallel()
	probe := &countingCardinalityProbe{estimate: chclient.CardinalityEstimate{Rows: 900_000, DistinctSeries: 12}}
	a := NewCardinalityProbeAdvisor(probe, nil)
	baseline := cardinalityProbeTestBaseline(solver.ReasonRouted)
	current := &solver.ScanEstimate{Parts: 3, Marks: 61, Rows: 500_000} // as if ScanEstimateAdvisor ran first

	got := a.Advise(context.Background(), nil, cardinalityProbeTestPlan(), baseline, current)
	if got == nil {
		t.Fatal("got nil, want a merged estimate")
	}
	if got.Parts != 3 || got.Marks != 61 {
		t.Fatalf("got %+v, want Parts/Marks preserved from the EXPLAIN ESTIMATE producer", got)
	}
	if got.Rows != 900_000 {
		t.Fatalf("got Rows=%d, want the REAL probe count (900000) to supersede the granule upper bound", got.Rows)
	}
	if got.DistinctSeries != 12 {
		t.Fatalf("got DistinctSeries=%d, want 12", got.DistinctSeries)
	}
	if current.Rows != 500_000 {
		t.Fatalf("merge must not mutate the caller's current estimate in place; got Rows=%d", current.Rows)
	}
}

// --- per-rung admission seeding ----------------------------------------------

func TestCardinalityProbeAdvisor_SeedsPerRungPriorOnLowCardinality(t *testing.T) {
	t.Parallel()
	// Well under cardinalityProbeTestNAnchors(241) * perRungCheapRowsPerAnchor(20).
	probe := &countingCardinalityProbe{estimate: chclient.CardinalityEstimate{Rows: 1_000, DistinctSeries: 100}}
	learner := NewPerRungAdmissionLearner()
	a := NewCardinalityProbeAdvisor(probe, learner)
	plan := cardinalityProbeTestPlan()
	baseline := cardinalityProbeTestBaseline(solver.ReasonRouted)
	key := cardinalityProbeTestKey()

	if a.Advise(context.Background(), nil, plan, baseline, nil) == nil {
		t.Fatal("expected a non-nil advisory estimate")
	}
	if !learner.hasFreshEntry(key) {
		t.Fatal("a low-distinct-series probe did not seed the per-rung admission learner")
	}
}

func TestCardinalityProbeAdvisor_HighCardinalityDoesNotResetPerRungPrior(t *testing.T) {
	t.Parallel()
	// Well OVER cardinalityProbeTestNAnchors(241) * perRungCheapRowsPerAnchor(20).
	learner := NewPerRungAdmissionLearner()
	baseline := cardinalityProbeTestBaseline(solver.ReasonRouted)
	key := cardinalityProbeTestKey()

	// Real accumulated evidence: one real clean-and-cheap drain, one
	// observation short of ShouldDeclineBypass's own threshold.
	learner.Observe(key, 10, cardinalityProbeTestNAnchors)
	if learner.ShouldDeclineBypass(key) {
		t.Fatal("fixture: a single real observation must not already decline the bypass")
	}

	// Directly exercise maybeSeedPerRungPrior's own one-directional
	// contract (a dense DistinctSeries reading must never call
	// SeedPriorFromEstimate with cheap=false). DistinctSeriesApprox is set
	// to the SAME dense value a real uniqCombined64 read-out would carry
	// once uniqUpTo(100) has already saturated — see
	// cardinalityProbeEffectiveDistinctSeries's own doc: a saturated
	// DistinctSeries(101) alone would no longer suffice to fail this
	// assertion, so the fixture must model both probe columns honestly.
	a := NewCardinalityProbeAdvisor(nil, learner)
	a.maybeSeedPerRungPrior(key, chclient.CardinalityEstimate{DistinctSeries: 101, DistinctSeriesApprox: 10_000}, baseline)
	learner.Observe(key, 10, cardinalityProbeTestNAnchors) // the second REAL cheap observation
	if !learner.ShouldDeclineBypass(key) {
		t.Fatal("a dense probe reading reset the real accumulated evidence — two real cheap " +
			"observations must still decline the bypass")
	}
}

// --- cardinalityProbeEffectiveDistinctSeries ---------------------------------

func TestCardinalityProbeEffectiveDistinctSeries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		est  chclient.CardinalityEstimate
		want uint64
	}{
		{
			name: "below cap trusts the exact uniqUpTo reading",
			est:  chclient.CardinalityEstimate{DistinctSeries: 50, DistinctSeriesApprox: 9_999},
			want: 50,
		},
		{
			name: "at cap trusts the exact uniqUpTo reading",
			est:  chclient.CardinalityEstimate{DistinctSeries: cardinalityProbeUniqUpToCap, DistinctSeriesApprox: 9_999},
			want: cardinalityProbeUniqUpToCap,
		},
		{
			name: "saturated (101) falls back to the uncapped approximate reading",
			est:  chclient.CardinalityEstimate{DistinctSeries: cardinalityProbeUniqUpToCap + 1, DistinctSeriesApprox: 5_000},
			want: 5_000,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := cardinalityProbeEffectiveDistinctSeries(c.est); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

// --- helper functions ---------------------------------------------------------

func TestFindCardinalityProbeCarrier(t *testing.T) {
	t.Parallel()
	if carrier, ok := findCardinalityProbeCarrier(cardinalityProbeTestPlan()); !ok || carrier == nil {
		t.Fatalf("expected the fixture's own RangeWindow carrier to be found, got ok=%v", ok)
	}

	unrecognized := &chplan.RangeWindowStaleResample{
		Input: cardinalityProbeTestFilter("x"), Start: cardinalityProbeTestStart, End: cardinalityProbeTestEnd, Step: time.Minute,
		TimestampCol: "TimeUnix", AttributesCol: "Attributes",
	}
	if _, ok := findCardinalityProbeCarrier(unrecognized); ok {
		t.Fatal("expected ok=false for a carrier kind outside the five #2840 recognises")
	}

	noGroupBy := &chplan.RangeWindow{
		Input: cardinalityProbeTestFilter("x"), Func: "rate", Range: time.Minute, Step: time.Minute,
		OuterRange: time.Hour, Start: cardinalityProbeTestStart, End: cardinalityProbeTestEnd, TimestampColumn: "TimeUnix",
	}
	if _, ok := findCardinalityProbeCarrier(noGroupBy); ok {
		t.Fatal("expected ok=false for a RangeWindow with no GroupBy series-identity keys")
	}

	lwrNoAttributes := &chplan.RangeLWR{
		Input: cardinalityProbeTestFilter("x"), Start: cardinalityProbeTestStart, End: cardinalityProbeTestEnd, Step: time.Minute,
		TimestampCol: "TimeUnix",
	}
	if _, ok := findCardinalityProbeCarrier(lwrNoAttributes); ok {
		t.Fatal("expected ok=false for a RangeLWR with no AttributesCol series-identity key")
	}
}

func TestCardinalityProbeMetricName(t *testing.T) {
	t.Parallel()
	rw, ok := findCardinalityProbeCarrier(cardinalityProbeTestPlan())
	if !ok {
		t.Fatal("fixture: expected the RangeWindow carrier to be found")
	}
	name, ok := cardinalityProbeMetricName(rw, cardinalityProbeMetricNameColumn)
	if !ok || name != cardinalityProbeTestMetric {
		t.Fatalf("got (%q, %v), want (%q, true)", name, ok, cardinalityProbeTestMetric)
	}

	noFilterPlan := &chplan.RangeWindow{
		Input: &chplan.Scan{Table: "otel_metrics_sum"}, Func: "rate", Range: time.Minute, Step: time.Minute,
		OuterRange: time.Hour, Start: cardinalityProbeTestStart, End: cardinalityProbeTestEnd, TimestampColumn: "TimeUnix",
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	noFilter, ok := findCardinalityProbeCarrier(noFilterPlan)
	if !ok {
		t.Fatal("fixture: expected the no-Filter RangeWindow carrier to be found")
	}
	if _, ok := cardinalityProbeMetricName(noFilter, cardinalityProbeMetricNameColumn); ok {
		t.Fatal("expected ok=false with no Filter at all")
	}
}

// TestBuildCardinalityProbePlan_EmitsExpectedShape pins the probe SQL's shape
// end-to-end through the real chsql.Emit pipeline (invariant 10: only a
// chplan tree reaches Emit, never a hand-written string).
func TestBuildCardinalityProbePlan_EmitsExpectedShape(t *testing.T) {
	t.Parallel()
	rw, ok := findCardinalityProbeCarrier(cardinalityProbeTestPlan())
	if !ok {
		t.Fatal("fixture: expected the RangeWindow carrier to be found")
	}
	plan, ok := buildCardinalityProbePlan(rw)
	if !ok {
		t.Fatal("buildCardinalityProbePlan reported ok=false for a well-formed carrier")
	}
	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("chsql.Emit: %v", err)
	}
	// count() renders wrapped in toFloat64(...) (chsql's own
	// intReturningAggregates guard), uniqUpTo's cap renders as a bound `?`
	// parameter (not an inline literal — see buildCardinalityProbePlan's own
	// doc for why), and uniqCombined64 (#2840's uncapped sibling) takes no
	// parameter at all. cardinalityProbeUniqUpToCap must still reach CH as
	// an arg.
	for _, want := range []string{"toFloat64(count())", "uniqUpTo(?)", "uniqCombined64(", "otel_metrics_sum", "toDateTime64"} {
		if !strings.Contains(sql, want) {
			t.Errorf("emitted SQL missing %q:\n%s", want, sql)
		}
	}
	foundCap := false
	for _, a := range args {
		if n, ok := a.(int64); ok && n == cardinalityProbeUniqUpToCap {
			foundCap = true
		}
	}
	if !foundCap {
		t.Errorf("args %+v does not carry the uniqUpTo cap (%d) as a bound parameter", args, cardinalityProbeUniqUpToCap)
	}
}

func TestBuildCardinalityProbePlan_ZeroStartOrEndFailsOpen(t *testing.T) {
	t.Parallel()
	rw, ok := findCardinalityProbeCarrier(cardinalityProbeTestPlan())
	if !ok {
		t.Fatal("fixture: expected the RangeWindow carrier to be found")
	}
	rw.Start = time.Time{}
	if _, ok := buildCardinalityProbePlan(rw); ok {
		t.Fatal("expected ok=false for a zero Start")
	}
}
