package gen

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/test/property"
)

// ExpHistogramTableName is the OTel-CH default exponential (native)
// histogram table. The DDL this generator emits targets this name;
// the chDB session and the SQL cerberus emits for an `_exp_hist`
// metric both expect it.
const ExpHistogramTableName = "otel_metrics_exponential_histogram"

// ExpHistogramMetricNamePool is the metric-name pool for the native
// (exponential) histogram dataset. Every name carries the `_exp_hist`
// suffix that [schema.Metrics.IsExpHistogramMetric] routes on (the
// default ExpHistogramSuffix): without it cerberus would read the
// classic-histogram table instead and every query would return zero
// series.
//
// Neither name ends in `_count` / `_sum` / `_total` / `_bucket`, so the
// companion-column and sum-table heuristics in internal/schema leave
// them alone.
var ExpHistogramMetricNamePool = []string{"request_latency_exp_hist", "queue_delay_exp_hist"}

// ExpHistogramScalePool is the OTel `scale` sweep. Scale fixes the
// bucket base as base = 2^(2^-scale), so a negative scale gives coarse
// buckets (scale -1 → base 4) and a positive one gives fine buckets
// (scale 2 → base ≈ 1.19). Sweeping both signs keeps the bucket-index
// interpolation in histogram_quantile and the geometric midpoints in
// histogram_stddev / _stdvar honest about the exponent arithmetic
// rather than only ever exercising the base-2 (scale 0) special case.
var ExpHistogramScalePool = []int32{-1, 0, 1, 2}

// ExpHistogramOffsetPool is the sweep for PositiveOffset /
// NegativeOffset — the absolute bucket index the first element of each
// bucket array sits at. Negative and positive offsets both appear so
// the `base^(offset+i)` mapping is exercised on either side of 1.0
// (bucket index 0), not just at the origin.
var ExpHistogramOffsetPool = []int32{-2, 0, 1, 3}

// ExpHistogramSumSkewPool scales the bucket-midpoint estimate into the
// stored Sum. A real OTel SDK records the true sum of observations,
// which is close to — but not equal to — the sum of the geometric
// bucket midpoints, because within a bucket the observations are not
// all at the midpoint. Skewing by these factors reproduces that
// mismatch, which matters: histogram_avg is Sum/Count and
// histogram_stddev takes Sum/Count as its mean, so pinning Sum to
// exactly the midpoint estimate would make the mean and the bucket
// mass artificially agree and hide a sign or offset error in the
// variance walk.
var ExpHistogramSumSkewPool = []float64{0.85, 1, 1.2}

const (
	// maxExpHistSeries bounds the series count per dataset. Small
	// enough that a shrunk counterexample stays readable, large enough
	// that a query matches several series at once.
	maxExpHistSeries = 4
	// maxExpHistPoints bounds the snapshots per series. Native
	// histograms are read via argMax(…, TimeUnix) — only the latest
	// snapshot per series is answered on — so extra points widen the
	// "pick the right row" axis rather than the arithmetic one, and a
	// handful is plenty.
	maxExpHistPoints = 4
	// maxExpHistPositiveBuckets / maxExpHistNegativeBuckets bound the
	// bucket-array lengths. Zero-length arrays are drawn too, which is
	// the positive-only / negative-only shape.
	maxExpHistPositiveBuckets = 4
	maxExpHistNegativeBuckets = 3
	// maxExpHistBucketCount bounds a single bucket's observation
	// count. Small integer counts keep the cumulative walk in
	// histogram_quantile landing on interpolation boundaries often.
	maxExpHistBucketCount = 5
	// maxExpHistZeroCount bounds the zero-bucket population. The zero
	// bucket collapses to a point at 0 (cerberus does not persist
	// ZeroThreshold), so it is its own region of the bucket walk.
	maxExpHistZeroCount = 3
	// expHistogramStep is the spacing between consecutive snapshots,
	// matching the gauge generator's cadence.
	expHistogramStep = 15 * time.Second
	// expHistogramBucketMidpointOffset is the fractional position of a
	// bucket's geometric midpoint in bucket-index space: the standard
	// exponential bucket k spans (base^k, base^(k+1)], whose geometric
	// midpoint is base^(k+0.5).
	expHistogramBucketMidpointOffset = 0.5
)

// ExpHistogramDataset returns a rapid generator drawing a random
// property.Dataset of OTel native (exponential) histogram series — the
// counterpart to [MetricsDataset], which draws gauge series only.
//
// Accept-set:
//
//   - 1–maxExpHistSeries series, each a distinct (name, label set)
//     pair drawn from ExpHistogramMetricNamePool and the shared
//     label pool.
//   - Per series, 1–maxExpHistPoints snapshots at expHistogramStep
//     spacing from the shared anchor.
//   - Per snapshot, an exponential histogram whose scale, offsets,
//     bucket arrays and zero bucket are all drawn (see
//     drawNativeHistogram).
//
// Every drawn histogram is internally consistent: Count is the exact
// total of the positive, negative and zero bucket populations, and Sum
// is a bucket-midpoint estimate skewed by ExpHistogramSumSkewPool.
// Inconsistent Count/Sum is not a shape any OTel SDK emits and has no
// defined answer to differ over, so drawing it would manufacture
// divergence rather than find it.
//
// The generator never draws a fully-empty histogram (Count == 0). Every
// one of the seven native-histogram functions is a division by Count or
// a walk over a zero-total cumulative array in that case, so the shape
// is its own semantic axis (NaN propagation and empty-vector handling)
// rather than more coverage of the arithmetic this dataset targets.
//
// The returned Dataset's DDL and Metrics mirror are in lock-step: the
// same drawn histograms are rendered into the INSERT and handed to the
// oracle.
func ExpHistogramDataset() *rapid.Generator[property.Dataset] {
	return rapid.Custom(func(t *rapid.T) property.Dataset {
		numSeries := rapid.IntRange(1, maxExpHistSeries).Draw(t, "numExpHistSeries")
		seen := map[string]struct{}{}

		series := make([]property.SeriesData, 0, numSeries)
		for i := 0; i < numSeries; i++ {
			name := rapid.SampledFrom(ExpHistogramMetricNamePool).Draw(t, fmt.Sprintf("expHistName_%d", i))
			lset := drawLabelSet(t, fmt.Sprintf("expHistLabels_%d", i))
			key := name + labelKey(lset)
			if _, dup := seen[key]; dup {
				// Same dedup rule as MetricsDataset: two series with
				// the same label-set hash would collapse into one on
				// both the storage and the oracle side.
				continue
			}
			seen[key] = struct{}{}

			series = append(series, property.SeriesData{
				MetricName: name,
				Labels:     lset,
				Points:     drawExpHistogramPoints(t, fmt.Sprintf("expHistPoints_%d", i)),
			})
		}

		return property.Dataset{
			DDL:     renderExpHistogramDDL(series),
			Metrics: &property.MetricsModel{Series: series},
		}
	})
}

// drawExpHistogramPoints emits 1–maxExpHistPoints snapshots, each
// carrying an independently drawn histogram payload. Each row is a full
// snapshot at its own TimeUnix rather than a delta: cerberus answers a
// native-histogram selector with argMax(…, TimeUnix), so the latest row
// is the whole answer.
func drawExpHistogramPoints(t *rapid.T, id string) []property.Point {
	count := rapid.IntRange(1, maxExpHistPoints).Draw(t, id+"_count")
	out := make([]property.Point, 0, count)
	for i := 0; i < count; i++ {
		h := drawNativeHistogram(t, fmt.Sprintf("%s_h_%d", id, i))
		out = append(out, property.Point{
			TimestampMs: anchorTime.Add(time.Duration(i) * expHistogramStep).UnixMilli(),
			Histogram:   &h,
		})
	}
	return out
}

// drawNativeHistogram draws one exponential-histogram sample. Count is
// derived (never drawn) so it always equals the bucket populations, and
// Sum is derived from the bucket midpoints so it always lies in the
// range the buckets describe.
//
// When every bucket draw comes up empty the zero bucket is populated
// with a single observation, which keeps the dataset inside the
// Count > 0 accept-set documented on [ExpHistogramDataset] without
// discarding the draw (a rejected draw would bias the shrinker).
func drawNativeHistogram(t *rapid.T, id string) property.NativeHistogram {
	scale := rapid.SampledFrom(ExpHistogramScalePool).Draw(t, id+"_scale")
	posOffset := rapid.SampledFrom(ExpHistogramOffsetPool).Draw(t, id+"_posOffset")
	negOffset := rapid.SampledFrom(ExpHistogramOffsetPool).Draw(t, id+"_negOffset")
	pos := drawBucketCounts(t, id+"_pos", maxExpHistPositiveBuckets)
	neg := drawBucketCounts(t, id+"_neg", maxExpHistNegativeBuckets)
	zero := rapid.Uint64Range(0, maxExpHistZeroCount).Draw(t, id+"_zero")

	total := zero + sumBucketCounts(pos) + sumBucketCounts(neg)
	if total == 0 {
		zero = 1
		total = 1
	}

	skew := rapid.SampledFrom(ExpHistogramSumSkewPool).Draw(t, id+"_sumSkew")

	return property.NativeHistogram{
		Count:                total,
		Sum:                  bucketMidpointSum(scale, posOffset, pos, negOffset, neg) * skew,
		Scale:                scale,
		ZeroCount:            zero,
		PositiveOffset:       posOffset,
		PositiveBucketCounts: pos,
		NegativeOffset:       negOffset,
		NegativeBucketCounts: neg,
	}
}

// drawBucketCounts draws a 0..maxLen bucket array with each element in
// [0, maxExpHistBucketCount]. Empty interior buckets are allowed —
// they are a real shape (a gap between populated buckets) and they make
// the cumulative walk in histogram_quantile step over zero-width
// regions, where a fraction of 0/0 must not leak a NaN.
func drawBucketCounts(t *rapid.T, id string, maxLen int) []uint64 {
	n := rapid.IntRange(0, maxLen).Draw(t, id+"_len")
	if n == 0 {
		return nil
	}
	out := make([]uint64, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, rapid.Uint64Range(0, maxExpHistBucketCount).Draw(t, fmt.Sprintf("%s_%d", id, i)))
	}
	return out
}

func sumBucketCounts(xs []uint64) uint64 {
	var total uint64
	for _, x := range xs {
		total += x
	}
	return total
}

// bucketMidpointSum estimates the sum of the observations a histogram
// describes by placing every bucket's population at that bucket's
// geometric midpoint, base^(offset+i+0.5), negated on the negative
// side. The zero bucket contributes nothing (its observations sit at
// 0). This is the same midpoint convention histogram_stddev /
// histogram_stdvar use, so a generated Sum is always a value the
// buckets could plausibly have produced.
func bucketMidpointSum(scale, posOffset int32, pos []uint64, negOffset int32, neg []uint64) float64 {
	base := math.Pow(2, math.Pow(2, -float64(scale)))
	var sum float64
	for i, c := range pos {
		sum += float64(c) * math.Pow(base, float64(posOffset)+float64(i)+expHistogramBucketMidpointOffset)
	}
	for i, c := range neg {
		sum -= float64(c) * math.Pow(base, float64(negOffset)+float64(i)+expHistogramBucketMidpointOffset)
	}
	return sum
}

// renderExpHistogramDDL produces the multi-statement seed script for
// series: one `CREATE OR REPLACE TABLE` plus one INSERT per series.
//
// The column set mirrors the OTel-CH exponential-histogram layout
// [schema.DefaultOTelMetrics] reads (ExpHistogramTable and its
// Count / Sum / Scale / ZeroCount / {Positive,Negative}Offset /
// {Positive,Negative}BucketCounts columns). As in renderDDL, the engine
// is MergeTree (chDB's Memory engine rejects the PREWHERE cerberus's
// optimizer promotes) and ResourceAttributes is present-but-empty so
// the read path's `mapUpdate(ResourceAttributes, Attributes)` label
// merge collapses to Attributes alone.
func renderExpHistogramDDL(series []property.SeriesData) string {
	var b strings.Builder
	b.WriteString(`CREATE OR REPLACE TABLE `)
	b.WriteString(ExpHistogramTableName)
	b.WriteString(` (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    Scale Int32,
    ZeroCount UInt64,
    PositiveOffset Int32,
    PositiveBucketCounts Array(UInt64),
    NegativeOffset Int32,
    NegativeBucketCounts Array(UInt64)
) ENGINE = MergeTree ORDER BY (MetricName, TimeUnix);
`)
	for _, s := range series {
		b.WriteString(`INSERT INTO `)
		b.WriteString(ExpHistogramTableName)
		b.WriteString(` (MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount,` +
			` PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES `)
		for i, p := range s.Points {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(renderExpHistogramRow(s.MetricName, s.Labels, p))
		}
		b.WriteString(";\n")
	}
	return b.String()
}

// renderExpHistogramRow renders one INSERT tuple from a Point carrying
// a Histogram payload.
func renderExpHistogramRow(name string, labels map[string]string, p property.Point) string {
	h := p.Histogram
	var b strings.Builder
	b.WriteByte('(')
	b.WriteByte('\'')
	b.WriteString(name)
	b.WriteByte('\'')
	b.WriteString(", ")
	b.WriteString(renderMap(labels))
	b.WriteString(", ")
	b.WriteString(tsLiteral(p.TimestampMs))
	b.WriteString(", ")
	b.WriteString(strconv.FormatUint(h.Count, 10))
	b.WriteString(", ")
	b.WriteString(formatFloat(h.Sum))
	b.WriteString(", ")
	b.WriteString(strconv.FormatInt(int64(h.Scale), 10))
	b.WriteString(", ")
	b.WriteString(strconv.FormatUint(h.ZeroCount, 10))
	b.WriteString(", ")
	b.WriteString(strconv.FormatInt(int64(h.PositiveOffset), 10))
	b.WriteString(", ")
	b.WriteString(renderUintArray(h.PositiveBucketCounts))
	b.WriteString(", ")
	b.WriteString(strconv.FormatInt(int64(h.NegativeOffset), 10))
	b.WriteString(", ")
	b.WriteString(renderUintArray(h.NegativeBucketCounts))
	b.WriteByte(')')
	return b.String()
}

// renderUintArray renders a bucket-count slice as a CH array literal.
// A nil/empty slice renders as `[]`, which CH accepts for an
// Array(UInt64) column.
func renderUintArray(xs []uint64) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range xs {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.FormatUint(x, 10))
	}
	b.WriteByte(']')
	return b.String()
}
