package testsql

import "testing"

func TestRewriteMapProjections(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bare attributes column",
			in:   "SELECT `MetricName`, `Attributes`, `TimeUnix`, `Value` FROM `otel_metrics_gauge`",
			want: "SELECT `MetricName`, toJSONString(`Attributes`) AS `Attributes`, `TimeUnix`, `Value` FROM `otel_metrics_gauge`",
		},
		{
			name: "aliased attributes column",
			in:   "SELECT `MetricName`, `Attributes` AS `Attributes`, `TimeUnix`, abs(`Value`) AS `Value` FROM `otel_metrics_gauge`",
			want: "SELECT `MetricName`, toJSONString(`Attributes`) AS `Attributes`, `TimeUnix`, abs(`Value`) AS `Value` FROM `otel_metrics_gauge`",
		},
		{
			name: "no map column",
			in:   "SELECT `MetricName`, `TimeUnix`, `Value` FROM `otel_metrics_gauge`",
			want: "SELECT `MetricName`, `TimeUnix`, `Value` FROM `otel_metrics_gauge`",
		},
		{
			// EmitQueryExemplars projects `attrs_arr[i] AS \`ExemplarAttributes\``
			// — a Map(LowCardinality(String),String) Subscript at the outer
			// SELECT. Without the toJSONString wrap chDB's parquet driver
			// panics scanning the column as a Go string.
			name: "exemplar attributes via subscript",
			in: "SELECT `MetricName`, `Attributes`, `ServiceName`, " +
				"ts[i] AS `Timestamp`, val[i] AS `Value`, " +
				"tid[i] AS `TraceID`, sid[i] AS `SpanID`, " +
				"attrs_arr[i] AS `ExemplarAttributes` FROM (SELECT 1) AS sub",
			want: "SELECT `MetricName`, toJSONString(`Attributes`) AS `Attributes`, " +
				"`ServiceName`, " +
				"ts[i] AS `Timestamp`, val[i] AS `Value`, " +
				"tid[i] AS `TraceID`, sid[i] AS `SpanID`, " +
				"toJSONString(attrs_arr[i]) AS `ExemplarAttributes` FROM (SELECT 1) AS sub",
		},
		{
			name: "non-select passthrough",
			in:   "INSERT INTO `otel_metrics_gauge` VALUES (1)",
			want: "INSERT INTO `otel_metrics_gauge` VALUES (1)",
		},
		{
			// A relational CTE head hides the outer SELECT from a prefix
			// match. Its body is a subquery, so its own Map columns stay raw.
			name: "relational with head",
			in: "WITH c AS (SELECT `Attributes` FROM `t`) " +
				"SELECT `MetricName`, `Attributes`, `TimeUnix`, `Value` FROM `c`",
			want: "WITH c AS (SELECT `Attributes` FROM `t`) " +
				"SELECT `MetricName`, toJSONString(`Attributes`) AS `Attributes`, `TimeUnix`, `Value` FROM `c`",
		},
		{
			// The scalar trace-scope binding chsql hoists onto the outermost
			// statement: `WITH (SELECT …) AS <alias>` heads the SELECT and its
			// body carries a nested `FROM (…)` whose parens must not be
			// mistaken for the end of the head.
			name: "scalar with head",
			in: "WITH (SELECT groupArray(`TraceId`) FROM (SELECT `TraceId` FROM `otel_traces`)) AS _cerberus_trace_scope " +
				"SELECT `MetricName`, `Attributes`, `TimeUnix`, `Value` FROM `otel_traces`",
			want: "WITH (SELECT groupArray(`TraceId`) FROM (SELECT `TraceId` FROM `otel_traces`)) AS _cerberus_trace_scope " +
				"SELECT `MetricName`, toJSONString(`Attributes`) AS `Attributes`, `TimeUnix`, `Value` FROM `otel_traces`",
		},
		{
			// `SELECT * FROM (<plan>)` — the emitter's binding wrapper. The
			// star hides the alias, so the wrap has to happen one level down;
			// the star then forwards the String column out under the same name.
			name: "star over subquery",
			in: "SELECT * FROM (SELECT `MetricName`, mapConcat(`ResourceAttributes`, map(?, `TraceId`)) AS `Attributes`, " +
				"`TimeUnix`, `Value` FROM `otel_traces`)",
			want: "SELECT * FROM (SELECT `MetricName`, toJSONString(mapConcat(`ResourceAttributes`, map(?, `TraceId`))) AS `Attributes`, " +
				"`TimeUnix`, `Value` FROM `otel_traces`)",
		},
		{
			// Both together — the exact shape chsql.Emit renders for the
			// Drilldown structure-tab search once the repeated top-N gates
			// collapse onto one binding.
			name: "scalar with head over star wrapper",
			in: "WITH (SELECT groupArray(`TraceId`) FROM (SELECT `TraceId` FROM `otel_traces`)) AS _cerberus_trace_scope " +
				"SELECT * FROM (SELECT `SpanName` AS `MetricName`, `ResourceAttributes` AS `Attributes`, " +
				"`Timestamp` AS `TimeUnix`, toFloat64(`Duration`) AS `Value` FROM `otel_traces` " +
				"WHERE has(_cerberus_trace_scope, `TraceId`))",
			want: "WITH (SELECT groupArray(`TraceId`) FROM (SELECT `TraceId` FROM `otel_traces`)) AS _cerberus_trace_scope " +
				"SELECT * FROM (SELECT `SpanName` AS `MetricName`, toJSONString(`ResourceAttributes`) AS `Attributes`, " +
				"`Timestamp` AS `TimeUnix`, toFloat64(`Duration`) AS `Value` FROM `otel_traces` " +
				"WHERE has(_cerberus_trace_scope, `TraceId`))",
		},
		{
			// A star the wrapper rule must NOT claim: the FROM operand does
			// not run to the end of the statement, so the columns the outer
			// clause sorts on are not the subquery's alone.
			name: "star over subquery with trailing clause",
			in:   "SELECT * FROM (SELECT `Attributes` FROM `t`) ORDER BY `Attributes`['h']",
			want: "SELECT * FROM (SELECT `Attributes` FROM `t`) ORDER BY `Attributes`['h']",
		},
		{
			// A star over a join takes columns from more than the first
			// relation; rewriting one arm on a guess would be wrong.
			name: "star over join untouched",
			in:   "SELECT * FROM (SELECT `Attributes` FROM `t`) AS L INNER JOIN (SELECT `Attributes` FROM `u`) AS R ON L.`k` = R.`k`",
			want: "SELECT * FROM (SELECT `Attributes` FROM `t`) AS L INNER JOIN (SELECT `Attributes` FROM `u`) AS R ON L.`k` = R.`k`",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RewriteMapProjections(tc.in)
			if got != tc.want {
				t.Errorf("rewrite mismatch\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestNestMapOrderBy pins both collision shapes NestMapOrderBy has to
// nest — the `sort_by_label` subscript and the route-A streaming-matrix
// `mapSort(...)` ordering — and, just as load-bearing, the shapes it must
// leave ALONE. The non-firing rows are the ones with teeth: broadening the
// detector from a `[`-subscript match to a bare-column-name match made a
// nested ORDER BY inside the FROM subquery (the topk/bottomk-over-subquery
// shape) look like the outer clause, and the "rest of the query" that
// followed it reliably mentioned `Attributes` somewhere further out, so the
// pass rewrote a query that never needed it into invalid SQL (ClickHouse
// NOT_AN_AGGREGATE). The depth-tracking split is what stops that.
func TestNestMapOrderBy(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			// sort_by_label: the original shape this pass was built for.
			name: "outer order by map subscript is nested",
			in:   "SELECT toJSONString(`Attributes`) AS `Attributes`, `Value` FROM (SELECT `Attributes`, `Value` FROM `t`) ORDER BY `Attributes`['h']",
			want: "SELECT toJSONString(`Attributes`) AS `Attributes`, `Value` FROM (SELECT * FROM (SELECT `Attributes`, `Value` FROM `t`) ORDER BY `Attributes`['h'])",
		},
		{
			// engine.rangeSeriesOrderer / prom.lang.RangeSeriesOrder: the Map
			// column is passed WHOLE to mapSort rather than subscripted, so a
			// `[`-only detector missed it and the raw Map reached the driver.
			name: "outer order by mapSort of map column is nested",
			in:   "SELECT `MetricName`, toJSONString(`Attributes`) AS `Attributes`, `TimeUnix`, `Value` FROM (SELECT `MetricName`, `Attributes`, `TimeUnix`, `Value` FROM `t`) ORDER BY mapSort(`Attributes`), `TimeUnix`",
			want: "SELECT `MetricName`, toJSONString(`Attributes`) AS `Attributes`, `TimeUnix`, `Value` FROM (SELECT * FROM (SELECT `MetricName`, `Attributes`, `TimeUnix`, `Value` FROM `t`) ORDER BY mapSort(`Attributes`), `TimeUnix`)",
		},
		{
			// The regression the depth-tracking split exists for: the ORDER BY
			// belongs to the INNER subquery, and the outer query has none at
			// all. Nothing may move, even though `Attributes` appears both
			// inside the subquery and in the outer GROUP BY.
			name: "nested order by inside from subquery is left alone",
			in:   "SELECT toJSONString(`Attributes`) AS `Attributes`, max(`Value`) AS `Value` FROM (SELECT `Attributes`, `Value` FROM `t` ORDER BY `Value` LIMIT 1 BY `anchor_ts`) GROUP BY `Attributes`",
			want: "SELECT toJSONString(`Attributes`) AS `Attributes`, max(`Value`) AS `Value` FROM (SELECT `Attributes`, `Value` FROM `t` ORDER BY `Value` LIMIT 1 BY `anchor_ts`) GROUP BY `Attributes`",
		},
		{
			name: "outer order by without a map column is left alone",
			in:   "SELECT `MetricName`, `Value` FROM (SELECT `MetricName`, `Value` FROM `t`) ORDER BY `Value` DESC",
			want: "SELECT `MetricName`, `Value` FROM (SELECT `MetricName`, `Value` FROM `t`) ORDER BY `Value` DESC",
		},
		{
			name: "no order by at all is left alone",
			in:   "SELECT toJSONString(`Attributes`) AS `Attributes` FROM (SELECT `Attributes` FROM `t`)",
			want: "SELECT toJSONString(`Attributes`) AS `Attributes` FROM (SELECT `Attributes` FROM `t`)",
		},
		{
			// A bare table scan is not the single-parenthesised-subquery FROM
			// this pass requires, so it declines rather than guessing.
			name: "bare table from is left alone",
			in:   "SELECT toJSONString(`Attributes`) AS `Attributes` FROM `t` ORDER BY mapSort(`Attributes`)",
			want: "SELECT toJSONString(`Attributes`) AS `Attributes` FROM `t` ORDER BY mapSort(`Attributes`)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NestMapOrderBy(tc.in); got != tc.want {
				t.Errorf("NestMapOrderBy:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestNestMapWhere pins the limit_ratio collision shape — a Filter
// fused directly atop the final self-aliased projection, so the WHERE
// clause and the toJSONString-wrapped Map alias sit in the same
// SELECT — and, just as load-bearing, the shapes it must leave alone.
func TestNestMapWhere(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			// limit_ratio: the shape #1986's parity enrolment exposed. The
			// hash predicate references both `Attributes` and `MetricName`
			// at the same level as their toJSONString-wrapped aliases.
			name: "outer where hash predicate over map column is nested",
			in: "SELECT `MetricName`, toJSONString(`Attributes`) AS `Attributes`, `TimeUnix`, `Value` FROM " +
				"(SELECT `MetricName`, `Attributes`, `TimeUnix`, `Value` FROM `t`) " +
				"WHERE ((toFloat64(xxHash64(mapConcat(`Attributes`, map('__name__', `MetricName`)))) / 1.0) < 0.5)",
			want: "SELECT `MetricName`, toJSONString(`Attributes`) AS `Attributes`, `TimeUnix`, `Value` FROM " +
				"(SELECT * FROM (SELECT `MetricName`, `Attributes`, `TimeUnix`, `Value` FROM `t`) " +
				"WHERE ((toFloat64(xxHash64(mapConcat(`Attributes`, map('__name__', `MetricName`)))) / 1.0) < 0.5))",
		},
		{
			// The mirror of NestMapOrderBy's own depth-tracking regression:
			// the WHERE belongs to the INNER subquery, and the outer query
			// has none at all. Nothing may move, even though `Attributes`
			// appears both inside the subquery and in the outer GROUP BY.
			name: "nested where inside from subquery is left alone",
			in:   "SELECT toJSONString(`Attributes`) AS `Attributes`, max(`Value`) AS `Value` FROM (SELECT `Attributes`, `Value` FROM `t` WHERE `Value` > 0) GROUP BY `Attributes`",
			want: "SELECT toJSONString(`Attributes`) AS `Attributes`, max(`Value`) AS `Value` FROM (SELECT `Attributes`, `Value` FROM `t` WHERE `Value` > 0) GROUP BY `Attributes`",
		},
		{
			name: "outer where without a map column is left alone",
			in:   "SELECT `MetricName`, `Value` FROM (SELECT `MetricName`, `Value` FROM `t`) WHERE `Value` > 0",
			want: "SELECT `MetricName`, `Value` FROM (SELECT `MetricName`, `Value` FROM `t`) WHERE `Value` > 0",
		},
		{
			name: "no where at all is left alone",
			in:   "SELECT toJSONString(`Attributes`) AS `Attributes` FROM (SELECT `Attributes` FROM `t`)",
			want: "SELECT toJSONString(`Attributes`) AS `Attributes` FROM (SELECT `Attributes` FROM `t`)",
		},
		{
			// A bare table scan is not the single-parenthesised-subquery FROM
			// this pass requires, so it declines rather than guessing.
			name: "bare table from is left alone",
			in:   "SELECT toJSONString(`Attributes`) AS `Attributes` FROM `t` WHERE mapContains(`Attributes`, 'k')",
			want: "SELECT toJSONString(`Attributes`) AS `Attributes` FROM `t` WHERE mapContains(`Attributes`, 'k')",
		},
		{
			// An ORDER BY trailer, not WHERE, is NestMapOrderBy's own shape —
			// this pass declines rather than double-handling it.
			name: "order by trailer is left to NestMapOrderBy",
			in:   "SELECT toJSONString(`Attributes`) AS `Attributes` FROM (SELECT `Attributes` FROM `t`) ORDER BY mapSort(`Attributes`)",
			want: "SELECT toJSONString(`Attributes`) AS `Attributes` FROM (SELECT `Attributes` FROM `t`) ORDER BY mapSort(`Attributes`)",
		},
		{
			// The regression a first version of this pass shipped with,
			// caught by the promql corpus's own subquery-reduction fixtures
			// (subquery_unary_minus and 38 others): every such fixture's SQL
			// ends `WHERE anchor_ts > … AND anchor_ts <= … GROUP BY
			// Attributes` — a WHERE this pass must nest, immediately
			// followed by a GROUP BY that has to stay OUTSIDE the nest. A
			// naive "everything after WHERE " capture swallowed the GROUP BY
			// into the rewritten WHERE's parens, so ClickHouse read a bare
			// column reference with no aggregation in scope and rejected the
			// query outright (NOT_AN_AGGREGATE) — not a wrong answer, every
			// one of those fixtures failing to execute at all.
			name: "trailing group by survives the where nesting",
			in: "SELECT toJSONString(`Attributes`) AS `Attributes`, max(`Value`) AS `Value` FROM " +
				"(SELECT `Attributes`, `anchor_ts`, `Value` FROM `t`) " +
				"WHERE (`anchor_ts` > 1) AND mapContains(`Attributes`, 'k') GROUP BY `Attributes`",
			want: "SELECT toJSONString(`Attributes`) AS `Attributes`, max(`Value`) AS `Value` FROM " +
				"(SELECT * FROM (SELECT `Attributes`, `anchor_ts`, `Value` FROM `t`) " +
				"WHERE (`anchor_ts` > 1) AND mapContains(`Attributes`, 'k')) GROUP BY `Attributes`",
		},
		{
			// The star-wrapper recursion (nestMapWhereThroughStarWrappers)
			// only fires when NOTHING trails the outer FROM's own closing
			// paren — the exact limit_ratio shape it exists for. A `GROUP
			// BY` trailing the OUTER FROM, with the WHERE hiding one level
			// deeper inside a star wrapper, is a shape no fixture in the
			// corpus produces; this pass declines it conservatively rather
			// than guessing how to re-attach the GROUP BY around a nest it
			// was never verified against, the same discipline
			// [NestMapOrderBy] applies to shapes outside its own verified
			// set.
			name: "star wrapper with a trailing group by outside it is left alone",
			in: "SELECT toJSONString(`Attributes`) AS `Attributes`, max(`Value`) AS `Value` FROM " +
				"(SELECT * FROM (SELECT `Attributes`, `anchor_ts`, `Value` FROM `t`) " +
				"WHERE mapContains(`Attributes`, 'k')) GROUP BY `Attributes`",
			want: "SELECT toJSONString(`Attributes`) AS `Attributes`, max(`Value`) AS `Value` FROM " +
				"(SELECT * FROM (SELECT `Attributes`, `anchor_ts`, `Value` FROM `t`) " +
				"WHERE mapContains(`Attributes`, 'k')) GROUP BY `Attributes`",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NestMapWhere(tc.in); got != tc.want {
				t.Errorf("NestMapWhere:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestSplitWherePredicate pins the depth- and quote-aware split
// [NestMapWhere] and [nestMapWhereThroughStarWrappers] both depend on:
// a WHERE predicate must stop exactly where a sibling GROUP BY / HAVING
// / ORDER BY / LIMIT begins, never a byte earlier or later.
func TestSplitWherePredicate(t *testing.T) {
	cases := []struct {
		name          string
		in            string
		wantPredicate string
		wantRest      string
	}{
		{
			name:          "no trailing clause",
			in:            "`Value` > 0",
			wantPredicate: "`Value` > 0",
			wantRest:      "",
		},
		{
			name:          "trailing group by",
			in:            "`Value` > 0 GROUP BY `Attributes`",
			wantPredicate: "`Value` > 0",
			wantRest:      "GROUP BY `Attributes`",
		},
		{
			name:          "trailing having",
			in:            "`Value` > 0 HAVING count() > 1",
			wantPredicate: "`Value` > 0",
			wantRest:      "HAVING count() > 1",
		},
		{
			name:          "trailing order by",
			in:            "`Value` > 0 ORDER BY `Value` DESC",
			wantPredicate: "`Value` > 0",
			wantRest:      "ORDER BY `Value` DESC",
		},
		{
			name:          "trailing limit",
			in:            "`Value` > 0 LIMIT 10",
			wantPredicate: "`Value` > 0",
			wantRest:      "LIMIT 10",
		},
		{
			// A keyword-shaped substring inside a paren-nested subquery or a
			// string literal must never split the predicate early.
			name:          "keyword text inside nested parens and a string literal is not a split point",
			in:            "`Value` IN (SELECT 1 LIMIT 5) AND `Name` != 'GROUP BY'",
			wantPredicate: "`Value` IN (SELECT 1 LIMIT 5) AND `Name` != 'GROUP BY'",
			wantRest:      "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPredicate, gotRest := splitWherePredicate(tc.in)
			if gotPredicate != tc.wantPredicate || gotRest != tc.wantRest {
				t.Errorf("splitWherePredicate(%q):\n got: (%q, %q)\nwant: (%q, %q)",
					tc.in, gotPredicate, gotRest, tc.wantPredicate, tc.wantRest)
			}
		})
	}
}

func TestTolerantRowsErr(t *testing.T) {
	if err := TolerantRowsErr(nil); err != nil {
		t.Errorf("nil -> %v, want nil", err)
	}
	if err := TolerantRowsErr(errString("empty row")); err != nil {
		t.Errorf("empty row sentinel -> %v, want nil", err)
	}
	real := errString("connection refused")
	if err := TolerantRowsErr(real); err == nil {
		t.Errorf("real error swallowed")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
