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
