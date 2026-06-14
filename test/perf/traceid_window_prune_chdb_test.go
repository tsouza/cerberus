//go:build chdb

// Lever: consume the otel_traces_trace_id_ts materialized view.
//
// The OTel-CH exporter populates a `<spans>_trace_id_ts` lookup table via
// a materialized view — one row per TraceId carrying (Start, End) =
// (min(Timestamp), max(Timestamp)) of the trace's spans — but cerberus has
// ZERO read consumers for it today. Trace-by-ID detail-open therefore
// renders `WHERE TraceId = ?` only, which cannot Partition/PrimaryKey-prune
// the spans table (ORDER BY (ServiceName, SpanName, toDateTime(Timestamp)),
// PARTITION BY toDate(Timestamp)): ClickHouse must read a granule from
// EVERY part to apply the `idx_trace_id` bloom filter.
//
// This bench seeds the production spans DDL across many day-partitions,
// populates the lookup table via the MV's own SELECT, and contrasts:
//
//	BEFORE : WHERE TraceId = ?                                  (today)
//	AFTER  : WHERE TraceId = ?
//	         AND Timestamp >= (SELECT min(Start) FROM <ts> WHERE TraceId=?)
//	         AND Timestamp <= addSeconds((SELECT max(End) FROM <ts> WHERE TraceId=?), 1)
//
// via EXPLAIN indexes=1 (parts/granules pruned). It also asserts BYTE
// PARITY: the AFTER window is a correctness-preserving SUPERSET (the exact
// TraceId Eq stays ANDed), so the two queries MUST return the identical
// span-id set. The +1s upper-bound pad compensates the MV's `End DateTime`
// (second precision) flooring the spans' `Timestamp DateTime64(9)`; without
// it the final fractional second of a trace is silently dropped (a result
// change = INCORRECT). Build-tagged `chdb`, same lane as the rest.
package perf

import (
	"database/sql"
	"fmt"
	"sort"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver" // registers "chdb" sql driver
)

// Grid: enough day-partitions and spans that the bloom-only BEFORE scan
// touches every part, while the AFTER window prunes to the target trace's
// single day-partition + sort-key range.
const (
	// A DENSE multi-day corpus: each day is its own MergeTree part with
	// many granules, so the bloom Skip index must be evaluated against
	// EVERY day-part's granule blooms unless the scan is partition-pruned
	// first. spansPerTrace spans share one TraceId and live within a
	// single day; the ORDER BY leads with ServiceName/SpanName, so a
	// trace's spans scatter across that day's granules (the bloom can't
	// collapse them to one) but never leave the day-partition.
	tsTotalSpans     = 3_000_000
	tsSpansPerTrace  = 8    // spans per trace (mix of sub-second offsets)
	tsNumDays        = 10   // day-partitions = dense parts the corpus spans
	tsTargetTraceIdx = 1234 // which trace we look up
)

func tsTargetTraceID() string {
	return fmt.Sprintf("%032x", tsTargetTraceIdx)
}

// tsSpansDDL is the production OTel-CH spans table (relevant columns +
// the bloom index + PARTITION/ORDER the exporter writes), trimmed to the
// columns this bench needs.
const tsSpansDDL = `CREATE OR REPLACE TABLE otel_traces (
    Timestamp DateTime64(9) CODEC(Delta, ZSTD(1)),
    TraceId String CODEC(ZSTD(1)),
    SpanId String CODEC(ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    SpanName LowCardinality(String) CODEC(ZSTD(1)),
    INDEX idx_trace_id TraceId TYPE bloom_filter(0.001) GRANULARITY 1
) ENGINE = MergeTree()
PARTITION BY toDate(Timestamp)
ORDER BY (ServiceName, SpanName, toDateTime(Timestamp))
SETTINGS index_granularity = 8192;`

// tsLookupDDL is the production `_trace_id_ts` lookup table.
const tsLookupDDL = `CREATE OR REPLACE TABLE otel_traces_trace_id_ts (
    TraceId String CODEC(ZSTD(1)),
    Start DateTime CODEC(Delta, ZSTD(1)),
    End DateTime CODEC(Delta, ZSTD(1)),
    INDEX idx_trace_id TraceId TYPE bloom_filter(0.01) GRANULARITY 1
) ENGINE = MergeTree()
PARTITION BY toDate(Start)
ORDER BY (TraceId, Start)
SETTINGS index_granularity = 8192;`

// tsSpansInsert materialises the corpus. Each trace (= 8 consecutive
// `number`s) lands in ONE day-partition chosen by the trace index, with a
// large within-day second spread so each day is a dense, many-granule part.
// The 8 spans carry a sub-second NANOSECOND offset (0..7 * 0.11s) so the
// MV's End (DateTime, second-floored max(Timestamp)) sits BELOW the true
// max Timestamp — the exact parity hazard the +1s pad addresses. Each span
// gets a distinct ServiceName/SpanName so the trace's rows scatter across
// the day's ServiceName/SpanName-led sort order (the bloom can't collapse
// the trace to a single granule, mirroring real span fan-out).
func tsSpansInsert() string {
	const withinDaySeconds = 80000 // ~22h spread per day-partition
	return fmt.Sprintf(
		`INSERT INTO otel_traces
SELECT
    toDateTime64('2026-01-01 00:00:00', 9)
        + INTERVAL (intDiv(number, %d) %% %d) DAY
        + INTERVAL (number %% %d) SECOND
        + INTERVAL ((number %% %d) * 110000000) NANOSECOND AS Timestamp,
    leftPad(lower(hex(intDiv(number, %d))), 32, '0') AS TraceId,
    leftPad(lower(hex(number)), 16, '0') AS SpanId,
    concat('svc.', toString(number %% 200)) AS ServiceName,
    concat('op.', toString(intDiv(number, %d) %% 500)) AS SpanName
FROM numbers(%d)`,
		tsSpansPerTrace, tsNumDays, // day partition (per-trace)
		withinDaySeconds, // within-day second spread
		tsSpansPerTrace,  // nanosecond sub-second offset (0..7 * 0.11s)
		tsSpansPerTrace,  // TraceId hex
		tsSpansPerTrace,  // SpanName bucket
		tsTotalSpans,     // total rows
	)
}

// tsLookupInsert populates the lookup table via the MV's OWN SELECT — the
// exact aggregation the exporter's materialized view runs.
const tsLookupInsert = `INSERT INTO otel_traces_trace_id_ts
SELECT TraceId, min(Timestamp) AS Start, max(Timestamp) AS End
FROM otel_traces
WHERE TraceId != ''
GROUP BY TraceId;`

// tsExplain holds the two index-stage prune counts that matter for this
// lever. ClickHouse's EXPLAIN indexes=1 lists index stages top-to-bottom:
// the FIRST stage is the MinMax/Partition prune (how many parts the query
// must consider at all — and therefore how many parts' bloom-index blocks
// the bloom Skip stage must subsequently evaluate); the LAST stage is the
// `idx_trace_id` bloom Skip (how many DATA granules survive to be read).
//
//   - firstParts / firstGranules: the leading MinMax-stage selection. This
//     is the axis the Timestamp window moves: it Partition-prunes the spans
//     table to the trace's day-partition(s) so the bloom only evaluates that
//     part's granule-blooms instead of every part's.
//   - bloomGranules: the final data granules the bloom leaves — identical
//     before/after (the bloom already isolates the trace within whatever
//     parts reach it), which is exactly why result parity holds.
type tsExplain struct {
	firstParts, firstGranules int
	bloomGranules             int
	firstCond                 string
}

func tsRunExplain(t *testing.T, db *sql.DB, query string, args ...any) tsExplain {
	t.Helper()
	rows, err := db.Query("EXPLAIN indexes=1 "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v\nquery: %s", err, query)
	}
	defer rows.Close()
	var ex tsExplain
	seenFirstParts := false
	seenFirstGranules := false
	seenFirstCond := false
	var lastGranules string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan: %v", err)
		}
		trim := trimSpace(line)
		switch {
		case hasPrefix(trim, "Condition:") && !seenFirstCond:
			ex.firstCond = trim
			seenFirstCond = true
		case hasPrefix(trim, "Parts:") && !seenFirstParts:
			ex.firstParts = tsSelectedParts(trim)
			seenFirstParts = true
		case hasPrefix(trim, "Granules:"):
			if !seenFirstGranules {
				ex.firstGranules = parseSelectedGranules(trim)
				seenFirstGranules = true
			}
			lastGranules = trim
		}
	}
	ex.bloomGranules = parseSelectedGranules(lastGranules)
	return ex
}

// tsSpanIDs runs `query` and returns the sorted SpanId set — the parity
// fingerprint. A SELECT SpanId projection keeps the row stream tiny.
func tsSpanIDs(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("query: %v\nquery: %s", err, query)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func TestTraceIDWindowPrune_ChDB(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}

	for _, stmt := range []string{tsSpansDDL, tsSpansInsert(), tsLookupDDL, tsLookupInsert} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup exec failed:\n%s\nerr: %v", stmt, err)
		}
	}
	// Force one part per day-partition so each day is a single dense,
	// many-granule part — the bloom Skip index must be evaluated against
	// every such part unless the scan is partition-pruned first. This also
	// makes the granule counts deterministic (not inflated by unmerged
	// insert parts).
	if _, err := db.Exec("OPTIMIZE TABLE otel_traces FINAL"); err != nil {
		t.Fatalf("optimize spans: %v", err)
	}
	if _, err := db.Exec("OPTIMIZE TABLE otel_traces_trace_id_ts FINAL"); err != nil {
		t.Fatalf("optimize lookup: %v", err)
	}

	id := tsTargetTraceID()

	// BEFORE: today's plain TraceId filter (bloom-only).
	before := "SELECT SpanId FROM otel_traces WHERE TraceId = ?"
	// AFTER: TraceId Eq AND the trace_id_ts Timestamp window. Mirrors the
	// chplan emit lowerTraceByID produces under TraceIDTsEnabled (the +1s
	// pad compensates End's DateTime second-flooring).
	after := `SELECT SpanId FROM otel_traces
WHERE TraceId = ?
  AND Timestamp >= (SELECT min(Start) FROM otel_traces_trace_id_ts WHERE TraceId = ?)
  AND Timestamp <= addSeconds((SELECT max(End) FROM otel_traces_trace_id_ts WHERE TraceId = ?), 1)`

	// --- PARITY: identical span-id set (the bar) -------------------------
	beforeIDs := tsSpanIDs(t, db, before, id)
	afterIDs := tsSpanIDs(t, db, after, id, id, id)

	if len(beforeIDs) != tsSpansPerTrace {
		t.Fatalf("BEFORE returned %d spans, want %d — corpus seed is degenerate",
			len(beforeIDs), tsSpansPerTrace)
	}
	if len(beforeIDs) != len(afterIDs) {
		t.Fatalf("PARITY VIOLATION: BEFORE=%d spans, AFTER=%d spans — the window "+
			"dropped/added rows; the optimization is INCORRECT", len(beforeIDs), len(afterIDs))
	}
	for i := range beforeIDs {
		if beforeIDs[i] != afterIDs[i] {
			t.Fatalf("PARITY VIOLATION at span %d: BEFORE=%s AFTER=%s — the window "+
				"is NOT a faithful superset; the optimization is INCORRECT",
				i, beforeIDs[i], afterIDs[i])
		}
	}
	t.Logf("PARITY OK: BEFORE and AFTER both return the identical %d-span set", len(beforeIDs))

	// --- PRUNE: EXPLAIN indexes=1, MinMax-stage part prune ---------------
	exBefore := tsRunExplain(t, db, before, id)
	exAfter := tsRunExplain(t, db, after, id, id, id)

	var totalMarks int64
	db.QueryRow(`SELECT sum(marks) FROM system.parts WHERE table='otel_traces' AND active`).Scan(&totalMarks)

	t.Logf("=== trace_id_ts window prune: %d spans, %d spans/trace, %d dense day-partitions, total marks=%d ===",
		tsTotalSpans, tsSpansPerTrace, tsNumDays, totalMarks)
	t.Logf("%-8s | %-22s | %-22s | %s", "variant", "MinMax-stage parts", "MinMax-stage granules", "data granules (post-bloom)")
	t.Log("---------+------------------------+------------------------+---------------------------")
	t.Logf("%-8s | %-22d | %-22d | %d", "BEFORE", exBefore.firstParts, exBefore.firstGranules, exBefore.bloomGranules)
	t.Logf("%-8s | %-22d | %-22d | %d", "AFTER", exAfter.firstParts, exAfter.firstGranules, exAfter.bloomGranules)

	for _, label := range []struct {
		name  string
		query string
		args  []any
	}{
		{"BEFORE", before, []any{id}},
		{"AFTER", after, []any{id, id, id}},
	} {
		t.Logf("--- EXPLAIN indexes=1  [%s] ---", label.name)
		rows, _ := db.Query("EXPLAIN indexes=1 "+label.query, label.args...)
		for rows.Next() {
			var s string
			rows.Scan(&s)
			t.Log("    " + s)
		}
		rows.Close()
	}

	t.Logf("MinMax part prune: before=%d after=%d  ratio=%.1fx  (the bloom Skip index is then "+
		"evaluated against only AFTER's pruned part-set) | data granules: before=%d after=%d (parity)",
		exBefore.firstParts, exAfter.firstParts,
		float64(exBefore.firstParts)/float64(maxInt1(exAfter.firstParts)),
		exBefore.bloomGranules, exAfter.bloomGranules)

	// --- ASSERTION: the window must partition-prune the spans table ------
	//
	// The structural win is the leading MinMax/Partition stage: BEFORE
	// leaves Condition:true → every day-part is a bloom candidate; AFTER's
	// Timestamp window prunes to the trace's day-partition(s), so the bloom
	// Skip index only evaluates that part's granule blooms. The FINAL data
	// granules the bloom leaves are identical before/after (the bloom
	// already isolates the trace within whatever parts reach it) — which is
	// exactly why result parity holds. We assert the part prune, not a
	// final-granule prune.
	if exAfter.firstParts <= 0 {
		t.Fatalf("AFTER MinMax-stage selected %d parts — EXPLAIN parse degenerate; cannot evaluate prune",
			exAfter.firstParts)
	}
	if exAfter.firstParts >= exBefore.firstParts {
		t.Fatalf("trace_id_ts window did NOT partition-prune the spans table: BEFORE considered "+
			"%d parts at the MinMax stage, AFTER considered %d. The Timestamp window should "+
			"prune to the trace's day-partition(s) so the bloom index evaluates fewer parts.",
			exBefore.firstParts, exAfter.firstParts)
	}
	if exAfter.bloomGranules != exBefore.bloomGranules {
		t.Fatalf("post-bloom data granules diverged (before=%d after=%d): the window must be a "+
			"pure superset that changes only WHICH parts the bloom scans, never the final "+
			"data-granule set — a divergence here signals a correctness problem.",
			exBefore.bloomGranules, exAfter.bloomGranules)
	}
}

// tsSelectedParts extracts the SELECTED count from an EXPLAIN
// `Parts: N/M` line (the first number). The granule sibling
// (parseSelectedGranules) is shared from orderby_chdb_test.go.
func tsSelectedParts(s string) int {
	s = stripPrefix(trimSpace(s), "Parts: ")
	sel := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			break
		}
		sel = sel*10 + int(c-'0')
	}
	return sel
}
