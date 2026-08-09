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
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
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
func cmpScanPartPrune(t *testing.T, db *sql.DB, query string, args ...any) (selected, total int) {
	t.Helper()
	rows, err := db.Query("EXPLAIN indexes=1 "+query, args...)
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

// nonRootWindowStep / nonRootWindowRange size the RangeWindow the real
// compare() emit path (chsql.emitRangeWindowCompare) requires to produce the
// non-root root-lookup shape below: Step must be > 0, and the lookback it
// implies (Range, defaulting to Step when zero) subtracts a single minute
// off the seed's lower bound — negligible against the corpus's year-scale
// date spread, so it does not change which rows the seed selects.
const (
	nonRootWindowStep  = time.Minute
	nonRootWindowRange = time.Minute
)

// compareNonRootRootLookupSQL builds the REAL chplan.MetricsCompare +
// chplan.RangeWindow tree for the non-root-selection root-lookup shape
// (matching TraceIDColumn "TraceId", ParentSpanId-rooted RootLookup over
// spansTable, optionally gated by the RootLookupTraceIDTsTable envelope) and
// runs it through the ACTUAL emitter — the same one production requests go
// through — rather than hand-typing the resulting SQL text (the gap #1439
// closes). windowRootLookupTraceIDSeed / rootLookupTraceIDTsBounds
// (internal/chsql/metrics_compare.go) are unexported, so this only reaches
// them indirectly; nothing here reimplements their predicate shape by hand.
//
// The full emitted statement is compare()'s three-layer matrix query (anchor
// fanout + outer count wrapper) around the `s LEFT JOIN r` join this test does
// not need — only the root ('r') leg is under test, since partition-pruning
// that leg's own scan is what the trace_id_ts envelope claims (#1443).
// chsql.EmitCompareRootLeg renders exactly that leg as a standalone,
// directly-executable statement paired with its own args, so this harness asks
// the emitter for the leg instead of recovering it from the emitted text by
// scanning for a `LEFT JOIN (` marker, counting parens to its match, and
// counting `?` placeholders to guess which slice of the args it consumes.
func compareNonRootRootLookupSQL(t *testing.T, spansTable, lookupTable string, winLo, winHi time.Time, envelope bool) (string, []any) {
	t.Helper()

	matchingChild := &chplan.Binary{
		Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "SpanName"}, Right: &chplan.LitString{V: "matching-child"},
	}
	m := &chplan.MetricsCompare{
		Selection: matchingChild,
		Pairs: &chplan.FuncCall{Name: "array", Args: []chplan.Expr{
			&chplan.FuncCall{Name: "tuple", Args: []chplan.Expr{
				&chplan.LitString{V: "name"}, &chplan.ColumnRef{Name: "ServiceName"},
			}},
		}},
		TraceIDColumn: "TraceId",
		Inner: &chplan.Filter{
			Input:     &chplan.Scan{Table: spansTable},
			Predicate: matchingChild,
		},
		RootLookup: &chplan.Aggregate{
			Input: &chplan.Filter{
				Input: &chplan.Scan{Table: spansTable},
				Predicate: &chplan.Binary{
					Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "ParentSpanId"}, Right: &chplan.LitString{V: ""},
				},
			},
			GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "TraceId"}},
			AggFuncs: []chplan.AggFunc{
				{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: "SpanName"}}, Alias: "root_name"},
				{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}}, Alias: "root_service"},
			},
		},
	}
	if envelope {
		m.RootLookupTraceIDTsTable = lookupTable
		m.RootLookupTraceIDTsStartColumn = "Start"
		m.RootLookupTraceIDTsEndColumn = "End"
	}
	rw := &chplan.RangeWindow{
		Input:           m,
		Range:           nonRootWindowRange,
		Step:            nonRootWindowStep,
		Start:           winLo,
		End:             winHi,
		TimestampColumn: "Timestamp",
	}
	rootSQL, rootArgs, err := chsql.EmitCompareRootLeg(context.Background(), rw)
	if err != nil {
		t.Fatalf("chsql.EmitCompareRootLeg: %v", err)
	}
	// The leg the emitter hands back must be the leg the matrix statement
	// embeds — otherwise this harness would be pruning-checking a query no
	// request ever runs. chsql pins that containment in its own unit test; here
	// it is re-asserted against the full statement this exact plan emits.
	matrixSQL, _, err := chsql.Emit(context.Background(), rw)
	if err != nil {
		t.Fatalf("chsql.Emit: %v", err)
	}
	if !strings.Contains(matrixSQL, rootSQL) {
		t.Fatalf("root leg is not the one the compare join embeds.\nleg:\n%s\nmatrix:\n%s", rootSQL, matrixSQL)
	}
	return rootSQL, rootArgs
}

// TestCompareNonRootRootLookupTraceIDTsEnvelope exercises the case a request
// window cannot represent: a child matches compare() on May 12 while its root
// was recorded on May 1. Tempo still attaches that root's name and service to
// every selected child. The trace_id_ts envelope must therefore retain the
// May-1 root while pruning unrelated retention partitions before that trace.
func TestCompareNonRootRootLookupTraceIDTsEnvelope(t *testing.T) {
	const nonRootEnvelopePartCount = 4

	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	defer db.Close()

	const spans = "compare_nonroot_spans"
	const lookup = "compare_nonroot_trace_id_ts"
	winLo := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	winHi := time.Date(2026, 5, 12, 11, 0, 0, 0, time.UTC)
	for _, stmt := range []string{
		`CREATE TABLE compare_nonroot_spans (
            Timestamp DateTime64(9), TraceId String, ParentSpanId String,
            ServiceName String, SpanName String
        ) ENGINE = MergeTree() PARTITION BY toDate(Timestamp)
        ORDER BY (ServiceName, SpanName, toDateTime(Timestamp))`,
		`CREATE TABLE compare_nonroot_trace_id_ts (
            TraceId String, Start DateTime, End DateTime
        ) ENGINE = MergeTree() PARTITION BY toDate(Start) ORDER BY (TraceId, Start)`,
		`INSERT INTO compare_nonroot_spans VALUES
            ('2022-01-01 00:00:00', 'noise-a', '', 'noise', 'root'),
            ('2023-01-01 00:00:00', 'noise-b', '', 'noise', 'root'),
            ('2024-01-01 00:00:00', 'noise-c', '', 'noise', 'root'),
            ('2026-05-01 08:00:00', 'selected', '', 'checkout', 'checkout-root'),
            ('2026-05-12 10:30:00', 'selected', 'root', 'checkout', 'matching-child')`,
		`INSERT INTO compare_nonroot_trace_id_ts VALUES
            ('noise-a', '2022-01-01 00:00:00', '2022-01-01 00:00:00'),
            ('noise-b', '2023-01-01 00:00:00', '2023-01-01 00:00:00'),
            ('noise-c', '2024-01-01 00:00:00', '2024-01-01 00:00:00'),
            ('selected', '2026-05-01 08:00:00', '2026-05-12 10:30:00')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup exec failed:\n%s\nerr: %v", stmt, err)
		}
	}
	if _, err := db.Exec("OPTIMIZE TABLE " + spans + " FINAL"); err != nil {
		t.Fatalf("optimize spans: %v", err)
	}

	// before / after are the REAL chsql-emitted root-lookup subquery for the
	// #1214 unbounded-but-lossless shape vs. the trace_id_ts-envelope shape —
	// captured from the actual emitter (see compareNonRootRootLookupSQL), not
	// hand-typed SQL text.
	before, beforeArgs := compareNonRootRootLookupSQL(t, spans, lookup, winLo, winHi, false)
	after, afterArgs := compareNonRootRootLookupSQL(t, spans, lookup, winLo, winHi, true)

	rows := func(query string, args ...any) []string {
		t.Helper()
		rs, qerr := db.Query(query, args...)
		if qerr != nil {
			t.Fatalf("query: %v\n%s", qerr, query)
		}
		defer rs.Close()
		var got []string
		for rs.Next() {
			var traceID, name, service string
			if scanErr := rs.Scan(&traceID, &name, &service); scanErr != nil {
				t.Fatalf("scan root lookup: %v", scanErr)
			}
			got = append(got, traceID+"|"+name+"|"+service)
		}
		return got
	}
	beforeRows, afterRows := rows(before, beforeArgs...), rows(after, afterArgs...)
	if len(beforeRows) != 1 || beforeRows[0] != "selected|checkout-root|checkout" {
		t.Fatalf("unbounded root lookup = %v, want Tempo root enrichment for selected child", beforeRows)
	}
	if len(afterRows) != len(beforeRows) || afterRows[0] != beforeRows[0] {
		t.Fatalf("Tempo parity violation: unbounded=%v trace_id_ts envelope=%v", beforeRows, afterRows)
	}

	beforeParts, beforeTotal := cmpScanPartPrune(t, db, before, beforeArgs...)
	afterParts, afterTotal := cmpScanPartPrune(t, db, after, afterArgs...)
	if beforeTotal < nonRootEnvelopePartCount || afterTotal != beforeTotal {
		t.Fatalf("root lookup EXPLAIN parts before=%d/%d after=%d/%d, want a shared multi-part corpus", beforeParts, beforeTotal, afterParts, afterTotal)
	}
	if afterParts >= beforeParts {
		t.Fatalf("trace_id_ts envelope did not partition-prune the non-root root scan: before=%d/%d after=%d/%d", beforeParts, beforeTotal, afterParts, afterTotal)
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
