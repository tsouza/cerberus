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

func aggregateFixture(fn chplan.Fn, input chplan.Node) *chplan.Aggregate {
	return &chplan.Aggregate{
		Input:    input,
		GroupBy:  []chplan.Expr{&chplan.ColumnRef{Name: "pod"}},
		AggFuncs: []chplan.AggFunc{{Fn: fn, Args: []chplan.Expr{&chplan.ColumnRef{Name: "value"}}}},
	}
}

// The three example shapes from the design doc — planShapeID (the coarser,
// non-routing key) collapses these onto one id; KeyFor must not.
func sumRateFixture() chplan.Node {
	return aggregateFixture(chplan.FnSum, rangeWindowFixture("rate", 5*time.Minute, 15*time.Second, filterFixture(scanFixture(), 1, 1)))
}

func quantileMaxOverTimeFixture() chplan.Node {
	return aggregateFixture(chplan.FnQuantile, rangeWindowFixture("max_over_time", 5*time.Minute, 15*time.Second, filterFixture(scanFixture(), 1, 1)))
}

func countRateFixture() chplan.Node {
	return aggregateFixture(chplan.FnCount, rangeWindowFixture("rate", 5*time.Minute, 15*time.Second, filterFixture(scanFixture(), 1, 1)))
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

// TestKeyForWalksEveryRangeAndCombinatorNodeKind exercises every chplan.Node
// kind keyWalker.walk switches on beyond the RangeWindow-based fixtures
// above — one per range-window sibling (RangeBucketFanout,
// RangeBucketGridNative, RangeLWR, RangeWindowGridNative,
// RangeWindowStaleResample) and one per non-join combinator (UnionAll,
// SetOperation, NaryVectorSetOp, Limit) — so a new case arm added to walk is
// never silently unexercised again. HasJoin is NOT covered here: it is a
// separate chplan.HasJoin sweep, not a walk case arm (see keyWalker.walk's
// own doc), and TestKeyForHasJoin below asserts it against the CANONICAL
// join-carrier set rather than whatever this walker happens to switch on.
func TestKeyForWalksEveryRangeAndCombinatorNodeKind(t *testing.T) {
	const (
		nAnchors = 241
		fanout   = int64(20)
		step     = 15 * time.Second
	)
	scan := scanFixture()

	t.Run("RangeBucketFanout sets AggFuncs and buckets Lookback", func(t *testing.T) {
		fanoutNode := func(lookback time.Duration) *chplan.RangeBucketFanout {
			return &chplan.RangeBucketFanout{
				Input:    scan,
				Lookback: lookback,
				AggFuncs: []chplan.AggFunc{{Fn: chplan.FnSum}},
			}
		}
		k := KeyFor(fanoutNode(5*time.Minute), nAnchors, fanout, step)
		if k.AggFuncs == "" {
			t.Fatalf("expected AggFuncs to be populated from RangeBucketFanout.AggFuncs, got %+v", k)
		}
		kOther := KeyFor(fanoutNode(30*time.Minute), nAnchors, fanout, step)
		if k.RangeFuncs == kOther.RangeFuncs {
			t.Fatalf("a wildly different Lookback must land in a different bucket: both %q", k.RangeFuncs)
		}
	})

	t.Run("RangeBucketGridNative sets HasNative", func(t *testing.T) {
		kGridNative := KeyFor(&chplan.RangeBucketGridNative{Input: scan, Range: 5 * time.Minute}, nAnchors, fanout, step)
		if !kGridNative.HasNative {
			t.Fatalf("RangeBucketGridNative must set HasNative")
		}
	})

	t.Run("RangeLWR, RangeWindowGridNative and RangeWindowStaleResample", func(t *testing.T) {
		kLWR := KeyFor(&chplan.RangeLWR{Input: scan, Lookback: 5 * time.Minute}, nAnchors, fanout, step)
		if kLWR.RangeFuncs == "" {
			t.Fatalf("RangeLWR must contribute to RangeFuncs, got %+v", kLWR)
		}
		kWindowGridNative := KeyFor(&chplan.RangeWindowGridNative{Input: scan, Func: "rate", Range: 5 * time.Minute}, nAnchors, fanout, step)
		if !kWindowGridNative.HasNative {
			t.Fatalf("RangeWindowGridNative must set HasNative")
		}
		kStaleResample := KeyFor(&chplan.RangeWindowStaleResample{Input: scan}, nAnchors, fanout, step)
		if !kStaleResample.HasResample {
			t.Fatalf("RangeWindowStaleResample must set HasResample")
		}
	})

	t.Run("combinator node kinds set their own presence bit", func(t *testing.T) {
		kUnion := KeyFor(&chplan.UnionAll{Inputs: []chplan.Node{scan, scan}}, nAnchors, fanout, step)
		if !kUnion.HasUnion {
			t.Fatalf("UnionAll must set HasUnion")
		}
		kSetOp := KeyFor(&chplan.SetOperation{Left: scan, Right: scan, Op: chplan.SetUnion}, nAnchors, fanout, step)
		if !kSetOp.HasUnion {
			t.Fatalf("SetOperation must set HasUnion")
		}
		kNary := KeyFor(&chplan.NaryVectorSetOp{Arms: []chplan.Node{scan, scan, scan}, Op: chplan.VectorSetOr}, nAnchors, fanout, step)
		if !kNary.HasUnion {
			t.Fatalf("NaryVectorSetOp must set HasUnion")
		}
		kLimit := KeyFor(&chplan.Limit{Input: scan, Count: 10}, nAnchors, fanout, step)
		if !kLimit.HasLimit {
			t.Fatalf("Limit must set HasLimit")
		}
	})
}

// wantRouteMemoJoinCarrierCount pins the row count of
// TestKeyForHasJoin_CoversEveryJoinCarrier's table. It exists so a table row
// silently dropped fails loudly here rather than the test quietly shrinking
// back to "whichever arms keyWalker.walk happens to have" — the exact defect
// this test replaces (cerberus issue #2886/#3008: the walker's switch grew
// case arms, but the test that was supposed to guard it only ever checked
// the arms already present, so it could not have caught InfoJoin or the
// delta-prefix RangeWindow carrier being missing either).
//
// chplan.HasJoin's OWN completeness — that this table names every
// join-emitting Node kind and no more — is chplan's own job, pinned by
// TestHasJoin_CoversEveryJoinEmittingNode in internal/chplan/join_test.go.
// This test's job is narrower and KeyFor-specific: prove Key.HasJoin
// actually forwards chplan.HasJoin's verdict, for the full known set,
// including a join chplan.HasJoin can only see via WalkDeep.
const wantRouteMemoJoinCarrierCount = 11

// TestKeyForHasJoin_CoversEveryJoinCarrier asserts Key.HasJoin against the
// CANONICAL join-carrier set (internal/chplan/join.go's HasJoin), not
// against whatever case arms keyWalker.walk's OWN switch happens to have —
// keyWalker.walk has no join arms at all any more; HasJoin is a dedicated
// chplan.HasJoin(n) sweep (see its doc). A carrier this table does not name
// is a gap this test cannot catch, but chplan's own
// TestHasJoin_CoversEveryJoinEmittingNode is what keeps THAT table from
// falling behind the IR — the two tests are deliberately not the same test.
func TestKeyForHasJoin_CoversEveryJoinCarrier(t *testing.T) {
	const (
		nAnchors = 241
		fanout   = int64(20)
		step     = 15 * time.Second
	)
	scan := scanFixture()

	cases := []struct {
		name string
		plan chplan.Node
	}{
		{"VectorJoin", &chplan.VectorJoin{Left: scan, Right: scan, Op: chplan.OpAdd}},
		{"HistogramVectorJoin", &chplan.HistogramVectorJoin{Left: scan, Right: scan}},
		{"HistogramFloatVectorJoin", &chplan.HistogramFloatVectorJoin{Left: scan, Right: scan}},
		{"MixedVectorJoin", &chplan.MixedVectorJoin{Left: scan, Right: scan}},
		{"InfoJoin", &chplan.InfoJoin{Input: scan, Info: scan}},
		{"StructuralJoin", &chplan.StructuralJoin{Left: scan, Right: scan, Op: chplan.StructuralChild}},
		{"CrossJoin", &chplan.CrossJoin{Left: scan, Right: scan}},
		{"NestedSetAnnotate", &chplan.NestedSetAnnotate{Input: scan}},
		{"MetricsCompare with RootLookup", &chplan.MetricsCompare{Inner: scan, RootLookup: scan}},
		{
			"RangeWindow with DeltaPrefixAggregateInput",
			&chplan.RangeWindow{Input: scan, DeltaPrefixAggregateInput: scan, Func: "rate"},
		},
		{
			// cerberus issue #3014: instant rate() over a temporality-projected
			// counter with no DeltaPrefixAggregateInput — see
			// internal/chplan/join_test.go's identical row for the emission
			// evidence.
			"RangeWindow instant rate() over temporality-projected counter (#3014)",
			&chplan.RangeWindow{Input: scan, Func: "rate", TemporalityColumn: "AggregationTemporality"},
		},
	}
	if len(cases) != wantRouteMemoJoinCarrierCount {
		t.Fatalf("join carrier table has %d rows, want %d", len(cases), wantRouteMemoJoinCarrierCount)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := KeyFor(tc.plan, nAnchors, fanout, step)
			if !k.HasJoin {
				t.Errorf("KeyFor(%s).HasJoin = false; want true", tc.name)
			}
		})
	}

	t.Run("non-join plan", func(t *testing.T) {
		k := KeyFor(scan, nAnchors, fanout, step)
		if k.HasJoin {
			t.Errorf("KeyFor(bare Scan).HasJoin = true; want false")
		}
	})

	// The WalkDeep-matters case: a join buried inside a Filter predicate's
	// ScalarSubquery is invisible to keyWalker's own shallow chplan.Walk
	// pass and must still be found. This is #2886/#3008's actual shipped
	// gap — before this consolidation, keyWalker.walk's join arms lived
	// INSIDE the chplan.Walk switch, so no number of added case arms could
	// ever have found a join here.
	t.Run("join nested inside a ScalarSubquery", func(t *testing.T) {
		plan := &chplan.Filter{
			Input:     scan,
			Predicate: &chplan.ScalarSubquery{Input: &chplan.VectorJoin{Left: scan, Right: scan, Op: chplan.OpAdd}},
		}
		k := KeyFor(plan, nAnchors, fanout, step)
		if !k.HasJoin {
			t.Error("KeyFor: join nested inside a ScalarSubquery predicate not found; want HasJoin true")
		}
	})
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
