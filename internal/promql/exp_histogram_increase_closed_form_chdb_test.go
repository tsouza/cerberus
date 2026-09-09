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

// closedFormBucketWidth is the stored PositiveBucketCounts width the
// reset-pattern family's rows carry. Two buckets are enough to prove the
// fold is applied per bucket rather than to a collapsed total; the
// rescaling family ([closedFormLadderRows]) is wider.
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
func closedFormPatternRows(resets []bool) []closedFormRow {
	rows := make([]closedFormRow, closedFormSamples)
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
		rows[i] = closedFormRow{pos: row}
	}
	return rows
}

// closedFormRow is one seeded sample: the stored exponential-histogram
// row shape the two renderings must agree over. Scale and the two offsets
// are per-ROW rather than per-series so a window can carry the
// rescaling — several stored positions folding onto one merged bucket,
// and rows starting at different absolute indices — that the closed
// form's dense per-row reading has to reconcile before it can add rows
// together. [closedFormPatternRows] leaves them all at the flat
// (scale 0, offset 0, no negative ladder) shape; [closedFormLadderRows]
// varies them.
type closedFormRow struct {
	scale     int32
	posOffset int32
	pos       []uint64
	negOffset int32
	neg       []uint64
}

// closedFormLadderScales is the per-sample Scale the rescaling family
// seeds, one entry per sample. It is NON-INCREASING in time so no pair is
// condemned for a resolution increase alone: what this family is for is
// the DOWNSCALE arithmetic (several stored positions folding onto one
// merged bucket), not another reset pattern — the reset walk above
// already covers those exhaustively.
var closedFormLadderScales = [closedFormSamples]int32{3, 3, 2, 1, 1}

// closedFormLadderOffsets is the per-sample PositiveOffset/NegativeOffset
// the rescaling family seeds. They differ per sample so the rows start at
// different absolute bucket indices and the merged range is wider than
// any one row, which is the case where a row's dense contribution has to
// be zero-padded on one side before rows can be added together.
var closedFormLadderOffsets = [closedFormSamples]int32{-2, 0, 1, 0, 2}

// closedFormLadderWidth is the stored ladder width the rescaling family
// seeds. Wider than [closedFormBucketWidth] so a downscale by two levels
// still leaves more than one merged target populated.
const closedFormLadderWidth = 6

// closedFormLadderMergedWidth is the number of merged-scale positive
// buckets [closedFormLadderRows]'s staggered rows fold onto. It is wider
// than one row's own [closedFormLadderWidth], which is the point: rows
// that all covered the same target range would never exercise the
// zero-padding one row's dense contribution needs before it can be added
// to another's.
const closedFormLadderMergedWidth = 9

// closedFormLadderRows builds the rescaling family's samples: a counter
// climbing in every stored bucket, with each sample carrying its own
// scale and offsets and a populated NEGATIVE ladder beside the positive
// one.
//
// It exists because the reset walk seeds every row at scale 0, offset 0,
// with an empty negative ladder — a shape under which the closed form's
// per-row rescaling is the identity, so a fold that got the rescaling
// wrong would still match the telescoping rendering there. Both
// renderings share [expHistogramBucketSliceBoundsExpr], but only the
// closed form adds the rescaled rows to each other OUTSIDE the
// per-target loop, and that addition is what this family pins.
func closedFormLadderRows() []closedFormRow {
	rows := make([]closedFormRow, closedFormSamples)
	for i := range rows {
		pos := make([]uint64, closedFormLadderWidth)
		neg := make([]uint64, closedFormLadderWidth)
		for j := range pos {
			pos[j] = uint64((i+1)*7 + j)
			neg[j] = uint64((i+1)*5 + 2*j)
		}
		rows[i] = closedFormRow{
			scale:     closedFormLadderScales[i],
			posOffset: closedFormLadderOffsets[i],
			pos:       pos,
			negOffset: closedFormLadderOffsets[i] - 1,
			neg:       neg,
		}
	}
	return rows
}

// closedFormSeedTuples renders one series' INSERT tuples.
func closedFormSeedTuples(name string, temporality int64, rows []closedFormRow) []string {
	start := closedFormBaseline.Add(-time.Duration(len(rows)-1) * closedFormStep)
	tuples := make([]string, 0, len(rows))
	cells := func(counts []uint64) (string, uint64) {
		out := make([]string, len(counts))
		var total uint64
		for j, c := range counts {
			out[j] = fmt.Sprintf("%d", c)
			total += c
		}
		return strings.Join(out, ","), total
	}
	for i, row := range rows {
		pos, posTotal := cells(row.pos)
		neg, negTotal := cells(row.neg)
		total := posTotal + negTotal
		tuples = append(tuples, fmt.Sprintf(
			"('%s', map('series', '%s'), toDateTime64('%s', 9), %d, %f, %d, 0, %d, [%s], %d, [%s], %d)",
			closedFormMetric, name,
			start.Add(time.Duration(i)*closedFormStep).Format("2006-01-02 15:04:05"),
			total, float64(total), row.scale, row.posOffset, pos, row.negOffset, neg, temporality,
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
	// posWidth is the merged positive range each family must fold onto:
	// the reset family's rows all sit at one scale and offset, so its
	// merged range is one row wide, while the rescaling family's rows are
	// deliberately staggered so theirs is wider than any single row.
	type closedFormSeries struct {
		name     string
		posWidth int
	}
	names := make([]closedFormSeries, 0, patterns*2)
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
			names = append(names, closedFormSeries{name, closedFormBucketWidth})
		}
		tuples = append(tuples, closedFormSeedTuples(
			fmt.Sprintf("t%d_ladder", temporality), temporality, closedFormLadderRows(),
		)...)
		names = append(names, closedFormSeries{
			fmt.Sprintf("t%d_ladder", temporality), closedFormLadderMergedWidth,
		})
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
	for _, series := range names {
		name := series.name
		got, ok := closed[name]
		if !ok {
			t.Fatalf("series %s missing from the closed-form result", name)
		}
		want, ok := telescoping[name]
		if !ok {
			t.Fatalf("series %s missing from the telescoping result", name)
		}
		if len(got.pos) != series.posWidth {
			t.Fatalf("series %s folded %d positive buckets, want %d — the seeded rows are "+
				"not landing on the merged range this test believes they are",
				name, len(got.pos), series.posWidth)
		}
		for _, side := range []struct {
			what      string
			got, want []float64
		}{
			{"positive", got.pos, want.pos},
			{"negative", got.neg, want.neg},
		} {
			if len(side.got) != len(side.want) {
				t.Fatalf("series %s %s ladder: %d buckets closed-form vs %d telescoping",
					name, side.what, len(side.got), len(side.want))
			}
			for j := range side.want {
				if side.got[j] != side.want[j] {
					t.Fatalf("series %s %s bucket %d: closed form = %v, telescoping = %v",
						name, side.what, j, side.got[j], side.want[j])
				}
			}
		}
	}
	// The rescaling family is the only one carrying a negative ladder;
	// asserting it is populated is what stops a seed change from quietly
	// reducing this test to the positive-only case.
	for _, temporality := range []int64{
		schema.AggregationTemporalityCumulative,
		schema.AggregationTemporalityDelta,
	} {
		if neg := closed[fmt.Sprintf("t%d_ladder", temporality)].neg; len(neg) == 0 {
			t.Fatalf("t%d_ladder folded no negative buckets", temporality)
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
func closedFormRun(t *testing.T, fixture *chdbFixture, opts promql.LowerOpts, wantClosedForm bool) map[string]closedFormLadders {
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
	const joinLadder = "arrayStringConcat(arrayMap(x -> toString(x), `%s`), ',')"
	rows, err := fixture.db.Query(
		"SELECT `Attributes`['series'], "+
			fmt.Sprintf(joinLadder, "HistogramPositiveBucketCounts")+", "+
			fmt.Sprintf(joinLadder, "HistogramNegativeBucketCounts")+
			" FROM ("+sqlStr+
			") WHERE `TimeUnix` = toDateTime64('"+closedFormBaseline.Format("2006-01-02 15:04:05")+"', 9)",
		args...,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	parse := func(name, joined string) []float64 {
		if joined == "" {
			return nil
		}
		parts := strings.Split(joined, ",")
		vals := make([]float64, len(parts))
		for i, p := range parts {
			if _, err := fmt.Sscanf(p, "%g", &vals[i]); err != nil {
				t.Fatalf("series %s: parse %q: %v", name, p, err)
			}
		}
		return vals
	}
	out := map[string]closedFormLadders{}
	for rows.Next() {
		var name, pos, neg string
		if err := rows.Scan(&name, &pos, &neg); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[name] = closedFormLadders{pos: parse(name, pos), neg: parse(name, neg)}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// closedFormLadders is one series' window-folded bucket ladders at one
// anchor: both sides, because a rendering that got the negative side
// wrong while getting the positive side right would otherwise go
// unnoticed.
type closedFormLadders struct {
	pos, neg []float64
}
