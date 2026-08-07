package main

import (
	"math"
	"strings"
	"testing"
)

// The normalise + diff layer IS the parity verdict: `diffTyped`
// returning "" is what makes a case pass. A comparator that under-reports
// (missing a divergent point) launders a real cerberus bug into a green
// score, and one that over-reports buries the real ones in noise. Each
// test below drives one comparison dimension and asserts BOTH directions
// — the equal case returns "", the divergent case names the offending
// index.

// TestLabelsCmp pins the total order the normaliser and every diff
// function key on. Equal sets compare 0; otherwise the first differing
// key wins, then the first differing value, then the shorter set sorts
// first.
func TestLabelsCmp(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b map[string]string
		want int // sign
	}{
		{"both-empty", map[string]string{}, map[string]string{}, 0},
		{"identical", map[string]string{"a": "1", "b": "2"}, map[string]string{"b": "2", "a": "1"}, 0},
		{"key-differs", map[string]string{"a": "1"}, map[string]string{"b": "1"}, -1},
		{"value-differs", map[string]string{"a": "1"}, map[string]string{"a": "2"}, -1},
		{"prefix-is-shorter", map[string]string{"a": "1"}, map[string]string{"a": "1", "b": "2"}, -1},
		{"longer-sorts-after", map[string]string{"a": "1", "b": "2"}, map[string]string{"a": "1"}, 1},
		{"nil-vs-populated", nil, map[string]string{"a": "1"}, -1},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := labelsCmp(tc.a, tc.b)
			if sign(got) != tc.want {
				t.Fatalf("labelsCmp(%v, %v) = %d, want sign %d", tc.a, tc.b, got, tc.want)
			}
			// Antisymmetry: swapping the operands must flip the sign,
			// or the sort the comparator feeds is not a total order.
			if rev := labelsCmp(tc.b, tc.a); sign(rev) != -tc.want {
				t.Fatalf("labelsCmp(b, a) = %d, want sign %d (antisymmetry)", rev, -tc.want)
			}
		})
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// TestNormaliseTypedResult_ReordersToAgreement is the normaliser's whole
// reason to exist: two backends that return the same data in different
// order must diff clean. Feeding both orderings through the normaliser
// and asserting the diff is empty pins that end to end, and the
// pre-normalisation assertion confirms the input really was scrambled.
func TestNormaliseTypedResult_ReordersToAgreement(t *testing.T) {
	t.Parallel()

	forward := typedResult{kind: "vector", vector: []decodedSample{
		{Metric: map[string]string{"level": "error"}, T: 1000, F: 1},
		{Metric: map[string]string{"level": "info"}, T: 1000, F: 2},
	}}
	reversed := typedResult{kind: "vector", vector: []decodedSample{
		{Metric: map[string]string{"level": "info"}, T: 1000, F: 2},
		{Metric: map[string]string{"level": "error"}, T: 1000, F: 1},
	}}
	if diffTyped(forward, reversed, 0) == "" {
		t.Fatal("un-normalised vectors in opposite order diffed clean; the fixture does not exercise the sort")
	}
	if diff := diffTyped(normaliseTypedResult(forward), normaliseTypedResult(reversed), 0); diff != "" {
		t.Fatalf("normalised vectors still diff: %s", diff)
	}

	matrixA := typedResult{kind: "matrix", matrix: []decodedSeries{
		{Metric: map[string]string{"level": "warn"}, Floats: []decodedPoint{{T: 1, F: 1}}},
		{Metric: map[string]string{"level": "debug"}, Floats: []decodedPoint{{T: 1, F: 2}}},
	}}
	matrixB := typedResult{kind: "matrix", matrix: []decodedSeries{
		{Metric: map[string]string{"level": "debug"}, Floats: []decodedPoint{{T: 1, F: 2}}},
		{Metric: map[string]string{"level": "warn"}, Floats: []decodedPoint{{T: 1, F: 1}}},
	}}
	if diff := diffTyped(normaliseTypedResult(matrixA), normaliseTypedResult(matrixB), 0); diff != "" {
		t.Fatalf("normalised matrices still diff: %s", diff)
	}
	if got := matrixA.matrix[0].Metric["level"]; got != "debug" {
		t.Fatalf("matrix series[0] = %q after normalise, want debug (label order)", got)
	}
}

// TestNormaliseTypedResult_SortsStreamsAndEntries — streams sort by
// label set, and entries WITHIN a stream sort by timestamp then line.
// The line tie-break matters: two entries sharing a nanosecond
// timestamp would otherwise order by arrival and diff spuriously.
func TestNormaliseTypedResult_SortsStreamsAndEntries(t *testing.T) {
	t.Parallel()
	in := typedResult{kind: "streams", streams: []decodedStream{
		{Labels: map[string]string{"svc": "web"}, Entries: []logEntry{{Timestamp: 5, Line: "e"}}},
		{Labels: map[string]string{"svc": "api"}, Entries: []logEntry{
			{Timestamp: 9, Line: "third"},
			{Timestamp: 3, Line: "zebra"},
			{Timestamp: 3, Line: "alpha"},
		}},
	}}
	got := normaliseTypedResult(in)
	if got.streams[0].Labels["svc"] != "api" {
		t.Fatalf("streams[0] = %v, want the api stream first", got.streams[0].Labels)
	}
	entries := got.streams[0].Entries
	wantLines := []string{"alpha", "zebra", "third"}
	for i, want := range wantLines {
		if entries[i].Line != want {
			t.Fatalf("entry[%d] line = %q, want %q (timestamp then line order)", i, entries[i].Line, want)
		}
	}
}

// TestNormaliseTypedResult_LeavesScalarUntouched — the scalar arm has no
// ordering to canonicalise, so the value must pass through unchanged.
func TestNormaliseTypedResult_LeavesScalarUntouched(t *testing.T) {
	t.Parallel()
	in := typedResult{kind: "scalar", scalar: decodedSample{T: 7, F: 3.5}, hasValue: true}
	got := normaliseTypedResult(in)
	if got.scalar.T != 7 || got.scalar.F != 3.5 || !got.hasValue {
		t.Fatalf("scalar changed under normalise: %+v", got.scalar)
	}
}

// TestDiffTyped_KindMismatch — a resultType disagreement is reported as
// such rather than falling through to a shape-specific comparator that
// would read the wrong (empty) slice and pass.
func TestDiffTyped_KindMismatch(t *testing.T) {
	t.Parallel()
	diff := diffTyped(typedResult{kind: "matrix"}, typedResult{kind: "streams"}, 0)
	if !strings.Contains(diff, "resultType differs") ||
		!strings.Contains(diff, "expected=matrix") ||
		!strings.Contains(diff, "actual=streams") {
		t.Fatalf("diff = %q, want a resultType mismatch naming both kinds", diff)
	}
}

// TestDiffTyped_UnknownKindComparesClean pins the fall-through: an
// unrecognised (or zero) kind has no comparator, so two such results
// agree. compareOne never reaches it — isEmpty() rejects the zero kind
// first — and this asserts the fall-through stays a no-op rather than
// panicking on a nil slice.
func TestDiffTyped_UnknownKindComparesClean(t *testing.T) {
	t.Parallel()
	if diff := diffTyped(typedResult{}, typedResult{}, 0); diff != "" {
		t.Fatalf("diffTyped over two zero results = %q, want empty", diff)
	}
}

// TestDiffVector drives every dimension the vector comparator checks.
func TestDiffVector(t *testing.T) {
	t.Parallel()
	base := []decodedSample{
		{Metric: map[string]string{"level": "info"}, T: 1000, F: 5},
		{Metric: map[string]string{"level": "warn"}, T: 1000, F: 6},
	}
	if diff := diffVector(base, base, 0); diff != "" {
		t.Fatalf("identical vectors diff: %s", diff)
	}

	cases := []struct {
		name     string
		actual   []decodedSample
		wantFrag string
	}{
		{"length", base[:1], "vector length: expected=2 actual=1"},
		{
			"metric",
			[]decodedSample{base[0], {Metric: map[string]string{"level": "error"}, T: 1000, F: 6}},
			"vector[1] metric differs",
		},
		{
			"timestamp",
			[]decodedSample{base[0], {Metric: map[string]string{"level": "warn"}, T: 2000, F: 6}},
			"vector[1] timestamp differs: expected=1000 actual=2000",
		},
		{
			"value",
			[]decodedSample{base[0], {Metric: map[string]string{"level": "warn"}, T: 1000, F: 99}},
			"vector[1] value differs: expected=6 actual=99",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			diff := diffVector(base, tc.actual, 0)
			if !strings.Contains(diff, tc.wantFrag) {
				t.Fatalf("diff = %q, want it to contain %q", diff, tc.wantFrag)
			}
		})
	}
}

// TestDiffMatrix drives the matrix comparator, including the per-series
// point-list dimensions a series-level-only comparison would miss.
func TestDiffMatrix(t *testing.T) {
	t.Parallel()
	base := []decodedSeries{{
		Metric: map[string]string{"level": "info"},
		Floats: []decodedPoint{{T: 1000, F: 1}, {T: 2000, F: 2}},
	}}
	if diff := diffMatrix(base, base, 0); diff != "" {
		t.Fatalf("identical matrices diff: %s", diff)
	}

	cases := []struct {
		name     string
		actual   []decodedSeries
		wantFrag string
	}{
		{"series-count", nil, "matrix length: expected=1 actual=0"},
		{
			"metric",
			[]decodedSeries{{Metric: map[string]string{"level": "warn"}, Floats: base[0].Floats}},
			"matrix[0] metric differs",
		},
		{
			"point-count",
			[]decodedSeries{{Metric: base[0].Metric, Floats: base[0].Floats[:1]}},
			"matrix[0] series length: expected=2 actual=1",
		},
		{
			"point-timestamp",
			[]decodedSeries{{Metric: base[0].Metric, Floats: []decodedPoint{{T: 1000, F: 1}, {T: 2500, F: 2}}}},
			"matrix[0].points[1] timestamp differs: expected=2000 actual=2500",
		},
		{
			"point-value",
			[]decodedSeries{{Metric: base[0].Metric, Floats: []decodedPoint{{T: 1000, F: 1}, {T: 2000, F: 7}}}},
			"matrix[0].points[1] value differs: expected=2 actual=7",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			diff := diffMatrix(base, tc.actual, 0)
			if !strings.Contains(diff, tc.wantFrag) {
				t.Fatalf("diff = %q, want it to contain %q", diff, tc.wantFrag)
			}
		})
	}
}

// TestDiffScalar covers both scalar dimensions.
func TestDiffScalar(t *testing.T) {
	t.Parallel()
	base := decodedSample{T: 1000, F: 4}
	if diff := diffScalar(base, base, 0); diff != "" {
		t.Fatalf("identical scalars diff: %s", diff)
	}
	if diff := diffScalar(base, decodedSample{T: 2000, F: 4}, 0); !strings.Contains(diff, "scalar timestamp differs: expected=1000 actual=2000") {
		t.Fatalf("timestamp diff = %q", diff)
	}
	if diff := diffScalar(base, decodedSample{T: 1000, F: 9}, 0); !strings.Contains(diff, "scalar value differs: expected=4 actual=9") {
		t.Fatalf("value diff = %q", diff)
	}
}

// TestDiffStreams drives the log-shape comparator. The line dimension is
// the one that catches a wrong parser stage: same labels, same
// timestamps, different text.
func TestDiffStreams(t *testing.T) {
	t.Parallel()
	base := []decodedStream{{
		Labels:  map[string]string{"svc": "api"},
		Entries: []logEntry{{Timestamp: 1, Line: "one"}, {Timestamp: 2, Line: "two"}},
	}}
	if diff := diffStreams(base, base); diff != "" {
		t.Fatalf("identical streams diff: %s", diff)
	}

	cases := []struct {
		name     string
		actual   []decodedStream
		wantFrag string
	}{
		{"stream-count", nil, "streams length: expected=1 actual=0"},
		{
			"labels",
			[]decodedStream{{Labels: map[string]string{"svc": "web"}, Entries: base[0].Entries}},
			"streams[0] labels differ",
		},
		{
			"entry-count",
			[]decodedStream{{Labels: base[0].Labels, Entries: base[0].Entries[:1]}},
			"streams[0] entry count: expected=2 actual=1",
		},
		{
			"entry-timestamp",
			[]decodedStream{{Labels: base[0].Labels, Entries: []logEntry{{Timestamp: 1, Line: "one"}, {Timestamp: 5, Line: "two"}}}},
			"streams[0].entries[1] timestamp differs: expected=2 actual=5",
		},
		{
			"entry-line",
			[]decodedStream{{Labels: base[0].Labels, Entries: []logEntry{{Timestamp: 1, Line: "one"}, {Timestamp: 2, Line: "TWO"}}}},
			`streams[0].entries[1] line differs: expected="two" actual="TWO"`,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			diff := diffStreams(base, tc.actual)
			if !strings.Contains(diff, tc.wantFrag) {
				t.Fatalf("diff = %q, want it to contain %q", diff, tc.wantFrag)
			}
		})
	}
}

// TestFloatEqual pins the tolerance semantics the whole value diff rests
// on: NaN equals NaN, an infinity equals only the same infinity (the
// tolerance never applies to one), and a positive tolerance is an
// inclusive absolute bound.
func TestFloatEqual(t *testing.T) {
	t.Parallel()
	const tol = 1e-5
	cases := []struct {
		name string
		a, b float64
		tol  float64
		want bool
	}{
		{"nan-nan", math.NaN(), math.NaN(), tol, true},
		{"nan-vs-number", math.NaN(), 1, tol, false},
		{"pos-inf-pair", math.Inf(1), math.Inf(1), tol, true},
		{"neg-inf-pair", math.Inf(-1), math.Inf(-1), tol, true},
		{"opposite-infinities", math.Inf(1), math.Inf(-1), tol, false},
		{"inf-vs-huge-number", math.Inf(1), math.MaxFloat64, tol, false},
		{"within-tolerance", 1.0, 1.0 + tol/2, tol, true},
		// The bound is inclusive. The operands are binary-exact so the
		// difference is exactly the tolerance rather than one ulp above
		// it, which is what `1.0 + 1e-5` would actually produce.
		{"exactly-at-tolerance", 1.0, 1.25, 0.25, true},
		{"outside-tolerance", 1.0, 1.0 + tol*10, tol, false},
		{"zero-tolerance-exact", 1.5, 1.5, 0, true},
		{"zero-tolerance-inexact", 1.5, 1.5000001, 0, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := floatEqual(tc.a, tc.b, tc.tol); got != tc.want {
				t.Fatalf("floatEqual(%v, %v, %v) = %v, want %v", tc.a, tc.b, tc.tol, got, tc.want)
			}
		})
	}
}

// TestDiffTyped_ToleranceIsApplied confirms the tolerance argument
// actually reaches the leaf comparison rather than being dropped on the
// way through diffTyped.
func TestDiffTyped_ToleranceIsApplied(t *testing.T) {
	t.Parallel()
	expected := typedResult{kind: "vector", vector: []decodedSample{{Metric: map[string]string{}, T: 1, F: 1.0}}}
	actual := typedResult{kind: "vector", vector: []decodedSample{{Metric: map[string]string{}, T: 1, F: 1.000001}}}
	if diff := diffTyped(expected, actual, 1e-5); diff != "" {
		t.Fatalf("within-tolerance vectors diff: %s", diff)
	}
	if diff := diffTyped(expected, actual, 0); diff == "" {
		t.Fatal("zero-tolerance comparison of unequal values returned no diff")
	}
}
