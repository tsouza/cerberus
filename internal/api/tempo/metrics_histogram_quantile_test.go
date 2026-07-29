package tempo

import (
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

// The tests in this file pin the boundary between the matrix SQL's zero-fill
// convention and the histogram-quantile algorithm.
//
// The emitter plants a synthetic (bucket 0, count 0) row for every (group,
// anchor) so an anchor with no spans still produces a row. The quantile
// algorithm, mirroring upstream Tempo's HistogramAggregator, assumes the
// opposite: a bucket exists only because something was observed in it. Feed
// the synthetic row in and it is read as a real boundary — it sorts to the
// front, becomes the predecessor the estimate interpolates down to, and
// log2(0) is -Inf, so the interpolation evaluates to NaN.
//
// NaN is the reason these are unit tests rather than a fixture: it does not
// surface as a wrong number a golden would catch. It reaches the JSON encoder,
// which rejects the entire response, and the handler used to answer that with
// a 200 and an empty body. The failure therefore presents to a client as "this
// query legitimately matched nothing".

// quantileProbePhis spans the interpolating and the boundary-landing cases:
// 0.5 lands mid-bucket (interpolation runs, so a bad lower boundary shows),
// 0.9 and 0.99 round up onto the top bucket's edge.
var quantileProbePhis = []float64{0, 0.5, 0.9, 0.95, 0.99, 1}

// TestLog2Quantile_ZeroFillBucketDoesNotChangeTheEstimate is the regression
// proper: the same observed histogram must produce the same estimate whether
// or not the emitter's synthetic zero-count row is sitting in front of it.
func TestLog2Quantile_ZeroFillBucketDoesNotChangeTheEstimate(t *testing.T) {
	t.Parallel()

	observed := []histogramBucket{{Max: 8, Count: 3}, {Max: 16, Count: 2}}
	withZeroFill := append([]histogramBucket{{Max: 0, Count: 0}}, observed...)

	for _, phi := range quantileProbePhis {
		want, _ := log2QuantileWithBucket(phi, observed)
		got, _ := log2QuantileWithBucket(phi, withZeroFill)

		if math.IsNaN(got) || math.IsInf(got, 0) {
			t.Errorf("phi=%v: a zero-count boundary produced %v — the JSON encoder "+
				"rejects the whole response rather than this one value", phi, got)
			continue
		}
		if got != want {
			t.Errorf("phi=%v: a zero-count boundary moved the estimate: got %v, want %v",
				phi, got, want)
		}
	}
}

// TestLog2Quantile_FirstBucketSpansOneOctave pins the convention the fix
// leans on: with no usable lower boundary the bucket is taken to span one
// octave below its Max. Both a genuinely-first bucket and one preceded only by
// a zero-count boundary have to resolve the same way, because they describe the
// same thing — an observed bucket with nothing beneath it.
func TestLog2Quantile_FirstBucketSpansOneOctave(t *testing.T) {
	t.Parallel()

	// One bucket, three samples; phi=0.5 targets the 2nd, so frac = 2/3 and
	// the estimate is 2^(log2(8)-1 + 1*2/3).
	const phi = 0.5
	want := math.Pow(2, 2+2.0/3.0)

	for _, tc := range []struct {
		name    string
		buckets []histogramBucket
	}{
		{"no predecessor", []histogramBucket{{Max: 8, Count: 3}}},
		{"zero-count predecessor", []histogramBucket{{Max: 0, Count: 0}, {Max: 8, Count: 3}}},
	} {
		got, _ := log2QuantileWithBucket(phi, tc.buckets)
		if got != want {
			t.Errorf("%s: got %v, want %v", tc.name, got, want)
		}
	}
}

// TestLog2Quantile_IsNeverNaN sweeps the boundary shapes the emitter can
// produce and asserts the helper is total over them. A NaN escaping here is
// not a local wrong answer — it fails the response.
func TestLog2Quantile_IsNeverNaN(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		buckets []histogramBucket
	}{
		{"empty", nil},
		{"zero-fill only", []histogramBucket{{Max: 0, Count: 0}}},
		{"zero-fill then one bucket", []histogramBucket{{Max: 0, Count: 0}, {Max: 8, Count: 3}}},
		{"zero-fill then many buckets", []histogramBucket{
			{Max: 0, Count: 0}, {Max: 2, Count: 1}, {Max: 8, Count: 5}, {Max: 64, Count: 2},
		}},
		{"zero-count bucket in the middle", []histogramBucket{
			{Max: 2, Count: 1}, {Max: 8, Count: 0}, {Max: 64, Count: 2},
		}},
		{"single observation", []histogramBucket{{Max: 1, Count: 1}}},
	} {
		for _, phi := range quantileProbePhis {
			got, _ := log2QuantileWithBucket(phi, tc.buckets)
			if math.IsNaN(got) || math.IsInf(got, 0) {
				t.Errorf("%s, phi=%v: got %v", tc.name, phi, got)
			}
		}
	}
}

// quantileBucketSample builds one row of the matrix quantile stream: a
// (group, anchor, bucket) tuple whose value is the bucket's count.
func quantileBucketSample(svc string, anchor time.Time, bucket string, count float64) chclient.Sample {
	return chclient.Sample{
		Labels:    map[string]string{"service.name": svc, tempoQuantileBucketLabel: bucket},
		Timestamp: anchor,
		Value:     count,
	}
}

// TestPostProcessQuantileBuckets_MultiPhiOverZeroFilledAnchors drives the
// handler-side fold over the exact shape the failing dashboard panel produces:
// `{ } | quantile_over_time(duration, .5, .9, .99)`, whose stream carries the
// emitter's zero-fill row alongside the observed buckets. Every emitted sample
// must be a finite number — one NaN anywhere in the response body is enough for
// the encoder to refuse all of it.
func TestPostProcessQuantileBuckets_MultiPhiOverZeroFilledAnchors(t *testing.T) {
	t.Parallel()

	populated := time.Unix(1_700_000_000, 0).UTC()
	empty := populated.Add(15 * time.Second)

	m := &chplan.MetricsAggregate{
		Op:        chplan.MetricsOpQuantileOverTime,
		Quantiles: []float64{0.5, 0.9, 0.99},
	}
	samples := []chclient.Sample{
		// A populated anchor: the zero-fill row plus two observed buckets.
		quantileBucketSample("checkout", populated, "0", 0),
		quantileBucketSample("checkout", populated, "8", 3),
		quantileBucketSample("checkout", populated, "16", 2),
		// An anchor with no spans: zero-fill only.
		quantileBucketSample("checkout", empty, "0", 0),
	}

	out := postProcessQuantileBuckets(samples, m)

	if want := len(m.Quantiles) * 2; len(out) != want {
		t.Fatalf("want one sample per (anchor, phi) = %d, got %d", want, len(out))
	}
	for _, s := range out {
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			t.Errorf("anchor %s p=%s: got %v — this value cannot be JSON-encoded, "+
				"so it fails the whole response", s.Timestamp, s.Labels[tempoQuantileLabel], s.Value)
		}
	}

	// The zero-fill-only anchor is what the emitter plants the row FOR: it
	// must still be reported, and reported as 0.
	for _, s := range out {
		if !s.Timestamp.Equal(empty) {
			continue
		}
		if s.Value != 0 {
			t.Errorf("an anchor with no observations must read 0, got %v at p=%s",
				s.Value, s.Labels[tempoQuantileLabel])
		}
	}

	// The populated anchor must read the estimate its observed buckets imply,
	// unshifted by the zero-fill row.
	observed := []histogramBucket{{Max: 8, Count: 3}, {Max: 16, Count: 2}}
	for _, s := range out {
		if !s.Timestamp.Equal(populated) {
			continue
		}
		phi, err := strconv.ParseFloat(s.Labels[tempoQuantileLabel], 64)
		if err != nil {
			t.Fatalf("unparsable p label %q: %v", s.Labels[tempoQuantileLabel], err)
		}
		want, _ := log2QuantileWithBucket(phi, observed)
		if s.Value != want {
			t.Errorf("p=%s: got %v, want %v — the zero-fill row moved the estimate",
				s.Labels[tempoQuantileLabel], s.Value, want)
		}
	}
}

// TestPostProcessQuantileBuckets_UnobservedBucketDoesNotMoveTheEstimate pins
// the fold's own contract, independently of the one-octave convention the
// quantile helper falls back on.
//
// The histogram handed to the algorithm must contain the buckets that were
// OBSERVED and nothing else — that is the invariant upstream's aggregator is
// built on, since it appends a bucket the first time something lands in it. A
// zero-count row is a zero-fill artefact of the SQL shape, not an observation.
// The emitter plants those at edge 0 today, where the helper's fallback happens
// to recover the same number; at any other edge a retained zero-count row is a
// real boundary the estimate interpolates against, and the percentile silently
// moves. Dropping the row is what makes the answer independent of where the
// zero-fill lands.
func TestPostProcessQuantileBuckets_UnobservedBucketDoesNotMoveTheEstimate(t *testing.T) {
	t.Parallel()

	anchor := time.Unix(1_700_000_000, 0).UTC()
	m := &chplan.MetricsAggregate{
		Op:        chplan.MetricsOpQuantileOverTime,
		Quantiles: []float64{0.5, 0.9, 0.99},
	}

	// Three observations in one bucket: phi=0.5 targets the 2nd, which sits
	// strictly inside the bucket, so the estimate is interpolated against the
	// lower boundary rather than landing on an edge. That is what makes a
	// spurious boundary observable at all.
	observedOnly := []chclient.Sample{
		quantileBucketSample("checkout", anchor, "8", 3),
	}
	// The same observations, plus zero-count rows at edges below and above.
	// The one at edge 2 is the dangerous shape: retained, it becomes the
	// predecessor and drags the interpolation an octave down.
	withUnobserved := []chclient.Sample{
		quantileBucketSample("checkout", anchor, "0", 0),
		quantileBucketSample("checkout", anchor, "2", 0),
		quantileBucketSample("checkout", anchor, "8", 3),
		quantileBucketSample("checkout", anchor, "64", 0),
	}

	want := postProcessQuantileBuckets(observedOnly, m)
	got := postProcessQuantileBuckets(withUnobserved, m)

	if len(got) != len(want) {
		t.Fatalf("sample count changed: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Value != want[i].Value {
			t.Errorf("p=%s: an unobserved bucket moved the estimate: got %v, want %v",
				want[i].Labels[tempoQuantileLabel], got[i].Value, want[i].Value)
		}
	}
}
