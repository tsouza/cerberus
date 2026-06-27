//go:build chdb

package prom_test

// REPRO (Layer 12 — compute fan-out / scan-volume, chDB EXPLAIN indexes=1).
//
// The empirical proof that the windowless `__name__` enumeration reads the
// `proj_metric_name` aggregating projection instead of full-scanning the
// metrics fact table. This drives the SQL the HANDLER ACTUALLY emits for a
// no-start/end `/api/v1/label/__name__/values` request (captured through a
// recording stub), then runs it against a production-shaped gauge+sum table —
// PARTITION BY toDate(TimeUnix), the OTel-CH exporter's own layout — that
// carries the aggregating projection the cerberus DDL apply path installs
// (internal/schema/ddl), and inspects the read source via EXPLAIN indexes=1.
//
//   - A bare `DISTINCT ... WHERE TimeUnix >= …` emit (the pre-projection
//     shape) keeps a raw column filter that no aggregating projection can
//     serve, so the read streams every granule of the fact table.
//   - The handler's windowless emit is `GROUP BY MetricName HAVING
//     max(TimeUnix) >= <lookback>` — an aggregate-only predicate that routes
//     to proj_metric_name, so EXPLAIN reads the projection with a strict
//     granule subset. The assertion (the read must route to the projection
//     and prune) is the flip this test pins.
//
// This complements the hand-written
// test/perf/metric_names_window_prune_chdb_test.go: that file recorded that
// no exact column-DISTINCT shape can be marks-bounded; the projection is the
// exact, marks-bounded shape, and this file asserts the property on the SQL
// cerberus's handler actually emits, so it tracks the handler rather than a
// hand-copied string.
//
// Honest scope note: the projection answers "names with a sample in
// [lookback, now]" exactly because samples are never future-dated
// (max(TimeUnix) >= lookback ⇔ a sample in the window), so the windowless
// catalog stays COMPLETE over the retention horizon — the result-identity
// guard for that lives in metadata_scan_bound_guard_chdb_test.go and
// W5_omitted in handler_chdb_metadata_window_sweep_test.go; both halves hold.

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"

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
)

// scanBoundPartitionedDDL is the production OTel-CH metric table trimmed to
// the columns the metric-name discovery scan reads, with the exporter's
// PARTITION BY toDate(TimeUnix) so the MinMax stage has day-partitions to
// prune.
func scanBoundPartitionedDDL(table string) string {
	return fmt.Sprintf(`CREATE OR REPLACE TABLE %s (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree()
PARTITION BY toDate(TimeUnix)
ORDER BY (MetricName, TimeUnix);`, table)
}

// scanBoundInsert scatters rows across scanBoundDays day-partitions. The
// rows are now-relative (newest sample seconds ago, oldest scanBoundDays ago)
// so they land inside the windowless emit's default retention lookback and
// the HAVING max(TimeUnix) >= start predicate selects them.
func scanBoundInsert(table string) string {
	return fmt.Sprintf(`INSERT INTO %s
SELECT
    concat('m_', toString(number %% 50)) AS MetricName,
    map('job', 'j') AS Attributes,
    now64(9) - INTERVAL (number %% %d) DAY AS TimeUnix,
    toFloat64(number) AS Value
FROM numbers(%d);`, table, scanBoundDays, scanBoundRows)
}

// scanBoundAddProjection mirrors the cerberus DDL apply path
// (internal/schema/ddl): the aggregating projection over MetricName carrying
// max(TimeUnix) that the windowless metric-name emit routes to.
func scanBoundAddProjection(table string) string {
	return fmt.Sprintf(
		`ALTER TABLE %s ADD PROJECTION proj_metric_name `+
			`(SELECT MetricName, max(TimeUnix) GROUP BY MetricName);`, table,
	)
}

// projectionGranules returns (selected, total) granules from the
// EXPLAIN indexes=1 read of the `proj_metric_name` aggregating projection —
// proof the windowless emit routes to the projection rather than scanning the
// fact table. It returns ok=false if no projection read appears in the plan.
func projectionGranules(t *testing.T, db *sql.DB, query string) (selected, total int, ok bool) {
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
			inProjection = strings.Contains(trim, "proj_metric_name")
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

// captureWindowlessMetricNamesSQL returns the gauge/sum-arm SQL the handler
// emits for a windowless /api/v1/label/__name__/values request. The
// recording stub feeds canned names back so the handler completes; the
// returned statement is the bare-group UNION (the one that references the
// gauge table).
func captureWindowlessMetricNamesSQL(t *testing.T) string {
	t.Helper()
	q := &stubQuerier{strings: []string{"m_0"}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/label/__name__/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	for _, stmt := range q.allSQL {
		if strings.Contains(stmt, "otel_metrics_gauge") {
			return stmt
		}
	}
	t.Fatalf("handler issued no gauge-table metric-name query; allSQL=%q", q.allSQL)
	return ""
}

// TestMetadataScanBound_MetricNamesExplain_Repro pins the projection-routing
// property on the handler's actual windowless `__name__` emit. The windowless
// enumeration (`GROUP BY MetricName HAVING max(TimeUnix) >= <lookback>`) must
// read the `proj_metric_name` aggregating projection — a tiny one-row-per-name
// part — instead of full-scanning the metrics fact table. Without the
// projection (or with the pre-projection `DISTINCT ... WHERE TimeUnix` emit
// that a column filter keeps off any projection) this read streams every
// granule of the fact table; the assertion below catches that regression.
func TestMetadataScanBound_MetricNamesExplain_Repro(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}

	for _, table := range []string{"otel_metrics_gauge", "otel_metrics_sum"} {
		stmts := []string{
			scanBoundPartitionedDDL(table),
			scanBoundInsert(table),
			scanBoundAddProjection(table),
			"ALTER TABLE " + table + " MATERIALIZE PROJECTION proj_metric_name",
		}
		for _, stmt := range stmts {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("setup %s: %v", stmt, err)
			}
		}
		// One dense part per day-partition so the projection collapses real
		// parts, not inflated unmerged insert parts.
		if _, err := db.Exec("OPTIMIZE TABLE " + table + " FINAL"); err != nil {
			t.Fatalf("optimize %s: %v", table, err)
		}
	}

	captured := captureWindowlessMetricNamesSQL(t)
	t.Logf("windowless __name__ emit:\n%s", captured)

	sel, tot, ok := projectionGranules(t, db, captured)
	if !ok {
		t.Fatalf("windowless /api/v1/label/__name__/values did not route to proj_metric_name — "+
			"it full-scans the fact table. emit:\n%s", captured)
	}
	t.Logf("windowless __name__ projection granules: %d/%d", sel, tot)
	if tot < scanBoundDays {
		t.Fatalf("projection read saw %d granules total, want >= %d — corpus not dense enough to prove pruning",
			tot, scanBoundDays)
	}
	if sel >= tot {
		t.Errorf("windowless /api/v1/label/__name__/values read %d of %d projection granules — no pruning; "+
			"the aggregating projection must collapse the metric-name scan (selected < total).", sel, tot)
	}
}
