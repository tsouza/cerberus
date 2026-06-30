package fanout

import (
	"strings"
	"testing"
)

// These fixtures encode the partition-pruning ground truth validated
// against prod ClickHouse: otel_traces is `PARTITION BY toDate(Timestamp)`,
// so ONLY a Timestamp range sitting directly on a physical scan prunes
// partitions. A recursive (`WITH RECURSIVE`) arm or a pre-`TraceId IN`
// `GROUP BY` cannot have a window pushed below it by CH, so an unwindowed
// scan there reads the whole table — the traces-OOM class. A windowed
// `TraceId IN (<seed>)` is INERT for pruning. The negative cases below are
// load-bearing: they pin the confirmed-FINE shapes (matrix-family
// pass-through wrapper, windowed recursive arm, plain windowed scan) so
// the backstop never false-rejects them.

// recursiveArmWindowed is the correct, windowed structural/nested-set
// shape: the recursive STEP arm carries the request window directly on its
// `otel_traces AS t` scan, alongside the (inert-for-pruning) `TraceId IN`
// seed. The anchor's seed leaf is a `SELECT *` pass-through wrapper.
const recursiveArmWindowed = "WITH RECURSIVE _struct_closure_1 AS (" +
	"SELECT DISTINCT `TraceId`, `SpanId`, `ParentSpanId`, 0 AS _depth " +
	"FROM (SELECT * FROM `otel_traces` WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000)) AND (`Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000)) AND (`ResourceAttributes`[?] = ?)) AS _seed " +
	"UNION ALL " +
	"SELECT DISTINCT t.`TraceId`, t.`SpanId`, t.`ParentSpanId`, c._depth + 1 " +
	"FROM `otel_traces` AS t INNER JOIN _struct_closure_1 AS c ON t.`TraceId` = c.`TraceId` AND t.`ParentSpanId` = c.`SpanId` " +
	"WHERE c._depth < 128 AND t.`TraceId` IN (SELECT `TraceId` FROM (SELECT * FROM `otel_traces` WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000)) AND (`Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000))) AS _seed_ids) " +
	"AND `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) AND `Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000)" +
	") SELECT DISTINCT `TraceId`, `SpanId` FROM _struct_closure_1 WHERE _depth > 0"

// recursiveArmUnwindowed is the regression: same windowed query (the seed
// still carries `fromUnixTimestamp64Nano(`), but the recursive STEP arm
// LOST its co-scope Timestamp push — its top-level WHERE has only the
// depth cap and the inert `TraceId IN` seed.
const recursiveArmUnwindowed = "WITH RECURSIVE _struct_closure_1 AS (" +
	"SELECT DISTINCT `TraceId`, `SpanId`, `ParentSpanId`, 0 AS _depth " +
	"FROM (SELECT * FROM `otel_traces` WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000)) AND (`Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000)) AND (`ResourceAttributes`[?] = ?)) AS _seed " +
	"UNION ALL " +
	"SELECT DISTINCT t.`TraceId`, t.`SpanId`, t.`ParentSpanId`, c._depth + 1 " +
	"FROM `otel_traces` AS t INNER JOIN _struct_closure_1 AS c ON t.`TraceId` = c.`TraceId` AND t.`ParentSpanId` = c.`SpanId` " +
	"WHERE c._depth < 128 AND t.`TraceId` IN (SELECT `TraceId` FROM (SELECT * FROM `otel_traces` WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000)) AND (`Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000))) AS _seed_ids)" +
	") SELECT DISTINCT `TraceId`, `SpanId` FROM _struct_closure_1 WHERE _depth > 0"

// groupByRootLookupUnwindowed is the metrics-compare root-enrichment
// regression: `FROM otel_traces … GROUP BY TraceId` with only an inert
// `TraceId IN (<windowed seed>)` — the GROUP BY runs over the whole table
// before the IN can filter. The query is windowed (the seed renders
// `fromUnixTimestamp64Nano(`).
const groupByRootLookupUnwindowed = "SELECT `TraceId`, any(`SpanName`) AS `__root_name` " +
	"FROM `otel_traces` " +
	"WHERE `ParentSpanId` = '' AND `TraceId` IN (SELECT `TraceId` FROM `otel_traces` WHERE `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) GROUP BY `TraceId`) " +
	"GROUP BY `TraceId`"

// groupByRootLookupWindowed is the fixed shape: the request window sits
// directly on the root-lookup scan, co-scope with its GROUP BY.
const groupByRootLookupWindowed = "SELECT `TraceId`, any(`SpanName`) AS `__root_name` " +
	"FROM `otel_traces` " +
	"WHERE `ParentSpanId` = '' AND `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) AND `Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000) AND `TraceId` IN (SELECT `TraceId` FROM `otel_traces` WHERE `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) GROUP BY `TraceId`) " +
	"GROUP BY `TraceId`"

// matrixFamilyWrapper is the CONFIRMED-FINE range-window matrix shape: a
// plain pass-through `(SELECT * FROM otel_traces WHERE …)` whose Timestamp
// window sits on the enclosing wrapper — CH pushes it into the scan
// (validated: 5.4M rows / 0.54s). The outer GROUP BY must NOT cause a
// flag, because the scan is a `SELECT *` pass-through.
const matrixFamilyWrapper = "SELECT `anchor_ts`, toFloat64(sum(in_window)) / 300 AS `Value` " +
	"FROM (SELECT arrayJoin(range(0, 61)) AS `anchor_ts`, 1 AS `in_window` " +
	"FROM (SELECT * FROM `otel_traces` WHERE (`ParentSpanId` = ?)) WHERE `Timestamp` > fromUnixTimestamp64Nano(1782571392000000000) AND `Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000)) " +
	"GROUP BY `anchor_ts`"

// plainWindowedScan is a direct (non-pass-through) scan that carries its
// own co-scope window — the leaf-bound search shape. No recursion, no
// GROUP BY: it must not be flagged.
const plainWindowedScan = "SELECT `TraceId`, `SpanId` FROM `otel_traces` " +
	"WHERE `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) AND `Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000) " +
	"ORDER BY `Timestamp` DESC LIMIT 100"

func countUnwindowed(vs []Violation) int {
	n := 0
	for _, v := range vs {
		if v.Rule == RuleUnwindowedSpansScan {
			n++
		}
	}
	return n
}

func TestLintUnwindowedSpansScan(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		sql  string
		want int
	}{
		// POSITIVE — these read the whole table.
		{"recursive_arm_unwindowed", recursiveArmUnwindowed, 1},
		{"group_by_root_lookup_unwindowed", groupByRootLookupUnwindowed, 1},

		// NEGATIVE — confirmed-FINE shapes; a flag here is a false reject.
		{"recursive_arm_windowed", recursiveArmWindowed, 0},
		{"group_by_root_lookup_windowed", groupByRootLookupWindowed, 0},
		{"matrix_family_wrapper", matrixFamilyWrapper, 0},
		{"plain_windowed_scan", plainWindowedScan, 0},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := countUnwindowed(lintUnwindowedSpansScan(tc.sql))
			if got != tc.want {
				t.Fatalf("lintUnwindowedSpansScan(%s): got %d violation(s), want %d\nSQL:\n%s",
					tc.name, got, tc.want, tc.sql)
			}
		})
	}
}

// TestLintUnwindowedSpansScan_NoWindowDefers pins the precondition: when
// the statement carries NO request window (`fromUnixTimestamp64Nano(`
// absent), there is nothing to push down and the rule stays silent — the
// unbounded-query concern is the resource-bound gate's, not this
// partition-pruning backstop's. Without this gate the rule would flag the
// no-window structural / nested-set emitter goldens that legitimately ship
// windowless (no time bounds in the query).
func TestLintUnwindowedSpansScan_NoWindowDefers(t *testing.T) {
	t.Parallel()

	// The recursive-arm-unwindowed shape with every window bound removed:
	// no `fromUnixTimestamp64Nano(` anywhere.
	noWindow := strings.ReplaceAll(recursiveArmUnwindowed,
		" WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000)) AND (`Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000)) AND (`ResourceAttributes`[?] = ?)",
		" WHERE (`ResourceAttributes`[?] = ?)")
	noWindow = strings.ReplaceAll(noWindow,
		" WHERE (`Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000)) AND (`Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000))",
		"")

	if strings.Contains(noWindow, requestWindowBound) {
		t.Fatalf("test setup: expected no window bound in fixture, but found %q", requestWindowBound)
	}
	if n := countUnwindowed(lintUnwindowedSpansScan(noWindow)); n != 0 {
		t.Fatalf("window-less query: got %d violation(s), want 0 (must defer to the resource-bound gate)", n)
	}
}
