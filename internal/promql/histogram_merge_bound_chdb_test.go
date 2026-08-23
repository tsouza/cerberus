//go:build chdb

// chDB-backed proof that the native-histogram merge budget guard (#2385,
// histogram_merge_bound.go) actually FIRES at real ClickHouse execution —
// not merely that the emitted SQL contains the right tokens — and that it
// stays silent on a legitimate, well-within-budget merge.
//
// `sum(<exp-histogram selector>)` is the simplest PromQL shape that reaches
// the shared across-series merge [wrapExpHistogramMergeBudgetGuard] wraps
// (histogram_native_sum.go's expHistogramSumMerge, via
// expHistogramMergeSortStage): every OTHER shape that reaches the same
// merge — histogram_quantile(phi, sum(...)), rate()/increase() wrapped in
// an outer sum(), and both instant/range grid modes — shares this exact
// guard function, so proving it activates here proves it for all of them.
package promql_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// histogramMergeBoundSeedDDL is the exp-histogram table shape every case
// below seeds. ResourceAttributes / ServiceName carry DEFAULTs (mirroring
// the OTel-CH default schema, see limitRatioSeedDDL's identical comment):
// the read path's canonicalAttributesExpr projects both unconditionally, so
// both columns must exist even though every seeded row here fills neither.
const histogramMergeBoundSeedDDL = "" +
	"CREATE OR REPLACE TABLE otel_metrics_exponential_histogram (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), " +
	"`Count` UInt64, `Sum` Float64, `Scale` Int32, `ZeroCount` UInt64, " +
	"`PositiveOffset` Int32, `PositiveBucketCounts` Array(UInt64), " +
	"`NegativeOffset` Int32, `NegativeBucketCounts` Array(UInt64)" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n"

// histogramMergeBoundMetric is the metric name every case queries.
const histogramMergeBoundMetric = "http_server_duration_exp_hist"

// histogramMergeBoundEvalTS is the instant eval time every case queries at;
// every seeded row sits one second earlier, well inside the bare-selector
// staleness lookback.
var histogramMergeBoundEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// histogramMergeBoundInsertColumns is the explicit column list every
// histogramMergeBoundRow tuple below fills, positionally — ResourceAttributes
// / ServiceName are left to their DEFAULTs.
const histogramMergeBoundInsertColumns = "(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts)"

// histogramMergeBoundRow renders one INSERT VALUES tuple, matching
// histogramMergeBoundInsertColumns: a single-bucket positive-only
// distribution at the given series label and PositiveOffset (Scale 0).
func histogramMergeBoundRow(series string, offset int) string {
	return histogramMergeBoundRowWide(series, offset, 1)
}

// histogramMergeBoundRowWide is [histogramMergeBoundRow] with a
// caller-chosen PositiveBucketCounts width instead of a fixed single
// bucket, so a test can isolate the rows axis of the
// `rows x width^3` cost model (histogram_merge_bound.go) by fixing width
// and scaling rows, rather than needing an impractically large row count
// at width 1.
func histogramMergeBoundRowWide(series string, offset, width int) string {
	buckets := make([]string, width)
	for i := range buckets {
		buckets[i] = "1"
	}
	return fmt.Sprintf(
		"('%s', map('series', '%s'), toDateTime64('2026-01-01 00:00:00', 9), 1, 1.0, 0, 0, %d, [%s], 0, [])",
		histogramMergeBoundMetric, series, offset, strings.Join(buckets, ","),
	)
}

// runHistogramMergeBoundQuery lowers + emits `sum(<metric>)` and runs it
// against fixture, returning the query error (nil on success).
//
// It wraps the emitted statement in `SELECT count() FROM (<emitted>)`
// rather than scanning the emitted row directly: the merged output carries
// Map/Array(UInt64) histogram columns (PositiveBucketCounts and friends),
// and chdb-go's parquet driver cannot decode those into a Go destination —
// the same limitation limit_ratio_chdb_test.go's header doc documents for
// Map cells. Wrapping in count() sidesteps decoding any of them while still
// forcing ClickHouse to fully evaluate the merge (a throwIf inside a column
// expression still fires even though the outer SELECT never reads that
// column's value — it is evaluated to produce the row count).
func runHistogramMergeBoundQuery(t *testing.T, fixture *chdbFixture) error {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	query := fmt.Sprintf("sum(%s)", histogramMergeBoundMetric)
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, histogramMergeBoundEvalTS, histogramMergeBoundEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	if err := testsql.CheckSeedCoversFanOut(fixture.seed, sqlStr); err != nil {
		t.Fatalf("seed does not cover the emitted fan-out: %v\nSQL: %s", err, sqlStr)
	}
	wrapped := "SELECT count() FROM (" + sqlStr + ")"
	rows, qerr := fixture.db.Query(wrapped, args...)
	if qerr != nil {
		return qerr
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		t.Fatal("count() query returned no rows")
	}
	var n int64
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("scan count(): %v", err)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n != 1 {
		t.Fatalf("query returned count()=%d, want exactly 1 (the merged sum() row)", n)
	}
	return nil
}

// TestHistogramMergeBudget_ChDB_BucketWidthExceeded seeds two series whose
// PositiveOffset diverges enough (0 vs 500, both Scale 0, one bucket each)
// that the merged bucket ladder spans 501 buckets — past the point where
// `rows(2) x width(501)^3` crosses maxHistogramMergeCostUnits
// (70,000,000; 2 x 501^3 = ~251M) — and asserts the query aborts with the
// budget guard's own throwIf, not a ClickHouse crash and not a silent
// wrong answer. This isolates the WIDTH factor of the cost model: 2 rows
// alone (histogramBinopOperandCount's own fixed row count) never trips it
// on its own.
func TestHistogramMergeBudget_ChDB_BucketWidthExceeded(t *testing.T) {
	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram " + histogramMergeBoundInsertColumns + " VALUES\n")
	b.WriteString("    " + histogramMergeBoundRow("near", 0) + ",\n")
	b.WriteString("    " + histogramMergeBoundRow("far", 500) + ";\n")
	fixture := newChDBFixture(t, b.String())

	err := runHistogramMergeBoundQuery(t, fixture)
	if err == nil {
		t.Fatal("expected the merge budget guard to abort the query (2 rows x width 501^3 exceeds the cost budget), got no error")
	}
	// This test drives chdb-go's raw driver handle directly (see
	// runHistogramMergeBoundQuery), never through chclient — the typed
	// *chclient.ThrowIfError chsql's production HTTP-facing classifiers use
	// only wraps errors that pass through chclient.Client's own driver-error
	// classification, so it doesn't apply here. A raw substring check
	// against chdb-go's own exception text is what this test actually needs:
	// proof the emitted SQL's throwIf fires at real ClickHouse execution,
	// carrying the right message.
	if !strings.Contains(err.Error(), chplan.HistogramMergeBudgetMessage) {
		t.Fatalf("query failed, but not with the merge budget guard's throwIf: %v", err)
	}
}

// TestHistogramMergeBudget_ChDB_RowCountExceeded seeds 100 series sharing
// an IDENTICAL 100-bucket layout (same PositiveOffset, same width, so the
// merged width stays 100 regardless of row count — isolating the ROWS
// factor of the cost model exactly the way issue #2490's own real-CH repro
// did) — `rows(100) x width(100)^3` = 100,000,000, past
// maxHistogramMergeCostUnits (70,000,000). Every series shares no `by()`
// label, so all 100 land in the query's one output group. This is the
// shape #2385's original independent series-count axis (capped at 4096)
// would have let straight through: 100 rows sits nowhere near that old
// cap, which is exactly why issue #2490 found real ClickHouse OOMs the old
// guard never caught.
func TestHistogramMergeBudget_ChDB_RowCountExceeded(t *testing.T) {
	const (
		rowsOverBudget = 100
		perRowWidth    = 100
		sharedOffset   = 0 // identical layout across every row
	)

	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram " + histogramMergeBoundInsertColumns + " VALUES\n")
	rows := make([]string, rowsOverBudget)
	for i := range rows {
		rows[i] = histogramMergeBoundRowWide("s"+strconv.Itoa(i), sharedOffset, perRowWidth)
	}
	b.WriteString("    " + strings.Join(rows, ",\n    ") + ";\n")
	fixture := newChDBFixture(t, b.String())

	err := runHistogramMergeBoundQuery(t, fixture)
	if err == nil {
		t.Fatal("expected the merge budget guard to abort the query (100 rows x width 100^3 exceeds the cost budget), got no error")
	}
	if !strings.Contains(err.Error(), chplan.HistogramMergeBudgetMessage) {
		t.Fatalf("query failed, but not with the merge budget guard's throwIf: %v", err)
	}
}

// TestHistogramMergeBudget_ChDB_RowCountOverflowGuard seeds 4,097 series —
// one past histogram_merge_bound.go's unexported
// maxHistogramMergeRowCountOverflowGuard (4096) — sharing an IDENTICAL
// single-bucket layout, so the merged width stays 1 and the real cost
// (`rows x width^3` = 4,097 x 1 = 4,097) sits nowhere near
// maxHistogramMergeCostUnits (70,000,000): the cost-formula disjunct alone
// would NOT reject this query. This isolates the OVERFLOW-SAFETY disjunct
// histogramMergeCostOverBudgetExpr ORs in ahead of the cost multiply —
// proving it independently forces rejection past the row count where the
// multiply risks wrapping Int64, rather than only ever mattering when the
// (impractical-to-seed-in-a-test) width axis is also near its clamp.
func TestHistogramMergeBudget_ChDB_RowCountOverflowGuard(t *testing.T) {
	const (
		rowsPastOverflowGuard = 4097
		perRowWidth           = 1
		sharedOffset          = 0 // identical layout across every row
	)

	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram " + histogramMergeBoundInsertColumns + " VALUES\n")
	rows := make([]string, rowsPastOverflowGuard)
	for i := range rows {
		rows[i] = histogramMergeBoundRowWide("s"+strconv.Itoa(i), sharedOffset, perRowWidth)
	}
	b.WriteString("    " + strings.Join(rows, ",\n    ") + ";\n")
	fixture := newChDBFixture(t, b.String())

	err := runHistogramMergeBoundQuery(t, fixture)
	if err == nil {
		t.Fatal("expected the merge budget guard to abort the query (4,097 rows exceeds the overflow-safety row cap, even though width 1 keeps the cost formula itself trivially under budget), got no error")
	}
	if !strings.Contains(err.Error(), chplan.HistogramMergeBudgetMessage) {
		t.Fatalf("query failed, but not with the merge budget guard's throwIf: %v", err)
	}
}

// TestHistogramMergeBudget_ChDB_WithinBudget seeds a small, legitimate merge
// (two series, adjacent offsets, well inside the cost budget) and asserts
// the query succeeds — the guard must not fire on ordinary data.
func TestHistogramMergeBudget_ChDB_WithinBudget(t *testing.T) {
	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram " + histogramMergeBoundInsertColumns + " VALUES\n")
	b.WriteString("    " + histogramMergeBoundRow("a", 0) + ",\n")
	b.WriteString("    " + histogramMergeBoundRow("b", 1) + ";\n")
	fixture := newChDBFixture(t, b.String())

	if err := runHistogramMergeBoundQuery(t, fixture); err != nil {
		t.Fatalf("a legitimate two-series merge must not trip the budget guard: %v", err)
	}
}
