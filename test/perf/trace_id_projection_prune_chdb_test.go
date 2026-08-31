//go:build chdb

// Lever: the proj_trace_id lookup projection (cerberus issue #2767).
//
// traceid_window_prune_chdb_test.go already measures the trace_id_ts
// materialized-view lever (a Timestamp window, partition-pruning WHICH
// PARTS the bloom filter evaluates). This file measures a DIFFERENT lever
// on the SAME underlying problem — otel_traces has no TraceId locality in
// its own ORDER BY, so a `WHERE TraceId IN (...)` reads a granule from every
// candidate part to apply the probabilistic idx_trace_id bloom filter — by
// contrasting an identical corpus with and without proj_trace_id (SELECT
// TraceId, _part_offset ORDER BY TraceId) plus the trace_id_bitmap_filter
// setting stamped, giving ClickHouse >= 25.5/25.11 exact row addressing
// instead of a probabilistic granule skip.
//
// # Two shapes, two different wins — MEASURED, not assumed
//
// TestTraceIDProjectionPrune_CoveringOnly probes a query the projection can
// answer ENTIRELY BY ITSELF (`SELECT count()` over only TraceId — an
// existence check, or the shape the trace_id_ts MV's own populate query
// takes). Here ClickHouse swaps the READ SOURCE to `ReadFromMergeTree
// (proj_trace_id)` outright — a real secondary-index seek instead of a
// granule scan — and this file's own numbers (t.Logf output; recorded
// verbatim in the cerberus issue #2767 PR body) show a large, genuine
// multi-x reduction in both post-index data granules AND median wall-clock
// latency at a 30M-span corpus.
//
// TestTraceIDProjectionPrune_FullRow probes cerberus's ACTUAL production
// shape — internal/api/tempo/root_lookup.go's `TraceId IN (...)` follow-up
// query, which needs SpanId (and in production, several more columns) the
// projection does not carry. This is NOT the same win. MEASURED across
// several probes at this corpus scale: neither `EXPLAIN indexes=1`'s
// granule count nor `EXPLAIN ESTIMATE`'s row/mark estimate moves AT ALL —
// both metrics are computed at the granule-pruning stage, and ClickHouse's
// >= 25.11 mechanism (see internal/chopt.FeatureTraceIDBitmapFilter's doc
// comment) applies the projection's bitmap as a row-level PREWHERE filter
// WITHIN the granules the bloom filter already selected, not as an
// additional granule-pruning stage on top of it — cerberus's existing
// bloom_filter(0.001) tuning already leaves little granule-level headroom
// at this scale (see TestTraceIDProjectionPrune_FullRow's own doc comment
// for the arithmetic). The real, measured effect there is a modest
// wall-clock latency improvement (row-level CPU filtering, not I/O), which
// this test reports and guards against REGRESSING — it does not assert a
// specific multiplier, because asserting one would misstate what actually
// moves for this shape.
//
// Both are real, both are cerberus issue #2767's actual production shapes,
// and reporting only the flattering one would misrepresent the feature.
//
// Test/perf carries no existing trace or log LFS sample data (only the
// metrics parquets under smoke/nightly's testdata/samples/) — this corpus
// is synthetic but production-shaped: the real OTel-CH column set + codecs,
// wide day-partitioned parts, many services, and a scale large enough that
// the bloom filter's absolute false-positive count is no longer negligible
// (see the file-level arithmetic in TestTraceIDProjectionPrune_FullRow).
package perf

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver" // registers "chdb" sql driver
)

const (
	// tpTotalSpans / tpSpansPerTrace / tpNumDays: a larger corpus than
	// traceid_window_prune_chdb_test.go's 3M/10-day grid — MEASURED to be
	// necessary: at 3M spans (~380 marks) the bloom filter's expected
	// absolute false-positive count across even a 50-ID IN-list is under 1,
	// leaving nothing for either shape below to prune. 30M/30 days keeps the
	// SAME per-day density (and hence the SAME per-trace span-scatter shape)
	// while growing the total mark count, which is the axis both levers
	// actually respond to.
	tpTotalSpans     = 30_000_000
	tpSpansPerTrace  = 8
	tpNumDays        = 30
	tpTargetTraceIdx = 1234

	// tpLookupTraceCount is how many distinct trace IDs the IN-list probes
	// look up in one query — matching internal/api/tempo/root_lookup.go's
	// real usage (a batch of trace IDs from one /api/search result page),
	// not an arbitrarily large number chosen to flatter the projection.
	tpLookupTraceCount = 50
	// tpLookupTraceStride scatters the looked-up trace indices across the
	// full corpus (rather than a contiguous block) so they land in
	// different day-partitions and ServiceName groups — the real shape a
	// search result page's trace IDs have, not an artificially clustered
	// best case for either variant.
	tpLookupTraceStride = 997

	// tpLatencyReps / tpLatencyWarmup: repeated timed runs (after warmup,
	// which primes chDB's own part/mark metadata caches so the FIRST timed
	// rep is not penalized for one-time setup cost neither variant pays in
	// steady state) — the median, not the mean, is reported since chDB's
	// in-process runtime under CI-shared cores has occasional long-tail
	// scheduling stalls unrelated to the query plan itself.
	tpLatencyReps   = 15
	tpLatencyWarmup = 3

	// tpFullRowRegressionTolerance bounds how much SLOWER the AFTER variant
	// (projection installed + setting stamped) may be than BEFORE before
	// TestTraceIDProjectionPrune_FullRow fails — the "watch out for
	// performance regressions" guard for the shape whose win is modest by
	// measurement (row-level CPU filtering, not granule-level I/O — see the
	// file doc comment), where a hard "must be N times faster" assertion
	// would be both false to what is actually measured and flaky against
	// chDB's own run-to-run noise. 1.5x is deliberately generous — a real
	// regression from the tiny per-part projection maintenance cost bleeding
	// into the READ path would be a multiple of that, not a rounding error.
	tpFullRowRegressionTolerance = 1.5
)

// tpLookupTraceIDs returns the tpLookupTraceCount trace IDs the IN-list
// probes look up, scattered by tpLookupTraceStride across the corpus.
func tpLookupTraceIDs() []string {
	ids := make([]string, tpLookupTraceCount)
	for i := range ids {
		ids[i] = fmt.Sprintf("%032x", i*tpLookupTraceStride+tpTargetTraceIdx)
	}
	return ids
}

// tpQuotedTraceIDs renders tpLookupTraceIDs as a comma-separated SQL literal
// list, shared by both the covering-only and full-row probes below.
func tpQuotedTraceIDs() string {
	ids := tpLookupTraceIDs()
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = "'" + id + "'"
	}
	return strings.Join(quoted, ", ")
}

// tpSpansDDL renders the production otel_traces columns this bench needs,
// optionally carrying proj_trace_id — table name and the projection clause
// are the only difference between the BEFORE and AFTER tables.
func tpSpansDDL(table string, withProjection bool) string {
	proj := ""
	if withProjection {
		proj = ",\n    PROJECTION proj_trace_id (SELECT TraceId, _part_offset ORDER BY TraceId)"
	}
	return fmt.Sprintf(`CREATE OR REPLACE TABLE %s (
    Timestamp DateTime64(9) CODEC(Delta, ZSTD(1)),
    TraceId String CODEC(ZSTD(1)),
    SpanId String CODEC(ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    SpanName LowCardinality(String) CODEC(ZSTD(1)),
    INDEX idx_trace_id TraceId TYPE bloom_filter(0.001) GRANULARITY 1%s
) ENGINE = MergeTree()
PARTITION BY toDate(Timestamp)
ORDER BY (ServiceName, SpanName, toDateTime(Timestamp))
SETTINGS index_granularity = 8192;`, table, proj)
}

// tpSpansInsert is the SAME generator traceid_window_prune_chdb_test.go's
// tsSpansInsert uses (byte-identical formula, different table name/scale),
// so the BEFORE and AFTER corpora are deterministically identical.
func tpSpansInsert(table string) string {
	const withinDaySeconds = 80000
	return fmt.Sprintf(
		`INSERT INTO %s
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
		table,
		tpSpansPerTrace, tpNumDays,
		withinDaySeconds,
		tpSpansPerTrace,
		tpSpansPerTrace,
		tpSpansPerTrace,
		tpTotalSpans,
	)
}

// tpSeedPair creates and populates the BEFORE (bloom-only) and AFTER
// (bloom + proj_trace_id) tables, sharing the identical corpus generator.
func tpSeedPair(t *testing.T, db *sql.DB, before, after string) {
	t.Helper()
	for _, stmt := range []string{
		tpSpansDDL(before, false), tpSpansInsert(before),
		tpSpansDDL(after, true), tpSpansInsert(after),
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup exec failed:\n%s\nerr: %v", stmt, err)
		}
	}
	for _, table := range []string{before, after} {
		if _, err := db.Exec("OPTIMIZE TABLE " + table + " FINAL"); err != nil {
			t.Fatalf("optimize %s: %v", table, err)
		}
	}
}

// tpDataGranules runs `EXPLAIN indexes=1 <query>` and returns the LAST
// `Granules: N/M` line's selected count — the final data granules ClickHouse
// actually reads, mirroring tsRunExplain's bloomGranules field.
func tpDataGranules(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	rows, err := db.Query("EXPLAIN indexes=1 " + query)
	if err != nil {
		t.Fatalf("EXPLAIN: %v\nquery: %s", err, query)
	}
	defer rows.Close()
	var last string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan: %v", err)
		}
		trim := trimSpace(line)
		if hasPrefix(trim, "Granules:") {
			last = trim
		}
	}
	return parseSelectedGranules(last)
}

// tpProjectionUsed runs `EXPLAIN indexes=1, projections=1 <query>` and
// reports whether the plan routes through proj_trace_id — either as a used
// secondary-index Projection (see internal/schema/ddl's
// trace_id_bitmap_filter_probe_chdb_test.go — `projections=1` is
// load-bearing: a plain `indexes=1` EXPLAIN never surfaces a used
// projection at all) OR, the covering-only probe's own shape, as the direct
// `ReadFromMergeTree (proj_trace_id)` read source, when the optimizer
// decides the projection alone answers the query without the base table.
func tpProjectionUsed(t *testing.T, db *sql.DB, query string) bool {
	t.Helper()
	rows, err := db.Query("EXPLAIN indexes=1, projections=1 " + query)
	if err != nil {
		t.Fatalf("EXPLAIN: %v\nquery: %s", err, query)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if strings.Contains(trimSpace(line), "proj_trace_id") {
			return true
		}
	}
	return false
}

// tpMedianLatency runs query tpLatencyReps times (discarding the first
// tpLatencyWarmup as cache warmup) and returns the median wall-clock
// duration — the headline "measured, not assumed" number cerberus issue
// #2767's performance-verification directive asks for.
func tpMedianLatency(t *testing.T, db *sql.DB, query string) time.Duration {
	t.Helper()
	var samples []time.Duration
	for i := 0; i < tpLatencyReps; i++ {
		start := time.Now()
		rows, err := db.Query(query)
		if err != nil {
			t.Fatalf("query: %v\nquery: %s", err, query)
		}
		for rows.Next() {
			cols, _ := rows.Columns()
			dest := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range dest {
				ptrs[i] = &dest[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan: %v", err)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		rows.Close()
		elapsed := time.Since(start)
		if i >= tpLatencyWarmup {
			samples = append(samples, elapsed)
		}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return samples[len(samples)/2]
}

// tpLogResult prints the shared before/after summary line both probes below
// use, and returns the measured latency ratio (before/after; >1 = AFTER
// faster).
func tpLogResult(t *testing.T, label string, beforeGranules, afterGranules int, beforeLatency, afterLatency time.Duration) float64 {
	t.Helper()
	ratio := float64(beforeLatency) / float64(maxInt1Duration(afterLatency))
	t.Logf("=== %s: %d spans, %d spans/trace, %d dense day-partitions, %d IDs looked up ===",
		label, tpTotalSpans, tpSpansPerTrace, tpNumDays, tpLookupTraceCount)
	t.Logf("%-8s | %-16s | %s", "variant", "data granules", "median latency")
	t.Logf("%-8s | %-16d | %s", "BEFORE", beforeGranules, beforeLatency)
	t.Logf("%-8s | %-16d | %s", "AFTER", afterGranules, afterLatency)
	t.Logf("granule reduction: %.1fx (before=%d after=%d) | latency: %.2fx (before=%s after=%s)",
		float64(beforeGranules)/float64(maxInt1(afterGranules)), beforeGranules, afterGranules,
		ratio, beforeLatency, afterLatency)
	return ratio
}

// TestTraceIDProjectionPrune_CoveringOnly probes an existence-check shape —
// `SELECT count()` filtered on ONLY TraceId, which proj_trace_id can answer
// entirely by itself (the same shape the trace_id_ts MV's own populate
// query and a "does this trace exist" check both take). This is where the
// projection acts as a genuine secondary-index READ SOURCE, not merely a
// row-level PREWHERE filter — see TestTraceIDProjectionPrune_FullRow's doc
// comment for the contrasting, narrower-margin shape.
func TestTraceIDProjectionPrune_CoveringOnly(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}

	const before, after = "otel_traces_covering_bloom", "otel_traces_covering_proj"
	tpSeedPair(t, db, before, after)

	beforeQuery := "SELECT count() FROM " + before + " WHERE TraceId IN (" + tpQuotedTraceIDs() + ")"
	afterQuery := "SELECT count() FROM " + after + " WHERE TraceId IN (" + tpQuotedTraceIDs() + ")" +
		" SETTINGS min_table_rows_to_use_projection_index = 0"

	// --- PARITY ----------------------------------------------------------
	var beforeCount, afterCount int64
	if err := db.QueryRow(beforeQuery).Scan(&beforeCount); err != nil {
		t.Fatalf("BEFORE count: %v", err)
	}
	if err := db.QueryRow(afterQuery).Scan(&afterCount); err != nil {
		t.Fatalf("AFTER count: %v", err)
	}
	wantSpans := int64(tpLookupTraceCount * tpSpansPerTrace)
	if beforeCount != wantSpans {
		t.Fatalf("BEFORE count=%d, want %d — corpus seed is degenerate", beforeCount, wantSpans)
	}
	if beforeCount != afterCount {
		t.Fatalf("PARITY VIOLATION: BEFORE count=%d, AFTER count=%d", beforeCount, afterCount)
	}
	t.Logf("PARITY OK: BEFORE and AFTER both count %d spans (%d trace IDs)", beforeCount, tpLookupTraceCount)

	// --- PROJECTION ACTUALLY USED, AFTER ONLY -----------------------------
	if tpProjectionUsed(t, db, beforeQuery) {
		t.Fatalf("BEFORE (no proj_trace_id on this table) reports the projection used — test setup is broken")
	}
	if !tpProjectionUsed(t, db, afterQuery) {
		t.Fatalf("AFTER never routes onto proj_trace_id — the optimization this test exists to measure did not fire")
	}

	beforeGranules := tpDataGranules(t, db, beforeQuery)
	afterGranules := tpDataGranules(t, db, afterQuery)
	beforeLatency := tpMedianLatency(t, db, beforeQuery)
	afterLatency := tpMedianLatency(t, db, afterQuery)
	tpLogResult(t, "proj_trace_id covering-only (existence check)", beforeGranules, afterGranules, beforeLatency, afterLatency)

	if afterGranules >= beforeGranules {
		t.Fatalf("proj_trace_id did NOT reduce data granules read for the covering-only shape: BEFORE=%d AFTER=%d",
			beforeGranules, afterGranules)
	}
}

// TestTraceIDProjectionPrune_FullRow probes cerberus's actual production
// shape: internal/api/tempo/root_lookup.go's own `TraceId IN (...)`
// follow-up query needs SpanId (and, in production, several more columns —
// SpanName, ResourceAttributes, Duration) the projection does not carry, so
// unlike the covering-only probe above the base table read is unavoidable
// regardless of the projection.
//
// MEASURED, not assumed: at this corpus's ~3720 total marks and 0.1% bloom
// FPR, the bloom filter's own expected false-positive count across a 50-ID
// lookup is ~3.7 — already a small fraction of the ~400 true-positive
// granules a real 50-trace, 8-span-each result touches, so there is little
// GRANULE-level headroom left regardless of the projection; neither `EXPLAIN
// indexes=1`'s granule count nor `EXPLAIN ESTIMATE`'s row/mark estimate
// moves at all for this shape (both are computed at the same
// granule-pruning stage the bloom filter already occupies). ClickHouse's own
// >= 25.11 mechanism applies the projection's bitmap as a row-level PREWHERE
// filter WITHIN the granules already selected — a CPU-side row-filtering
// win, not an I/O-side granule reduction — so this test does not assert a
// granule reduction (it would be asserting something false) and does not
// assert a specific latency multiplier (asserting one would overstate a
// modest, noisy number). It DOES assert PARITY, that the projection is
// genuinely used, and that AFTER is not a REGRESSION beyond
// tpFullRowRegressionTolerance — the "watch out for performance
// regressions" guard cerberus issue #2767's own directive asks for.
func TestTraceIDProjectionPrune_FullRow(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}

	const before, after = "otel_traces_fullrow_bloom", "otel_traces_fullrow_proj"
	tpSeedPair(t, db, before, after)

	beforeQuery := "SELECT SpanId FROM " + before + " WHERE TraceId IN (" + tpQuotedTraceIDs() + ")"
	afterQuery := "SELECT SpanId FROM " + after + " WHERE TraceId IN (" + tpQuotedTraceIDs() + ")" +
		" SETTINGS min_table_rows_to_use_projection_index = 0"

	// --- PARITY ----------------------------------------------------------
	beforeIDs := tpSpanIDs(t, db, beforeQuery)
	afterIDs := tpSpanIDs(t, db, afterQuery)
	wantSpans := tpLookupTraceCount * tpSpansPerTrace
	if len(beforeIDs) != wantSpans {
		t.Fatalf("BEFORE returned %d spans, want %d — corpus seed is degenerate", len(beforeIDs), wantSpans)
	}
	if len(beforeIDs) != len(afterIDs) {
		t.Fatalf("PARITY VIOLATION: BEFORE=%d spans, AFTER=%d spans", len(beforeIDs), len(afterIDs))
	}
	for i := range beforeIDs {
		if beforeIDs[i] != afterIDs[i] {
			t.Fatalf("PARITY VIOLATION at span %d: BEFORE=%s AFTER=%s", i, beforeIDs[i], afterIDs[i])
		}
	}
	t.Logf("PARITY OK: BEFORE and AFTER both return the identical %d-span set (%d trace IDs)", len(beforeIDs), tpLookupTraceCount)

	// --- PROJECTION ACTUALLY USED, AFTER ONLY -----------------------------
	if tpProjectionUsed(t, db, beforeQuery) {
		t.Fatalf("BEFORE (no proj_trace_id on this table) reports the projection used — test setup is broken")
	}
	if !tpProjectionUsed(t, db, afterQuery) {
		t.Fatalf("AFTER never routes onto proj_trace_id — the optimization this test exists to measure did not fire")
	}

	beforeGranules := tpDataGranules(t, db, beforeQuery)
	afterGranules := tpDataGranules(t, db, afterQuery)
	beforeLatency := tpMedianLatency(t, db, beforeQuery)
	afterLatency := tpMedianLatency(t, db, afterQuery)
	tpLogResult(t, "proj_trace_id full-row (root_lookup.go shape)", beforeGranules, afterGranules, beforeLatency, afterLatency)

	// --- REGRESSION GUARD (not a win assertion — see the doc comment) ----
	if float64(afterLatency) > float64(beforeLatency)*tpFullRowRegressionTolerance {
		t.Fatalf("AFTER (%s) is more than %.1fx slower than BEFORE (%s) for the full-row shape — "+
			"the projection's write-path/read-path cost is regressing this query, not merely failing to help it",
			afterLatency, tpFullRowRegressionTolerance, beforeLatency)
	}
}

// tpSpanIDs runs query and returns the sorted SpanId set — the parity
// fingerprint, mirroring tsSpanIDs.
func tpSpanIDs(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
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

// maxInt1Duration floors d at 1ns so a latency ratio never divides by zero
// on an implausibly-fast AFTER sample.
func maxInt1Duration(d time.Duration) time.Duration {
	if d < 1 {
		return 1
	}
	return d
}
