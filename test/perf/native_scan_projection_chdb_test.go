//go:build chdb

// Perf A/B for the RangeWindowNative inner-scan projection narrowing
// (follow-up to the lever-1 Aggregate/RangeWindow pushdown).
//
// ProjectionPushdown's stage-aware arm now fires on a chplan.RangeWindowNative
// over Scan/Filter(Scan), narrowing the inner Scan from `SELECT *` to the
// exact column union the timeSeriesRateToGrid emit reads — the GroupBy
// series-identity columns plus the (TimestampColumn, ValueColumn) pair fed
// into the aggregate. On the OTel-CH metrics table that wide `SELECT *`
// drags every resource/scope attribute map + exemplar array off disk even
// though the native rate aggregate touches only four columns.
//
// This test seeds a resource-attribute-heavy otel_metrics_sum, lowers the
// SAME `sum by (...) (rate(...[5m]))` query with the experimental native
// flag ON, emits it WIDE (un-optimized) and NARROWED (after the default
// optimizer pipeline runs the new pushdown arm), reads the inner-scan
// projected column set out of each emitted SQL, and sums the compressed
// bytes those columns occupy on disk (system.parts_columns, after an
// OPTIMIZE FINAL flush). The narrowed projection must read strictly fewer
// bytes — the native path is where this matters most (high-cardinality
// resource attributes ride in the wide maps).
package perf

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// nativeScanProjSeed builds a wide otel_metrics_sum carrying the columns a
// real OTel-CH export drags: the per-sample (MetricName, Attributes,
// TimeUnix, Value) the rate aggregate needs PLUS the wide
// ResourceAttributes / ScopeAttributes maps and an exemplar-style array
// that the native rate path never reads. min_bytes_for_wide_part=0 forces a
// wide part so system.parts_columns reports real per-column bytes.
const nativeScanProjSeed = `
CREATE OR REPLACE TABLE otel_metrics_sum (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String),
    ScopeAttributes Map(String, String),
    ExemplarValues Array(Float64),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix)
SETTINGS min_bytes_for_wide_part = 0;
`

// nativeScanProjInsert populates the seed with a wide resource/scope map
// per row so the unread columns are genuinely heavy on disk.
const nativeScanProjInsert = `
INSERT INTO otel_metrics_sum
SELECT
    'cerberus_queries_total' AS MetricName,
    map('cerberus_ql', if(number % 2 = 0, 'promql', 'logql'), 'series', toString(number % 64)) AS Attributes,
    map('k8s.pod.name', concat('pod-', toString(number % 512)),
        'k8s.namespace.name', concat('ns-', toString(number % 32)),
        'k8s.node.name', concat('node-', toString(number % 16)),
        'service.name', concat('svc-', toString(number % 128)),
        'cloud.region', concat('region-', toString(number % 8)),
        'host.id', concat('host-', toString(number % 1024))) AS ResourceAttributes,
    map('otel.scope.name', 'cerberus', 'otel.scope.version', '1.0.0') AS ScopeAttributes,
    [toFloat64(number), toFloat64(number) * 2, toFloat64(number) * 3] AS ExemplarValues,
    toDateTime64('2026-01-01 00:00:00', 9) + toIntervalSecond(number % 300) AS TimeUnix,
    toFloat64(number) AS Value
FROM numbers(200000);
`

const nativeScanProjQuery = `sum by(cerberus_ql) (rate(cerberus_queries_total[5m]))`

func TestNativeScanProjection_ProjectedBytesShrink(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}
	for _, stmt := range []string{nativeScanProjSeed, nativeScanProjInsert, "OPTIMIZE TABLE otel_metrics_sum FINAL"} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	colBytes := projectedColumnBytes(t, db)

	wideCols := innerScanColumns(t, false)
	narrowCols := innerScanColumns(t, true)

	wideBytes := sumColumnBytes(t, colBytes, wideCols)
	narrowBytes := sumColumnBytes(t, colBytes, narrowCols)

	t.Logf("wide native inner-scan columns   = %v", wideCols)
	t.Logf("narrowed native inner-scan columns = %v", narrowCols)
	t.Logf("wide   projected bytes = %d", wideBytes)
	t.Logf("narrow projected bytes = %d", narrowBytes)

	if narrowBytes >= wideBytes {
		t.Fatalf("narrowing did not shrink projected bytes: wide=%d narrow=%d", wideBytes, narrowBytes)
	}
	ratio := float64(wideBytes) / float64(narrowBytes)
	t.Logf("native scan-projection delta: %.2fx fewer projected bytes (%d -> %d)", ratio, wideBytes, narrowBytes)
}

// innerScanColumns lowers the native-rate query, optionally runs the default
// optimizer pipeline (which fires the new RangeWindowNative pushdown arm),
// emits the SQL, and extracts the column set the INNERMOST scan projects.
// The wide form emits `SELECT *`; the narrowed form emits an explicit column
// list — exactly the difference the bytes A/B measures.
func innerScanColumns(t *testing.T, optimize bool) []string {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(nativeScanProjQuery)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(5 * time.Minute)
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		rangeStart, rangeEnd, 30*time.Second,
		promql.LowerOpts{ExperimentalTSGridRange: true})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	if optimize {
		plan = optimizer.Default().Run(context.Background(), plan)
	}
	sqlStr, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	return innermostScanProjection(t, sqlStr)
}

// innermostScanProjection returns the projected column set of the innermost
// `SELECT ... FROM otel_metrics_sum` in sqlStr. A `SELECT *` reads every
// table column (the wide case); an explicit list reads exactly that set
// (the narrowed case). The table is a base relation, so the SELECT directly
// preceding the `FROM otel_metrics_sum` token is the scan projection.
func innermostScanProjection(t *testing.T, sqlStr string) []string {
	t.Helper()
	const tableTok = "FROM `otel_metrics_sum`"
	idx := strings.Index(sqlStr, tableTok)
	if idx < 0 {
		t.Fatalf("table token %q not found in emitted SQL:\n%s", tableTok, sqlStr)
	}
	head := sqlStr[:idx]
	selPos := strings.LastIndex(head, "SELECT ")
	if selPos < 0 {
		t.Fatalf("no SELECT preceding the scan in emitted SQL:\n%s", sqlStr)
	}
	projList := strings.TrimSpace(head[selPos+len("SELECT ") : idx])
	if projList == "*" {
		return allMetricsSumColumns()
	}
	cols := make([]string, 0)
	for _, raw := range strings.Split(projList, ",") {
		name := strings.Trim(strings.TrimSpace(raw), "`")
		if name != "" {
			cols = append(cols, name)
		}
	}
	sort.Strings(cols)
	return cols
}

// allMetricsSumColumns is the full column set a `SELECT *` on the seeded
// table reads — the wide-scan projection. Kept in lockstep with
// nativeScanProjSeed.
func allMetricsSumColumns() []string {
	cols := []string{
		"Attributes", "ExemplarValues", "MetricName",
		"ResourceAttributes", "ScopeAttributes", "TimeUnix", "Value",
	}
	sort.Strings(cols)
	return cols
}

// projectedColumnBytes reads the on-disk compressed byte size of every
// active column of the seeded table from system.parts_columns (populated
// after the OPTIMIZE FINAL flush).
func projectedColumnBytes(t *testing.T, db *sql.DB) map[string]uint64 {
	t.Helper()
	rows, err := db.Query(
		"SELECT column, sum(column_data_compressed_bytes) " +
			"FROM system.parts_columns WHERE table = 'otel_metrics_sum' AND active GROUP BY column",
	)
	if err != nil {
		t.Fatalf("parts_columns: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]uint64{}
	for rows.Next() {
		var col string
		var b uint64
		if err := rows.Scan(&col, &b); err != nil {
			t.Fatalf("scan parts_columns: %v", err)
		}
		out[col] = b
	}
	// The chdb-go parquet driver returns a spurious "empty row"
	// end-of-iteration error in place of io.EOF; any other error is real.
	if err := rows.Err(); err != nil && !strings.Contains(err.Error(), "empty row") {
		t.Fatalf("parts_columns rows.Err: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("system.parts_columns reported zero columns; the seed did not flush a wide part")
	}
	return out
}

// sumColumnBytes totals the compressed bytes of the named columns.
func sumColumnBytes(t *testing.T, colBytes map[string]uint64, cols []string) uint64 {
	t.Helper()
	var total uint64
	for _, c := range cols {
		b, ok := colBytes[c]
		if !ok {
			t.Fatalf("column %q has no parts_columns byte entry (have %s)", c, fmt.Sprint(keys(colBytes)))
		}
		total += b
	}
	return total
}

func keys(m map[string]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
