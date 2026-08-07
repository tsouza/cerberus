package chsql

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// A PromQL rewrite that replaces ONE column of its input — `label_replace` /
// `label_join` swapping Attributes, the instant-fn / clamp / scalar-arithmetic
// family swapping Value — has to forward every OTHER column its input
// publishes. Which columns those are is a property of the input's row shape,
// and the shape a range-mode `rate()` carries depends on which lowering
// strategy is wired: the arrayJoin fan-out builds a chplan.RangeWindow, the
// ts_grid_range strategy builds a chplan.RangeWindowNative with the IDENTICAL
// row shape.
//
// Both forwarders used to classify by asserting the fan-out node kind, which
// answers "is this a *RangeWindow" rather than "what does my input expose". On
// the native path the assertion missed and both fell through to the canonical
// Sample-row branch. For the Attributes half that is a hard failure —
// ClickHouse rejects the emitted statement outright:
//
//	Code: 47. DB::Exception: Unknown expression identifier `MetricName` in scope
//
// for `label_replace(rate(m[1m]), …)`, a live 502 on any Grafana panel doing a
// label rewrite over rate() with ts_grid_range on. The Value half is quieter:
// its canonical branch SYNTHESISES the name (`'' AS MetricName`) rather than
// referencing one and forwards the anchor under the timestamp column's name,
// so it emits valid SQL and returns correct points. What it loses is
// substitutability — the two strategies publish different columns for one row
// shape, which is exactly the property chplan.RangeWindowNative's doc comment
// claims — and that divergence is what turns the next consumer to read
// chplan.RangeWindowAnchorColumn off a plan root into the same 502.
//
// These tests therefore assert on the OUTERMOST SELECT list of the emitted
// statement — the column set that actually breaks — for both strategies at
// once. A golden-text pin would not do: the goldens are regenerated, and a
// regenerated golden accepts the wrong column set. The fan-out rows are the
// control that pins the pre-existing behaviour the derivation must preserve.

// nativeRowShapeLowerers is the boot-wired strategy table that turns the
// ts_grid_range lowering on for rate, exactly as cmd/cerberus wires it when the
// feature resolves enabled.
func nativeRowShapeLowerers() promql.RangeLowerers {
	return promql.RangeLowerers{Rate: promql.NativeRateLowerer{Fallback: promql.FanoutRateLowerer{}}}
}

// emitNativeRowShape lowers q in range mode with the supplied strategy table
// and emits it, returning both the plan (so the caller can prove which node
// kind it actually got) and the SQL. It shares lowerNativeScanBound's pinned
// grid deliberately: a native node is only built when Step > 0 with Start and
// End pinned, and one definition of "a query_range grid" per package keeps the
// eligibility conditions from drifting apart between its tests.
func emitNativeRowShape(t *testing.T, q string, lowerers promql.RangeLowerers) (chplan.Node, string) {
	t.Helper()

	plan := lowerNativeScanBound(t, q, lowerers)
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit %q: %v", q, err)
	}
	return plan, sql
}

// outerSelectList returns the SELECT list of the OUTERMOST statement in sql —
// the text between the leading `SELECT ` and its own `FROM`, skipping any FROM
// that belongs to a nested subquery.
//
// Depth tracking is what makes it the outermost one: the select list itself is
// full of parenthesised function calls, and several of those (a lambda over a
// mapFilter, an IN-subquery) contain the token FROM.
func outerSelectList(t *testing.T, sql string) string {
	t.Helper()

	const selectKeyword = "SELECT "
	if !strings.HasPrefix(sql, selectKeyword) {
		t.Fatalf("emitted SQL does not start with %q: %s", selectKeyword, sql)
	}
	depth := 0
	for i := len(selectKeyword); i < len(sql); i++ {
		switch sql[i] {
		case '(':
			depth++
		case ')':
			depth--
		case 'F':
			if depth == 0 && strings.HasPrefix(sql[i:], "FROM ") {
				return sql[len(selectKeyword):i]
			}
		}
	}
	t.Fatalf("no top-level FROM in emitted SQL: %s", sql)
	return ""
}

// namesColumn reports whether a SELECT list mentions col as a backtick-quoted
// identifier, in EITHER position the emitter can put one: as a reference to an
// input column, or as the output alias of a synthesised expression. The
// quoting is what separates both of those from an incidental substring.
//
// Not distinguishing the two is the point. Over a grid-window input a
// reference is the ClickHouse code 47, and an alias is a column that row shape
// must not publish at all — both are the canonical branch having run where the
// windowed one should have.
func namesColumn(selectList, col string) bool {
	return strings.Contains(selectList, "`"+col+"`")
}

// nativeRowShapeRange is the matrix window every case below reads over. It is
// shorter than the grid span so the native eligibility check (Step > 0, Start
// and End pinned) is the only thing deciding the strategy.
const nativeRowShapeRange = "1m"

// nativeRowShapeCase is one single-column rewrite over a range-mode rate().
type nativeRowShapeCase struct {
	name  string
	query string
}

// nativeRowShapeCases covers both forwarders and every family that reaches
// them: the two label rewrites go through promql.projectAttributesOverInner,
// and the instant-fn, clamp and scalar-arithmetic families each reach
// projectValueOverInner by a different call site.
var nativeRowShapeCases = []nativeRowShapeCase{
	{
		name:  "label_replace",
		query: `label_replace(rate(demo_cpu_usage_seconds_total[` + nativeRowShapeRange + `]), "core", "$1", "mode", "(.*)")`,
	},
	{
		name:  "label_join",
		query: `label_join(rate(demo_cpu_usage_seconds_total[` + nativeRowShapeRange + `]), "core", "-", "mode", "cpu")`,
	},
	{
		name:  "instant_fn",
		query: `abs(rate(demo_cpu_usage_seconds_total[` + nativeRowShapeRange + `]))`,
	},
	{
		name:  "clamp",
		query: `clamp_max(rate(demo_cpu_usage_seconds_total[` + nativeRowShapeRange + `]), 1)`,
	},
	{
		name:  "scalar_arithmetic",
		query: `100 * rate(demo_cpu_usage_seconds_total[` + nativeRowShapeRange + `])`,
	},
}

// assertGridWindowColumns fails unless the outermost SELECT list publishes
// exactly what a grid-window row shape carries: no MetricName, and both names
// the anchor travels under plus the Attributes and Value the rewrite operates
// on.
func assertGridWindowColumns(t *testing.T, list, strategy string) {
	t.Helper()

	s := schema.DefaultOTelMetrics()
	if namesColumn(list, s.MetricNameColumn) {
		t.Errorf("outer SELECT names %s over a %s grid window, whose scope never exposes it: %s",
			s.MetricNameColumn, strategy, list)
	}
	for _, col := range []string{
		chplan.RangeWindowAnchorColumn,
		s.TimestampColumn,
		s.AttributesColumn,
		s.ValueColumn,
	} {
		if !namesColumn(list, col) {
			t.Errorf("outer SELECT drops %s, which the %s grid-window row shape publishes: %s",
				col, strategy, list)
		}
	}
}

// TestNativeRowShapeForwardsGridColumns pins the emitted column set of a
// single-column rewrite over a range-mode rate() on the ts_grid_range path.
//
// The rewrite sits on a chplan.RangeWindowNative, whose scope publishes
// (Attributes, anchor_ts, TimeUnix, Value) and NO MetricName. Naming the
// missing column is the 502; dropping either timestamp column empties the
// matrix response.
func TestNativeRowShapeForwardsGridColumns(t *testing.T) {
	for _, tc := range nativeRowShapeCases {
		t.Run(tc.name, func(t *testing.T) {
			plan, sql := emitNativeRowShape(t, tc.query, nativeRowShapeLowerers())
			// Without this the test proves nothing: a rewrite that fell back
			// to the fan-out lowering would pass on the fan-out's own branch.
			requireNativeNodeCount(t, plan, 1)

			assertGridWindowColumns(t, outerSelectList(t, sql), "native")
		})
	}
}

// TestFanoutRowShapeForwardsGridColumns is the control: the same rewrites over
// the arrayJoin fan-out matrix RangeWindow must publish the same column set.
// Deriving the shape instead of asserting the node kind is only correct if it
// leaves the shape it already handled untouched.
func TestFanoutRowShapeForwardsGridColumns(t *testing.T) {
	for _, tc := range nativeRowShapeCases {
		t.Run(tc.name, func(t *testing.T) {
			plan, sql := emitNativeRowShape(t, tc.query, promql.RangeLowerers{})
			requireNativeNodeCount(t, plan, 0)

			assertGridWindowColumns(t, outerSelectList(t, sql), "fan-out")
		})
	}
}
