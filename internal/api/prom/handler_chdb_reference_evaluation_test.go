//go:build chdb

// chDB-backed differential pins for the queries whose evaluation leaves
// the span cerberus's lowering represents exactly
// (promql.LeavesEvaluationDomain): `@` pins before 1900 or after 2262,
// windows reaching below the int64-nanosecond floor, request instants at
// and beyond the int64-nanosecond window, and pins far enough from the eval instant that
// reference Prometheus's own offset arithmetic wraps.
//
// Each case runs through the real HTTP handler against a seeded chDB
// session and through the upstream Prometheus engine over the same
// samples (test/spec/parityoracle/promql), and the two answers must be
// equal point for point.

package prom_test

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	oracle "github.com/tsouza/cerberus/test/spec/parityoracle/promql"
)

// referenceEvalTime is the instant every instant-mode case evaluates at,
// and the end of every range-mode case.
var referenceEvalTime = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// referenceSeedLead / referenceSeedTail bound the seeded samples around
// referenceEvalTime, one per minute.
const (
	referenceSeedLead = 30 * time.Minute
	referenceSeedTail = 10 * time.Minute
	referenceSeedBase = 100
)

// referenceSeed returns the chDB seed and the same samples in the
// oracle's shape: `up{job="a"}` once a minute, valued 100, 101, ….
func referenceSeed() (string, []oracle.Series) {
	var rows []string
	series := oracle.Series{Labels: map[string]string{"__name__": "up", "job": "a"}}
	value := referenceSeedBase
	for ts := referenceEvalTime.Add(-referenceSeedLead); !ts.After(referenceEvalTime.Add(referenceSeedTail)); ts = ts.Add(time.Minute) {
		rows = append(rows, fmt.Sprintf("('up', map('job', 'a'), toDateTime64('%s', 9), %d.0)",
			ts.Format("2006-01-02 15:04:05.000000000"), value))
		series.Points = append(series.Points, oracle.Point{TMillis: ts.UnixMilli(), Value: float64(value)})
		value++
	}
	// `edge` holds one sample at each end of the storage domain, the
	// first and last instants a DateTime64(9) column holds to the
	// millisecond.
	edge := oracle.Series{Labels: map[string]string{"__name__": "edge"}}
	for i, ts := range []time.Time{referenceStorageFirst, referenceStorageLast} {
		rows = append(rows, fmt.Sprintf("('edge', map(), toDateTime64('%s', 9), %d.0)",
			ts.Format("2006-01-02 15:04:05.000000000"), i+1))
		edge.Points = append(edge.Points, oracle.Point{TMillis: ts.UnixMilli(), Value: float64(i + 1)})
	}
	// `latency_exp_hist` is a native histogram: two scrapes a minute
	// apart, so rate() over it has a value.
	hist := oracle.Series{Labels: map[string]string{"__name__": "latency_exp_hist", "service": "api"}}
	for i, h := range []oracle.Histogram{
		{Count: 6, Sum: 3, ZeroCount: 1, PositiveOffset: -1, PositiveBuckets: []float64{2, 3}},
		{Count: 30, Sum: 15, ZeroCount: 1, PositiveOffset: -1, PositiveBuckets: []float64{14, 15}},
	} {
		hist.Points = append(hist.Points, oracle.Point{
			TMillis:   referenceEvalTime.Add(time.Duration(i-1) * time.Minute).UnixMilli(),
			Histogram: &h,
		})
	}
	const histRows = `
INSERT INTO otel_metrics_exponential_histogram VALUES
    ('latency_exp_hist', map('service', 'api'), toDateTime64('2025-12-31 23:59:01', 9),  6,  3.0, 0, 1, -1, [ 2,  3], 0, []),
    ('latency_exp_hist', map('service', 'api'), toDateTime64('2026-01-01 00:00:01', 9), 30, 15.0, 0, 1, -1, [14, 15], 0, []);`
	return gaugeDDL + "\n" + histValuedDDL + "\nINSERT INTO otel_metrics_gauge VALUES\n    " + strings.Join(rows, ",\n    ") + ";" + histRows,
		[]oracle.Series{series, edge, hist}
}

// The `@` instants the cases pin to, in Unix seconds.
const (
	// referenceAtYearOne is 0001-01-01T00:00:00Z, Go's zero time.Time.
	referenceAtYearOne = "-62135596800"
	// referenceAt1675 lies below the int64-nanosecond floor.
	referenceAt1675 = "-9300000000"
	// referenceAt2264 lies above the int64-nanosecond ceiling.
	referenceAt2264 = "9300000000"
	// referenceAtHuge overflows the parser's own int64 milliseconds.
	referenceAtHuge = "1e17"
	// referenceAtFloor is the first whole millisecond inside the
	// int64-nanosecond window, the subquery window below it.
	referenceAtFloor = "-9223372036.854"
	// referenceAt1910 is inside the storage domain, but more than 292
	// years before referenceFarEvalTime.
	referenceAt1910 = "-1893456000"
)

// referenceStorageFirst and referenceStorageLast are the first and last
// whole milliseconds a DateTime64(9) column holds.
var (
	referenceStorageFirst = time.Date(1900, time.January, 1, 0, 0, 0, 0, time.UTC)
	referenceStorageLast  = time.Unix(0, math.MaxInt64).UTC().Truncate(time.Millisecond)
)

// referenceFarEvalTime is 2250-01-01, more than 292 years after
// referenceAt1910.
var referenceFarEvalTime = time.Date(2250, 1, 1, 0, 0, 0, 0, time.UTC)

// referenceNanoFloor is the first whole millisecond inside the
// int64-nanosecond window, and referenceYearOne Go's zero time.Time.
var (
	referenceNanoFloor = time.Unix(0, math.MinInt64).UTC().Truncate(time.Millisecond).Add(time.Millisecond)
	referenceYearOne   = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)
)

type referenceCase struct {
	query      string
	start, end time.Time
	step       time.Duration
	// readsData marks a case whose reference answer depends on seeded
	// samples, so an empty answer on both sides cannot pass it.
	readsData bool
}

func referenceCases() []referenceCase {
	instant := func(q string) referenceCase {
		return referenceCase{query: q, start: referenceEvalTime, end: referenceEvalTime}
	}
	ranged := func(q string) referenceCase {
		return referenceCase{query: q, start: referenceEvalTime.Add(-5 * time.Minute), end: referenceEvalTime, step: time.Minute}
	}
	var cases []referenceCase
	for _, at := range []string{referenceAtYearOne, referenceAt1675, referenceAt2264, referenceAtHuge, referenceAtFloor} {
		cases = append(
			cases,
			instant(`count_over_time(vector(1)[5m:1m] @ `+at+`)`),
			ranged(`count_over_time(vector(1)[5m:1m] @ `+at+`)`),
			instant(`count_over_time(up[5m:1m] @ `+at+`)`),
			instant(`up @ `+at),
			instant(`vector(1)[5m:1m] @ `+at),
			instant(`absent(up @ `+at+`)`),
			instant(`max_over_time(timestamp(vector(1))[5m:1m] @ `+at+`)`),
			referenceCase{
				query: `sum(up) + sum(count_over_time(vector(1)[5m:1m] @ ` + at + `))`,
				start: referenceEvalTime.Add(-5 * time.Minute), end: referenceEvalTime, step: time.Minute, readsData: true,
			},
		)
	}
	cases = append(
		cases,
		instant(`max_over_time(year()[5m:1m] @ `+referenceAtYearOne+`)`),
		instant(`max_over_time(year()[5m:1m] @ `+referenceAt1675+`)`),
		instant(`max_over_time(day_of_week()[5m:1m] @ `+referenceAt1675+`)`),
		instant(`count_over_time(vector(1)[5m:1m] @ -9223372036 offset 1h)`),
		instant(`count_over_time(count_over_time(vector(1)[5m:1m])[10m:2m] @ `+referenceAtYearOne+`)`),
		// A window reaching below 1900 over real data: the reference
		// engine reads it from ClickHouse.
		referenceCase{query: `count_over_time(up[200y])`, start: referenceEvalTime, end: referenceEvalTime, readsData: true},
		referenceCase{
			query: `sum_over_time(up[150y])`,
			start: referenceEvalTime.Add(-5 * time.Minute), end: referenceEvalTime, step: time.Minute, readsData: true,
		},
		// Native histogram samples read through the reference engine.
		referenceCase{query: `histogram_count(rate(latency_exp_hist[150y]))`, start: referenceEvalTime, end: referenceEvalTime, readsData: true},
		referenceCase{query: `histogram_sum(rate(latency_exp_hist[150y]))`, start: referenceEvalTime, end: referenceEvalTime, readsData: true},
		referenceCase{query: `histogram_quantile(0.5, rate(latency_exp_hist[150y]))`, start: referenceEvalTime, end: referenceEvalTime, readsData: true},
		referenceCase{query: `histogram_quantile(0.9, latency_exp_hist @ ` + referenceAt1675 + `)`, start: referenceEvalTime, end: referenceEvalTime},
		// Reads reaching the storage domain's two ends.
		referenceCase{
			query: `count_over_time(edge[1d] @ ` + formatReferenceTime(referenceStorageFirst.Add(time.Hour)) + `)`,
			start: referenceEvalTime, end: referenceEvalTime, readsData: true,
		},
		referenceCase{
			query: `count_over_time(edge[1d] @ ` + formatReferenceTime(referenceStorageLast) + `)`,
			start: referenceEvalTime, end: referenceEvalTime, readsData: true,
		},
		referenceCase{
			// 250 years: wider than one sample-read piece.
			query: `count_over_time(edge[250y] @ ` + formatReferenceTime(referenceStorageFirst.Add(250*365*24*time.Hour-time.Hour)) + `)`,
			start: referenceEvalTime, end: referenceEvalTime, readsData: true,
		},
		// A pin inside the storage domain, wrapped by reference
		// Prometheus's offset arithmetic because it lies more than 292
		// years from the eval instant.
		referenceCase{
			query: `count_over_time(vector(1)[5m:1m] @ ` + referenceAt1910 + `)`,
			start: referenceFarEvalTime, end: referenceFarEvalTime,
		},
		// Request instants at and beyond the int64-nanosecond window
		// (#3825): an unpinned subquery's window reaches below it.
		referenceCase{query: `count_over_time(vector(1)[5m:1m])`, start: referenceNanoFloor, end: referenceNanoFloor},
		referenceCase{
			query: `count_over_time(vector(1)[5m:1m])`,
			start: referenceNanoFloor, end: referenceNanoFloor.Add(5 * time.Minute), step: time.Minute,
		},
		referenceCase{
			query: `time()`,
			start: referenceNanoFloor, end: referenceNanoFloor.Add(2 * time.Minute), step: time.Minute,
		},
		referenceCase{query: `time()`, start: referenceYearOne, end: referenceYearOne},
		referenceCase{query: `1 + 1`, start: referenceYearOne, end: referenceYearOne},
		referenceCase{query: `1 + 1`, start: referenceYearOne, end: referenceYearOne.Add(2 * time.Minute), step: time.Minute},
		referenceCase{
			query: `count_over_time(vector(1)[5m:1m])`,
			start: referenceYearOne, end: referenceYearOne.Add(5 * time.Minute), step: time.Minute,
		},
	)
	return cases
}

// TestQuery_OutsideEvaluationDomain_MatchesReference_ChDB pins that
// cerberus answers every case exactly as reference Prometheus does.
func TestQuery_OutsideEvaluationDomain_MatchesReference_ChDB(t *testing.T) {
	seed, series := referenceSeed()
	srv, _ := newChDBServer(t, seed)
	for _, tc := range referenceCases() {
		name := tc.query
		if tc.step > 0 {
			name = "range/" + name
		}
		t.Run(name, func(t *testing.T) {
			want, err := oracle.Evaluate(t, series, oracle.Query{Expr: tc.query, Start: tc.start, End: tc.end, Step: tc.step})
			if err != nil {
				t.Fatalf("reference: %v", err)
			}
			if tc.readsData && len(want) == 0 {
				t.Fatalf("reference answered nothing for a case meant to read the seeded samples")
			}
			got := referenceHandlerAnswer(t, srv.URL, tc)
			assertSameReferenceResults(t, got, want)
		})
	}
}

// referenceHandlerAnswer runs tc through the handler and flattens the
// answer into the oracle's shape.
func referenceHandlerAnswer(t *testing.T, base string, tc referenceCase) []oracle.Result {
	t.Helper()
	params := url.Values{"query": {tc.query}}
	endpoint := "/api/v1/query"
	if tc.step > 0 {
		endpoint = "/api/v1/query_range"
		params.Set("start", formatReferenceTime(tc.start))
		params.Set("end", formatReferenceTime(tc.end))
		params.Set("step", strconv.FormatFloat(tc.step.Seconds(), 'f', -1, 64))
	} else {
		params.Set("time", formatReferenceTime(tc.start))
	}
	resp, err := http.Get(base + endpoint + "?" + params.Encode())
	if err != nil {
		t.Fatalf("GET %s: %v", endpoint, err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var parsed struct {
		Data struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	var out []oracle.Result
	switch parsed.Data.ResultType {
	case "scalar":
		var point []any
		if err := json.Unmarshal(parsed.Data.Result, &point); err != nil {
			t.Fatalf("decode scalar %s: %v", parsed.Data.Result, err)
		}
		out = append(out, referencePoint(t, map[string]string{}, point))
	case "vector":
		var vec []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		}
		if err := json.Unmarshal(parsed.Data.Result, &vec); err != nil {
			t.Fatalf("decode vector %s: %v", parsed.Data.Result, err)
		}
		for _, s := range vec {
			out = append(out, referencePoint(t, s.Metric, s.Value))
		}
	case "matrix":
		var mat []struct {
			Metric map[string]string `json:"metric"`
			Values [][]any           `json:"values"`
		}
		if err := json.Unmarshal(parsed.Data.Result, &mat); err != nil {
			t.Fatalf("decode matrix %s: %v", parsed.Data.Result, err)
		}
		for _, s := range mat {
			for _, p := range s.Values {
				out = append(out, referencePoint(t, s.Metric, p))
			}
		}
	default:
		t.Fatalf("unexpected resultType %q in %s", parsed.Data.ResultType, body)
	}
	return out
}

// formatReferenceTime renders t as the Unix-seconds float the HTTP API
// takes, exact to the millisecond.
func formatReferenceTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixMilli())/1e3, 'f', 3, 64)
}

func referencePoint(t *testing.T, metric map[string]string, pair []any) oracle.Result {
	t.Helper()
	if len(pair) != 2 {
		t.Fatalf("malformed sample %v", pair)
	}
	ts, ok := pair[0].(float64)
	if !ok {
		t.Fatalf("malformed timestamp %v", pair[0])
	}
	raw, ok := pair[1].(string)
	if !ok {
		t.Fatalf("malformed value %v", pair[1])
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("value %q: %v", raw, err)
	}
	if metric == nil {
		metric = map[string]string{}
	}
	return oracle.Result{Labels: metric, TMillis: int64(math.Round(ts * 1e3)), Value: v}
}

func assertSameReferenceResults(t *testing.T, got, want []oracle.Result) {
	t.Helper()
	key := func(r oracle.Result) string {
		names := make([]string, 0, len(r.Labels))
		for k := range r.Labels {
			names = append(names, k)
		}
		sort.Strings(names)
		var b strings.Builder
		for _, k := range names {
			fmt.Fprintf(&b, "%s=%q,", k, r.Labels[k])
		}
		return fmt.Sprintf("%s@%d", b.String(), r.TMillis)
	}
	sortResults := func(rs []oracle.Result) {
		sort.Slice(rs, func(i, j int) bool { return key(rs[i]) < key(rs[j]) })
	}
	sortResults(got)
	sortResults(want)
	if len(got) != len(want) {
		t.Fatalf("got %d points, reference %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range got {
		if key(got[i]) != key(want[i]) {
			t.Fatalf("point %d: got %s, reference %s\ngot:  %v\nwant: %v", i, key(got[i]), key(want[i]), got, want)
		}
		g, w := got[i].Value, want[i].Value
		if g != w && !(math.IsNaN(g) && math.IsNaN(w)) {
			t.Fatalf("point %s: got %v, reference %v", key(got[i]), g, w)
		}
	}
}
