//go:build chdb

// chDB-backed proof that a native-histogram rank walk which exhausts every
// bucket answers the way reference Prometheus answers it.
//
// Reference `promql.HistogramQuantile` returns NaN from that path only when the
// histogram's Sum is NaN; with a finite Sum it falls back to the upper bound of
// the bucket its iterator was left sitting on (quantile.go, the `if count <
// rank` block added for prometheus/prometheus#16578). cerberus returned NaN in
// both cases (#2405). The walk exhausts whenever the stored Count exceeds the
// buckets' combined reach, which a NaN observation causes — and so does a +Inf
// one, which leaves Sum finite and therefore leaves the fallback reachable.
//
// The assertion runs the emitted SQL rather than inspecting it, because the bug
// was a wrong NUMBER produced by well-formed SQL.
//
// Gated by `//go:build chdb` so the default `check` lane (CGO off, no
// libchdb.so) skips it; the dedicated `chdb` workflow runs it.
package chsql_test

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

// exhaustedWalkRow is one stored exp-histogram row. Scale and both offsets are
// fixed at 0 across every case, so the base is 2 and bucket index 0 covers
// (1, 2]: every expected answer below is a whole power of two, an exact bucket
// edge no interpolation can move.
//
// ZeroThreshold is absent on purpose — the upstream OTel-CH exp-histogram DDL
// does not persist it, so this is the production column set and the zero bucket
// is a point at 0.
type exhaustedWalkRow struct {
	zeroCount uint64
	positive  []uint64
	negative  []uint64
	count     uint64
	// sum is spelled as SQL because the cases that matter store nan and inf,
	// which no Go float literal survives into a query text.
	sum string
}

func (r exhaustedWalkRow) sql() string {
	return fmt.Sprintf("SELECT "+
		"map('service', 'api') AS Attributes, "+
		"toInt32(0) AS Scale, "+
		"toUInt64(%d) AS ZeroCount, "+
		"toInt32(0) AS PositiveOffset, "+
		"%s AS PositiveBucketCounts, "+
		"toInt32(0) AS NegativeOffset, "+
		"%s AS NegativeBucketCounts, "+
		"toUInt64(%d) AS Count, "+
		"%s AS Sum",
		r.zeroCount, bucketArrayLiteral(r.positive), bucketArrayLiteral(r.negative), r.count, r.sum)
}

func bucketArrayLiteral(counts []uint64) string {
	if len(counts) == 0 {
		return "emptyArrayUInt64()"
	}
	parts := make([]string, 0, len(counts))
	for _, c := range counts {
		parts = append(parts, "toUInt64("+strconv.FormatUint(c, 10)+")")
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// exhaustedWalkInterpolationTolerance is the relative agreement demanded of the
// control cases, which land strictly inside a bucket and so are moved by 1-5
// ULP by cerberus interpolating with pow() where reference uses Log2/Exp2. That
// residual is issue #2024 and is deliberately not what this test is about; the
// tolerance is tight enough to catch a changed bucket — which moves the answer
// by a factor of the base — while ignoring last-digit noise.
const exhaustedWalkInterpolationTolerance = 1e-12

func TestEmitHistogramQuantileNative_ExhaustedWalkMatchesReferenceFallback(t *testing.T) {
	// Every histogram whose Count (100) exceeds its bucket-derived total
	// exhausts the walk for the phi values paired with it below.
	var (
		positiveOnly = exhaustedWalkRow{positive: []uint64{2, 3}, count: 100, sum: "toFloat64(3)"}
		nanSum       = exhaustedWalkRow{positive: []uint64{2, 3}, count: 100, sum: "nan"}
		infSum       = exhaustedWalkRow{positive: []uint64{2, 3}, count: 100, sum: "inf"}
		negativeOnly = exhaustedWalkRow{negative: []uint64{2, 3}, count: 100, sum: "toFloat64(-3)"}
		bothSides    = exhaustedWalkRow{negative: []uint64{2}, zeroCount: 1, positive: []uint64{3}, count: 100, sum: "toFloat64(3)"}
		zeroAndPos   = exhaustedWalkRow{zeroCount: 1, positive: []uint64{2, 3}, count: 100, sum: "toFloat64(3)"}
		negAndZero   = exhaustedWalkRow{negative: []uint64{2, 3}, zeroCount: 1, count: 100, sum: "toFloat64(-3)"}
		zeroOnly     = exhaustedWalkRow{zeroCount: 2, count: 100, sum: "toFloat64(0)"}
		noBuckets    = exhaustedWalkRow{count: 5, sum: "inf"}
		// The control: the same buckets with a Count that matches their
		// total, so the walk stops on a bucket and the fallback must not
		// fire at all.
		notExhausted = exhaustedWalkRow{positive: []uint64{2, 3}, count: 5, sum: "toFloat64(3)"}
	)

	cases := []struct {
		name string
		row  exhaustedWalkRow
		phi  float64
		// want is reference Prometheus's own answer for this histogram,
		// obtained from promql.HistogramQuantile in the pinned fork
		// (github.com/tsouza/prometheus@v0.0.1-cerberus-parser).
		want float64
		// wantNaN marks the one shape reference still answers NaN for: a
		// NaN Sum.
		wantNaN bool
		// interpolated marks a control that lands strictly inside a bucket,
		// where only relative agreement is meaningful.
		interpolated bool
	}{
		// Forward arm (phi < 0.5): the fallback is the upper edge of the
		// LAST position the iterator yields.
		{name: "forward/positive_only", row: positiveOnly, phi: 0.2, want: 4},
		{name: "forward/positive_only_just_below_reverse", row: positiveOnly, phi: 0.49, want: 4},
		{name: "forward/inf_sum", row: infSum, phi: 0.2, want: 4},
		{name: "forward/negative_only", row: negativeOnly, phi: 0.2, want: -1},
		{name: "forward/both_sides", row: bothSides, phi: 0.2, want: 2},
		{name: "forward/zero_and_positive", row: zeroAndPos, phi: 0.2, want: 4},
		// No positive buckets, so the forward walk ends on the populated
		// zero bucket, whose upper edge reference clamps to 0 for a
		// histogram with nothing above zero.
		{name: "forward/negative_and_zero", row: negAndZero, phi: 0.2, want: 0},
		{name: "forward/zero_bucket_only", row: zeroOnly, phi: 0.2, want: 0},
		// The iterator yields nothing at all, leaving reference's bucket at
		// its zero value.
		{name: "forward/no_buckets", row: noBuckets, phi: 0.2, want: 0},

		// Backward arm (phi >= 0.5): the fallback is the upper edge of the
		// FIRST position the iterator yields.
		{name: "backward/positive_only", row: positiveOnly, phi: 0.9, want: 2},
		{name: "backward/positive_only_at_reverse_boundary", row: positiveOnly, phi: 0.5, want: 2},
		{name: "backward/inf_sum", row: infSum, phi: 0.9, want: 2},
		{name: "backward/negative_only", row: negativeOnly, phi: 0.9, want: -2},
		{name: "backward/both_sides", row: bothSides, phi: 0.9, want: -1},
		// No negative buckets, so the backward walk ends on the populated
		// zero bucket.
		{name: "backward/zero_and_positive", row: zeroAndPos, phi: 0.9, want: 0},
		{name: "backward/negative_and_zero", row: negAndZero, phi: 0.9, want: -2},
		{name: "backward/zero_bucket_only", row: zeroOnly, phi: 0.9, want: 0},
		{name: "backward/no_buckets", row: noBuckets, phi: 0.9, want: 0},

		// A NaN Sum keeps the NaN answer in both directions — reference
		// forces the forward arm there, and returns NaN from it.
		{name: "nan_sum/forward", row: nanSum, phi: 0.2, wantNaN: true},
		{name: "nan_sum/reverse_phi", row: nanSum, phi: 0.9, wantNaN: true},

		// Controls: the walk reaches the rank, so the answer comes from the
		// stop bucket's interpolation and not from either array end.
		{name: "control/forward", row: notExhausted, phi: 0.2, want: 1.414213562373095, interpolated: true},
		{name: "control/backward", row: notExhausted, phi: 0.9, want: 3.5635948725613575, interpolated: true},
	}

	db := chsqltest.OpenIsolatedChDB(t)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := &chplan.HistogramQuantileNative{
				Input:                      &chplan.Scan{Table: "hq"},
				Phi:                        tc.phi,
				ScaleColumn:                "Scale",
				ZeroCountColumn:            "ZeroCount",
				PositiveOffsetColumn:       "PositiveOffset",
				PositiveBucketCountsColumn: "PositiveBucketCounts",
				NegativeOffsetColumn:       "NegativeOffset",
				NegativeBucketCountsColumn: "NegativeBucketCounts",
				CountColumn:                "Count",
				SumColumn:                  "Sum",
				GroupBy:                    []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
				GroupByAliases:             []string{"Attributes"},
				MetricNameColumn:           "MetricName",
				AttributesColumn:           "Attributes",
				TimestampColumn:            "TimeUnix",
			}
			gotSQL, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("emit: %v", err)
			}
			if len(args) != 0 {
				t.Fatalf("plan has no parameters, got args %v", args)
			}

			query := replaceScanTable(t, gotSQL, "`hq`", "("+tc.row.sql()+")")

			// Read the answer back as a string so the assertion turns on
			// ClickHouse's own Float64, not on the chDB driver's column-type
			// mapping.
			var raw string
			if err := db.QueryRow("SELECT toString(`Value`) FROM (" + query + ")").Scan(&raw); err != nil {
				t.Fatalf("query %s: %v", query, err)
			}
			got, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				t.Fatalf("parse %q: %v", raw, err)
			}

			switch {
			case tc.wantNaN:
				if !math.IsNaN(got) {
					t.Fatalf("histogram_quantile(%v) = %v, want NaN: reference answers NaN from an "+
						"exhausted walk exactly when the histogram's Sum is NaN", tc.phi, got)
				}
			case tc.interpolated:
				if rel := math.Abs(got-tc.want) / math.Abs(tc.want); rel > exhaustedWalkInterpolationTolerance {
					t.Fatalf("histogram_quantile(%v) = %v, want %v (relative error %g > %g).\n"+
						"This walk reaches its rank, so the answer must come from the stop "+
						"bucket's interpolation; an error this large means the exhausted-walk "+
						"fallback leaked into the normal path.",
						tc.phi, got, tc.want, rel, exhaustedWalkInterpolationTolerance)
				}
			default:
				if got != tc.want {
					t.Fatalf("histogram_quantile(%v) = %v, want exactly %v.\n"+
						"The rank walk exhausts every bucket here, and reference Prometheus "+
						"answers with the upper bound of the bucket its iterator was left on "+
						"rather than with NaN, which it reserves for a NaN Sum (#2405).",
						tc.phi, got, tc.want)
				}
			}
		})
	}
}
