//go:build chdb

// chDB-backed proof for cerberus issue #2571: a further vector set
// operator (`or`) composing over an operand that is itself a NESTED
// `and`/`unless` whose OWN forwarded (LHS) side is histogram-valued —
// not a literal `(a or b)` mixed-or shape, the case
// histogram_native_mixed_or_setop_nesting_chdb_test.go already pins for
// cerberus issue #2555 — computes the correct payload for both
// discriminator values, and that the `and` filtering genuinely still
// applies at EVERY nesting level, not merely that the plan lowers
// without error.
//
// [fsoQuery1] is this issue's own evidence query, verbatim:
// `(sum by (series) (fwd_hist_exp_hist) and fwd_and_filter_gauge) or
// fwd_or_plain_gauge` — a `sum by(series)(...)` wrapping the exp-
// histogram selector, mirroring the issue's own
// `sum(demo_latency_exp_hist)` (a `by` clause is added purely so the
// aggregation preserves the `series` label this file's rows are keyed
// on; the code path — histogram_native_sum.go's lowerExpHistogramSumOrAvg
// — is identical with or without one). [fsoQuery2] is a second-level
// variant proving the recognizer this issue adds
// ([isExpHistogramForwardedThroughSetOp], histogram_native_set_op.go)
// really does recurse rather than stopping after one `and` level: its
// own second `and fwd_and_filter2_gauge` genuinely drops a series
// ("f2") the first `and` alone would have kept, and the series that
// DOES survive both levels ("h2") stays histogram-valued through the
// outer `or`.
package promql_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

const (
	fsoHistMetric    = "fwd_hist_exp_hist"
	fsoAndFilter1    = "fwd_and_filter_gauge"
	fsoAndFilter2    = "fwd_and_filter2_gauge"
	fsoOrPlainMetric = "fwd_or_plain_gauge"
)

var fsoEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// fsoSeed backs both queries in this file.
//
//   - "h1"/"f1": exercise the single-`and` query ([fsoQuery1]). "h1" is
//     present in BOTH the `and`'s own filter AND the `or`'s other arm —
//     proving the outer `or` still dedupes against the `and`-forwarded
//     LHS's histogram row rather than duplicating it as a float one.
//     "f1" is present in the histogram table but absent from the
//     `and`'s filter, so the `and` drops it entirely; it is present on
//     the `or`'s other (plain) arm, so the anti-right union still
//     surfaces it there as a float row.
//   - "h2"/"f2": exercise the double-`and` query ([fsoQuery2]). "h2"
//     is present in the histogram table AND both filter tables, so it
//     survives both `and` levels and stays histogram-valued through the
//     `or`. "f2" is present in the histogram table and the FIRST
//     filter, but ABSENT from the second — the first `and` alone would
//     keep it, but the second drops it — falling through to the `or`'s
//     plain arm as a float row, proving the recognizer recurses past one
//     level rather than stopping at it.
//   - "p1"/"p2": present ONLY on the `or`'s plain arm, absent from the
//     histogram table and both filters — proving the outer `or`'s
//     anti-right union still surfaces a series the `and` chain never
//     had a signature for at all.
var fsoSeed = "" +
	"CREATE OR REPLACE TABLE otel_metrics_exponential_histogram (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), " +
	"`Count` UInt64, `Sum` Float64, `Scale` Int32, `ZeroCount` UInt64, " +
	"`PositiveOffset` Int32, `PositiveBucketCounts` Array(UInt64), " +
	"`NegativeOffset` Int32, `NegativeBucketCounts` Array(UInt64)" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"INSERT INTO otel_metrics_exponential_histogram " +
	"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
	"    ('" + fsoHistMetric + "', map('series', 'h1'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
	"    ('" + fsoHistMetric + "', map('series', 'f1'), toDateTime64('2026-01-01 00:00:00', 9), 3, 9.0, 0, 0, 0, [7], 0, []),\n" +
	"    ('" + fsoHistMetric + "', map('series', 'h2'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
	"    ('" + fsoHistMetric + "', map('series', 'f2'), toDateTime64('2026-01-01 00:00:00', 9), 3, 9.0, 0, 0, 0, [7], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + fsoAndFilter1 + "', map('series', 'h1'), toDateTime64('2026-01-01 00:00:00', 9), 1),\n" +
	"    ('" + fsoAndFilter1 + "', map('series', 'h2'), toDateTime64('2026-01-01 00:00:00', 9), 1),\n" +
	"    ('" + fsoAndFilter1 + "', map('series', 'f2'), toDateTime64('2026-01-01 00:00:00', 9), 1),\n" +
	"    ('" + fsoAndFilter2 + "', map('series', 'h2'), toDateTime64('2026-01-01 00:00:00', 9), 1),\n" +
	"    ('" + fsoOrPlainMetric + "', map('series', 'h1'), toDateTime64('2026-01-01 00:00:00', 9), " + fsoDedupFloatStr + "),\n" +
	"    ('" + fsoOrPlainMetric + "', map('series', 'f1'), toDateTime64('2026-01-01 00:00:00', 9), " + fsoF1FloatStr + "),\n" +
	"    ('" + fsoOrPlainMetric + "', map('series', 'p1'), toDateTime64('2026-01-01 00:00:00', 9), " + fsoP1FloatStr + "),\n" +
	"    ('" + fsoOrPlainMetric + "', map('series', 'f2'), toDateTime64('2026-01-01 00:00:00', 9), " + fsoF2FloatStr + "),\n" +
	"    ('" + fsoOrPlainMetric + "', map('series', 'p2'), toDateTime64('2026-01-01 00:00:00', 9), " + fsoP2FloatStr + ");\n"

// The `or` arm's own float values for each series, kept as named
// constants (both the numeric const, for assertions, and its string
// form, for the seed's own VALUES list) so the two never drift apart.
const (
	fsoDedupFloat = 55.0
	fsoF1Float    = 99.0
	fsoP1Float    = 77.0
	fsoF2Float    = 88.0
	fsoP2Float    = 66.0

	fsoDedupFloatStr = "55.0"
	fsoF1FloatStr    = "99.0"
	fsoP1FloatStr    = "77.0"
	fsoF2FloatStr    = "88.0"
	fsoP2FloatStr    = "66.0"
)

const (
	fsoQuery1 = "(sum by (series) (" + fsoHistMetric + ") and " + fsoAndFilter1 + ") or " + fsoOrPlainMetric
	fsoQuery2 = "((sum by (series) (" + fsoHistMetric + ") and " + fsoAndFilter1 + ") and " + fsoAndFilter2 + ") or " + fsoOrPlainMetric
)

// fsoRow mirrors monRow (histogram_native_mixed_or_setop_nesting_chdb_test.go).
type fsoRow struct {
	series   string
	disc     int
	val      float64
	cnt, sum float64
	bucket1  float64
}

// fsoRunQuery mirrors monRunQuery: every query in this file lowers to a
// Mixed-shaped plan (the outer `or`'s two arms disagree in row shape —
// histogram-forwarded-through-`and` on the left, plain float on the
// right), so the fourteen-column projection is unconditional here.
func fsoRunQuery(t *testing.T, fixture *chdbFixture, s schema.Metrics, p parser.Parser, query string) map[string]fsoRow {
	t.Helper()
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, fsoEvalTS, fsoEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	if shape := chplan.RowShapeOf(plan); shape != chplan.MixedRowShape {
		t.Fatalf("RowShapeOf(%q) = %v, want MixedRowShape", query, shape)
	}
	projection := "`Attributes`['series'] AS series, `_setop_is_histogram` AS disc, " +
		"`Value` AS val, `HistogramCount` AS cnt, `HistogramSum` AS sum, `HistogramPositiveBucketCounts`[1] AS bucket1"

	rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
	defer func() { _ = rows.Close() }()

	out := map[string]fsoRow{}
	for rows.Next() {
		var r fsoRow
		if err := rows.Scan(&r.series, &r.disc, &r.val, &r.cnt, &r.sum, &r.bucket1); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[r.series] = r
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

func assertFsoHistogramRow(t *testing.T, rows map[string]fsoRow, series string) {
	t.Helper()
	r, ok := rows[series]
	if !ok {
		t.Fatalf("%s: no row, want one (histogram arm)", series)
	}
	if r.disc != 1 {
		t.Errorf("%s: disc = %d, want 1", series, r.disc)
	}
	if r.cnt != 2 {
		t.Errorf("%s: HistogramCount = %v, want 2", series, r.cnt)
	}
	if r.sum != 4.0 {
		t.Errorf("%s: HistogramSum = %v, want 4.0", series, r.sum)
	}
	if r.bucket1 != 6 {
		t.Errorf("%s: HistogramPositiveBucketCounts[1] = %v, want 6", series, r.bucket1)
	}
}

func assertFsoFloatRow(t *testing.T, rows map[string]fsoRow, series string, want float64) {
	t.Helper()
	r, ok := rows[series]
	if !ok {
		t.Fatalf("%s: no row, want one (float arm)", series)
	}
	if r.disc != 0 {
		t.Errorf("%s: disc = %d, want 0", series, r.disc)
	}
	if math.Abs(r.val-want) > 1e-9 {
		t.Errorf("%s: Value = %v, want %v", series, r.val, want)
	}
}

// TestNestedAndForwardsHistogramIntoOr_ChDB proves cerberus issue #2571's
// own evidence query: `(sum by (series) (hist) and filter1) or plain`.
// The inner `and` forwards the histogram-valued `sum by(series)(...)`
// rows verbatim for every signature ALSO present on filter1 ("h1"),
// drops the ones that aren't ("f1"), and the outer `or` unions those
// with `plain`'s own rows — deduping "h1" against its own float row on
// `plain` rather than emitting it twice, and forwarding "f1"/"p1" as
// ordinary float rows since the `and` chain has no histogram-shaped
// answer for either.
func TestNestedAndForwardsHistogramIntoOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, fsoSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	rows := fsoRunQuery(t, fixture, s, p, fsoQuery1)
	assertFsoHistogramRow(t, rows, "h1")
	assertFsoFloatRow(t, rows, "f1", fsoF1Float)
	assertFsoFloatRow(t, rows, "p1", fsoP1Float)
}

// TestDeeplyNestedAndForwardsHistogramIntoOr_ChDB proves the recognizer
// this issue adds ([isExpHistogramForwardedThroughSetOp]) recurses past
// a single `and` level: `((sum by (series) (hist) and filter1) and
// filter2) or plain`. "h2" survives BOTH `and` levels and stays
// histogram-valued through the `or`; "f2" survives the FIRST `and`
// alone (it is present on filter1) but is dropped by the SECOND (absent
// from filter2) — proving the second level's filtering genuinely still
// applies rather than the recognizer merely treating the whole chain as
// unconditionally histogram-valued — and falls through to `plain`'s own
// float row instead. "p2" is present only on `plain`, proving the outer
// `or`'s anti-right union still reaches a series the `and` chain never
// had at all.
func TestDeeplyNestedAndForwardsHistogramIntoOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, fsoSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	rows := fsoRunQuery(t, fixture, s, p, fsoQuery2)
	assertFsoHistogramRow(t, rows, "h2")
	assertFsoFloatRow(t, rows, "f2", fsoF2Float)
	assertFsoFloatRow(t, rows, "p2", fsoP2Float)
	// "h1" is present on filter1 (the first `and`'s own filter) but NOT
	// on filter2 (the second `and`'s own filter) — so the double-`and`
	// query drops it at the SECOND level even though the first level
	// alone would have kept it (see TestNestedAndForwardsHistogramIntoOr_ChDB
	// above, where the single-`and` query keeps "h1" as a histogram
	// row). It falls through to `plain`'s own float row here instead —
	// the sharpest proof the recursion is genuine: an implementation
	// that only inspected one `and` level (or unconditionally trusted
	// the chain once ANY level matched) would wrongly keep "h1"
	// histogram-valued in THIS query too.
	assertFsoFloatRow(t, rows, "h1", fsoDedupFloat)
}
