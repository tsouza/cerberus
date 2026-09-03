//go:build chdb

// chDB-backed differential proof for cerberus issue #2865's multi-group
// extension of [expHistogramGroupMergeSumMap] (exp_histogram_merge_summap.go):
// the SAME query and seed, lowered once through the default groupArray +
// picker fold (expHistogramGroupMergeFanout) and once through
// [promql.NativeExpHistogramMergeLowerer] (chopt exp_histogram_merge_summap),
// executed against real ClickHouse (chDB) and compared row set for row
// set. Mirrors exp_histogram_merge_summap_chdb_test.go's structure exactly,
// widened to `sum by(...)` / `sum without(...)` — every fixture there was
// single-group (or degenerated to it); every fixture here produces MORE
// THAN ONE output group in the SAME query, exercising the WindowExpr
// mergedScale pre-pass (expHistogramMergeScaleWindowProject) this issue
// added.
//
//   - TestExpHistogramMergeSumMapMultiGroupDifferential_By pins identical
//     output for `sum by(route)`, two groups, each with its OWN
//     cross-series scale negotiation (mirrors
//     TestExpHistogramMergeSumMapDifferential_ScaleNegotiation's seed,
//     split across groups) — the core proof that each group's mergedScale
//     comes from its OWN partition, not the whole input's.
//   - TestExpHistogramMergeSumMapMultiGroupDifferential_Without is the
//     SAME seed under `sum without(route)`, proving the partition-key
//     plumbing works identically for both histogramAggGroupBy branches.
//   - TestExpHistogramMergeSumMapMultiGroupDifferential_Avg is the `by`
//     case's avg() twin (cerberus issue #2866's division, now exercised
//     per group rather than once).
//   - TestExpHistogramMergeSumMapMultiGroupDifferential_SingletonGroup
//     seeds one group with TWO series and one group with exactly ONE — the
//     degenerate single-member partition, proving WindowExpr's `OVER
//     (PARTITION BY ...)` over a singleton partition reproduces that row's
//     own Scale untouched, same as the single-group ScalarSubquery path
//     does for a lone series.
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

// expHistSumMapMultiGroupMetric is the metric name every case below
// queries — distinct from every other fixture sharing this package's chDB
// session (fixture_chdb_test.go).
const expHistSumMapMultiGroupMetric = "exp_histogram_merge_summap_multigroup_diff_exp_hist"

var expHistSumMapMultiGroupEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// expHistSumMapMultiGroupDiffRow is one merged-histogram output row,
// keyed by its output group's own toString(Attributes) — the differential
// comparison sorts rows by that key so the two strategies' potentially
// differently-ORDERED output still compares equal.
type expHistSumMapMultiGroupDiffRow struct {
	attrs                  string
	scale                  int64
	zeroCount              float64
	posOffset, negOffset   int64
	posBuckets, negBuckets string
	count, sum             float64
}

// runExpHistSumMapMultiGroupDiffQuery lowers query under the given
// strategy and returns every resulting merged-histogram row, sorted by its
// own Attributes so two differently-ordered GROUP BY outputs still compare
// equal.
func runExpHistSumMapMultiGroupDiffQuery(t *testing.T, fixture *chdbFixture, native bool, query string) []expHistSumMapMultiGroupDiffRow {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s, expHistSumMapMultiGroupEvalTS, expHistSumMapMultiGroupEvalTS, 0,
		promql.LowerOpts{Lowerers: expHistSumMapDiffLowerers(native)})
	if err != nil {
		t.Fatalf("LowerAtRangeOpts(native=%v, %q): %v", native, query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(native=%v, %q): %v", native, query, err)
	}
	rows := fixture.queryOverEmitted(t,
		"toString(Attributes), HistogramScale, toString(HistogramZeroCount), HistogramPositiveOffset, toString(HistogramPositiveBucketCounts), "+
			"HistogramNegativeOffset, toString(HistogramNegativeBucketCounts), toString(HistogramCount), toString(HistogramSum)",
		sqlStr, args)
	defer func() { _ = rows.Close() }()

	var out []expHistSumMapMultiGroupDiffRow
	for rows.Next() {
		var row expHistSumMapMultiGroupDiffRow
		var zeroCountStr, countStr, sumStr string
		if err := rows.Scan(&row.attrs, &row.scale, &zeroCountStr, &row.posOffset, &row.posBuckets,
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
	sort.Slice(out, func(i, j int) bool { return out[i].attrs < out[j].attrs })
	return out
}

func assertExpHistSumMapMultiGroupDiffEqual(t *testing.T, fanout, native []expHistSumMapMultiGroupDiffRow) {
	t.Helper()
	if len(fanout) != len(native) {
		t.Fatalf("fanout and native (sumMap) merges disagree on GROUP COUNT: fanout=%d native=%d\n  fanout = %+v\n  native = %+v",
			len(fanout), len(native), fanout, native)
	}
	for i := range fanout {
		if fanout[i] != native[i] {
			t.Fatalf("fanout and native (sumMap) merges disagree at group %d:\n  fanout = %+v\n  native = %+v", i, fanout[i], native[i])
		}
	}
}

// newExpHistSumMapMultiGroupDiffFixture seeds TWO route groups: `a` gets
// two series at DIFFERENT Scales (0 and 2, mirroring
// newExpHistSumMapDiffFixtureScaleNegotiation's own downscale-collapse
// shape) so its mergedScale must come from ITS OWN partition's min(Scale),
// not the whole table's; `b` gets a single series at Scale 1 — if the
// WindowExpr partition leaked across groups, `b`'s mergedScale would
// wrongly become 0 (route `a`'s minimum) instead of staying 1.
func newExpHistSumMapMultiGroupDiffFixture(t *testing.T) *chdbFixture {
	t.Helper()
	var b strings.Builder
	b.WriteString(expHistSumMapDiffSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram (MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n")
	rows := []string{
		fmt.Sprintf("('%s', map('route', 'a', 'series', 's1'), toDateTime64('2026-01-01 00:00:00', 9), 10, 1.0, 0, 0, 5, [1,2,3,4], 0, [])", expHistSumMapMultiGroupMetric),
		fmt.Sprintf("('%s', map('route', 'a', 'series', 's2'), toDateTime64('2026-01-01 00:00:00', 9), 283, 1.0, 2, 0, 17, [10,20,30,40,50,60,70], 0, [])", expHistSumMapMultiGroupMetric),
		fmt.Sprintf("('%s', map('route', 'b', 'series', 's3'), toDateTime64('2026-01-01 00:00:00', 9), 6, 3.0, 1, 0, -2, [1,2,3], 0, [])", expHistSumMapMultiGroupMetric),
	}
	b.WriteString("    " + strings.Join(rows, ",\n    ") + ";\n")
	return newChDBFixture(t, b.String())
}

// TestExpHistogramMergeSumMapMultiGroupDifferential_By is the `sum
// by(route)` case over [newExpHistSumMapMultiGroupDiffFixture].
func TestExpHistogramMergeSumMapMultiGroupDifferential_By(t *testing.T) {
	fixture := newExpHistSumMapMultiGroupDiffFixture(t)
	query := fmt.Sprintf("sum by(route) (%s)", expHistSumMapMultiGroupMetric)
	fanout := runExpHistSumMapMultiGroupDiffQuery(t, fixture, false, query)
	native := runExpHistSumMapMultiGroupDiffQuery(t, fixture, true, query)
	if len(native) != 2 {
		t.Fatalf("expected 2 output groups (route=a, route=b), got %d: %+v", len(native), native)
	}
	assertExpHistSumMapMultiGroupDiffEqual(t, fanout, native)
}

// TestExpHistogramMergeSumMapMultiGroupDifferential_Without is the SAME
// seed and shape under `sum without(route)` — histogramAggGroupBy's OTHER
// branch (MapWithoutKeys instead of the by-list lookup), proving the
// WindowExpr partition-key plumbing is agnostic to which branch built it.
// `without(route)` groups by every OTHER label, which here is just
// `series` — so this produces THREE groups (one per distinct series),
// unlike the `by(route)` case's two.
func TestExpHistogramMergeSumMapMultiGroupDifferential_Without(t *testing.T) {
	fixture := newExpHistSumMapMultiGroupDiffFixture(t)
	query := fmt.Sprintf("sum without(route) (%s)", expHistSumMapMultiGroupMetric)
	fanout := runExpHistSumMapMultiGroupDiffQuery(t, fixture, false, query)
	native := runExpHistSumMapMultiGroupDiffQuery(t, fixture, true, query)
	if len(native) != 3 {
		t.Fatalf("expected 3 output groups (one per distinct series), got %d: %+v", len(native), native)
	}
	assertExpHistSumMapMultiGroupDiffEqual(t, fanout, native)
}

// TestExpHistogramMergeSumMapMultiGroupDifferential_Avg is
// [TestExpHistogramMergeSumMapMultiGroupDifferential_By]'s `avg()` twin
// (cerberus issue #2866): route `a`'s two-series group divides by 2, route
// `b`'s one-series group divides by 1 — both PER GROUP, not once for the
// whole query.
func TestExpHistogramMergeSumMapMultiGroupDifferential_Avg(t *testing.T) {
	fixture := newExpHistSumMapMultiGroupDiffFixture(t)
	query := fmt.Sprintf("avg by(route) (%s)", expHistSumMapMultiGroupMetric)
	fanout := runExpHistSumMapMultiGroupDiffQuery(t, fixture, false, query)
	native := runExpHistSumMapMultiGroupDiffQuery(t, fixture, true, query)
	if len(native) != 2 {
		t.Fatalf("expected 2 output groups (route=a, route=b), got %d: %+v", len(native), native)
	}
	assertExpHistSumMapMultiGroupDiffEqual(t, fanout, native)
}

// newExpHistSumMapMultiGroupSingletonFixture seeds one route (`solo`) with
// exactly ONE series alongside a second route (`multi`) with two — the
// singleton-partition degenerate case: `solo`'s WindowExpr partition has
// exactly one row, so its mergedScale must equal that row's own Scale
// untouched, mirroring what the single-group ScalarSubquery path already
// guarantees for a lone series.
func newExpHistSumMapMultiGroupSingletonFixture(t *testing.T) *chdbFixture {
	t.Helper()
	var b strings.Builder
	b.WriteString(expHistSumMapDiffSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram (MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n")
	rows := []string{
		fmt.Sprintf("('%s', map('route', 'solo', 'series', 's1'), toDateTime64('2026-01-01 00:00:00', 9), 6, 3.0, 3, 0, -2, [1,2,3], 0, [])", expHistSumMapMultiGroupMetric),
		fmt.Sprintf("('%s', map('route', 'multi', 'series', 's2'), toDateTime64('2026-01-01 00:00:00', 9), 6, 12.0, 0, 0, 0, [1,2,3], 0, [])", expHistSumMapMultiGroupMetric),
		fmt.Sprintf("('%s', map('route', 'multi', 'series', 's3'), toDateTime64('2026-01-01 00:00:00', 9), 9, 20.0, 0, 0, 0, [4,5,0], 0, [])", expHistSumMapMultiGroupMetric),
	}
	b.WriteString("    " + strings.Join(rows, ",\n    ") + ";\n")
	return newChDBFixture(t, b.String())
}

// TestExpHistogramMergeSumMapMultiGroupDifferential_SingletonGroup is the
// `sum by(route)` case over
// [newExpHistSumMapMultiGroupSingletonFixture].
func TestExpHistogramMergeSumMapMultiGroupDifferential_SingletonGroup(t *testing.T) {
	fixture := newExpHistSumMapMultiGroupSingletonFixture(t)
	query := fmt.Sprintf("sum by(route) (%s)", expHistSumMapMultiGroupMetric)
	fanout := runExpHistSumMapMultiGroupDiffQuery(t, fixture, false, query)
	native := runExpHistSumMapMultiGroupDiffQuery(t, fixture, true, query)
	if len(native) != 2 {
		t.Fatalf("expected 2 output groups (route=solo, route=multi), got %d: %+v", len(native), native)
	}
	assertExpHistSumMapMultiGroupDiffEqual(t, fanout, native)
}
