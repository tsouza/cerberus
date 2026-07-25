package routememo

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// synthetic fixture builders. Every value here is invented for the test —
// no production metric/label names, no production data.

func scanFixture() chplan.Node {
	return &chplan.Scan{Table: "fixture_metrics"}
}

func filterFixture(input chplan.Node, eqCount, matchCount int) chplan.Node {
	var pred chplan.Expr = &chplan.LitBool{V: true}
	for i := 0; i < eqCount; i++ {
		pred = &chplan.Binary{Op: chplan.OpEq, Left: pred, Right: &chplan.LitString{V: "x"}}
	}
	for i := 0; i < matchCount; i++ {
		pred = &chplan.Binary{Op: chplan.OpMatch, Left: pred, Right: &chplan.LitString{V: "y.*"}}
	}
	return &chplan.Filter{Input: input, Predicate: pred}
}

func rangeWindowFixture(fn string, rng, step time.Duration, input chplan.Node) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Input: input,
		Func:  fn,
		Range: rng,
		Step:  step,
	}
}

func aggregateFixture(aggName string, input chplan.Node) *chplan.Aggregate {
	return &chplan.Aggregate{
		Input:    input,
		GroupBy:  []chplan.Expr{&chplan.ColumnRef{Name: "pod"}},
		AggFuncs: []chplan.AggFunc{{Name: aggName, Args: []chplan.Expr{&chplan.ColumnRef{Name: "value"}}}},
	}
}

// The three example shapes from the design doc — planShapeID (the coarser,
// non-routing key) collapses these onto one id; KeyFor must not.
func sumRateFixture() chplan.Node {
	return aggregateFixture("sum", rangeWindowFixture("rate", 5*time.Minute, 15*time.Second, filterFixture(scanFixture(), 1, 1)))
}

func quantileMaxOverTimeFixture() chplan.Node {
	return aggregateFixture("quantile", rangeWindowFixture("max_over_time", 5*time.Minute, 15*time.Second, filterFixture(scanFixture(), 1, 1)))
}

func countRateFixture() chplan.Node {
	return aggregateFixture("count", rangeWindowFixture("rate", 5*time.Minute, 15*time.Second, filterFixture(scanFixture(), 1, 1)))
}

func TestKeyForSeparatesDifferentFunctionShapes(t *testing.T) {
	const (
		nAnchors = 241
		fanout   = int64(20)
		step     = 15 * time.Second
	)

	kSumRate := KeyFor(sumRateFixture(), nAnchors, fanout, step)
	kQuantileMax := KeyFor(quantileMaxOverTimeFixture(), nAnchors, fanout, step)
	kCountRate := KeyFor(countRateFixture(), nAnchors, fanout, step)

	if kSumRate == kQuantileMax {
		t.Fatalf("sum(rate(...)) and quantile(max_over_time(...)) must not collide: both hashed to %+v", kSumRate)
	}
	if kSumRate == kCountRate {
		t.Fatalf("sum(rate(...)) and count(rate(...)) must not collide: both hashed to %+v", kSumRate)
	}
	if kQuantileMax == kCountRate {
		t.Fatalf("quantile(max_over_time(...)) and count(rate(...)) must not collide: both hashed to %+v", kQuantileMax)
	}
}

func TestKeyForFusesRepeatTraffic(t *testing.T) {
	// A dashboard panel refreshing on a rolling window: same shape, same
	// grid-geometry buckets, only the concrete Start/End (not part of the
	// key) drift with each refresh.
	const step = 15 * time.Second
	k1 := KeyFor(sumRateFixture(), 241, 20, step)
	k2 := KeyFor(sumRateFixture(), 242, 20, step) // one anchor later
	if k1 != k2 {
		t.Fatalf("a one-anchor drift within the same bucket must still fuse: %+v != %+v", k1, k2)
	}
}

func TestKeyForGridGeometryBucketsDiffer(t *testing.T) {
	kSmall := KeyFor(sumRateFixture(), 4, 4, 15*time.Second)
	kHuge := KeyFor(sumRateFixture(), 1_000_000, 4, 15*time.Second)
	if kSmall == kHuge {
		t.Fatalf("wildly different anchor counts must land in different cost buckets")
	}
}

func TestKeyForMatcherOperatorMaskIsClosedVocabulary(t *testing.T) {
	k := KeyFor(filterFixture(scanFixture(), 2, 1), 10, 2, time.Second)
	if k.MatcherOpMask&matcherOpEq == 0 {
		t.Fatalf("expected the Eq bit set, got mask %b", k.MatcherOpMask)
	}
	if k.MatcherOpMask&matcherOpMatch == 0 {
		t.Fatalf("expected the Match bit set, got mask %b", k.MatcherOpMask)
	}
	if k.MatcherOpMask&matcherOpNe != 0 || k.MatcherOpMask&matcherOpNotMatch != 0 {
		t.Fatalf("unexpected operator bit set in mask %b", k.MatcherOpMask)
	}
}

func TestBucketLgBoundaries(t *testing.T) {
	cases := []struct {
		v    int64
		want int
	}{
		{0, 0},
		{1, 0},
		{2, 1},
		{3, 1},
		{4, 2},
		{1023, 9},
		{1024, 10},
	}
	for _, c := range cases {
		if got := bucketLg(c.v); got != c.want {
			t.Errorf("bucketLg(%d) = %d, want %d", c.v, got, c.want)
		}
	}
}

func TestBucketLgClampsAtCeiling(t *testing.T) {
	huge := int64(1) << 62
	if got := bucketLg(huge); got != maxCostBucketExponent {
		t.Fatalf("bucketLg(2^62) = %d, want the clamp %d", got, maxCostBucketExponent)
	}
}

func TestKeyForIsLiteralFree(t *testing.T) {
	// The Key must never carry the literal matcher VALUE ("y.*"/"x") — only
	// operator kinds and counts. Sanity-check by construction: Key has no
	// string field capable of holding it except the closed-vocabulary
	// RangeFuncs/AggFuncs/RootKind strings, which this test's fixture never
	// feeds a matcher literal into.
	k := KeyFor(filterFixture(scanFixture(), 1, 1), 10, 2, time.Second)
	for _, s := range []string{k.RootKind, k.RangeFuncs, k.AggFuncs} {
		if s == "y.*" || s == "x" {
			t.Fatalf("matcher literal leaked into Key: %q", s)
		}
	}
}
