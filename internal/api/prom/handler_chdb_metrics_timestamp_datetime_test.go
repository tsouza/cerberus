//go:build chdb

// chDB-backed end-to-end coverage for the OTel-CH metrics schema whose
// timestamp columns are `DateTime` — the collector-contrib ClickHouse
// exporter's schema from v0.157 (TimeUnix, StartTimeUnix and the Nested
// Exemplars.TimeUnix typed `DateTime` instead of `DateTime64(9)`).
//
// The metric tables are created from the exporter's own v0.157.0 CREATE
// TABLE templates (chsqltest.UpstreamDateTimeMetricsDDL), not from a
// hand-written approximation, so the physical shape the emitted SQL meets
// — LowCardinality maps, CODECs, the minmax index, the Nested exemplar
// column, the upstream sorting key — is the one a v0.157+ collector
// creates. Every seeded sample sits on a whole second, which is all the
// `DateTime` type can hold, so each expected answer below is computed by
// hand from the seed rather than read back from cerberus.
//
// The handler renders for schema.MetricsTimestampSeconds, the precision
// preflight resolves for this schema. The known-positive control renders
// for the zero precision over the same tables and must fail:
// toUnixTimestamp64Milli rejects a `DateTime` argument with
// ILLEGAL_TYPE_OF_ARGUMENT, the runtime gap the precision closes.

package prom_test

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/schema"
)

// The seed's time axis: samples every dtSampleInterval from dtSeedStart,
// dtSampleCount of them, so the last one sits at dtSeedStart + 300s.
var dtSeedStart = time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)

const (
	dtSampleInterval = 15 * time.Second
	dtSampleCount    = 21
	// dtCounterStep is the reqs_total increment per sample, so the
	// counter's per-second rate is dtCounterStep / dtSampleInterval.
	dtCounterStep = 10
	// dtHistogramEvery spaces the histogram rows: one per minute.
	dtHistogramEvery = 4
)

// dtLastSample is the timestamp of the seed's last sample.
var dtLastSample = dtSeedStart.Add((dtSampleCount - 1) * dtSampleInterval)

// dtFloatTolerance bounds the float comparison of a computed answer
// (rate, histogram_quantile) against its hand-computed value.
const dtFloatTolerance = 1e-9

// dtSeed renders the INSERTs: a gauge temp_celsius{room="a"} whose value
// is the sample index; a cumulative counter reqs_total{job="api"} rising
// dtCounterStep per sample, each sample carrying one exemplar at its own
// timestamp; and a classic histogram lat_seconds{job="api"} with bounds
// [0.1, 0.5] whose k-th row (one per minute) has per-bucket counts
// [k, 2k, k].
func dtSeed() string {
	var gauge, sum, hist []string
	for i := range dtSampleCount {
		ts := dtSeedStart.Add(time.Duration(i) * dtSampleInterval).Format(time.DateTime)
		gauge = append(gauge, fmt.Sprintf("('temp_celsius', map('room', 'a'), toDateTime('%s', 'UTC'), %d)", ts, i))
		sum = append(sum, fmt.Sprintf(
			"('reqs_total', map('job', 'api'), toDateTime('%[1]s', 'UTC'), %[2]d, 2, true, "+
				"[map('request_id', 'r%[3]d')], [toDateTime('%[1]s', 'UTC')], [%[2]d], ['span%[3]d'], ['trace%[3]d'])",
			ts, i*dtCounterStep, i,
		))
		if i%dtHistogramEvery == 0 {
			k := i / dtHistogramEvery
			hist = append(hist, fmt.Sprintf(
				"('lat_seconds', map('job', 'api'), toDateTime('%s', 'UTC'), %d, %d, [%d, %d, %d], [0.1, 0.5], 2)",
				ts, 4*k, k, k, 2*k, k,
			))
		}
	}
	return "INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
		strings.Join(gauge, ",\n") + ";\n" +
		"INSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value, AggregationTemporality, IsMonotonic, " +
		"Exemplars.FilteredAttributes, Exemplars.TimeUnix, Exemplars.Value, Exemplars.SpanId, Exemplars.TraceId) VALUES\n" +
		strings.Join(sum, ",\n") + ";\n" +
		"INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, Count, Sum, BucketCounts, ExplicitBounds, " +
		"AggregationTemporality) VALUES\n" +
		strings.Join(hist, ",\n") + ";\n"
}

// newChDBUpstreamDateTimeServer seeds the upstream v0.157.0 metric tables
// and serves a prom handler over them that renders for the given metrics
// timestamp precision.
func newChDBUpstreamDateTimeServer(t *testing.T, precision schema.MetricsTimestampPrecision) *httptest.Server {
	t.Helper()
	return newChDBMetricsTimestampServer(t, chsqltest.UpstreamDateTimeMetricsDDL, precision)
}

// newChDBMetricsTimestampServer creates the metric tables renderDDL returns
// for the chDB session's current database, seeds them with dtSeed, and
// serves a prom handler over them that renders for the given metrics
// timestamp precision. The tables are dropped first — the templates say CREATE TABLE
// IF NOT EXISTS, which would keep an earlier test's table of the other
// timestamp type — and again on cleanup, so no later test in the shared
// chDB session reads a table its seed did not create.
func newChDBMetricsTimestampServer(
	t *testing.T,
	renderDDL func(database string) ([]string, error),
	precision schema.MetricsTimestampPrecision,
) *httptest.Server {
	t.Helper()
	c := chclienttest.NewChDB(t)
	dbs, err := c.QueryStrings(t.Context(), "SELECT currentDatabase()")
	if err != nil || len(dbs) != 1 {
		t.Fatalf("currentDatabase: %v %v", dbs, err)
	}
	m := schema.DefaultOTelMetrics()
	var drop strings.Builder
	for _, table := range []string{m.GaugeTable, m.SumTable, m.HistogramTable, m.ExpHistogramTable, m.SummaryTable} {
		fmt.Fprintf(&drop, "DROP TABLE IF EXISTS %s;\n", table)
	}
	c.Seed(t, drop.String())
	t.Cleanup(func() { c.Seed(t, drop.String()) })
	ddl, err := renderDDL(dbs[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range ddl {
		c.Seed(t, stmt+";")
	}
	c.Seed(t, dtSeed())

	h := prom.New(c, m, nil)
	h.Strategies = chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{
		Metrics: chsql.SchemaStrategies{MetricsTimestamp: precision},
	})
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// dtGet issues a GET against srv and decodes the Prom envelope, failing the
// test on anything but a 200 success.
func dtGet(t *testing.T, srv *httptest.Server, path string, params url.Values) json.RawMessage {
	t.Helper()
	resp, err := http.Get(srv.URL + path + "?" + params.Encode())
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	body := readBody(t, resp)
	var env struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("unmarshal %s: %v\nbody=%s", path, err, body)
	}
	if resp.StatusCode != http.StatusOK || env.Status != "success" {
		t.Fatalf("%s %v: status=%d body=%s", path, params, resp.StatusCode, body)
	}
	return env.Data
}

// dtInstant runs an instant query at ts and returns its vector.
func dtInstant(t *testing.T, srv *httptest.Server, query string, ts time.Time) []prom.VectorSample {
	t.Helper()
	data := dtGet(t, srv, "/api/v1/query", url.Values{
		"query": {query}, "time": {strconv.FormatInt(ts.Unix(), 10)},
	})
	var qd struct {
		ResultType string              `json:"resultType"`
		Result     []prom.VectorSample `json:"result"`
	}
	if err := json.Unmarshal(data, &qd); err != nil {
		t.Fatalf("decode %s: %v\ndata=%s", query, err, data)
	}
	if qd.ResultType != "vector" {
		t.Fatalf("%s: resultType=%q, want vector", query, qd.ResultType)
	}
	return qd.Result
}

// dtSingleValue runs an instant query that must answer one series and
// returns its value.
func dtSingleValue(t *testing.T, srv *httptest.Server, query string, ts time.Time) float64 {
	t.Helper()
	vec := dtInstant(t, srv, query, ts)
	if len(vec) != 1 {
		t.Fatalf("%s: got %d series, want 1: %+v", query, len(vec), vec)
	}
	raw, err := json.Marshal(vec[0].Value)
	if err != nil {
		t.Fatal(err)
	}
	var pair [2]any
	if err := json.Unmarshal(raw, &pair); err != nil {
		t.Fatalf("%s: decode value %s: %v", query, raw, err)
	}
	s, ok := pair[1].(string)
	if !ok {
		t.Fatalf("%s: value %v is not a string", query, pair[1])
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("%s: parse value %q: %v", query, s, err)
	}
	return v
}

// TestUpstreamDateTimeSchema_PromQL_ChDB runs representative PromQL over the
// upstream DateTime-typed metric tables and grades each answer against the
// value the whole-second seed implies.
func TestUpstreamDateTimeSchema_PromQL_ChDB(t *testing.T) {
	dtRunPromQLCases(t, newChDBUpstreamDateTimeServer(t, schema.MetricsTimestampSeconds))
}

// dtRunPromQLCases runs representative PromQL over a server seeded with
// dtSeed and grades each answer against the value the whole-second seed
// implies. The seed holds only whole seconds, so the answers are the same
// whichever timestamp type the tables carry.
func dtRunPromQLCases(t *testing.T, srv *httptest.Server) {
	t.Helper()
	lastIndex := float64(dtSampleCount - 1)
	ratePerSecond := dtCounterStep / dtSampleInterval.Seconds()

	cases := []struct {
		name  string
		query string
		at    time.Time
		want  float64
	}{
		// The latest gauge sample is the last index.
		{"instant selector", "temp_celsius", dtLastSample, lastIndex},
		// timestamp() answers the sample's own second, read back through
		// toUnixTimestamp64Milli on the widened DateTime column.
		{"timestamp", "timestamp(temp_celsius)", dtLastSample.Add(7 * time.Second), float64(dtLastSample.Unix())},
		// ts_of_max_over_time: the maximum is the newest sample in the window.
		{"ts_of_max_over_time", "ts_of_max_over_time(temp_celsius[1m])", dtLastSample, float64(dtLastSample.Unix())},
		// A linear counter's rate is its slope, independent of extrapolation.
		{"rate", "rate(reqs_total[1m])", dtLastSample, ratePerSecond},
		// offset 1m shifts the window to (t-2m, t-1m]; its newest sample is
		// four samples before the last.
		{"offset", "max_over_time(temp_celsius[1m] offset 1m)", dtLastSample, lastIndex - 4},
		// The subquery's 30s steps in (t-2m, t] land on t-90s … t; the
		// oldest of them is six samples before the last.
		{"subquery", "min_over_time(temp_celsius[2m:30s])", dtLastSample, lastIndex - 6},
		// Cumulative bucket rates are r, 3r, 4r for le 0.1, 0.5, +Inf, so
		// the median (rank 2r) interpolates halfway into (0.1, 0.5].
		{"histogram_quantile", "histogram_quantile(0.5, rate(lat_seconds_bucket[2m]))", dtLastSample, 0.3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dtSingleValue(t, srv, tc.query, tc.at)
			if math.Abs(got-tc.want) > dtFloatTolerance {
				t.Errorf("%s = %v, want %v", tc.query, got, tc.want)
			}
		})
	}

	t.Run("range", func(t *testing.T) {
		start := dtSeedStart.Add(time.Minute)
		data := dtGet(t, srv, "/api/v1/query_range", url.Values{
			"query": {"temp_celsius"},
			"start": {strconv.FormatInt(start.Unix(), 10)},
			"end":   {strconv.FormatInt(dtLastSample.Unix(), 10)},
			"step":  {"60"},
		})
		var qd struct {
			Result []prom.MatrixSample `json:"result"`
		}
		if err := json.Unmarshal(data, &qd); err != nil {
			t.Fatalf("decode: %v\ndata=%s", err, data)
		}
		if len(qd.Result) != 1 {
			t.Fatalf("got %d series, want 1: %s", len(qd.Result), data)
		}
		// Each minute step lands on a sample: indices 4, 8, 12, 16, 20.
		want := []string{"4", "8", "12", "16", "20"}
		got := make([]string, 0, len(qd.Result[0].Values))
		for _, v := range qd.Result[0].Values {
			got = append(got, fmt.Sprint(v[1]))
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("range values = %v, want %v", got, want)
		}
	})

	t.Run("series", func(t *testing.T) {
		data := dtGet(t, srv, "/api/v1/series", url.Values{
			"match[]": {"temp_celsius"},
			"start":   {strconv.FormatInt(dtSeedStart.Unix(), 10)},
			"end":     {strconv.FormatInt(dtLastSample.Unix(), 10)},
		})
		var series []map[string]string
		if err := json.Unmarshal(data, &series); err != nil {
			t.Fatalf("decode: %v\ndata=%s", err, data)
		}
		if len(series) != 1 || series[0]["__name__"] != "temp_celsius" || series[0]["room"] != "a" {
			t.Errorf("series = %v, want one temp_celsius{room=\"a\"}", series)
		}
	})

	t.Run("label values", func(t *testing.T) {
		data := dtGet(t, srv, "/api/v1/label/room/values", url.Values{
			"start": {strconv.FormatInt(dtSeedStart.Unix(), 10)},
			"end":   {strconv.FormatInt(dtLastSample.Unix(), 10)},
		})
		var values []string
		if err := json.Unmarshal(data, &values); err != nil {
			t.Fatalf("decode: %v\ndata=%s", err, data)
		}
		if len(values) != 1 || values[0] != "a" {
			t.Errorf("room values = %v, want [a]", values)
		}
	})

	t.Run("exemplars", func(t *testing.T) {
		data := dtGet(t, srv, "/api/v1/query_exemplars", url.Values{
			"query": {"reqs_total"},
			"start": {strconv.FormatInt(dtSeedStart.Unix(), 10)},
			"end":   {strconv.FormatInt(dtLastSample.Unix(), 10)},
		})
		var series []struct {
			Exemplars []struct {
				Labels    map[string]string `json:"labels"`
				Timestamp float64           `json:"timestamp"`
			} `json:"exemplars"`
		}
		if err := json.Unmarshal(data, &series); err != nil {
			t.Fatalf("decode: %v\ndata=%s", err, data)
		}
		if len(series) != 1 || len(series[0].Exemplars) != dtSampleCount {
			t.Fatalf("want one series with %d exemplars, got %s", dtSampleCount, data)
		}
		// Each exemplar sits at its own sample's second.
		for _, ex := range series[0].Exemplars {
			i, err := strconv.Atoi(strings.TrimPrefix(ex.Labels["trace_id"], "trace"))
			if err != nil {
				t.Fatalf("exemplar trace_id %q: %v", ex.Labels["trace_id"], err)
			}
			want := float64(dtSeedStart.Add(time.Duration(i) * dtSampleInterval).Unix())
			if ex.Timestamp != want {
				t.Errorf("exemplar %d timestamp = %v, want %v", i, ex.Timestamp, want)
			}
		}
	})
}

// TestUpstreamDateTimeSchema_NanosPrecisionFails_ChDB is the known-positive
// control: rendering for nanosecond precision over the same DateTime tables
// calls toUnixTimestamp64Milli on a DateTime column, which ClickHouse
// rejects. Without it the test above could pass on SQL that never reaches a
// timestamp function at all.
func TestUpstreamDateTimeSchema_NanosPrecisionFails_ChDB(t *testing.T) {
	dtAssertTimestampQueryRejected(t, newChDBUpstreamDateTimeServer(t, schema.MetricsTimestampNanos))
}

// dtAssertTimestampQueryRejected asserts that timestamp() over srv fails
// with ClickHouse's rejection of a toUnixTimestamp64 call on a non-DateTime64
// argument.
func dtAssertTimestampQueryRejected(t *testing.T, srv *httptest.Server) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/v1/query?" + url.Values{
		"query": {"timestamp(temp_celsius)"},
		"time":  {strconv.FormatInt(dtLastSample.Unix(), 10)},
	}.Encode())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("nanosecond rendering must fail on DateTime columns, got 200: %s", body)
	}
	// ClickHouse's ILLEGAL_TYPE_OF_ARGUMENT is exception code 43.
	if !strings.Contains(body, "Code: 43.") || !strings.Contains(body, "toUnixTimestamp64") {
		t.Errorf("want ClickHouse's illegal-type rejection, got status=%d body=%s", resp.StatusCode, body)
	}
}
