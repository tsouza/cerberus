//go:build chdb

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
	oracle "github.com/tsouza/cerberus/test/spec/parityoracle/promql"
)

const selectorWireSeed = `
CREATE TABLE otel_metrics_exponential_histogram (
 MetricName String, Attributes Map(String,String),
 ResourceAttributes Map(String,String) DEFAULT map(), ServiceName String DEFAULT '',
 TimeUnix DateTime64(9), Count UInt64, Sum Float64, Scale Int32, ZeroCount UInt64,
 PositiveOffset Int32, PositiveBucketCounts Array(UInt64), NegativeOffset Int32, NegativeBucketCounts Array(UInt64)
) ENGINE=MergeTree ORDER BY (MetricName,Attributes,TimeUnix);
CREATE TABLE otel_metrics_gauge (
 MetricName String, Attributes Map(String,String),
 ResourceAttributes Map(String,String) DEFAULT map(), ServiceName String DEFAULT '',
 TimeUnix DateTime64(9), Value Float64
) ENGINE=MergeTree ORDER BY (MetricName,Attributes,TimeUnix);
INSERT INTO otel_metrics_exponential_histogram
 (MetricName,Attributes,TimeUnix,Count,Sum,Scale,ZeroCount,PositiveOffset,PositiveBucketCounts,NegativeOffset,NegativeBucketCounts) VALUES
 ('selector_wire_exp_hist', map('series','hist'), toDateTime64('2026-01-01 00:00:00',9), 2, 3., 0, 0, 0, [2], 0, []);
INSERT INTO otel_metrics_gauge (MetricName,Attributes,TimeUnix,Value) VALUES
 ('selector_wire_gauge', map('series','float'), toDateTime64('2026-01-01 00:00:00',9), 42.);
`

func TestMixedSelectorsWireSampleKinds_ChDB(t *testing.T) {
	standard := schema.DefaultOTelMetrics()
	custom := standard
	custom.MetricNameColumn, custom.TimestampColumn, custom.ValueColumn = "metric_id", "sample_time", "sample_value"
	seeded := []oracle.Series{
		{Labels: map[string]string{"__name__": "selector_wire_exp_hist", "series": "hist"}, Points: []oracle.Point{{TMillis: histValuedEvalTime.Add(-time.Second).UnixMilli(), Histogram: &oracle.Histogram{Count: 2, Sum: 3, PositiveBuckets: []float64{2}}}}},
		{Labels: map[string]string{"__name__": "selector_wire_gauge", "series": "float"}, Points: []oracle.Point{{TMillis: histValuedEvalTime.Add(-time.Second).UnixMilli(), Value: 42}}},
	}
	for _, tc := range []struct {
		name      string
		op        string
		parameter string
		schema    schema.Metrics
		step      time.Duration
		histFirst bool
	}{
		{"topk/literal/instant/float_first", "topk", "10", standard, 0, false},
		{"topk/computed/range/hist_first", "topk", "scalar(vector(10))", custom, time.Second, true},
		{"bottomk/literal/range/hist_first", "bottomk", "10", custom, time.Second, true},
		{"bottomk/computed/instant/float_first", "bottomk", "scalar(vector(10))", standard, 0, false},
		{"limitk/literal/range/hist_first", "limitk", "10", standard, time.Second, true},
		{"limitk/computed/instant/float_first", "limitk", "scalar(vector(10))", custom, 0, false},
		{"limit_ratio/literal/instant/hist_first", "limit_ratio", "1", custom, 0, true},
		{"limit_ratio/computed/range/float_first", "limit_ratio", "scalar(vector(1))", standard, time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, right := "selector_wire_gauge", "selector_wire_exp_hist"
			if tc.histFirst {
				left, right = right, left
			}
			query := tc.op + "(" + tc.parameter + ", sort_by_label(" + left + " or " + right + `, "series"))`
			assertSelectorWireParity(t, tc.schema, seeded, query, tc.step)
		})
	}
}

func assertSelectorWireParity(t *testing.T, s schema.Metrics, seeded []oracle.Series, query string, step time.Duration) {
	t.Helper()
	at := histValuedEvalTime
	reference, err := oracle.Evaluate(t, seeded, oracle.Query{Expr: query, Start: at, End: at.Add(step), Step: step})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{}
	for _, sample := range reference {
		key := fmt.Sprintf("%s/%s/%d", sample.Labels["__name__"], sample.Labels["series"], sample.TMillis)
		if sample.Histogram != nil {
			want = append(want, fmt.Sprintf("%s/hist/%g/%g", key, sample.Histogram.Count, sample.Histogram.Sum))
		} else {
			want = append(want, fmt.Sprintf("%s/float/%g", key, sample.Value))
		}
	}
	slices.Sort(want)
	client := chclienttest.NewChDB(t)
	seed := strings.NewReplacer("MetricName", s.MetricNameColumn, "TimeUnix", s.TimestampColumn, "Value", s.ValueColumn).Replace(selectorWireSeed)
	client.Seed(t, seed)
	handler := prom.New(client, s, nil)
	mux := http.NewServeMux()
	handler.Mount(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	params := url.Values{"query": {query}}
	endpoint := "/api/v1/query"
	if step == 0 {
		params.Set("time", at.Format(time.RFC3339))
	} else {
		endpoint = "/api/v1/query_range"
		params.Set("start", at.Format(time.RFC3339))
		params.Set("end", at.Add(step).Format(time.RFC3339))
		params.Set("step", step.String())
	}
	response, err := http.Get(server.URL + endpoint + "?" + params.Encode()) //nolint:noctx // test-local server
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", query, response.StatusCode, body)
	}
	got := mixedSelectorWireRows(t, body, step > 0)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: wire=%v reference=%v", query, got, want)
	}
}

func mixedSelectorWireRows(t *testing.T, body string, matrix bool) []string {
	t.Helper()
	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric     map[string]string   `json:"metric"`
				Value      []json.RawMessage   `json:"value"`
				Histogram  []json.RawMessage   `json:"histogram"`
				Values     [][]json.RawMessage `json:"values"`
				Histograms [][]json.RawMessage `json:"histograms"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	wantType := "vector"
	if matrix {
		wantType = "matrix"
	}
	if parsed.Status != "success" || parsed.Data.ResultType != wantType {
		t.Fatalf("invalid response: %s", body)
	}
	got := []string{}
	for _, series := range parsed.Data.Result {
		floats, histograms := series.Values, series.Histograms
		if !matrix {
			if len(series.Value) != 0 {
				floats = append(floats, series.Value)
			}
			if len(series.Histogram) != 0 {
				histograms = append(histograms, series.Histogram)
			}
		}
		if (len(floats) == 0) == (len(histograms) == 0) {
			t.Fatalf("seeded series must contain exactly one sample kind: %s", body)
		}
		for kind, points := range [][][]json.RawMessage{floats, histograms} {
			for _, point := range points {
				if len(point) != 2 {
					t.Fatal("invalid wire point", point)
				}
				var stamp float64
				if err := json.Unmarshal(point[0], &stamp); err != nil {
					t.Fatal(err)
				}
				const millisPerSecond = 1000
				key := fmt.Sprintf("%s/%s/%d", series.Metric["__name__"], series.Metric["series"], int64(stamp*millisPerSecond))
				if kind == 0 {
					var value string
					if err := json.Unmarshal(point[1], &value); err != nil {
						t.Fatal(err)
					}
					got = append(got, key+"/float/"+value)
				} else {
					var histogram struct {
						Count   string  `json:"count"`
						Sum     string  `json:"sum"`
						Buckets [][]any `json:"buckets"`
					}
					if err := json.Unmarshal(point[1], &histogram); err != nil {
						t.Fatal(err)
					}
					wantBuckets := [][]any{{float64(0), "1", "2", "2"}}
					if !reflect.DeepEqual(histogram.Buckets, wantBuckets) {
						t.Fatalf("histogram buckets=%v, want=%v", histogram.Buckets, wantBuckets)
					}
					got = append(got, key+"/hist/"+histogram.Count+"/"+histogram.Sum)
				}
			}
		}
	}
	slices.Sort(got)
	return got
}
