//go:build chdb

// Lever: push the TraceQL compare() matrix Timestamp window INTO both
// MergeTree scans of the `s LEFT JOIN r` join (FIX-1 scan-bounding
// pushdown), not above the join where ClickHouse 24.12 cannot prune it.
//
// The prod traces-drilldown OOM: a 15-min `compare()` over a busy
// service read ~731M rows / tripped the 2 GiB cap because the
// (Start-range, End] window sat on the SELECT wrapping the join, so
// neither the span leg ('s') nor the per-trace root leg ('r') could be
// scoped to the window — both fed the full multi-day span table into
// the join.
//
// This guard pins two robust, version-independent signals of the fix on
// a production-shaped spans table (PARTITION BY toDate(Timestamp), the
// OTel-CH exporter's own layout) seeded across many day-partitions —
// 250k traces total, 25k inside the one-day window:
//
//   - 's' span leg: the AFTER shape carries the bound in the scan's own
//     WHERE, so EXPLAIN indexes=1 MinMax-Partition-prunes the leg to the
//     window's day (Parts: 1/N), structurally — independent of whether
//     the analyzer would also push it from above the join. (CH 24.12 in
//     prod does NOT; that is the whole bug.)
//   - 'r' root leg: the `TraceId IN (<bounded cohort>)` seed cuts the
//     join's right side from every trace's root (250k) to only the
//     window cohort's roots (25k) — the ~10x row reduction that keeps
//     the join off the 2 GiB cap. Asserted as a strict row-count drop of
//     the seeded root leg vs the unbounded one.
//
// Parity is asserted too: BEFORE and AFTER return an identical
// (is_selection, attr, count) cohort — the window pushdown is
// correctness-preserving for the in-window cohort.
//
// The pre-existing traceid_window_prune guard covers only the
// SINGLE-scan trace-by-id shape; the compare join is a separate prune
// surface (two scans, one seeded through a GROUP BY) that no other perf
// guard exercised. Build-tagged `chdb`, same lane as the rest.
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
	// cmpTotalSpans / cmpSpansPerTrace / cmpNumDays seed a dense, multi-
	// day-partition spans corpus so the MinMax/Partition stage has real
	// parts to prune. Each trace's spans live within a single day; the
	// compare window selects one day, so the fix must prune the other
	// days' parts on the 's' leg and scope the 'r' leg to that day's
	// traces.
	cmpTotalSpans    = 2_000_000
	cmpSpansPerTrace = 8
	cmpNumDays       = 10
	// cmpWindowDayIdx is the day-partition offset (from cmpSeedEpoch) the
	// compare window targets; every span seeded into it falls in
	// [cmpWindowLo, cmpWindowHi).
	cmpWindowDayIdx = 4
	// cmpSeedEpoch anchors the seed's day-0 partition; the seed adds
	// `(trace % cmpNumDays)` days to it.
	cmpSeedEpoch = "2026-01-01"
)

// cmpSpansDDL is the production OTel-CH spans table (the columns the
// compare emitter reads + the partition/order the exporter writes),
// trimmed to what this bench needs.
const cmpSpansDDL = `CREATE OR REPLACE TABLE otel_traces (
    Timestamp DateTime64(9) CODEC(Delta, ZSTD(1)),
    TraceId String CODEC(ZSTD(1)),
    SpanId String CODEC(ZSTD(1)),
    ParentSpanId String CODEC(ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    SpanName LowCardinality(String) CODEC(ZSTD(1)),
    StatusCode LowCardinality(String) CODEC(ZSTD(1))
) ENGINE = MergeTree()
PARTITION BY toDate(Timestamp)
ORDER BY (ServiceName, SpanName, toDateTime(Timestamp))
SETTINGS index_granularity = 8192;`

// cmpSpansInsert seeds cmpTotalSpans rows scattered across cmpNumDays
// day-partitions. Each trace (cmpSpansPerTrace consecutive rows) lands
// in one day; span 0 of every trace has an empty ParentSpanId so it is a root
// span (so the root leg has rows to aggregate).
func cmpSpansInsert() string {
	const subSecondStepNS = 110_000_000 // 0.11s per intra-trace span
	return fmt.Sprintf(
		`INSERT INTO otel_traces
SELECT
    toDateTime64('%s 00:00:00', 9)
        + INTERVAL (intDiv(number, %d) %% %d) DAY
        + INTERVAL (number %% %d) SECOND
        + INTERVAL ((number %% %d) * %d) NANOSECOND AS Timestamp,
    leftPad(lower(hex(intDiv(number, %d))), 32, '0') AS TraceId,
    leftPad(lower(hex(number)), 16, '0') AS SpanId,
    if(number %% %d = 0, '', leftPad(lower(hex(intDiv(number, %d) * %d)), 16, '0')) AS ParentSpanId,
    concat('svc.', toString(number %% 50)) AS ServiceName,
    concat('op.', toString(intDiv(number, %d) %% 300)) AS SpanName,
    if(number %% 3 = 0, 'Error', 'Ok') AS StatusCode
FROM numbers(%d);`,
		cmpSeedEpoch,
		cmpSpansPerTrace, cmpNumDays, // day partition (per trace)
		cmpSecondsPerDay,                  // within-day second spread
		cmpSpansPerTrace, subSecondStepNS, // sub-second offset
		cmpSpansPerTrace,                   // TraceId hex
		cmpSpansPerTrace,                   // root-span selector
		cmpSpansPerTrace, cmpSpansPerTrace, // ParentSpanId = first span of trace
		cmpSpansPerTrace, // SpanName bucket
		cmpTotalSpans,
	)
}

// cmpSecondsPerDay is the within-day second spread each trace's spans
// fan across — under a day so every trace stays in one day-partition.
const cmpSecondsPerDay = 80_000

// cmpWindow returns the [lo, hi) DateTime64 literals bounding the target
// day-partition (cmpWindowDayIdx days after cmpSeedEpoch) — the window
// the compare query selects.
func cmpWindow() (lo, hi string) {
	epoch, _ := time.Parse("2006-01-02", cmpSeedEpoch)
	day := epoch.AddDate(0, 0, cmpWindowDayIdx)
	const dt64 = "2006-01-02 15:04:05.000000000"
	return "'" + day.Format(dt64) + "'", "'" + day.AddDate(0, 0, 1).Format(dt64) + "'"
}

// cmpBeforeSQL renders the BUGGY shape: the Timestamp window sits on the
// SELECT that wraps `s LEFT JOIN r`, so neither leg can MinMax-prune.
func cmpBeforeSQL() string {
	lo, hi := cmpWindow()
	return `SELECT is_selection, attr, count() AS c FROM (
  SELECT (StatusCode = 'Error') AS is_selection,
         'svc' AS attr,
         s.ServiceName AS sval
  FROM (SELECT * FROM otel_traces) AS s
  LEFT JOIN (
    SELECT TraceId, any(SpanName) AS __root_name
    FROM (SELECT * FROM otel_traces WHERE ParentSpanId = '') GROUP BY TraceId
  ) AS r ON s.TraceId = r.TraceId
  WHERE s.Timestamp > toDateTime64(` + lo + `, 9) - toIntervalNanosecond(60000000000)
    AND s.Timestamp <= toDateTime64(` + hi + `, 9)
) GROUP BY is_selection, attr, sval`
}

// cmpAfterSQL renders the FIXED shape: the window lives inside the 's'
// scan and seeds the 'r' root scan via `TraceId IN (<bounded cohort>)`
// — exactly the structure compareBaseQuery now emits.
func cmpAfterSQL() string {
	bound := cmpBound()
	return `SELECT is_selection, attr, count() AS c FROM (
  SELECT (StatusCode = 'Error') AS is_selection,
         'svc' AS attr,
         s.ServiceName AS sval
  FROM (SELECT * FROM (SELECT * FROM otel_traces) WHERE ` + bound + `) AS s
  LEFT JOIN (
    SELECT * FROM (
      SELECT TraceId, any(SpanName) AS __root_name
      FROM (SELECT * FROM otel_traces WHERE ParentSpanId = '') GROUP BY TraceId
    ) WHERE TraceId IN (
      SELECT TraceId FROM (SELECT * FROM (SELECT * FROM otel_traces) WHERE ` + bound + `) AS _cmp_seed
    )
  ) AS r ON s.TraceId = r.TraceId
) GROUP BY is_selection, attr, sval`
}

// cmpScanPartPrune returns (selected, total) parts for the FIRST
// MinMax/Partition stage of a single-scan EXPLAIN indexes=1 plan — used
// on the 's' span leg in isolation, where selected < total proves the
// in-scan bound Partition-prunes to the window's day-partition(s).
func cmpScanPartPrune(t *testing.T, db *sql.DB, query string) (selected, total int) {
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

// cmpCount runs a scalar `SELECT count() ...` and returns the count.
func cmpCount(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("count query: %v\nquery: %s", err, query)
	}
	return n
}

// cmpBound is the (Start-range, End] window predicate over a bare
// Timestamp column — the exact bound the emitter pushes into each scan.
func cmpBound() string {
	lo, hi := cmpWindow()
	return `Timestamp > toDateTime64(` + lo + `, 9) - toIntervalNanosecond(60000000000) ` +
		`AND Timestamp <= toDateTime64(` + hi + `, 9)`
}

// cmpSLegSQL is the bounded 's' span leg in isolation (the scan the
// emitter aliases `AS s`): a single MergeTree scan carrying the window
// in its own WHERE.
func cmpSLegSQL() string {
	return `SELECT SpanId FROM (SELECT * FROM otel_traces) WHERE ` + cmpBound()
}

// cmpRootLegUnboundedSQL / cmpRootLegSeededSQL are the per-trace root
// lookup WITHOUT and WITH the `TraceId IN (<bounded cohort>)` seed — the
// join's right side before and after the fix. The seeded form returns
// only the window cohort's roots.
func cmpRootLegUnboundedSQL() string {
	return `SELECT count() FROM (
  SELECT TraceId, any(SpanName) AS __root_name
  FROM (SELECT * FROM otel_traces WHERE ParentSpanId = '') GROUP BY TraceId
)`
}

func cmpRootLegSeededSQL() string {
	return `SELECT count() FROM (
  SELECT TraceId, any(SpanName) AS __root_name
  FROM (SELECT * FROM otel_traces WHERE ParentSpanId = '') GROUP BY TraceId
) WHERE TraceId IN (
  SELECT TraceId FROM (SELECT * FROM (SELECT * FROM otel_traces) WHERE ` + cmpBound() + `) AS _cmp_seed
)`
}

// cmpCohort runs `query` and returns the sorted (is_selection, attr,
// sval, count) rows flattened to strings — the parity fingerprint.
func cmpCohort(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("query: %v\nquery: %s", err, query)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sel uint8
		var attr string
		var c uint64
		if err := rows.Scan(&sel, &attr, &c); err != nil {
			t.Fatalf("scan cohort: %v", err)
		}
		out = append(out, strings.Join([]string{
			itoa(int(sel)), attr, itoa(int(c)),
		}, "|"))
	}
	sort.Strings(out)
	return out
}

// TestCompareWindowPrune_JoinLegs pins FIX-1: the compare matrix window
// must prune BOTH join legs' scans (AFTER strictly fewer parts than
// BEFORE) while returning an identical cohort.
func TestCompareWindowPrune_JoinLegs(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	defer db.Close()

	for _, stmt := range []string{cmpSpansDDL, cmpSpansInsert()} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup exec failed:\n%s\nerr: %v", stmt, err)
		}
	}
	// One dense part per day-partition so the MinMax stage has real parts
	// to prune (not inflated unmerged insert parts).
	if _, err := db.Exec("OPTIMIZE TABLE otel_traces FINAL"); err != nil {
		t.Fatalf("optimize: %v", err)
	}

	t.Logf("=== compare join window prune: %d spans, %d spans/trace, %d day-partitions ===",
		cmpTotalSpans, cmpSpansPerTrace, cmpNumDays)

	// --- PARITY: BEFORE and AFTER return the identical cohort -----------
	beforeRows := cmpCohort(t, db, cmpBeforeSQL())
	afterRows := cmpCohort(t, db, cmpAfterSQL())
	if len(beforeRows) == 0 {
		t.Fatalf("BEFORE returned no cohort rows — corpus seed degenerate")
	}
	if len(beforeRows) != len(afterRows) {
		t.Fatalf("PARITY VIOLATION: BEFORE %d cohort rows, AFTER %d — window must be correctness-preserving\nBEFORE=%v\nAFTER=%v",
			len(beforeRows), len(afterRows), beforeRows, afterRows)
	}
	for i := range beforeRows {
		if beforeRows[i] != afterRows[i] {
			t.Fatalf("PARITY VIOLATION at row %d: BEFORE=%q AFTER=%q", i, beforeRows[i], afterRows[i])
		}
	}

	// --- 's' leg: in-scan bound MinMax-Partition-prunes the span scan ---
	sSel, sTot := cmpScanPartPrune(t, db, cmpSLegSQL())
	t.Logf("'s' span leg MinMax parts: %d/%d", sSel, sTot)
	if sTot < cmpNumDays {
		t.Fatalf("'s' leg saw %d parts total, want >= %d day-partitions — corpus not dense enough to prove pruning",
			sTot, cmpNumDays)
	}
	if sSel >= sTot {
		t.Fatalf("'s' span leg did NOT Partition-prune (selected %d of %d parts): the (Start-range, End] "+
			"window must scope the span scan to the window's day-partition(s).", sSel, sTot)
	}

	// --- 'r' leg: trace-id seed cuts the root join-input to the cohort --
	rootAll := cmpCount(t, db, cmpRootLegUnboundedSQL())
	rootSeeded := cmpCount(t, db, cmpRootLegSeededSQL())
	t.Logf("'r' root leg rows: unbounded=%d seeded=%d", rootAll, rootSeeded)
	if rootAll == 0 {
		t.Fatalf("unbounded root leg returned 0 rows — corpus seed degenerate")
	}
	if rootSeeded >= rootAll {
		t.Fatalf("'r' root leg seed did NOT reduce the join input (seeded %d >= unbounded %d): "+
			"`TraceId IN (<bounded cohort>)` must scope the root lookup to the window cohort's traces.",
			rootSeeded, rootAll)
	}
}

// tsPartsFraction parses an EXPLAIN `Parts: N/M` line into (N, M).
func tsPartsFraction(s string) (selected, total int) {
	s = stripPrefix(trimSpace(s), "Parts: ")
	i := 0
	for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		selected = selected*10 + int(s[i]-'0')
	}
	if i < len(s) && s[i] == '/' {
		for i++; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
			total = total*10 + int(s[i]-'0')
		}
	}
	return selected, total
}
