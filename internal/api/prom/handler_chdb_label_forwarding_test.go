//go:build chdb

package prom_test

import (
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestQueryRange_LabelRewritePhysicalColumns_ChDB exercises name-dropping
// windows behind broadcasts and value rewrites through the complete HTTP path.
// The label rewrite must forward the physical timestamps without referring to
// a metric-name column that its input no longer publishes.
func TestQueryRange_LabelRewritePhysicalColumns_ChDB(t *testing.T) {
	const (
		seedLeadMinutes = 20
		seedSampleCount = 31
		querySteps      = 5
		valueTolerance  = 1e-12
	)
	start := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	step := time.Minute
	end := start.Add(querySteps * step)
	rows := make([]string, 0, seedSampleCount)
	for i := range seedSampleCount {
		ts := start.Add(time.Duration(i-seedLeadMinutes) * step).Format("2006-01-02 15:04:05.000000000")
		rows = append(rows, fmt.Sprintf("('http_requests_total', map('job','api'), 'svc', toDateTime64('%s',9), %d.0)", ts, i))
	}
	seedDDL := strings.Replace(rangeOffsetSumDDL, "Value Float64", fmt.Sprintf("Value Float64, AggregationTemporality Int32 DEFAULT %d", schema.AggregationTemporalityCumulative), 1)
	seed := seedDDL + "\nINSERT INTO otel_metrics_sum (MetricName,Attributes,ServiceName,TimeUnix,Value) VALUES " + strings.Join(rows, ",") + ";"
	for _, schemaName := range []string{"default", "custom"} {
		t.Run(schemaName, func(t *testing.T) {
			metrics := schema.DefaultOTelMetrics()
			physicalSeed := seed
			if schemaName == "custom" {
				metrics.MetricNameColumn = "metric_id"
				metrics.AttributesColumn = "labels"
				metrics.TimestampColumn = "sample_time"
				metrics.ValueColumn = "sample_value"
				// Preserve ResourceAttributes as a separate physical field while
				// renaming the canonical sample attributes column.
				physicalSeed = strings.NewReplacer("ResourceAttributes", "ResourceAttributes", "MetricName", metrics.MetricNameColumn, "Attributes", metrics.AttributesColumn, "TimeUnix", metrics.TimestampColumn, "Value", metrics.ValueColumn).Replace(seed)
			}
			client := chclienttest.NewChDB(t)
			client.Seed(t, physicalSeed)
			handler := prom.New(client, metrics, nil)
			mux := http.NewServeMux()
			handler.Mount(mux)
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			replace := func(inner string) string {
				return `label_replace(` + inner + `, "dst", "$1", "job", "(.*)")`
			}
			join := func(inner string) string {
				return `label_join(` + inner + `, "dst", "-", "job", "job")`
			}
			const rate = `rate(http_requests_total[5m])`
			pinned := fmt.Sprintf(`rate(http_requests_total[5m] @ %d)`, start.Unix())
			for _, tc := range []struct {
				name, query, dst, metricName string
				lastSample                   bool
			}{
				{name: "replace_direct", query: replace(rate), dst: "api"},
				{name: "replace_pinned", query: replace(pinned), dst: "api"},
				{name: "replace_abs", query: replace("abs(" + rate + ")"), dst: "api"},
				{name: "join_direct", query: join(rate), dst: "api-api"},
				{name: "join_pinned", query: join(pinned), dst: "api-api"},
				{name: "join_abs", query: join("abs(" + rate + ")"), dst: "api-api"},
				{name: "replace_after_join", query: replace(join("abs(" + rate + ")")), dst: "api"},
				{name: "join_after_replace", query: join(replace(pinned)), dst: "api-api"},
				{name: "replace_nested_abs", query: replace("abs(abs(" + rate + "))"), dst: "api"},
				{name: "replace_offset", query: replace("abs(rate(http_requests_total[5m] offset 1m))"), dst: "api"},
				{name: "replace_last", query: replace("last_over_time(http_requests_total[5m])"), dst: "api", metricName: "http_requests_total", lastSample: true},
				{name: "join_last", query: join("last_over_time(http_requests_total[5m])"), dst: "api-api", metricName: "http_requests_total", lastSample: true},
				{name: "replace_abs_last", query: replace("abs(last_over_time(http_requests_total[5m]))"), dst: "api", lastSample: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					matrix := runRangeModeQueryRange(t, srv.URL, tc.query, start, end, step)
					if len(matrix) != 1 {
						t.Fatalf("got %d series, want one", len(matrix))
					}
					got := matrix[0]
					wantLabels := map[string]string{"job": "api", "dst": tc.dst, "service_name": "svc"}
					if tc.metricName != "" {
						wantLabels["__name__"] = tc.metricName
					}
					if !maps.Equal(got.Metric, wantLabels) {
						t.Fatalf("labels = %v, want %v", got.Metric, wantLabels)
					}
					if len(got.Values) != querySteps+1 {
						t.Fatalf("got %d points, want %d", len(got.Values), querySteps+1)
					}
					for i, point := range got.Values {
						wantTime := float64(start.Add(time.Duration(i) * step).Unix())
						if point[0] != wantTime {
							t.Fatalf("point %d timestamp = %v, want %v", i, point[0], wantTime)
						}
						valueText, ok := point[1].(string)
						if !ok {
							t.Fatalf("point %d value = %T, want string", i, point[1])
						}
						value, err := strconv.ParseFloat(valueText, 64)
						if err != nil {
							t.Fatal(err)
						}
						wantValue := 1 / step.Seconds()
						if tc.lastSample {
							wantValue = float64(seedLeadMinutes + i)
						}
						if math.IsNaN(value) || math.Abs(value-wantValue) > valueTolerance {
							t.Fatalf("point %d value = %v, want %v", i, value, wantValue)
						}
					}
				})
			}
		})
	}
}
