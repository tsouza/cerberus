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
// input is a bare Scan), the drain filters the input's rows from outside.
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
