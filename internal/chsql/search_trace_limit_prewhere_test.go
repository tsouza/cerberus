package chsql

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// searchTraceLimitSpanScan is a closed spans Scan that reads the wide
// ResourceAttributes column, so a Filter over it gets the emitter's PREWHERE.
func searchTraceLimitSpanScan() *chplan.Scan {
	return &chplan.Scan{
		Table:   "otel_traces",
		Columns: []string{"Duration", "ParentSpanId", "ResourceAttributes", "SpanId", "SpanKind", "SpanName", "Timestamp", "TraceId"},
		Roles:   []chplan.Column{{Name: "TraceId", Role: chplan.RoleTraceID}, {Name: "Timestamp", Role: chplan.RoleTimestamp}},
	}
}

// searchTraceLimitServerSpans filters searchTraceLimitSpanScan to unset
// Server spans: the emitter keeps the first conjunct in PREWHERE and the
// second in WHERE.
func searchTraceLimitServerSpans() chplan.Node {
	return &chplan.Filter{
		Input: searchTraceLimitSpanScan(),
		Predicate: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "SpanKind"}, Right: &chplan.LitString{V: "Server"}},
			Right: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "StatusCode"}, Right: &chplan.LitString{V: "Unset"}},
		},
	}
}

// searchTraceLimitDrainPrewhere is the drain's PREWHERE when it tests the
// chosen traces there: the input's own conjunct first, then the membership.
const searchTraceLimitDrainPrewhere = "FROM `otel_traces` PREWHERE (`SpanKind` = ?) AND `TraceId` GLOBAL IN (SELECT `TraceId` FROM ("

// TestEmitSearchTraceLimit_DrainTestsTheChosenTracesInPrewhere pins where the
// drain tests a row's trace against the chosen traces. Over a Filter(Scan)
// the emitter splits itself, the test joins the Filter's own PREWHERE after
// its conjuncts, so the rest of the row is read only for the chosen traces'
// rows: the drain is the Filter(Scan) statement, not a SELECT over it. Where
// the emitter writes no PREWHERE of its own (the server places it, or the
// input is a bare Scan), the drain filters the input's rows from outside,
// whatever SearchDrainScalarTraces and SearchDrainTraceBounds ask for.
func TestEmitSearchTraceLimit_DrainTestsTheChosenTracesInPrewhere(t *testing.T) {
	t.Parallel()

	own := emitUnder(t, SchemaStrategies{}, &chplan.SearchTraceLimit{Input: searchTraceLimitServerSpans(), TraceLimit: 7})
	if !strings.HasPrefix(own, "SELECT `Duration`, ") || !strings.Contains(own, searchTraceLimitDrainPrewhere) || strings.Contains(own, "s.*") {
		t.Fatalf("zero strategies: want the drain to be the Filter(Scan) statement, testing the chosen traces after its own PREWHERE conjunct:\n%s", own)
	}
	if ranking := own[strings.Index(own, searchTraceLimitDrainPrewhere)+len(searchTraceLimitDrainPrewhere):]; !strings.Contains(ranking, "PREWHERE (`SpanKind` = ?)") {
		t.Fatalf("zero strategies: want the ranking to read the same Server spans:\n%s", own)
	}

	for _, tc := range []struct {
		name       string
		strategies SchemaStrategies
		input      chplan.Node
	}{
		{"prewhere by server", SchemaStrategies{PrewhereByServer: true}, searchTraceLimitServerSpans()},
		{"bare scan", SchemaStrategies{}, searchTraceLimitSpanScan()},
		{"scalar traces, prewhere by server", SchemaStrategies{PrewhereByServer: true, SearchDrainScalarTraces: true}, searchTraceLimitServerSpans()},
		{"scalar traces, bare scan", SchemaStrategies{SearchDrainScalarTraces: true}, searchTraceLimitSpanScan()},
		{"trace bounds, prewhere by server", SchemaStrategies{PrewhereByServer: true, SearchDrainTraceBounds: true}, searchTraceLimitServerSpans()},
		{"trace bounds, bare scan", SchemaStrategies{SearchDrainTraceBounds: true}, searchTraceLimitSpanScan()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sql := emitUnder(t, tc.strategies, &chplan.SearchTraceLimit{Input: tc.input, TraceLimit: 7})
			if !strings.HasPrefix(sql, "SELECT s.* FROM (") || !strings.Contains(sql, ") AS s WHERE `TraceId` GLOBAL IN (") {
				t.Fatalf("want the drain to filter the input's rows from outside:\n%s", sql)
			}
		})
	}
}

// The scalar drains' statement pieces: the scalar the chosen traces are
// bound to, alone (scalarTracesDrain) or with their rows' earliest and latest
// timestamp (traceBoundsDrain), and the PREWHERE that tests it after the input's own
// conjunct.
const (
	searchTraceLimitScalarStart = "WITH (SELECT (groupArray(`TraceId`), min(`_cerberus_search_trace_start`), max(`_cerberus_search_trace_end`)) " +
		"FROM (SELECT `TraceId`, min(`Timestamp`) AS `_cerberus_search_trace_start`, max(`Timestamp`) AS `_cerberus_search_trace_end` FROM (SELECT "
	searchTraceLimitScalarIDs     = "WITH (SELECT groupArray(`TraceId`) FROM (SELECT `TraceId` FROM (SELECT "
	searchTraceLimitScalarRanking = "GROUP BY `TraceId` ORDER BY min(`Timestamp`) DESC, `TraceId` LIMIT 7)) AS _cerberus_search_traces SELECT `Duration`, "
	searchTraceLimitPrewhereStart = "FROM `otel_traces` PREWHERE (`SpanKind` = ?) AND `Timestamp` >= _cerberus_search_traces.2 AND `Timestamp` <= _cerberus_search_traces.3 " +
		"AND `TraceId` IN (SELECT arrayJoin(_cerberus_search_traces.1)) AND indexHint(has(_cerberus_search_traces.1, `TraceId`)) WHERE (`StatusCode` = ?)"
	searchTraceLimitPrewhereIDs = "FROM `otel_traces` PREWHERE (`SpanKind` = ?) AND `TraceId` IN (SELECT arrayJoin(_cerberus_search_traces)) " +
		"AND indexHint(has(_cerberus_search_traces, `TraceId`)) WHERE (`StatusCode` = ?)"
)

// TestEmitSearchTraceLimit_ScalarDrainsTestABoundScalarInPrewhere pins the
// drains SearchDrainScalarTraces and SearchDrainTraceBounds ask for over a
// Filter(Scan) the emitter splits itself: the chosen traces bound once as a
// scalar ahead of the Filter(Scan) statement, and appended to its PREWHERE
// after its own conjunct the trace id IN a set built from that scalar, then
// has() against the scalar inside indexHint, which the server only reads for
// skipping granules. There is no GLOBAL IN set over a second ranking, no outer
// filter, and no has() a row is tested with: has() per row scans every chosen
// id, a cost that grows with the limit. SearchDrainTraceBounds applies to a search with no Rank
// only, whatever SearchDrainScalarTraces asks for: its scalar also carries
// the chosen traces' first and last timestamp, and the PREWHERE keeps the
// rows between them before it tests the trace id. A search with a Rank, whose window is
// narrower than the drain's, carries the trace ids alone, and only under
// SearchDrainScalarTraces.
func TestEmitSearchTraceLimit_ScalarDrainsTestABoundScalarInPrewhere(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name             string
		strategies       SchemaStrategies
		rank             chplan.Node
		scalar, prewhere string
	}{
		{"scalar traces, no rank", SchemaStrategies{SearchDrainScalarTraces: true}, nil, searchTraceLimitScalarIDs, searchTraceLimitPrewhereIDs},
		{"scalar traces, rank", SchemaStrategies{SearchDrainScalarTraces: true}, searchTraceLimitServerSpans(), searchTraceLimitScalarIDs, searchTraceLimitPrewhereIDs},
		{"trace bounds, no rank", SchemaStrategies{SearchDrainTraceBounds: true}, nil, searchTraceLimitScalarStart, searchTraceLimitPrewhereStart},
		{"both, no rank", SchemaStrategies{SearchDrainScalarTraces: true, SearchDrainTraceBounds: true}, nil, searchTraceLimitScalarStart, searchTraceLimitPrewhereStart},
		{"both, rank", SchemaStrategies{SearchDrainScalarTraces: true, SearchDrainTraceBounds: true}, searchTraceLimitServerSpans(), searchTraceLimitScalarIDs, searchTraceLimitPrewhereIDs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sql := emitUnder(t, tc.strategies, &chplan.SearchTraceLimit{Input: searchTraceLimitServerSpans(), Rank: tc.rank, TraceLimit: 7})
			if !strings.HasPrefix(sql, tc.scalar) {
				t.Fatalf("want the drain to open with the chosen traces bound as a scalar:\n%s", sql)
			}
			if !strings.Contains(sql, searchTraceLimitScalarRanking) {
				t.Fatalf("want the scalar to rank the newest traces and the Filter(Scan) statement to follow it:\n%s", sql)
			}
			if !strings.HasSuffix(sql, tc.prewhere) {
				t.Fatalf("want the Filter(Scan) statement to test the scalar in its PREWHERE after its own conjunct:\n%s", sql)
			}
			if strings.Contains(sql, "GLOBAL IN") || strings.Contains(sql, "s.*") {
				t.Fatalf("want neither a GLOBAL IN set nor an outer filter:\n%s", sql)
			}
			if strings.Count(sql, "has(") != strings.Count(sql, "indexHint(has(") {
				t.Fatalf("want has() over the chosen traces only inside indexHint, never tested per row:\n%s", sql)
			}
		})
	}

	// A search with a Rank under SearchDrainTraceBounds alone tests the chosen
	// traces as the GLOBAL IN set in the Filter(Scan) statement's PREWHERE.
	sql := emitUnder(t, SchemaStrategies{SearchDrainTraceBounds: true},
		&chplan.SearchTraceLimit{Input: searchTraceLimitServerSpans(), Rank: searchTraceLimitServerSpans(), TraceLimit: 7})
	if !strings.HasPrefix(sql, "SELECT `Duration`, ") || !strings.Contains(sql, searchTraceLimitDrainPrewhere) || strings.Contains(sql, searchTracesAlias) {
		t.Fatalf("trace bounds with a rank: want the GLOBAL IN set in the Filter(Scan) statement's PREWHERE:\n%s", sql)
	}
}
