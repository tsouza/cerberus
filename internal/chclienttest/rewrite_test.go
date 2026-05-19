//go:build chdb

package chclienttest

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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteMapProjections(tc.in)
			if got != tc.want {
				t.Errorf("rewrite mismatch\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func TestTolerantRowsErr(t *testing.T) {
	if err := tolerantRowsErr(nil); err != nil {
		t.Errorf("nil -> %v, want nil", err)
	}
	if err := tolerantRowsErr(errString("empty row")); err != nil {
		t.Errorf("empty row sentinel -> %v, want nil", err)
	}
	real := errString("connection refused")
	if err := tolerantRowsErr(real); err == nil {
		t.Errorf("real error swallowed")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
