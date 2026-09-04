//go:build chdb

// chDB-backed differential proof for cerberus issue #3027's range/
// query_range extension of [expHistogramGroupMergeSumMap]
// (exp_histogram_merge_summap.go): the SAME range query and seed, lowered
// once through the default groupArray + picker fold
// (expHistogramGroupMergeFanout, via lowerExpHistogramSumOrAvgRange) and
// once through [promql.NativeExpHistogramMergeLowerer] (chopt
// exp_histogram_merge_summap), executed against real ClickHouse (chDB) and
// compared row set for row set. Mirrors
// exp_histogram_merge_summap_multigroup_chdb_test.go's structure, widened
// with a non-nil anchor: every fixture here produces MORE THAN ONE step
// anchor in the SAME query_range request, exercising the anchor-prefixed
// WindowExpr PARTITION BY [expHistogramMergeScaleWindowPartitionBy] builds
// when anchor != nil.
//
//   - TestExpHistogramMergeSumMapRangeDifferential_NoGrouping is `sum(...)`
//     with no by()/without() clause over TWO steps — every anchor is its
//     own output group (cerberus issue #3027's core claim: range mode is
//     ALWAYS multi-group, see [expHistogramGroupMergeSumMap]'s own doc),
//     each with its OWN cross-series scale negotiation via a series
//     update that lands strictly between the two anchors.
//   - TestExpHistogramMergeSumMapRangeDifferential_By is `sum by(route)`
//     over the SAME two-anchor seed — TWO groups PER anchor, proving the
//     anchor-prefixed partition key does not leak across either axis.
//   - TestExpHistogramMergeSumMapRangeDifferential_Without is the
//     `without(route)` twin (histogramAggGroupBy's OTHER branch).
//   - TestExpHistogramMergeSumMapRangeDifferential_Avg is the `by()`
//     case's avg() twin — cerberus issue #3027's own "confirm avg()
//     generalizes for free" item, verified explicitly rather than
//     assumed from the instant-mode result (cerberus issue #2866 did the
//     identical verification for instant mode).
package promql_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// expHistSumMapRangeMetric is the metric name every case below queries —
// distinct from every other fixture sharing this package's chDB session
// (fixture_chdb_test.go).
const expHistSumMapRangeMetric = "exp_histogram_merge_summap_range_diff_exp_hist"

// expHistSumMapRangeStep is the query_range step every case below uses.
const expHistSumMapRangeStep = 60 * time.Second

// expHistSumMapRangeStart / expHistSumMapRangeEnd are the two step
// anchors every case below evaluates.
var (
	expHistSumMapRangeStart = time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
	expHistSumMapRangeEnd   = expHistSumMapRangeStart.Add(expHistSumMapRangeStep)
)

// expHistSumMapRangeDiffRow is one merged-histogram output row, keyed by
// its own (timestamp, Attributes) pair — the differential comparison
// sorts rows by that key so the two strategies' potentially differently
// -ordered output still compares equal.
type expHistSumMapRangeDiffRow struct {
	ts                     string
	attrs                  string
	scale                  int64
	zeroCount              float64
	posOffset, negOffset   int64
	posBuckets, negBuckets string
	count, sum             float64
}

// runExpHistSumMapRangeDiffQuery lowers query as a query_range request
// (expHistSumMapRangeStart to expHistSumMapRangeEnd, step
// expHistSumMapRangeStep — two anchors) under the given strategy and
// returns every resulting merged-histogram row, sorted by (timestamp,
// Attributes) so the two strategies' potentially differently-ordered
// output still compares equal.
func runExpHistSumMapRangeDiffQuery(t *testing.T, fixture *chdbFixture, native bool, query string) []expHistSumMapRangeDiffRow {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s,
		expHistSumMapRangeStart, expHistSumMapRangeEnd, expHistSumMapRangeStep,
		promql.LowerOpts{Lowerers: expHistSumMapDiffLowerers(native)})
	if err != nil {
		t.Fatalf("LowerAtRangeOpts(native=%v, %q): %v", native, query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(native=%v, %q): %v", native, query, err)
	}
	rows := fixture.queryOverEmitted(t,
		"toString(TimeUnix), toString(Attributes), HistogramScale, toString(HistogramZeroCount), HistogramPositiveOffset, toString(HistogramPositiveBucketCounts), "+
			"HistogramNegativeOffset, toString(HistogramNegativeBucketCounts), toString(HistogramCount), toString(HistogramSum)",
		sqlStr, args)
	defer func() { _ = rows.Close() }()

	var out []expHistSumMapRangeDiffRow
	for rows.Next() {
		var row expHistSumMapRangeDiffRow
		var zeroCountStr, countStr, sumStr string
		if err := rows.Scan(&row.ts, &row.attrs, &row.scale, &zeroCountStr, &row.posOffset, &row.posBuckets,
			&row.negOffset, &row.negBuckets, &countStr, &sumStr); err != nil {
			t.Fatalf("scan (native=%v): %v", native, err)
		}
		row.zeroCount = mustParseFloat(t, zeroCountStr)
		row.count = mustParseFloat(t, countStr)
		row.sum = mustParseFloat(t, sumStr)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("query error (native=%v): %v", native, err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ts != out[j].ts {
			return out[i].ts < out[j].ts
		}
		return out[i].attrs < out[j].attrs
	})
	return out
}

func assertExpHistSumMapRangeDiffEqual(t *testing.T, fanout, native []expHistSumMapRangeDiffRow) {
	t.Helper()
	if len(fanout) != len(native) {
		t.Fatalf("fanout and native (sumMap) range merges disagree on ROW COUNT: fanout=%d native=%d\n  fanout = %+v\n  native = %+v",
			len(fanout), len(native), fanout, native)
	}
	for i := range fanout {
		if fanout[i] != native[i] {
			t.Fatalf("fanout and native (sumMap) range merges disagree at row %d:\n  fanout = %+v\n  native = %+v", i, fanout[i], native[i])
		}
	}
}

// newExpHistSumMapRangeDiffFixture seeds route `a` with two series: `s1`
// holds ONE sample (scale 0) valid at BOTH anchors, and `s2` holds TWO
// samples — an OLDER one (scale 2) valid only at the FIRST anchor and a
// NEWER one (scale 1) landing strictly between the two anchors, so it
// becomes the newest-in-window sample for the SECOND anchor only. Route
// `a`'s own cross-series scale negotiation therefore differs across the
// two anchors even though its mergedScale happens to coincide
// (min(0,2)=0, then min(0,1)=0) — the RECONSTRUCTED bucket ladder still
// differs, since s2's own contribution changes between the two anchors.
// Route `b` holds a single, stable series (`s3`, unchanged across both
// anchors) as a control: if the anchor-prefixed WindowExpr partition
// leaked across anchors OR across groups, route `b`'s output would drift
// even though nothing about it changes between the two anchors — and any
// such leak would show up as a divergence from the fold's own,
// independently-correct per-(anchor, group) computation.
func newExpHistSumMapRangeDiffFixture(t *testing.T) *chdbFixture {
	t.Helper()
	var b strings.Builder
	b.WriteString(expHistSumMapDiffSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram (MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n")
	rows := []string{
		fmt.Sprintf("('%s', map('route', 'a', 'series', 's1'), toDateTime64('2026-01-01 00:00:30', 9), 10, 1.0, 0, 0, 5, [1,2,3,4], 0, [])", expHistSumMapRangeMetric),
		fmt.Sprintf("('%s', map('route', 'a', 'series', 's2'), toDateTime64('2026-01-01 00:00:30', 9), 283, 1.0, 2, 0, 17, [10,20,30,40,50,60,70], 0, [])", expHistSumMapRangeMetric),
		fmt.Sprintf("('%s', map('route', 'a', 'series', 's2'), toDateTime64('2026-01-01 00:01:30', 9), 6, 4.0, 1, 0, -2, [1,2,3], 0, [])", expHistSumMapRangeMetric),
		fmt.Sprintf("('%s', map('route', 'b', 'series', 's3'), toDateTime64('2026-01-01 00:00:30', 9), 6, 3.0, 1, 0, -2, [1,2,3], 0, [])", expHistSumMapRangeMetric),
	}
	b.WriteString("    " + strings.Join(rows, ",\n    ") + ";\n")
	return newChDBFixture(t, b.String())
}

// TestExpHistogramMergeSumMapRangeDifferential_NoGrouping is `sum(...)`
// (no by()/without()) over [newExpHistSumMapRangeDiffFixture] — every
// step anchor is its own output group, cerberus issue #3027's core claim
// that range mode is ALWAYS multi-group.
func TestExpHistogramMergeSumMapRangeDifferential_NoGrouping(t *testing.T) {
	fixture := newExpHistSumMapRangeDiffFixture(t)
	query := fmt.Sprintf("sum(%s)", expHistSumMapRangeMetric)
	fanout := runExpHistSumMapRangeDiffQuery(t, fixture, false, query)
	native := runExpHistSumMapRangeDiffQuery(t, fixture, true, query)
	if len(native) != 2 {
		t.Fatalf("expected 2 rows (one per step anchor), got %d: %+v", len(native), native)
	}
	assertExpHistSumMapRangeDiffEqual(t, fanout, native)
}

// TestExpHistogramMergeSumMapRangeDifferential_By is `sum by(route)` over
// the SAME two-anchor seed — TWO groups PER anchor (route a, route b),
// four rows total, proving the anchor-prefixed partition key does not
// leak across either axis.
func TestExpHistogramMergeSumMapRangeDifferential_By(t *testing.T) {
	fixture := newExpHistSumMapRangeDiffFixture(t)
	query := fmt.Sprintf("sum by(route) (%s)", expHistSumMapRangeMetric)
	fanout := runExpHistSumMapRangeDiffQuery(t, fixture, false, query)
	native := runExpHistSumMapRangeDiffQuery(t, fixture, true, query)
	if len(native) != 4 {
		t.Fatalf("expected 4 rows (2 anchors x 2 routes), got %d: %+v", len(native), native)
	}
	assertExpHistSumMapRangeDiffEqual(t, fanout, native)
}

// TestExpHistogramMergeSumMapRangeDifferential_Without is the SAME seed
// and shape under `sum without(route)` — histogramAggGroupBy's OTHER
// branch. `without(route)` groups by every OTHER label, which here is
// just `series` — three distinct values (s1, s2, s3), all present at
// both anchors — so this produces SIX rows (2 anchors x 3 series),
// unlike the `by(route)` case's four.
func TestExpHistogramMergeSumMapRangeDifferential_Without(t *testing.T) {
	fixture := newExpHistSumMapRangeDiffFixture(t)
	query := fmt.Sprintf("sum without(route) (%s)", expHistSumMapRangeMetric)
	fanout := runExpHistSumMapRangeDiffQuery(t, fixture, false, query)
	native := runExpHistSumMapRangeDiffQuery(t, fixture, true, query)
	if len(native) != 6 {
		t.Fatalf("expected 6 rows (2 anchors x 3 series), got %d: %+v", len(native), native)
	}
	assertExpHistSumMapRangeDiffEqual(t, fanout, native)
}

// TestExpHistogramMergeSumMapRangeDifferential_Avg is
// [TestExpHistogramMergeSumMapRangeDifferential_By]'s `avg()` twin —
// cerberus issue #3027's own "confirm avg() generalizes for free" item:
// route `a`'s two-series group divides by 2, route `b`'s one-series group
// divides by 1, independently at EACH anchor.
func TestExpHistogramMergeSumMapRangeDifferential_Avg(t *testing.T) {
	fixture := newExpHistSumMapRangeDiffFixture(t)
	query := fmt.Sprintf("avg by(route) (%s)", expHistSumMapRangeMetric)
	fanout := runExpHistSumMapRangeDiffQuery(t, fixture, false, query)
	native := runExpHistSumMapRangeDiffQuery(t, fixture, true, query)
	if len(native) != 4 {
		t.Fatalf("expected 4 rows (2 anchors x 2 routes), got %d: %+v", len(native), native)
	}
	assertExpHistSumMapRangeDiffEqual(t, fanout, native)
}
