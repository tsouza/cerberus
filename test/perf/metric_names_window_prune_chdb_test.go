//go:build chdb

// Lever: the no-`match[]` metric-name discovery endpoint
// (`/api/v1/label/__name__/values`) must push the request [start,end]
// window INTO each per-table scan so ClickHouse MinMax/Partition-prunes
// by `toDate(TimeUnix)` — instead of streaming the full leading-key
// column.
//
// The prod incident: `SELECT DISTINCT MetricName FROM otel_metrics_gauge`
// (the unbounded arm cerberus emitted) read ~2.6B rows / 8-18s to return
// ~2027 distinct names, timing out Grafana's 30s datasource limit. The
// `optimize_distinct_in_order` path does NOT reduce read_rows — it removes
// the hash table, not the full-column read — so the leading-key DISTINCT
// is O(rows), not O(marks). There is no exact, marks-bounded shape for the
// unbounded case (CH has no skip-scan for an exact distinct, and a
// correlated-recursive loose-index-scan is unsupported), so the lever is
// the request window: a bounded picker request must prune to its
// day-partition(s).
//
// This guard pins two version-independent signals on a production-shaped
// gauge table (PARTITION BY toDate(TimeUnix), the OTel-CH exporter's own
// layout) seeded across many day-partitions, each day carrying its own
// disjoint set of metric names:
//
//   - UNBOUNDED `SELECT DISTINCT MetricName` selects EVERY part
//     (Parts: N/N) — the full-column scan that is the bug.
//   - WINDOWED `... WHERE TimeUnix >= lo AND TimeUnix <= hi`
//     MinMax-Partition-prunes to the window's day(s) (Parts: k/N, k < N)
//     — the fix.
//
// Exactness is asserted too: the windowed distinct set equals exactly the
// names whose samples fall in [lo,hi] (a strict subset of the full
// catalog), so the prune is correctness-preserving — a discovery catalog
// must stay exact. Build-tagged `chdb`, same lane as the rest.
package perf

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
)

const (
	// mnwTotalRows / mnwNumDays / mnwNamesPerDay seed a dense, multi-day
	// gauge corpus. Each row's MetricName is namespaced by its day, so the
	// full catalog is mnwNumDays*mnwNamesPerDay names and any single-day
	// window selects exactly mnwNamesPerDay of them — making the prune both
	// measurable (other days' parts drop) and exactness-checkable (the
	// other days' names must NOT appear).
	mnwTotalRows     = 2_000_000
	mnwNumDays       = 10
	mnwNamesPerDay   = 20
	mnwSecondsPerDay = 80_000
	// mnwWindowDayIdx is the day-partition offset (from mnwSeedEpoch) the
	// discovery window targets.
	mnwWindowDayIdx = 4
	mnwSeedEpoch    = "2026-01-01"
)

// mnwGaugeDDL is the production OTel-CH gauge table trimmed to the columns
// this bench reads, with the exporter's partition + order key (MetricName
// leads the sort, TimeUnix drives the partition).
const mnwGaugeDDL = `CREATE OR REPLACE TABLE otel_metrics_gauge (
    MetricName String CODEC(ZSTD(1)),
    Attributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    TimeUnix DateTime64(9) CODEC(Delta, ZSTD(1)),
    Value Float64 CODEC(ZSTD(1))
) ENGINE = MergeTree()
PARTITION BY toDate(TimeUnix)
ORDER BY (MetricName, Attributes, ServiceName, toUnixTimestamp64Nano(TimeUnix))
SETTINGS index_granularity = 8192;`

// mnwGaugeInsert scatters mnwTotalRows across mnwNumDays day-partitions
// (dayIdx = number % mnwNumDays); each row's MetricName is
// `m_<dayIdx>_<k>` so day W owns names `m_W_0 .. m_W_(mnwNamesPerDay-1)`
// and no name crosses a day boundary.
func mnwGaugeInsert() string {
	return fmt.Sprintf(
		`INSERT INTO otel_metrics_gauge
SELECT
    concat('m_', toString(number %% %d), '_', toString(intDiv(number, %d) %% %d)) AS MetricName,
    map('job', concat('j', toString(number %% 5))) AS Attributes,
    concat('svc.', toString(number %% 50)) AS ServiceName,
    toDateTime64('%s 00:00:00', 9)
        + INTERVAL (number %% %d) DAY
        + INTERVAL (number %% %d) SECOND AS TimeUnix,
    toFloat64(number %% 1000) AS Value
FROM numbers(%d);`,
		mnwNumDays, mnwNumDays, mnwNamesPerDay, // MetricName = m_<day>_<k>, k decorrelated from day via intDiv
		mnwSeedEpoch,
		mnwNumDays,       // day partition
		mnwSecondsPerDay, // within-day second spread
		mnwTotalRows,
	)
}

// mnwWindow returns the [lo, hi] DateTime64 literals bounding the target
// day-partition — the closed window the discovery request carries.
func mnwWindow() (lo, hi string) {
	epoch, _ := time.Parse("2006-01-02", mnwSeedEpoch)
	day := epoch.AddDate(0, 0, mnwWindowDayIdx)
	const dt64 = "2006-01-02 15:04:05.000000000"
	return "'" + day.Format(dt64) + "'", "'" + day.AddDate(0, 0, 1).Format(dt64) + "'"
}

// mnwUnboundedSQL is the arm cerberus emitted before the fix: an unbounded
// leading-key DISTINCT over the whole table.
func mnwUnboundedSQL() string {
	return `SELECT DISTINCT MetricName AS value FROM otel_metrics_gauge`
}

// mnwWindowedSQL is the arm cerberus emits after the fix: the same DISTINCT
// with the closed [start,end] TimeUnix bound pushed into the scan.
func mnwWindowedSQL() string {
	lo, hi := mnwWindow()
	return `SELECT DISTINCT MetricName AS value FROM otel_metrics_gauge ` +
		`WHERE TimeUnix >= toDateTime64(` + lo + `, 9) AND TimeUnix <= toDateTime64(` + hi + `, 9)`
}

// mnwFirstPartsFraction returns (selected, total) parts from the FIRST
// `Parts:` line of an `EXPLAIN indexes=1` plan — the MinMax/Partition
// stage of the MergeTree scan.
func mnwFirstPartsFraction(t *testing.T, db *sql.DB, query string) (selected, total int) {
	t.Helper()
	rows, err := db.Query("EXPLAIN indexes=1 " + query)
	if err != nil {
		t.Fatalf("EXPLAIN: %v\nquery: %s", err, query)
	}
	defer rows.Close()
	seen := false
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan: %v", err)
		}
		trim := trimSpace(line)
		if hasPrefix(trim, "Parts:") && !seen {
			selected, total = tsPartsFraction(trim)
			seen = true
		}
	}
	if !seen {
		t.Fatalf("EXPLAIN produced no Parts line for:\n%s", query)
	}
	return selected, total
}

// mnwDistinctNames runs `query` and returns the sorted distinct names.
func mnwDistinctNames(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("query: %v\nquery: %s", err, query)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan name: %v", err)
		}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// TestMetricNamesWindowPrune pins the discovery fix: the windowed
// `__name__` arm must MinMax-Partition-prune to its day (strictly fewer
// parts than the unbounded full scan) while returning EXACTLY the names in
// the window — a strict, exact subset of the full catalog.
func TestMetricNamesWindowPrune(t *testing.T) {
	db := openChDB(t)
	defer db.Close()

	for _, stmt := range []string{mnwGaugeDDL, mnwGaugeInsert()} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup exec failed:\n%s\nerr: %v", stmt, err)
		}
	}
	// One dense part per day-partition so the MinMax stage has real parts
	// to prune (not inflated unmerged insert parts).
	if _, err := db.Exec("OPTIMIZE TABLE otel_metrics_gauge FINAL"); err != nil {
		t.Fatalf("optimize: %v", err)
	}

	t.Logf("=== metric-name discovery window prune: %d rows, %d day-partitions, %d names/day ===",
		mnwTotalRows, mnwNumDays, mnwNamesPerDay)

	// --- UNBOUNDED: leading-key DISTINCT selects every part (the bug) ----
	uSel, uTot := mnwFirstPartsFraction(t, db, mnwUnboundedSQL())
	t.Logf("unbounded DISTINCT MinMax parts: %d/%d", uSel, uTot)
	if uTot < mnwNumDays {
		t.Fatalf("unbounded scan saw %d parts total, want >= %d day-partitions — corpus not dense enough to prove pruning",
			uTot, mnwNumDays)
	}
	if uSel != uTot {
		t.Fatalf("unbounded DISTINCT pruned parts (selected %d of %d) — expected the full-column scan that motivates the fix",
			uSel, uTot)
	}

	// --- WINDOWED: in-scan bound Partition-prunes to the window day ------
	wSel, wTot := mnwFirstPartsFraction(t, db, mnwWindowedSQL())
	t.Logf("windowed DISTINCT MinMax parts: %d/%d", wSel, wTot)
	if wSel >= wTot {
		t.Fatalf("windowed DISTINCT did NOT Partition-prune (selected %d of %d parts): the [start,end] "+
			"window must scope the metric-name scan to the window's day-partition(s).", wSel, wTot)
	}

	// --- EXACTNESS: windowed names == in-window names, ⊊ full catalog ----
	full := mnwDistinctNames(t, db, mnwUnboundedSQL())
	windowed := mnwDistinctNames(t, db, mnwWindowedSQL())
	oracle := mnwDistinctNames(t, db, mnwWindowedSQL()+" SETTINGS optimize_distinct_in_order=0")
	if len(full) != mnwNumDays*mnwNamesPerDay {
		t.Fatalf("full catalog = %d names, want %d — seed degenerate", len(full), mnwNumDays*mnwNamesPerDay)
	}
	if strings.Join(windowed, ",") != strings.Join(oracle, ",") {
		t.Fatalf("windowed distinct not setting-independent:\nwindowed=%v\noracle=%v", windowed, oracle)
	}
	if len(windowed) != mnwNamesPerDay {
		t.Fatalf("windowed catalog = %d names, want %d (one day's namespace)\n%v", len(windowed), mnwNamesPerDay, windowed)
	}
	if len(windowed) >= len(full) {
		t.Fatalf("windowed catalog (%d) must be a strict subset of full (%d)", len(windowed), len(full))
	}
	wantPrefix := fmt.Sprintf("m_%d_", mnwWindowDayIdx)
	for _, n := range windowed {
		if !hasPrefix(n, wantPrefix) {
			t.Fatalf("windowed name %q outside window day (want prefix %q) — prune dropped a real name or leaked another day's",
				n, wantPrefix)
		}
	}
}
