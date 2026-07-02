package spansscan_test

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/spansscan"
)

// These fixtures encode the partition-pruning ground truth validated against
// prod ClickHouse: otel_traces is `PARTITION BY toDate(Timestamp)`, so ONLY a
// Timestamp range sitting directly on a physical scan prunes partitions. A
// recursive (`WITH RECURSIVE`) arm or a pre-`TraceId IN` `GROUP BY` cannot have a
// window pushed below it by CH, so an unwindowed scan there reads the whole
// table — the traces-OOM class. A windowed `TraceId IN (<seed>)` is INERT for
// pruning.
//
// The matcher arms on ANY `Timestamp` range comparison in the statement,
// regardless of rendering: the search / leaf path emits the window as
// `fromUnixTimestamp64Nano(<nanos>)`, the metrics range-window grid as
// `toDateTime64('<ts>', 9)`. The metricsWindow* fixtures below are the
// regression guard for that second rendering — keying only on
// fromUnixTimestamp64Nano (the pre-fix behaviour) left the metrics path
// uncovered.

const spansTable = "otel_traces"

// recursiveArmUnwindowed: windowed query (the seed carries
// fromUnixTimestamp64Nano), but the recursive STEP arm LOST its co-scope
// Timestamp — its top-level WHERE has only the depth cap and the inert
// `TraceId IN` seed. Reads the whole table → flagged.
const recursiveArmUnwindowed = "WITH RECURSIVE _struct_closure_1 AS (" +
	"SELECT DISTINCT `TraceId`, `SpanId`, `ParentSpanId`, 0 AS _depth " +
	"FROM (SELECT * FROM `otel_traces` WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000)) AND (`ResourceAttributes`[?] = ?)) AS _seed " +
	"UNION ALL " +
	"SELECT DISTINCT t.`TraceId`, t.`SpanId`, t.`ParentSpanId`, c._depth + 1 " +
	"FROM `otel_traces` AS t INNER JOIN _struct_closure_1 AS c ON t.`TraceId` = c.`TraceId` AND t.`ParentSpanId` = c.`SpanId` " +
	"WHERE c._depth < 128 AND t.`TraceId` IN (SELECT `TraceId` FROM (SELECT * FROM `otel_traces` WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000))) AS _seed_ids)" +
	") SELECT DISTINCT `TraceId`, `SpanId` FROM _struct_closure_1 WHERE _depth > 0"

// recursiveArmWindowed: the same query with the request window restored on the
// recursive `otel_traces AS t` scan (co-scope with the depth cap / seed). Must
// NOT be flagged.
const recursiveArmWindowed = "WITH RECURSIVE _struct_closure_1 AS (" +
	"SELECT DISTINCT `TraceId`, `SpanId`, `ParentSpanId`, 0 AS _depth " +
	"FROM (SELECT * FROM `otel_traces` WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000)) AND (`ResourceAttributes`[?] = ?)) AS _seed " +
	"UNION ALL " +
	"SELECT DISTINCT t.`TraceId`, t.`SpanId`, t.`ParentSpanId`, c._depth + 1 " +
	"FROM `otel_traces` AS t INNER JOIN _struct_closure_1 AS c ON t.`TraceId` = c.`TraceId` AND t.`ParentSpanId` = c.`SpanId` " +
	"WHERE c._depth < 128 AND t.`TraceId` IN (SELECT `TraceId` FROM (SELECT * FROM `otel_traces` WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000))) AS _seed_ids) " +
	"AND `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) AND `Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000)" +
	") SELECT DISTINCT `TraceId`, `SpanId` FROM _struct_closure_1 WHERE _depth > 0"

// groupByRootLookupUnwindowed: `FROM otel_traces … GROUP BY TraceId` with only
// an inert `TraceId IN (<windowed seed>)` — the GROUP BY runs over the whole
// table before the IN can filter. Flagged.
const groupByRootLookupUnwindowed = "SELECT `TraceId`, any(`SpanName`) AS `__root_name` " +
	"FROM `otel_traces` " +
	"WHERE `ParentSpanId` = '' AND `TraceId` IN (SELECT `TraceId` FROM `otel_traces` WHERE `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) GROUP BY `TraceId`) " +
	"GROUP BY `TraceId`"

// groupByRootLookupWindowed: the request window sits directly on the root-lookup
// scan, co-scope with its GROUP BY. Must NOT be flagged. This is also the
// trace-by-id derived-window shape: a co-scope Timestamp range (MV-derived or a
// fallback lookback) on the spans scan prunes, so it passes.
const groupByRootLookupWindowed = "SELECT `TraceId`, any(`SpanName`) AS `__root_name` " +
	"FROM `otel_traces` " +
	"WHERE `ParentSpanId` = '' AND `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) AND `Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000) AND `TraceId` IN (SELECT `TraceId` FROM `otel_traces` WHERE `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) GROUP BY `TraceId`) " +
	"GROUP BY `TraceId`"

// recursiveArmMaskedBySibling is the F3 false-accept regression: a windowless
// recursive STEP arm (`FROM otel_traces AS t`, no co-scope Timestamp) FOLLOWED at
// the SAME paren depth, across a `UNION ALL`, by a genuinely windowed sibling arm
// (`FROM otel_traces WHERE Timestamp >= …`). Before the set-op-boundary fix, the
// windowless arm's forward scope ran past the `UNION ALL` and folded in the
// sibling arm's Timestamp predicate, so reTimestampCmp matched the borrowed
// window and the windowless full-table scan was silently NOT flagged. The scope
// walk now stops at the depth-0 `UNION ALL`, so the windowless arm is flagged
// (1 finding) while the windowed sibling stays clean.
const recursiveArmMaskedBySibling = "WITH RECURSIVE _struct_closure_1 AS (" +
	"SELECT DISTINCT `TraceId`, `SpanId`, `ParentSpanId`, 0 AS _depth " +
	"FROM (SELECT * FROM `otel_traces` WHERE (`ResourceAttributes`[?] = ?)) AS _seed " +
	"UNION ALL " +
	"SELECT DISTINCT t.`TraceId`, t.`SpanId`, t.`ParentSpanId`, c._depth + 1 " +
	"FROM `otel_traces` AS t INNER JOIN _struct_closure_1 AS c ON t.`TraceId` = c.`TraceId` AND t.`ParentSpanId` = c.`SpanId` " +
	"WHERE c._depth < 128 AND t.`TraceId` IN (SELECT `TraceId` FROM (SELECT * FROM `otel_traces` WHERE (`ResourceAttributes`[?] = ?)) AS _seed_ids) " +
	"UNION ALL " +
	"SELECT DISTINCT `TraceId`, `SpanId`, `ParentSpanId`, 0 AS _depth " +
	"FROM `otel_traces` " +
	"WHERE `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) AND `Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000)" +
	") SELECT DISTINCT `TraceId`, `SpanId` FROM _struct_closure_1 WHERE _depth > 0"

// metricsWindowRecursiveUnwindowed is THE regression guard for the metrics-path
// gap: a `{ } >> { } | rate()` shape where the request window appears ONLY as the
// range-window grid's `toDateTime64(...)` wrapper predicate — there is no
// fromUnixTimestamp64Nano anywhere — and the recursive STEP arm is windowless.
// Pre-fix the matcher keyed solely on fromUnixTimestamp64Nano and therefore
// DEFERRED on this statement, shipping a full-retention recursive scan. It must
// now be flagged.
const metricsWindowRecursiveUnwindowed = "SELECT `anchor_ts`, sum(in_window) AS `Value` FROM (" +
	"SELECT 1 AS in_window FROM (" +
	"WITH RECURSIVE _struct_closure_1 AS (" +
	"SELECT DISTINCT `TraceId`, `SpanId`, `ParentSpanId`, 0 AS _depth FROM (SELECT * FROM `otel_traces` WHERE (`SpanAttributes`[?] = ?)) AS _seed " +
	"UNION ALL " +
	"SELECT DISTINCT t.`TraceId`, t.`SpanId`, t.`ParentSpanId`, c._depth + 1 " +
	"FROM `otel_traces` AS t INNER JOIN _struct_closure_1 AS c ON t.`TraceId` = c.`TraceId` AND t.`ParentSpanId` = c.`SpanId` " +
	"WHERE c._depth < 128 AND t.`TraceId` IN (SELECT `TraceId` FROM (SELECT * FROM `otel_traces` WHERE (`SpanAttributes`[?] = ?)) AS _seed_ids)" +
	") SELECT DISTINCT `TraceId`, `SpanId` FROM _struct_closure_1 WHERE _depth > 0" +
	") AS L) " +
	"WHERE `Timestamp` > toDateTime64('2026-06-27 14:43:12.000000000', 9) AND `Timestamp` <= toDateTime64('2026-06-27 15:13:12.000000000', 9) " +
	"GROUP BY `anchor_ts`"

// matrixFamilyWrapper is the CONFIRMED-FINE range-window matrix shape: a plain
// pass-through `(SELECT * FROM otel_traces WHERE …)` whose Timestamp window sits
// on the enclosing wrapper — CH pushes it into the scan. The outer GROUP BY must
// NOT cause a flag, because the scan is a `SELECT *` pass-through. Renders its
// window as toDateTime64, so it also pins that the broadened precondition does
// not over-fire on the pass-through shape.
const matrixFamilyWrapper = "SELECT `anchor_ts`, toFloat64(sum(in_window)) / 300 AS `Value` " +
	"FROM (SELECT arrayJoin(range(0, 61)) AS `anchor_ts`, 1 AS `in_window` " +
	"FROM (SELECT * FROM `otel_traces` WHERE (`ParentSpanId` = ?)) WHERE `Timestamp` > toDateTime64('2026-06-27 14:43:12.000000000', 9) AND `Timestamp` <= toDateTime64('2026-06-27 15:13:12.000000000', 9)) " +
	"GROUP BY `anchor_ts`"

// plainWindowedScan is a direct (non-pass-through) scan that carries its own
// co-scope window. No recursion, no GROUP BY: must not be flagged.
const plainWindowedScan = "SELECT `TraceId`, `SpanId` FROM `otel_traces` " +
	"WHERE `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) AND `Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000) " +
	"ORDER BY `Timestamp` DESC LIMIT 100"

// nonSpansMetrics is a pure PromQL-style metrics statement: it carries a window
// but never touches otel_traces. The matcher is table-scoped, so a non-spans
// head can never be flagged.
const nonSpansMetrics = "SELECT `MetricName`, sum(`Value`) AS `Value` " +
	"FROM `otel_metrics_sum` " +
	"WHERE `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) AND `Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000) " +
	"GROUP BY `MetricName`"

func TestUnwindowedSpansScans(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		sql  string
		want int
	}{
		// POSITIVE — these read the whole table.
		{"recursive_arm_unwindowed", recursiveArmUnwindowed, 1},
		{"group_by_root_lookup_unwindowed", groupByRootLookupUnwindowed, 1},
		// POSITIVE regression: metrics window rendered as toDateTime64 only.
		{"metrics_window_recursive_unwindowed", metricsWindowRecursiveUnwindowed, 1},
		// POSITIVE regression (F3): a windowed sibling arm must not mask a
		// windowless one across a UNION ALL.
		{"recursive_arm_masked_by_sibling", recursiveArmMaskedBySibling, 1},

		// NEGATIVE — confirmed-FINE shapes; a flag here is a false reject.
		{"recursive_arm_windowed", recursiveArmWindowed, 0},
		{"group_by_root_lookup_windowed", groupByRootLookupWindowed, 0},
		{"matrix_family_wrapper", matrixFamilyWrapper, 0},
		{"plain_windowed_scan", plainWindowedScan, 0},
		{"non_spans_metrics", nonSpansMetrics, 0},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := len(spansscan.UnwindowedSpansScans(tc.sql, spansTable))
			if got != tc.want {
				t.Fatalf("UnwindowedSpansScans(%s): got %d finding(s), want %d\nSQL:\n%s",
					tc.name, got, tc.want, tc.sql)
			}
		})
	}
}

// TestUnwindowedSpansScans_MetricsWindowRendering is the focused regression for
// the toDateTime64 rendering: the metrics fixture carries NO fromUnixTimestamp64Nano
// at all, yet is still recognised as windowed (so its windowless recursive arm is
// flagged). This is the exact case the pre-fix `Contains(RequestWindowBound)`
// precondition let slip.
func TestUnwindowedSpansScans_MetricsWindowRendering(t *testing.T) {
	t.Parallel()
	if strings.Contains(metricsWindowRecursiveUnwindowed, spansscan.RequestWindowBound) {
		t.Fatalf("test setup: metrics fixture should not contain the search-window marker %q", spansscan.RequestWindowBound)
	}
	if n := len(spansscan.UnwindowedSpansScans(metricsWindowRecursiveUnwindowed, spansTable)); n != 1 {
		t.Fatalf("metrics-window recursive arm: got %d finding(s), want 1 (toDateTime64 window must arm the matcher)", n)
	}
}

// TestUnwindowedSpansScans_SiblingArmDoesNotMask pins the F3 fix at the finding
// level: the single flagged scan must be the windowless STEP arm, NOT the
// windowed sibling that follows it across the UNION ALL. Asserting the offset
// (not just the count) proves the matcher attributes the finding to the right
// arm and does not borrow the sibling's Timestamp predicate.
func TestUnwindowedSpansScans_SiblingArmDoesNotMask(t *testing.T) {
	t.Parallel()
	findings := spansscan.UnwindowedSpansScans(recursiveArmMaskedBySibling, spansTable)
	if len(findings) != 1 {
		t.Fatalf("masked-by-sibling: got %d finding(s), want 1\nSQL:\n%s", len(findings), recursiveArmMaskedBySibling)
	}
	windowlessArm := strings.Index(recursiveArmMaskedBySibling, "FROM `otel_traces` AS t")
	windowedSibling := strings.LastIndex(recursiveArmMaskedBySibling, "FROM `otel_traces`")
	if windowlessArm < 0 || windowedSibling <= windowlessArm {
		t.Fatalf("test setup: expected the windowless `AS t` arm to precede the windowed sibling (got %d, %d)", windowlessArm, windowedSibling)
	}
	// The finding's FROM offset is the keyword start; the substring index points
	// at the same `FROM` token, so they coincide.
	if findings[0].Offset != windowlessArm {
		t.Fatalf("masked-by-sibling: finding flagged at offset %d, want the windowless STEP arm at %d (sibling scan is at %d)",
			findings[0].Offset, windowlessArm, windowedSibling)
	}
}

// TestUnwindowedSpansScans_NoWindowDefers pins the precondition: a statement with
// NO Timestamp range comparison in any rendering has nothing to push down, so the
// rule stays silent — the unbounded-query concern is the resource-bound gate's.
func TestUnwindowedSpansScans_NoWindowDefers(t *testing.T) {
	t.Parallel()
	// recursiveArmUnwindowed with every Timestamp predicate removed.
	noWindow := strings.ReplaceAll(recursiveArmUnwindowed,
		"(`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000)) AND ", "")
	noWindow = strings.ReplaceAll(noWindow,
		" WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000))", "")
	if strings.Contains(noWindow, spansscan.RequestWindowBound) {
		t.Fatalf("test setup: expected no window bound, but found %q", spansscan.RequestWindowBound)
	}
	if strings.Contains(noWindow, "toDateTime64") {
		t.Fatalf("test setup: expected no toDateTime64 window either")
	}
	if n := len(spansscan.UnwindowedSpansScans(noWindow, spansTable)); n != 0 {
		t.Fatalf("window-less query: got %d finding(s), want 0 (must defer to the resource-bound gate)", n)
	}
}

// TestUnwindowedSpansScans_Defers pins the trivial defer cases.
func TestUnwindowedSpansScans_Defers(t *testing.T) {
	t.Parallel()
	if n := len(spansscan.UnwindowedSpansScans(recursiveArmUnwindowed, "")); n != 0 {
		t.Fatalf("empty spansTable must defer, got %d", n)
	}
	if n := len(spansscan.UnwindowedSpansScans("   ", spansTable)); n != 0 {
		t.Fatalf("blank sql must defer, got %d", n)
	}
}
