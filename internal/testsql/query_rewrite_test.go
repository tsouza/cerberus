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
