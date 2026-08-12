//go:build chdb

// chDB-backed proof that the native-histogram quantile walks its buckets in
// the direction reference Prometheus walks them.
//
// Reference `promql.HistogramQuantile` accumulates the rank forward from the
// lowest bucket for phi < 0.5 and BACKWARD from the highest for phi >= 0.5,
// skipping empty buckets in either direction. The two arms agree everywhere
// except at an exact rank tie: forward stops on the bucket whose cumulative
// count ENDS at the rank and reports its upper edge, backward stops on the
// next POPULATED bucket and reports its lower edge. cerberus walked forward at
// every phi, so a histogram with equal mass either side of an empty zero
// bucket answered its own median with the wrong SIGN (#2066).
//
// The assertion runs the emitted SQL rather than inspecting it, because the
// bug was a wrong NUMBER, not a missing token: the pre-fix emitter produced
// well-formed SQL that computed -1 where Prometheus computes +1.
//
// Gated by `//go:build chdb` so the default `check` lane (CGO off, no
// libchdb.so) skips it; the dedicated `chdb` workflow runs it.
package chsql_test

import (
	"context"
	"math"
	"strconv"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// rankTieHistogramRow is the reproduction from #2066: base-2 buckets
// (Scale 0), two observations in the bucket immediately below zero and two in
// the bucket immediately above it, with an EMPTY zero bucket between them. The
// median rank of 2 lands exactly on the boundary the two walk directions
// disagree about.
//
// ZeroThreshold is absent on purpose — the upstream OTel-CH exp-histogram DDL
// does not persist it, so this is the production column set.
const rankTieHistogramRow = `SELECT ` +
	`map('service', 'api') AS Attributes, ` +
	`toInt32(0) AS Scale, ` +
	`toUInt64(0) AS ZeroCount, ` +
	`toInt32(0) AS PositiveOffset, ` +
	`[toUInt64(2)] AS PositiveBucketCounts, ` +
	`toInt32(0) AS NegativeOffset, ` +
	`[toUInt64(2)] AS NegativeBucketCounts`

// rankTieInterpolationTolerance is the relative agreement demanded of the
// phi values that are NOT at the tie. Those land strictly inside a bucket, so
// both walk directions pick the same one and only the interpolation arithmetic
// can move them — and it does, by 1-5 ULP, because cerberus interpolates with
// pow() where reference uses Log2/Exp2. That residual is issue #2024 and is
// deliberately NOT what this test is about; the tolerance is tight enough to
// catch a changed bucket (which moves the answer by a factor of the base,
// i.e. 100% relative) while ignoring last-digit noise.
const rankTieInterpolationTolerance = 1e-12

func TestEmitHistogramQuantileNative_RankTieMatchesReferenceWalk(t *testing.T) {
	cases := []struct {
		phi float64
		// want is reference Prometheus's own answer for this histogram,
		// obtained from promql.HistogramQuantile in the pinned fork
		// (github.com/tsouza/prometheus@v0.0.1-cerberus-parser).
		want float64
		// exact marks the rank tie, where the walk direction decides the
		// answer outright and no interpolation noise can enter: the stop
		// bucket is consumed at fraction 0, so the result is a bucket edge,
		// a whole power of the base.
		exact bool
	}{
		{phi: 0.1, want: -1.7411011265922482},
		{phi: 0.25, want: -1.414213562373095},
		// The tie. Forward answers -1 (upper edge of the negative bucket),
		// backward answers +1 (lower edge of the positive one) — same
		// magnitude, opposite sign, and only the second is Prometheus's.
		{phi: 0.5, want: 1, exact: true},
		{phi: 0.75, want: 1.414213562373095},
		{phi: 0.9, want: 1.7411011265922482},
		{phi: 0.99, want: 1.9724654089867184},
	}

	db := chsql.OpenIsolatedChDB(t)

	for _, tc := range cases {
		t.Run("phi="+strconv.FormatFloat(tc.phi, 'g', -1, 64), func(t *testing.T) {
			plan := &chplan.HistogramQuantileNative{
				Input:                      &chplan.Scan{Table: "hq"},
				Phi:                        tc.phi,
				ScaleColumn:                "Scale",
				ZeroCountColumn:            "ZeroCount",
				PositiveOffsetColumn:       "PositiveOffset",
				PositiveBucketCountsColumn: "PositiveBucketCounts",
				NegativeOffsetColumn:       "NegativeOffset",
				NegativeBucketCountsColumn: "NegativeBucketCounts",
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

			query := replaceScanTable(t, gotSQL, "`hq`", "("+rankTieHistogramRow+")")

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

			if tc.exact {
				if got != tc.want {
					t.Fatalf("histogram_quantile(%v) = %v, want exactly %v.\n"+
						"At an exact rank tie the walk DIRECTION decides the answer: "+
						"a forward-only walk stops on the bucket ending at the rank and "+
						"reports its upper edge, where reference Prometheus walks backward "+
						"for phi >= 0.5, skips the empty zero bucket and reports the lower "+
						"edge of the next populated one (#2066).", tc.phi, got, tc.want)
				}
				return
			}
			// Away from the tie both directions select the same bucket, so any
			// disagreement beyond last-digit noise means the walk moved.
			if rel := math.Abs(got-tc.want) / math.Abs(tc.want); rel > rankTieInterpolationTolerance {
				t.Fatalf("histogram_quantile(%v) = %v, want %v (relative error %g > %g).\n"+
					"This phi lands strictly inside a bucket, so both walk directions "+
					"agree on it; an error this large means the stop bucket changed, "+
					"not that interpolation drifted (#2024).",
					tc.phi, got, tc.want, rel, rankTieInterpolationTolerance)
			}
		})
	}
}
