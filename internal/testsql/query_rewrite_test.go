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
