//go:build chdb

package prom_test

// REPRO (Layer 12 — compute fan-out / scan-volume, chDB EXPLAIN indexes=1).
//
// The empirical proof that the windowless `__name__` enumeration is an
// O(all-partitions) full-column scan. This drives the SQL the HANDLER
// ACTUALLY emits for a no-start/end `/api/v1/label/__name__/values`
// request (captured through a recording stub), then runs it against a
// production-shaped gauge+sum table — PARTITION BY toDate(TimeUnix), the
// OTel-CH exporter's own layout — seeded across many day-partitions, and
// inspects the MergeTree MinMax/partition stage via EXPLAIN indexes=1.
//
//   - On current main the handler emits a WHERE-less DISTINCT, so the
//     first `Parts:` line reads N/N — every partition selected. The
//     assertion (the windowless emit must Partition-prune to a strict
//     subset) is RED.
//   - Once a default lookback bounds the windowless path, the captured SQL
//     carries the `toDateTime64('…', 9)` TimeUnix predicate and the same
//     EXPLAIN reads k/N with k < N — GREEN.
//
// This complements (does not duplicate) the existing hand-written
// test/perf/metric_names_window_prune_chdb_test.go: that file pins the
// *baseline* (a hand-written unbounded SQL scans all parts) and that a
// hand-written *windowed* SQL prunes. This file closes the gap between
// them — it asserts the property on the SQL cerberus's handler emits for a
// windowless request, so it auto-flips when the handler is fixed rather
// than tracking a hand-copied string.
//
// Honest scope note: partition pruning a windowless request narrows the
// scan ONLY because the default window is narrower than the seeded span.
// That narrowing is correctness-preserving ONLY if the default is the
// table's retention horizon (data older than retention is gone from a real
// Prometheus too) — a short recent default would silently drop
// recently-quiet metric names and DIVERGE from reference Prometheus. The
// result-identity guard that the windowless catalog stays complete lives
// in metadata_scan_bound_guard_chdb_test.go and W5_omitted in
// handler_chdb_metadata_window_sweep_test.go; both halves must hold.

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
	// per day-partition (real parts for the MinMax stage to prune).
	scanBoundRows  = 200_000
	scanBoundEpoch = "2026-01-01"
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

// scanBoundInsert scatters rows across scanBoundDays day-partitions.
func scanBoundInsert(table string) string {
	return fmt.Sprintf(`INSERT INTO %s
SELECT
    concat('m_', toString(number %% 50)) AS MetricName,
    map('job', 'j') AS Attributes,
    toDateTime64('%s 00:00:00', 9) + INTERVAL (number %% %d) DAY AS TimeUnix,
    toFloat64(number) AS Value
FROM numbers(%d);`, table, scanBoundEpoch, scanBoundDays, scanBoundRows)
}

// firstPartsFraction returns (selected, total) from the FIRST `Parts: N/M`
// line of an EXPLAIN indexes=1 plan — the MinMax/partition stage of the
// MergeTree scan.
func firstPartsFraction(t *testing.T, db *sql.DB, query string) (selected, total int) {
	t.Helper()
	rows, err := db.Query("EXPLAIN indexes=1 " + query)
	if err != nil {
		t.Fatalf("EXPLAIN: %v\nquery: %s", err, query)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan: %v", err)
		}
		trim := strings.TrimSpace(line)
		if !strings.HasPrefix(trim, "Parts:") {
			continue
		}
		frac := strings.TrimSpace(strings.TrimPrefix(trim, "Parts:"))
		if _, err := fmt.Sscanf(frac, "%d/%d", &selected, &total); err != nil {
			t.Fatalf("parse Parts line %q: %v", trim, err)
		}
		return selected, total
	}
	t.Fatalf("EXPLAIN produced no Parts line for:\n%s", query)
	return 0, 0
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

// TestMetadataScanBound_MetricNamesExplain_Repro pins the partition-prune
// property on the handler's actual windowless `__name__` emit. RED on main
// (Parts: N/N, the unbounded full-partition scan); green once a default
// lookback bounds the windowless path.
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
		for _, stmt := range []string{scanBoundPartitionedDDL(table), scanBoundInsert(table)} {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("setup %s: %v", stmt, err)
			}
		}
		// One dense part per day-partition so the MinMax stage prunes real
		// parts, not inflated unmerged insert parts.
		if _, err := db.Exec("OPTIMIZE TABLE " + table + " FINAL"); err != nil {
			t.Fatalf("optimize %s: %v", table, err)
		}
	}

	captured := captureWindowlessMetricNamesSQL(t)
	t.Logf("windowless __name__ emit:\n%s", captured)

	sel, tot := firstPartsFraction(t, db, captured)
	t.Logf("windowless __name__ MinMax parts: %d/%d", sel, tot)

	if tot < scanBoundDays {
		t.Fatalf("scan saw %d parts total, want >= %d day-partitions — corpus not dense enough to prove pruning",
			tot, scanBoundDays)
	}
	if sel >= tot {
		t.Errorf("windowless /api/v1/label/__name__/values selected %d of %d parts — the full-partition scan "+
			"that is the bug; a default lookback must bound the windowless emit so it Partition-prunes (k < N).",
			sel, tot)
	}
}
