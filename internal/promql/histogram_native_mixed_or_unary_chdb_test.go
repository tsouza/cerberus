//go:build chdb

// chDB-backed proof that unary `+`/`-` over a mixed float/histogram `or`
// (cerberus issue #2613) actually scales the histogram-shaped row's own
// nine Histogram*Column fields at real ClickHouse execution — not merely
// that the emitted plan's Go shape looks right. Reuses the identical seed
// and baseline histogram_native_mixed_or_scale_chdb_test.go pins for `* 3`
// / `/ 3`, since unary `-` is reference Prometheus's `Mul(-1)` and so goes
// through the exact same scale fold this file's sibling test already
// verifies field-by-field.
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

func TestUnaryOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, scaleWrappedSeed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name         string
		query        string
		wantFloatVal float64
		wantCount    float64
		wantSum      float64
		wantBucket1  float64
	}{
		{
			name:         "unary minus negates both arms",
			query:        `-(histogram_quantile(0.5, ` + scaleWrappedFloatMetric + `) or ` + scaleWrappedHistMetric + `)`,
			wantFloatVal: -scaleWrappedQuantileBaseline,
			wantCount:    -3,
			wantSum:      -6.0,
			wantBucket1:  -9,
		},
		{
			name:         "unary plus is the identity",
			query:        `+(histogram_quantile(0.5, ` + scaleWrappedFloatMetric + `) or ` + scaleWrappedHistMetric + `)`,
			wantFloatVal: scaleWrappedQuantileBaseline,
			wantCount:    3,
			wantSum:      6.0,
			wantBucket1:  9,
		},
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

			// See histogram_native_mixed_or_scale_chdb_test.go's identical
			// comment: arrayElement[1] on an out-of-bounds/empty array
			// answers the type default (0), which is what the float row's
			// placeholder reads.
			projection := "`Attributes`['series'] AS series, `_setop_is_histogram` AS disc, " +
				"`Value` AS val, `HistogramCount` AS cnt, `HistogramSum` AS sum, `HistogramPositiveBucketCounts`[1] AS bucket1"
			rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
			defer func() { _ = rows.Close() }()

			seen := map[string]bool{}
			for rows.Next() {
				var series string
				var disc int
				var val, cnt, sum, bucket1 float64
				if err := rows.Scan(&series, &disc, &val, &cnt, &sum, &bucket1); err != nil {
					t.Fatalf("scan: %v", err)
				}
				seen[series] = true
				switch series {
				case "float":
					if disc != 0 {
						t.Errorf("%s: float row's discriminator = %d, want 0", tc.query, disc)
					}
					if math.Abs(val-tc.wantFloatVal) > 1e-9 {
						t.Errorf("%s: float row's Value = %v, want %v", tc.query, val, tc.wantFloatVal)
					}
				case "hist":
					if disc != 1 {
						t.Errorf("%s: histogram row's discriminator = %d, want 1", tc.query, disc)
					}
					if cnt != tc.wantCount {
						t.Errorf("%s: histogram row's HistogramCount = %v, want %v (raw seeded value is 3 — an unfired or drop-family fix would leave this at 3, or the row missing entirely)", tc.query, cnt, tc.wantCount)
					}
					if sum != tc.wantSum {
						t.Errorf("%s: histogram row's HistogramSum = %v, want %v (raw seeded value is 6)", tc.query, sum, tc.wantSum)
					}
					if bucket1 != tc.wantBucket1 {
						t.Errorf("%s: histogram row's HistogramPositiveBucketCounts[1] = %v, want %v (raw seeded value is 9)", tc.query, bucket1, tc.wantBucket1)
					}
				default:
					t.Errorf("%s: unexpected series label %q", tc.query, series)
				}
			}
			if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
				t.Fatalf("rows.Err: %v", err)
			}
			if !seen["float"] || !seen["hist"] {
				t.Errorf("%s: expected both float and hist rows to survive, got %v", tc.query, seen)
			}
		})
	}
}
