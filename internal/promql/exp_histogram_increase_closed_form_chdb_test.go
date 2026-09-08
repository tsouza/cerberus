//go:build chdb

// chDB-backed differential proof that the closed-form counter fold
// (histogram_native_window_closed_form.go) answers exactly what the
// per-consecutive-pair telescoping fold it replaced answers, for EVERY
// reset pattern a window can carry and for both aggregation
// temporalities.
//
// # Why a differential rather than a hand-written reference
//
// The closed form is justified by an algebraic identity — both
// temporality readings of counterIncreaseFold are linear combinations of
// the same per-row values — and an identity is exactly the kind of claim
// that is easy to state and easy to get off by one. A Go reference for
// increase() would have to reproduce not just the counter rule but
// reference Prometheus's boundary extrapolation as well, and that is a
// second reading of upstream to get wrong: a first draft of this test did
// exactly that and reported 7.875 against an expected 8 — the reference
// was missing the extrapolation factor, not the SQL. Running the SAME
// query through BOTH lowerings cannot make that mistake, because every
// part outside the fold is literally the same code on both sides.
//
// This is the shape exp_histogram_merge_summap_chdb_test.go already uses
// to prove the sumMap merge against the groupArray fold.
package promql_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// closedFormSamples is the window size every pattern below uses. Five
// samples give four pairs, hence sixteen reset patterns — enough to cover
// a reset at the first pair (which cancels the closed form's own -1 on
// the earliest row, the one coefficient interaction with no analogue in
// the pair sum), a reset at the last pair, adjacent resets, and none at
// all, while keeping one seed small enough to walk exhaustively.
const closedFormSamples = 5

// closedFormBucketWidth is the stored PositiveBucketCounts width every
// seeded row carries. Two buckets are enough to prove the fold is applied
// per bucket rather than to a collapsed total.
const closedFormBucketWidth = 2

// closedFormStep spaces the seeded samples and is the query_range step.
const closedFormStep = 30 * time.Second

// closedFormMetric routes onto the exponential-histogram table.
const closedFormMetric = "closed_form_exp_hist"

// closedFormCoeffsAlias is the column the closed form projects. The test
// asserts the DEFAULT lowering carries it and the telescoping lowering
// does not, so neither arm can quietly become the other and leave this
// comparing a rendering against itself.
const closedFormCoeffsAlias = "_hq_win_coeffs"

// closedFormBaseline anchors the seed's final sample.
var closedFormBaseline = time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC)

const closedFormDDL = "" +
	"CREATE OR REPLACE TABLE otel_metrics_exponential_histogram (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), " +
	"`Count` UInt64, `Sum` Float64, `Scale` Int32, `ZeroCount` UInt64, " +
	"`PositiveOffset` Int32, `PositiveBucketCounts` Array(UInt64), " +
	"`NegativeOffset` Int32, `NegativeBucketCounts` Array(UInt64), " +
	"`AggregationTemporality` Int32" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n"

// closedFormPatternRows builds one series' per-sample bucket readings: a
// counter climbing by a per-bucket increment each sample, restarting from
// a low value at every pair the pattern marks. A restart drops every
// bucket below its previous reading, which is what DetectReset condemns —
// the test seeds the DATA that produces a mask, never the mask itself.
func closedFormPatternRows(resets []bool) [][]uint64 {
	rows := make([][]uint64, closedFormSamples)
	current := make([]uint64, closedFormBucketWidth)
	for j := range current {
		current[j] = uint64(10 * (j + 1))
	}
	for i := range rows {
		if i > 0 {
			if resets[i-1] {
				for j := range current {
					current[j] = uint64(j + 1)
				}
			} else {
				for j := range current {
					current[j] += uint64(3 * (j + 1))
				}
			}
		}
		row := make([]uint64, closedFormBucketWidth)
		copy(row, current)
		rows[i] = row
	}
	return rows
}

// closedFormSeedTuples renders one series' INSERT tuples.
func closedFormSeedTuples(name string, temporality int64, rows [][]uint64) []string {
	start := closedFormBaseline.Add(-time.Duration(len(rows)-1) * closedFormStep)
	tuples := make([]string, 0, len(rows))
	for i, counts := range rows {
		cells := make([]string, len(counts))
		var total uint64
		for j, c := range counts {
			cells[j] = fmt.Sprintf("%d", c)
			total += c
		}
		tuples = append(tuples, fmt.Sprintf(
			"('%s', map('series', '%s'), toDateTime64('%s', 9), %d, %f, 0, 0, 0, [%s], 0, [], %d)",
			closedFormMetric, name,
			start.Add(time.Duration(i)*closedFormStep).Format("2006-01-02 15:04:05"),
			total, float64(total), strings.Join(cells, ","), temporality,
		))
	}
	return tuples
}

// TestExpHistogramIncreaseClosedForm_ChDB_MatchesTelescopingOverEveryResetPattern
// walks every reset pattern at both temporalities and asserts the
// closed-form lowering and the telescoping lowering agree bucket for
// bucket, exactly.
//
// All patterns are seeded as distinct SERIES in ONE fixture and both
// renderings read back in one query each: a chDB fixture per pattern
// would spend its whole runtime standing sessions up rather than
// exercising the fold.
func TestExpHistogramIncreaseClosedForm_ChDB_MatchesTelescopingOverEveryResetPattern(t *testing.T) {
	patterns := 1 << (closedFormSamples - 1)
	tuples := make([]string, 0, patterns*2*closedFormSamples)
	names := make([]string, 0, patterns*2)
	for _, temporality := range []int64{
		schema.AggregationTemporalityCumulative,
		schema.AggregationTemporalityDelta,
	} {
		for mask := 0; mask < patterns; mask++ {
			resets := make([]bool, closedFormSamples-1)
			for i := range resets {
				resets[i] = mask&(1<<i) != 0
			}
			name := fmt.Sprintf("t%d_p%04b", temporality, mask)
			tuples = append(tuples, closedFormSeedTuples(name, temporality, closedFormPatternRows(resets))...)
			names = append(names, name)
		}
	}
	seed := closedFormDDL +
		"INSERT INTO otel_metrics_exponential_histogram (MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts, AggregationTemporality) VALUES\n    " +
		strings.Join(tuples, ",\n    ") + ";\n"
	fixture := newChDBFixture(t, seed)

	closed := closedFormRun(t, fixture, promql.LowerOpts{}, true)
	telescoping := closedFormRun(t, fixture, promql.LowerOpts{
		Lowerers: promql.RangeLowerers{
			ExpHistogramWindowFold: promql.TelescopingExpHistogramWindowFoldLowerer{},
		},
	}, false)

	if len(closed) != len(names) {
		t.Fatalf("closed form emitted %d series, want %d", len(closed), len(names))
	}
	for _, name := range names {
		got, ok := closed[name]
		if !ok {
			t.Fatalf("series %s missing from the closed-form result", name)
		}
		want, ok := telescoping[name]
		if !ok {
			t.Fatalf("series %s missing from the telescoping result", name)
		}
		if len(got) != closedFormBucketWidth || len(want) != closedFormBucketWidth {
			t.Fatalf("series %s: %d/%d buckets, want %d each", name, len(got), len(want), closedFormBucketWidth)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("series %s bucket %d: closed form = %v, telescoping = %v",
					name, j, got[j], want[j])
			}
		}
	}
}

// closedFormRun lowers `increase(<metric>[5m])` over a two-anchor
// query_range under opts and returns, per seeded series, the LAST
// anchor's own PositiveBucketCounts — the anchor whose window holds every
// seeded sample.
//
// wantClosedForm asserts which rendering the emitted SQL actually is, by
// the presence of the coefficient column. Without that check a lowering
// change that routed both calls onto the same arm would leave this test
// green while comparing a rendering against itself.
func closedFormRun(t *testing.T, fixture *chdbFixture, opts promql.LowerOpts, wantClosedForm bool) map[string][]float64 {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	expr, err := promparser.NewParser(promparser.Options{}).ParseExpr(
		"increase(" + closedFormMetric + "[5m])",
	)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.LowerAtRangeOpts(
		context.Background(), expr, s,
		closedFormBaseline.Add(-closedFormStep), closedFormBaseline, closedFormStep, opts,
	)
	if err != nil {
		t.Fatalf("LowerAtRangeOpts: %v", err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got := strings.Contains(sqlStr, closedFormCoeffsAlias); got != wantClosedForm {
		t.Fatalf("emitted SQL carries %s = %v, want %v — the two arms are not the two "+
			"renderings this test believes it is comparing", closedFormCoeffsAlias, got, wantClosedForm)
	}
	rows, err := fixture.db.Query(
		"SELECT `Attributes`['series'], arrayStringConcat(arrayMap(x -> toString(x), `HistogramPositiveBucketCounts`), ',') FROM ("+sqlStr+
			") WHERE `TimeUnix` = toDateTime64('"+closedFormBaseline.Format("2006-01-02 15:04:05")+"', 9)",
		args...,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]float64{}
	for rows.Next() {
		var name, joined string
		if err := rows.Scan(&name, &joined); err != nil {
			t.Fatalf("scan: %v", err)
		}
		parts := strings.Split(joined, ",")
		vals := make([]float64, len(parts))
		for i, p := range parts {
			if _, err := fmt.Sscanf(p, "%g", &vals[i]); err != nil {
				t.Fatalf("series %s: parse %q: %v", name, p, err)
			}
		}
		out[name] = vals
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}
