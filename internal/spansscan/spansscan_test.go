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

// matrixFamilyWrapper is the CONFIRMED-FINE range-window matrix shape: a derived
// table `(SELECT * FROM otel_traces WHERE …)` whose Timestamp window sits on the
// enclosing wrapper — CH pushes it into the scan. The outer GROUP BY must NOT
// cause a flag, because the scan sits in a derived table. Renders its window as
// toDateTime64, so it also pins that the broadened precondition does not
// over-fire on the derived-table shape.
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

// otherTableRecursiveUnwindowed is the exact shape of a POSITIVE fixture —
// windowed statement, `WITH RECURSIVE`, a windowless physical scan in the step
// arm — except the table scanned is not the spans table. It must not flag, and
// it is what proves the substring prefilter in UnwindowedSpansScans is keyed on
// the table rather than on the danger markers that surround it.
const otherTableRecursiveUnwindowed = "WITH RECURSIVE _closure AS (" +
	"SELECT DISTINCT `TraceId`, `SpanId`, `ParentSpanId`, 0 AS _depth " +
	"FROM (SELECT * FROM `otel_logs` WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000))) AS _seed " +
	"UNION ALL " +
	"SELECT DISTINCT t.`TraceId`, t.`SpanId`, t.`ParentSpanId`, c._depth + 1 " +
	"FROM `otel_logs` AS t INNER JOIN _closure AS c ON t.`TraceId` = c.`TraceId` " +
	"WHERE c._depth < 128" +
	") SELECT DISTINCT `TraceId` FROM _closure WHERE _depth > 0"

// TestUnwindowedSpansScansPrefilterCannotSuppressAFinding pins the necessary
// condition the fast path relies on: every finding is anchored on a
// `FROM <spans table>` regexp match, so a statement that does not contain the
// table name as a substring cannot produce one. The fast path answers those
// without touching the regexp engine — worth ~37% of request CPU on a broad
// metadata probe, where the emit chokepoint scans large metrics statements that
// never name the spans table.
//
// Asserting the property over the POSITIVE corpus (rather than trusting the
// reasoning) is what keeps the two in step: a future fixture that flags without
// naming the table would mean the prefilter silently suppresses a real finding,
// and that is the one way this optimisation could ever go wrong.
func TestUnwindowedSpansScansPrefilterCannotSuppressAFinding(t *testing.T) {
	t.Parallel()

	for _, tc := range positiveFixtures() {
		if !strings.Contains(tc.sql, spansTable) {
			t.Errorf("positive fixture %q flags a finding but never names %q, so the "+
				"substring prefilter in UnwindowedSpansScans would return nil before "+
				"the matcher ran — the fast path must be widened or this fixture is "+
				"reaching the matcher by a route the prefilter does not model.\nSQL:\n%s",
				tc.name, spansTable, tc.sql)
		}
	}

	if got := len(spansscan.UnwindowedSpansScans(otherTableRecursiveUnwindowed, spansTable)); got != 0 {
		t.Errorf("a windowless recursive scan of a NON-spans table produced %d finding(s), "+
			"want 0 — the matcher is keying on the recursive/window markers rather than "+
			"on the spans table itself", got)
	}
}

// positiveFixtures is the flagging half of the corpus, shared by
// TestUnwindowedSpansScans and the prefilter pin above so neither can drift
// away from the other.
func positiveFixtures() []struct {
	name string
	sql  string
	want int
} {
	return []struct {
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
	}
}

func TestUnwindowedSpansScans(t *testing.T) {
	t.Parallel()

	cases := append(positiveFixtures(), []struct {
		name string
		sql  string
		want int
	}{
		// NEGATIVE — confirmed-FINE shapes; a flag here is a false reject.
		{"recursive_arm_windowed", recursiveArmWindowed, 0},
		{"group_by_root_lookup_windowed", groupByRootLookupWindowed, 0},
		{"matrix_family_wrapper", matrixFamilyWrapper, 0},
		{"plain_windowed_scan", plainWindowedScan, 0},
		{"non_spans_metrics", nonSpansMetrics, 0},
		{"other_table_recursive_unwindowed", otherTableRecursiveUnwindowed, 0},
	}...)

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

// emptyTableWouldFlag is windowed SQL whose GROUP-BY arm scans a bare
// `FROM “ ` (empty-ident table). With an empty spansTable the per-table regex
// compiles to exactly that "FROM “" shape, so the downstream matcher WOULD
// emit a finding here — proving the empty-spansTable guard is what suppresses
// it, not an incidental no-match. The sibling arm carries the request window so
// the precondition is armed; the bare-table arm is windowless under a GROUP BY.
const emptyTableWouldFlag = "SELECT a FROM `` GROUP BY a " +
	"UNION ALL " +
	"SELECT b FROM x WHERE `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000)"

// TestUnwindowedSpansScans_EmptyTableDefersEvenWhenMatchable pins the
// short-circuit in the entry guard `spansTable == "" || TrimSpace(sql) == ""`.
// An `||`→`&&` mutation would stop deferring when ONLY the table is empty, fall
// through, and (because the empty-table regex matches the bare `FROM “ `)
// report a finding. Asserting zero findings on this matchable input kills that
// mutant — the prior defer fixtures used SQL with no bare `FROM “ `, so the
// fall-through path produced nil anyway and never exercised the operator.
func TestUnwindowedSpansScans_EmptyTableDefersEvenWhenMatchable(t *testing.T) {
	t.Parallel()
	// Sanity: the same SQL under a real table name is genuinely flaggable, so
	// the zero below is the empty-table guard at work, not a dead input.
	if n := len(spansscan.UnwindowedSpansScans(
		strings.ReplaceAll(emptyTableWouldFlag, "FROM ``", "FROM `otel_traces`"), spansTable,
	)); n != 1 {
		t.Fatalf("test setup: substituting a real table must flag 1, got %d", n)
	}
	if n := len(spansscan.UnwindowedSpansScans(emptyTableWouldFlag, "")); n != 0 {
		t.Fatalf("empty spansTable must defer even when sql carries a bare `FROM ``` the empty-table regex matches, got %d", n)
	}
}

// windowedThenWindowless places a co-scope-WINDOWED spans scan (which hits the
// prune `continue`) at a LOWER offset than a windowless GROUP-BY spans scan
// (which must be flagged). The windowed arm is examined first; the flaggable
// one comes after the UNION ALL.
const windowedThenWindowless = "SELECT a FROM `otel_traces` " +
	"WHERE `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) " +
	"UNION ALL " +
	"SELECT b FROM `otel_traces` GROUP BY b"

// TestUnwindowedSpansScans_PruneSkipDoesNotHaltScan pins the `continue` in the
// co-scope-Timestamp prune branch. An INVERT_LOOPCTRL mutation (`continue` →
// `break`) would abandon the scan loop at the first already-pruned scan, so the
// later windowless GROUP-BY scan would never be examined and the finding lost.
// Asserting that the flagged scan is the SECOND (higher-offset) one proves the
// loop kept going past the pruned scan.
func TestUnwindowedSpansScans_PruneSkipDoesNotHaltScan(t *testing.T) {
	t.Parallel()
	findings := spansscan.UnwindowedSpansScans(windowedThenWindowless, spansTable)
	if len(findings) != 1 {
		t.Fatalf("windowed-then-windowless: got %d finding(s), want 1\nSQL:\n%s", len(findings), windowedThenWindowless)
	}
	windowlessScan := strings.LastIndex(windowedThenWindowless, "FROM `otel_traces`")
	firstScan := strings.Index(windowedThenWindowless, "FROM `otel_traces`")
	if windowlessScan <= firstScan {
		t.Fatalf("test setup: expected the windowless scan to follow the windowed one (first=%d, last=%d)", firstScan, windowlessScan)
	}
	if findings[0].Offset != windowlessScan {
		t.Fatalf("expected the finding at the windowless scan (offset %d), got %d — the loop must continue past the pruned scan, not break",
			windowlessScan, findings[0].Offset)
	}
}

// The bracketed-co-scope corpus (#1889). ClickHouse prunes on a Timestamp range
// sitting on the physical scan regardless of how the emitter brackets it, so the
// matcher's scope walk must read a bracketed conjunct as co-scope while still
// refusing to borrow a Timestamp that lives inside a nested SUBQUERY. The
// distinction is what a parenthesis OPENS (`SELECT`/`WITH` = another scan's
// scope), never how deeply it nests.

// recursiveArmBracketedWindow: the same windowed step arm as
// recursiveArmWindowed, with each window bound wrapped in its own brackets —
// the shape chsql renders whenever the window is one operand of a larger
// conjunction. Same physical scan, same pruning, so it must NOT be flagged.
const recursiveArmBracketedWindow = "WITH RECURSIVE _struct_closure_1 AS (" +
	"SELECT DISTINCT `TraceId`, `SpanId`, `ParentSpanId`, 0 AS _depth " +
	"FROM (SELECT * FROM `otel_traces` WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000)) AND (`ResourceAttributes`[?] = ?)) AS _seed " +
	"UNION ALL " +
	"SELECT DISTINCT t.`TraceId`, t.`SpanId`, t.`ParentSpanId`, c._depth + 1 " +
	"FROM `otel_traces` AS t INNER JOIN _struct_closure_1 AS c ON t.`TraceId` = c.`TraceId` AND t.`ParentSpanId` = c.`SpanId` " +
	"WHERE c._depth < 128 AND t.`TraceId` IN (SELECT `TraceId` FROM (SELECT * FROM `otel_traces` WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000))) AS _seed_ids) " +
	"AND ((`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000)) AND (`Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000)))" +
	") SELECT DISTINCT `TraceId`, `SpanId` FROM _struct_closure_1 WHERE _depth > 0"

// columnPrunedSeedInRecursiveBody is the #1889 production shape: the nested-set
// numbering CTE's `TraceId IN (<seed>)` list holds a union of three seed arms,
// and the optimizer's late-materialisation rewrite has replaced one arm's
// `SELECT *` projection with an explicit column list. That arm is a fully
// windowed physical scan carrying its own bracketed `Timestamp` bounds, and it
// is cleared by reading that bracketed WHERE as co-scope — independently of the
// derived-table exclusion, which also covers it. It must NOT be flagged.
const columnPrunedSeedInRecursiveBody = "WITH RECURSIVE _cerberus_ns_paths AS (" +
	"SELECT `TraceId`, `SpanId`, `ParentSpanId`, 0 AS `_depth` FROM `otel_traces` " +
	"WHERE `ParentSpanId` = '' AND `TraceId` IN (" +
	"(SELECT `TraceId` FROM (SELECT * FROM `otel_traces` WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000)) AND (`Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000)))) " +
	"UNION ALL " +
	"(SELECT `TraceId` FROM (SELECT `SpanId`, `Timestamp`, `TraceId` FROM `otel_traces` WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000)) AND (`Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000))))" +
	") AND `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) AND `Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000) " +
	"UNION ALL " +
	"SELECT t.`TraceId`, t.`SpanId`, t.`ParentSpanId`, c.`_depth` + 1 AS `_depth` " +
	"FROM `otel_traces` AS t INNER JOIN `_cerberus_ns_paths` AS c ON t.`TraceId` = c.`TraceId` AND t.`ParentSpanId` = c.`SpanId` " +
	"WHERE c._depth < 128 AND `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) AND `Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000)" +
	") SELECT `TraceId`, `SpanId` FROM _cerberus_ns_paths"

// windowBracketedBesideSubquery mixes both halves inside ONE bracketed group:
// the scan's real window sits next to a windowed `TraceId IN (<subquery>)` under
// the same brackets. The group is an expression group (it opens with a column,
// not `SELECT`), so the walk descends it and finds the real window, while the
// subquery nested one level further in is still dropped. Must NOT be flagged.
const windowBracketedBesideSubquery = "WITH RECURSIVE _closure AS (" +
	"SELECT `TraceId`, `SpanId`, 0 AS _depth FROM `otel_traces` WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) AND `TraceId` IN (SELECT `TraceId` FROM `otel_traces` WHERE `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000))) " +
	"UNION ALL " +
	"SELECT t.`TraceId`, t.`SpanId`, c._depth + 1 FROM `otel_traces` AS t INNER JOIN _closure AS c ON t.`TraceId` = c.`TraceId` " +
	"WHERE c._depth < 128 AND `Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000)" +
	") SELECT `TraceId` FROM _closure"

// bracketedSubqueryWindowNotBorrowed is the false-ACCEPT direction of the same
// change: the step arm's ONLY `Timestamp` comparison lives inside the bracketed
// `TraceId IN ((SELECT …) UNION ALL (SELECT …))` seed — a set-op group whose
// outermost bracket opens onto nested `SELECT`s. That window bounds the seed's
// scans, not this one, and an inert `TraceId IN` prunes no partitions, so the
// step arm reads the whole table and MUST still be flagged.
const bracketedSubqueryWindowNotBorrowed = "WITH RECURSIVE _closure AS (" +
	"SELECT `TraceId`, `SpanId`, 0 AS _depth FROM (SELECT * FROM `otel_traces` WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000))) AS _seed " +
	"UNION ALL " +
	"SELECT t.`TraceId`, t.`SpanId`, c._depth + 1 FROM `otel_traces` AS t INNER JOIN _closure AS c ON t.`TraceId` = c.`TraceId` " +
	"WHERE c._depth < 128 AND t.`TraceId` IN (" +
	"(SELECT `TraceId` FROM `otel_traces` WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000))) " +
	"UNION ALL " +
	"(SELECT `TraceId` FROM `otel_traces` WHERE (`Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000)))" +
	")" +
	") SELECT `TraceId` FROM _closure"

// TestUnwindowedSpansScans_BracketedCoScope pins both directions of the #1889
// scope rule on realistic emitted shapes.
func TestUnwindowedSpansScans_BracketedCoScope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sql  string
		want int
	}{
		{"recursive_arm_bracketed_window", recursiveArmBracketedWindow, 0},
		{"column_pruned_seed_in_recursive_body", columnPrunedSeedInRecursiveBody, 0},
		{"window_bracketed_beside_subquery", windowBracketedBesideSubquery, 0},
		{"bracketed_subquery_window_not_borrowed", bracketedSubqueryWindowNotBorrowed, 1},
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

// maxCoScopeBracketDepth is how many redundant bracket layers the invariance
// test wraps a co-scope predicate in. Anything beyond one layer is already past
// what any emitter renders; the extra layers exist so the property is checked
// against nesting depth rather than against the single shape that broke.
const maxCoScopeBracketDepth = 4

// TestUnwindowedSpansScans_BracketingDoesNotChangeVerdict is the class
// assertion. #1889 was not "one shape the matcher misreads" — it was the scope
// walk treating bracket DEPTH as a scope change, so any emitter choice that
// added a bracket around a co-scope predicate turned a fully pruned scan into a
// rejected one. Pinning one more fixture would have left the next bracketing
// free to regress, so the property itself is pinned: wrapping a scan's own
// predicate in redundant brackets must never change the verdict, at any depth,
// for a flagged statement or a clean one.
func TestUnwindowedSpansScans_BracketingDoesNotChangeVerdict(t *testing.T) {
	t.Parallel()
	// Both fixtures carry this exact co-scope conjunct on the recursive step
	// arm — present in the clean one, absent from the flagged one — so wrapping
	// it exercises the clean verdict, and wrapping the flagged fixture's own
	// depth-cap conjunct exercises the flagged verdict.
	const stepWindow = "`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) AND `Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000)"
	const depthCap = "c._depth < 128"

	cases := []struct {
		name     string
		sql      string
		conjunct string
		want     int
	}{
		{"clean_arm_window", recursiveArmWindowed, stepWindow, 0},
		{"flagged_arm_depth_cap", recursiveArmUnwindowed, depthCap, 1},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(tc.sql, tc.conjunct) {
				t.Fatalf("test setup: %s does not contain %q, so the bracketing below rewrites nothing", tc.name, tc.conjunct)
			}
			wrapped := tc.conjunct
			for depth := 0; depth <= maxCoScopeBracketDepth; depth++ {
				sql := strings.Replace(tc.sql, tc.conjunct, wrapped, 1)
				if got := len(spansscan.UnwindowedSpansScans(sql, spansTable)); got != tc.want {
					t.Fatalf("%s at bracket depth %d: got %d finding(s), want %d — bracketing a co-scope predicate changed the verdict\nSQL:\n%s",
						tc.name, depth, got, tc.want, sql)
				}
				wrapped = "(" + wrapped + ")"
			}
		})
	}
}

// The three fixtures below pin that pass-through membership is decided by the
// scan's TABLE POSITION and never by the wrapper's projection spelling (#1912).
// Each fails under the superseded `SELECT\s+\*\s+FROM` exclusion, and they fail
// in OPPOSITE directions, so neither a blanket exclusion nor a blanket flag can
// satisfy the set.

// columnListedDerivedTableSeed is a recursive anchor arm whose seed derived
// table carries an EXPLICIT COLUMN LIST and no window of its own; the window
// sits on the enclosing arm. The scan is a bare relational operand, so CH pushes
// that window in and prunes — it must NOT be flagged. Under the projection-
// keyed exclusion the wrapper stopped matching the moment late materialisation
// dropped the `*`, and this pruning-safe scan was reported as unbounded, taking
// a live query off the wire (#1889).
const columnListedDerivedTableSeed = "WITH RECURSIVE _closure AS (" +
	"SELECT `TraceId`, `SpanId`, 0 AS _depth " +
	"FROM (SELECT `TraceId`, `SpanId`, `ParentSpanId` FROM `otel_traces`) AS _seed " +
	"WHERE `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) AND `Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000) " +
	"UNION ALL " +
	"SELECT t.`TraceId`, t.`SpanId`, c._depth + 1 " +
	"FROM `otel_traces` AS t INNER JOIN _closure AS c ON t.`TraceId` = c.`TraceId` " +
	"WHERE c._depth < 128 AND `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000)" +
	") SELECT `TraceId`, `SpanId` FROM _closure"

// starProjectedRecursiveArm is the mirror image: a genuinely windowless scan
// that WEARS the `SELECT *` spelling while sitting as a recursive CTE arm, not
// as a derived table. Nothing encloses a CTE body that CH could push a window in
// from, so this closure reads the whole table and MUST be flagged. The
// projection-keyed exclusion skipped it on the strength of its `*` alone — the
// whole-table read the guard exists to stop (#1109).
const starProjectedRecursiveArm = "WITH RECURSIVE _closure AS (" +
	"SELECT * FROM `otel_traces` WHERE `ParentSpanId` = '' " +
	"UNION ALL " +
	"SELECT t.`TraceId`, t.`SpanId`, c._depth + 1 " +
	"FROM `otel_traces` AS t INNER JOIN _closure AS c ON t.`TraceId` = c.`TraceId` " +
	"WHERE c._depth < 128 AND `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000)" +
	") SELECT `TraceId`, `SpanId` FROM _closure"

// starProjectedRootGroupBy puts the same `SELECT *` spelling at the STATEMENT
// ROOT, above a `GROUP BY` and a windowed `TraceId IN (<seed>)`. The seed is
// inert for pruning and the root scan has no enclosing scope at all, so the
// aggregation runs over the whole table and it MUST be flagged — the GROUP BY
// leg of the same false negative.
const starProjectedRootGroupBy = "SELECT * FROM `otel_traces` " +
	"WHERE `TraceId` IN (SELECT `TraceId` FROM `otel_traces` WHERE `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000)) " +
	"GROUP BY `TraceId`"

// TestUnwindowedSpansScans_PassThroughIsPositional pins that the derived-table
// position, not the projection list, decides the pass-through exclusion.
func TestUnwindowedSpansScans_PassThroughIsPositional(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sql  string
		want int
	}{
		{"column_listed_derived_table_seed", columnListedDerivedTableSeed, 0},
		{"star_projected_recursive_arm", starProjectedRecursiveArm, 1},
		{"star_projected_root_group_by", starProjectedRootGroupBy, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := len(spansscan.UnwindowedSpansScans(tc.sql, spansTable))
			if got != tc.want {
				t.Fatalf("UnwindowedSpansScans(%s): got %d finding(s), want %d — pass-through membership must follow the scan's table position, not its projection spelling\nSQL:\n%s",
					tc.name, got, tc.want, tc.sql)
			}
		})
	}
}
