//go:build chdb

// chDB-backed proof that limitk()/limit_ratio() over a mixed
// float/histogram `or` (cerberus issue #2613) PRESERVE both arms — unlike
// topk()/bottomk(), which drop the histogram-shaped side (see
// histogram_native_mixed_or_aggregate_topk.go), reference Prometheus's
// LIMITK/LIMIT_RATIO arms of aggregationK push every selected sample onto
// the heap/sampler with no `s.H != nil` branch anywhere in their control
// flow. This reuses histogram_native_mixed_or_scale_chdb_test.go's own
// two-series seed (one histogram-valued series, one that reduces to float
// via histogram_quantile) so a discriminator-blind or drop-family bug is
// visible: it would either error (referencing the wrong column set) or
// silently return only one of the two series.
package promql_test

import (
	"context"
	"math"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

func TestLimitKAndLimitRatioOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, scaleWrappedSeed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	mixedOr := `(histogram_quantile(0.5, ` + scaleWrappedFloatMetric + `) or ` + scaleWrappedHistMetric + `)`

	cases := []struct {
		name  string
		query string
	}{
		// K=2 over exactly two available series (one float-shaped, one
		// histogram-shaped) selects both deterministically — limitk has no
		// ORDER BY, so a global (no by/without) `LIMIT 2` with only 2 rows
		// available cannot drop either.
		{name: "limitk preserves both arms", query: `limitk(2, ` + mixedOr + `)`},
		// r=1 is reference's "keep everyone" boundary (limitRatioPredicate
		// emits `offset < r`, and every offset lands in [0,1)) — chosen
		// specifically so this test does not depend on which side of an
		// arbitrary hash threshold either series' label set falls on.
		{name: "limit_ratio(1, ...) preserves both arms", query: `limit_ratio(1, ` + mixedOr + `)`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, scaleWrappedEvalTS, scaleWrappedEvalTS)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", tc.query, err)
			}
			sqlStr, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", tc.query, err)
			}

			projection := "`Attributes`['series'] AS series, `_setop_is_histogram` AS disc, " +
				"`Value` AS val, `HistogramCount` AS cnt, `HistogramSum` AS sum, `HistogramPositiveBucketCounts`[1] AS bucket1"
			rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
			defer func() { _ = rows.Close() }()

			seen := map[string]bool{}
			n := 0
			for rows.Next() {
				var series string
				var disc int
				var val, cnt, sum, bucket1 float64
				if err := rows.Scan(&series, &disc, &val, &cnt, &sum, &bucket1); err != nil {
					t.Fatalf("scan: %v", err)
				}
				n++
				seen[series] = true
				switch series {
				case "float":
					if disc != 0 {
						t.Errorf("%s: float row's discriminator = %d, want 0", tc.query, disc)
					}
					if math.Abs(val-scaleWrappedQuantileBaseline) > 1e-9 {
						t.Errorf("%s: float row's Value = %v, want %v (unchanged — limitk/limit_ratio never transform the payload)", tc.query, val, scaleWrappedQuantileBaseline)
					}
				case "hist":
					if disc != 1 {
						t.Errorf("%s: histogram row's discriminator = %d, want 1", tc.query, disc)
					}
					// Raw seeded values (Count=3, Sum=6.0, bucket=[9]) —
					// limitk/limit_ratio select rows, they never scale a
					// payload, so these must be byte-identical to the seed.
					if cnt != 3 || sum != 6.0 || bucket1 != 9 {
						t.Errorf("%s: histogram row payload = (cnt=%v sum=%v bucket1=%v), want (3, 6, 9) unchanged", tc.query, cnt, sum, bucket1)
					}
				default:
					t.Errorf("%s: unexpected series label %q", tc.query, series)
				}
			}
			if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
				t.Fatalf("rows.Err: %v", err)
			}
			if n != 2 || !seen["float"] || !seen["hist"] {
				t.Errorf("%s: expected exactly the float AND hist rows to survive (got %d rows, seen=%v) — a drop-family or discriminator-blind bug loses one arm", tc.query, n, seen)
			}
		})
	}
}
