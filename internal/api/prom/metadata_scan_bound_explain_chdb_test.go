//go:build chdb

package prom_test

// REPRO (Layer 12 — compute fan-out / scan-volume, chDB EXPLAIN).
//
// The empirical guard that every windowless metadata-enumeration shape the
// catalog answers reads a curated aggregating projection instead of
// full-scanning the metrics fact table. It drives the SQL the HANDLER ACTUALLY
// emits (captured through a recording stub) for:
//
//   - /api/v1/label/__name__/values  → routes onto proj_series (max-of-maxes
//     re-aggregation of the finer (MetricName, Attributes) projection to the
//     coarser GROUP BY MetricName);
//   - /api/v1/label/<label>/values   → routes onto proj_series;
//   - /api/v1/labels (label names)   → routes onto proj_series;
//   - /api/v1/metadata               → routes onto proj_metric_metadata.
//
// Each captured emit runs against a production-shaped table — PARTITION BY
// toDate(TimeUnix), the OTel-CH exporter's own layout — carrying the curated
// projections the cerberus DDL apply path installs (internal/schema/ddl). Two
// independent properties are asserted per shape:
//
//   - EXPLAIN indexes=1: the read routes to the NAMED projection and prunes
//     granules (selected < total) — a bare `DISTINCT ... WHERE` emit keeps a
//     raw column filter no aggregating projection can serve, so it would stream
//     every granule;
//   - EXPLAIN ESTIMATE: the routed read_rows is far below the unprojected full
//     scan (optimize_use_projections=0 baseline) AND under an absolute ceiling.
//
// The read_rows half makes the guard non-vacuous against silent un-routing on a
// ClickHouse upgrade: if a future CH stopped serving the shape from the
// projection, the read would fall back to the full scan and read_rows would
// jump to the baseline — failing this test instead of silently regressing prod
// to a 4.2B-row / 139 GiB scan.

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
)

const (
	// scanBoundDays is the number of day-partitions the EXPLAIN corpus
	// scatters rows across, so an unbounded scan shows Parts: N/N with
	// N == scanBoundDays and a bounded scan can prune to a strict subset.
	scanBoundDays = 10
	// scanBoundRows seeds a dense corpus so OPTIMIZE FINAL yields one part
	// per day-partition (real parts for the projection to collapse).
	scanBoundRows = 200_000
	// maxRoutedReadRows is the absolute ceiling on the routed read's
	// EXPLAIN-ESTIMATE row count. The curated projections pre-aggregate the
	// corpus to at most a few hundred (MetricName, Attributes) rows, so a
	// routed read stays orders below this; a read at or above it means the
	// query fell back to scanning the scanBoundRows-row fact table.
	maxRoutedReadRows = 50_000
	// minReadRowsPruneFactor is the floor on baseline/routed read_rows
	// (projection off vs on). A routed read that the projection genuinely
	// serves prunes by ~1000x here; this floor is set well below that so the
	// guard is robust, but a factor of 1 (un-routed: routed == baseline)
	// fails it.
	minReadRowsPruneFactor = 4
)

// scanBoundPartitionedDDL is the production OTel-CH metric table trimmed to the
// columns the metadata-enumeration scans read, with the exporter's PARTITION BY
// toDate(TimeUnix) so the MinMax stage has day-partitions to prune. It carries
// the proj_series source columns (MetricName, Attributes, TimeUnix) plus the
// proj_metric_metadata source columns (MetricDescription, MetricUnit).
func scanBoundPartitionedDDL(table string) string {
	return fmt.Sprintf(`CREATE OR REPLACE TABLE %s (
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree()
PARTITION BY toDate(TimeUnix)
ORDER BY (MetricName, TimeUnix);`, table)
}

// scanBoundInsert scatters rows across scanBoundDays day-partitions with a
// realistic series fan-out (a job + an instance key in Attributes). The rows
// are now-relative (newest sample seconds ago, oldest scanBoundDays ago) so
// they land inside the windowless emit's default retention lookback and the
// HAVING max(TimeUnix) >= start predicate selects them.
func scanBoundInsert(table string) string {
	return fmt.Sprintf(`INSERT INTO %s
SELECT
    concat('m_', toString(number %% 50)) AS MetricName,
    concat('desc ', toString(number %% 50)) AS MetricDescription,
    'unit' AS MetricUnit,
    map('job', 'j', 'instance', toString(number %% 200)) AS Attributes,
    now64(9) - INTERVAL (number %% %d) DAY AS TimeUnix,
    toFloat64(number) AS Value
FROM numbers(%d);`, table, scanBoundDays, scanBoundRows)
}

// scanBoundProjections mirrors the curated registry in internal/schema/ddl:
// proj_series (the (MetricName, Attributes) aggregating projection every
// enumeration shape routes onto) + proj_metric_metadata (the per-name
// description/unit projection /api/v1/metadata routes onto).
func scanBoundProjections(table string) []string {
	return []string{
		"ALTER TABLE " + table + " ADD PROJECTION proj_series " +
			"(SELECT MetricName, Attributes, max(TimeUnix) GROUP BY MetricName, Attributes)",
		"ALTER TABLE " + table + " MATERIALIZE PROJECTION proj_series",
		"ALTER TABLE " + table + " ADD PROJECTION proj_metric_metadata " +
			"(SELECT MetricName, any(MetricDescription), any(MetricUnit), max(TimeUnix) GROUP BY MetricName)",
		"ALTER TABLE " + table + " MATERIALIZE PROJECTION proj_metric_metadata",
	}
}

// projectionGranules returns (selected, total) granules from the EXPLAIN
// indexes=1 read of the named aggregating projection — proof the emit routes to
// that projection rather than scanning the fact table. It returns ok=false if
// no read of that projection appears in the plan.
func projectionGranules(t *testing.T, db *sql.DB, query, projName string) (selected, total int, ok bool) {
	t.Helper()
	rows, err := db.Query("EXPLAIN indexes=1 " + query)
	if err != nil {
		t.Fatalf("EXPLAIN: %v\nquery: %s", err, query)
	}
	defer rows.Close()
	inProjection := false
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan: %v", err)
		}
		trim := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trim, "ReadFromMergeTree"):
			inProjection = strings.Contains(trim, projName)
		case inProjection && strings.HasPrefix(trim, "Granules:"):
			frac := strings.TrimSpace(strings.TrimPrefix(trim, "Granules:"))
			if _, err := fmt.Sscanf(frac, "%d/%d", &selected, &total); err != nil {
				t.Fatalf("parse Granules line %q: %v", trim, err)
			}
			return selected, total, true
		}
	}
	return 0, 0, false
}

// explainEstimateRows returns the total estimated rows EXPLAIN ESTIMATE reports
// for query (summed across the per-table rows). With a projection serving the
// read this reflects the projection's tiny pre-aggregated row count; with
// `optimize_use_projections=0` appended it reflects the full fact-table scan.
func explainEstimateRows(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()
	rows, err := db.Query("EXPLAIN ESTIMATE " + query)
	if err != nil {
		t.Fatalf("EXPLAIN ESTIMATE: %v\nquery: %s", err, query)
	}
	defer rows.Close()
	var total int64
	for rows.Next() {
		var database, table string
		var parts, r, marks int64
		if err := rows.Scan(&database, &table, &parts, &r, &marks); err != nil {
			t.Fatalf("scan estimate: %v", err)
		}
		total += r
	}
	return total
}

// assertRoutesAndPrunes pins both halves of the projection-routing guard for
// one captured emit: it routes to projName and prunes granules, and its
// read_rows is far below the unprojected baseline and under the absolute
// ceiling.
func assertRoutesAndPrunes(t *testing.T, db *sql.DB, label, query, projName string) {
	t.Helper()
	t.Logf("%s emit:\n%s", label, query)
	sel, tot, ok := projectionGranules(t, db, query, projName)
	if !ok {
		t.Fatalf("%s did not route to %s — it full-scans the fact table. emit:\n%s", label, projName, query)
	}
	t.Logf("%s %s granules: %d/%d", label, projName, sel, tot)
	if tot < scanBoundDays {
		t.Fatalf("%s: %s read saw %d granules total, want >= %d — corpus not dense enough to prove pruning",
			label, projName, tot, scanBoundDays)
	}
	if sel >= tot {
		t.Errorf("%s: read %d of %d %s granules — no pruning (selected must be < total)", label, sel, tot, projName)
	}

	routed := explainEstimateRows(t, db, query)
	baseline := explainEstimateRows(t, db, query+" SETTINGS optimize_use_projections=0")
	if routed <= 0 {
		t.Fatalf("%s: routed read_rows=%d — EXPLAIN ESTIMATE returned nothing", label, routed)
	}
	t.Logf("%s read_rows: routed=%d baseline=%d (factor %d)", label, routed, baseline, baseline/routed)
	if routed > maxRoutedReadRows {
		t.Errorf("%s: routed read_rows=%d exceeds ceiling %d — the read fell back to scanning the fact table",
			label, routed, maxRoutedReadRows)
	}
	if baseline/routed < minReadRowsPruneFactor {
		t.Errorf("%s: read_rows prune factor %d (baseline=%d routed=%d) below floor %d — projection not pruning",
			label, baseline/routed, baseline, routed, minReadRowsPruneFactor)
	}
}

// newCaptureServer builds a stub-backed prom handler with the resource arm
// disabled (ResourceAttributesColumn cleared), so a /label/<name>/values
// request emits only the Attributes-map arm — the path proj_series serves. The
// resource arm reads ResourceAttributes (absent from proj_series) and is a
// separate, documented non-routing path, out of scope for this guard.
func newCaptureServer(t *testing.T, q prom.Querier) *httptest.Server {
	t.Helper()
	sch := schema.DefaultOTelMetrics()
	sch.ResourceAttributesColumn = ""
	h := prom.New(q, sch, nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// inlineArgs replaces each `?` placeholder in sql with its bound arg so the
// captured emit is EXPLAIN-ready. The metadata enumeration binds only string
// args (map keys + the empty-string sentinel), rendered as single-quoted CH
// literals.
func inlineArgs(t *testing.T, sql string, args []any) string {
	t.Helper()
	out := sql
	for _, a := range args {
		s, ok := a.(string)
		if !ok {
			t.Fatalf("non-string arg %v (%T) in captured SQL; the inliner handles only string metadata args", a, a)
		}
		lit := "'" + strings.ReplaceAll(s, "'", "''") + "'"
		out = strings.Replace(out, "?", lit, 1)
	}
	if strings.Contains(out, "?") {
		t.Fatalf("unbound placeholder left after inlining: %s", out)
	}
	return out
}

// captureGaugeArm issues the metadata request at path against a capture server,
// then returns the first recorded statement that references the gauge table and
// satisfies match, with its placeholders inlined. match disambiguates between
// arms (e.g. the grouped Attributes arm vs others) when a request fans out.
func captureGaugeArm(t *testing.T, path string, match func(sql string) bool) string {
	t.Helper()
	q := &stubQuerier{
		strings:  []string{"j"},
		metaRows: []chclient.MetricMetaRow{{Name: "m_0", Description: "desc 0", Unit: "unit"}},
	}
	srv := newCaptureServer(t, q)

	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	resp.Body.Close()

	for i, stmt := range q.allSQL {
		if strings.Contains(stmt, "otel_metrics_gauge") && match(stmt) {
			return inlineArgs(t, stmt, q.allArgs[i])
		}
	}
	t.Fatalf("handler issued no matching gauge-table query for %s; allSQL=%q", path, q.allSQL)
	return ""
}

// setupScanBoundTable creates + seeds the production-shaped gauge table and
// installs+materializes the curated projections, ready for EXPLAIN.
func setupScanBoundTable(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	stmts := []string{scanBoundPartitionedDDL(table), scanBoundInsert(table)}
	stmts = append(stmts, scanBoundProjections(table)...)
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup %s: %v", stmt, err)
		}
	}
	// One dense part per day-partition so the projection collapses real parts,
	// not inflated unmerged insert parts.
	if _, err := db.Exec("OPTIMIZE TABLE " + table + " FINAL"); err != nil {
		t.Fatalf("optimize %s: %v", table, err)
	}
}

func openScanBoundDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}
	return db
}

// TestMetadataScanBound_MetricNamesExplain_Repro pins that the windowless
// /api/v1/label/__name__/values emit routes onto proj_series (re-aggregated
// max-of-maxes to the coarser GROUP BY MetricName) and prunes.
func TestMetadataScanBound_MetricNamesExplain_Repro(t *testing.T) {
	db := openScanBoundDB(t)
	for _, table := range []string{"otel_metrics_gauge", "otel_metrics_sum"} {
		setupScanBoundTable(t, db, table)
	}
	// The bare-group __name__ arm references the gauge table and groups by
	// MetricName only (no Attributes) — distinguishes it from any other arm.
	emit := captureGaugeArm(t, "/api/v1/label/__name__/values", func(sql string) bool {
		return strings.Contains(sql, "GROUP BY `MetricName`") &&
			!strings.Contains(sql, "`MetricName`, `Attributes`")
	})
	assertRoutesAndPrunes(t, db, "windowless __name__", emit, "proj_series")
}

// TestMetadataScanBound_LabelValuesExplain_Repro pins that the generic
// windowless /api/v1/label/<label>/values emit (grouped DISTINCT Attributes[k]
// over GROUP BY MetricName, Attributes HAVING max(TimeUnix) >= lookback) routes
// onto proj_series and prunes.
func TestMetadataScanBound_LabelValuesExplain_Repro(t *testing.T) {
	db := openScanBoundDB(t)
	setupScanBoundTable(t, db, "otel_metrics_gauge")
	setupScanBoundTable(t, db, "otel_metrics_sum")
	setupScanBoundTable(t, db, "otel_metrics_histogram")
	emit := captureGaugeArm(t, "/api/v1/label/instance/values", func(sql string) bool {
		return strings.Contains(sql, "GROUP BY `MetricName`, `Attributes`")
	})
	assertRoutesAndPrunes(t, db, "windowless label_values(instance)", emit, "proj_series")
}

// TestMetadataScanBound_LabelNamesExplain_Repro pins that the windowless
// /api/v1/labels emit (arrayJoin(mapKeys(Attributes)) over GROUP BY MetricName,
// Attributes HAVING max(TimeUnix) >= lookback) routes onto proj_series.
func TestMetadataScanBound_LabelNamesExplain_Repro(t *testing.T) {
	db := openScanBoundDB(t)
	setupScanBoundTable(t, db, "otel_metrics_gauge")
	setupScanBoundTable(t, db, "otel_metrics_sum")
	setupScanBoundTable(t, db, "otel_metrics_histogram")
	emit := captureGaugeArm(t, "/api/v1/labels", func(sql string) bool {
		return strings.Contains(sql, "mapKeys(`Attributes`)") &&
			strings.Contains(sql, "GROUP BY `MetricName`, `Attributes`")
	})
	assertRoutesAndPrunes(t, db, "windowless label names", emit, "proj_series")
}

// TestMetadataScanBound_MetadataExplain_Repro pins that the windowless
// /api/v1/metadata gauge arm (SELECT MetricName, any(MetricDescription),
// any(MetricUnit) GROUP BY MetricName HAVING max(TimeUnix) >= lookback) routes
// onto proj_metric_metadata.
func TestMetadataScanBound_MetadataExplain_Repro(t *testing.T) {
	db := openScanBoundDB(t)
	setupScanBoundTable(t, db, "otel_metrics_gauge")
	// The gauge metadata arm is the one over the gauge table grouping by
	// MetricName and selecting the description/unit aggregates.
	emit := captureGaugeArm(t, "/api/v1/metadata", func(sql string) bool {
		return strings.Contains(sql, "any(`MetricDescription`)") &&
			strings.Contains(sql, "GROUP BY `MetricName`")
	})
	assertRoutesAndPrunes(t, db, "windowless metadata", emit, "proj_metric_metadata")
}
