package tempo_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
)

// metricsQueryInstantURL builds the test URL for /api/metrics/query.
// Same q-escaping rationale as metricsQueryRangeURL — TraceQL queries
// always contain `{` / `}` / quotes / pipes.
func metricsQueryInstantURL(base, q string, params map[string]string) string {
	vals := url.Values{}
	vals.Set("q", q)
	for k, v := range params {
		vals.Set(k, v)
	}
	return base + "/api/metrics/query?" + vals.Encode()
}

// TestMetricsQueryInstant_SingleSeriesNoGroupBy — bare `| rate()` over
// the full spans table returns a single series with one (labels, value)
// tuple. Validates the response envelope is
// `{series: [{labels: [{__name__: rate}], value: N}]}` — no `samples`
// array. Cerberus mirrors Tempo's UngroupedAggregator wire shape
// (`{__name__="<op>"}`) rather than emitting an empty label set; see
// wrapMetricsForSample for the cross-reference.
func TestMetricsQueryInstant_SingleSeriesNoGroupBy(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: []chclient.Sample{
		// With step=end-start the inner RangeWindow emits exactly one
		// anchor per series; the handler propagates that single sample
		// as the InstantSeries value. The matrix SQL emits the
		// `map('__name__', 'rate')` Attributes projection for the
		// ungrouped case (see wrapMetricsForSample), so the stub
		// mimics what the CH cursor would surface as Labels.
		{MetricName: "", Labels: map[string]string{"__name__": "rate"}, Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC), Value: 42.0},
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryInstantURL(srv.URL, "{} | rate()", map[string]string{
		"start": fixtureStartUnix,
		"end":   fixtureEndUnix,
	})

	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	var body tempo.MetricsQueryInstantResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Series) != 1 {
		t.Fatalf("expected 1 series, got %d: %+v", len(body.Series), body)
	}
	s := body.Series[0]
	// Ungrouped instant queries surface a single `__name__=<op>` label
	// (matching Tempo's UngroupedAggregator wire shape) so the response
	// is keyed by at least one label rather than the empty label set
	// that previously diverged from Tempo and caused the differ to
	// flag `missing_in_a series key e3b0c44298fc1c14`.
	if len(s.Labels) != 1 || s.Labels[0].Key != "__name__" || s.Labels[0].Value != "rate" {
		t.Errorf("expected single {__name__=rate} label for ungrouped rate(), got %+v", s.Labels)
	}
	if s.Value != 42.0 {
		t.Errorf("expected value 42, got %v", s.Value)
	}

	// JSON body must NOT contain a `samples` key — instant shape is a
	// scalar `value`, not a samples array. Probing the raw response
	// guards against a future struct-tag drift that would silently
	// double-emit both shapes.
	rawResp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET (raw): %v", err)
	}
	rawBody := readBody(t, rawResp)
	if strings.Contains(rawBody, `"samples"`) {
		t.Errorf("instant response must not include 'samples' key: %s", rawBody)
	}
	if !strings.Contains(rawBody, `"value"`) {
		t.Errorf("instant response must include 'value' key: %s", rawBody)
	}

	// Should hit the matrix-shape SQL emitter, same emitter as
	// /api/metrics/query_range. Probe for the hallmark substrings to
	// confirm we're routing through emitRangeWindowMetrics rather than
	// a Sprintf fallback.
	assertSQLContains(t, q.lastSQL, "arrayJoin")
	assertSQLContains(t, q.lastSQL, "anchor_ts")
}

// TestMetricsQueryInstant_MultiSeriesGroupBy — `| count_over_time() by
// (resource.service.name)` returns one InstantSeries per unique service,
// each carrying a single Value. Order across series is deterministic
// (sorted by canonical label-set key).
func TestMetricsQueryInstant_MultiSeriesGroupBy(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	q := &stubQuerier{samples: []chclient.Sample{
		{Labels: map[string]string{"resource.service.name": "frontend"}, Timestamp: ts, Value: 12},
		{Labels: map[string]string{"resource.service.name": "backend"}, Timestamp: ts, Value: 3},
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryInstantURL(srv.URL,
		"{} | count_over_time() by (resource.service.name)",
		map[string]string{
			"start": fixtureStartUnix,
			"end":   fixtureEndUnix,
		})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	var body tempo.MetricsQueryInstantResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Series) != 2 {
		t.Fatalf("expected 2 series, got %d: %+v", len(body.Series), body)
	}
	byService := map[string]tempo.MetricsInstantSeries{}
	for _, s := range body.Series {
		if len(s.Labels) != 1 || s.Labels[0].Key != "resource.service.name" {
			t.Errorf("unexpected labels: %+v", s.Labels)
			continue
		}
		byService[s.Labels[0].Value] = s
	}
	if byService["frontend"].Value != 12 {
		t.Errorf("expected frontend=12, got %v", byService["frontend"].Value)
	}
	if byService["backend"].Value != 3 {
		t.Errorf("expected backend=3, got %v", byService["backend"].Value)
	}
}

// TestMetricsQueryInstant_FirstSamplePerSeries — when the inner range
// envelope emits multiple anchors per series (a defensive case for
// future RangeWindow shapes), the handler picks the *first* sample
// (sorted ascending by timestamp), matching Tempo's
// translateQueryRangeToInstant rule upstream.
func TestMetricsQueryInstant_FirstSamplePerSeries(t *testing.T) {
	t.Parallel()

	ts := func(min int) time.Time {
		return time.Date(2026, 5, 12, 10, min, 0, 0, time.UTC)
	}
	q := &stubQuerier{samples: []chclient.Sample{
		// Intentionally out of order: handler must pick the earliest TS.
		{Labels: map[string]string{}, Timestamp: ts(2), Value: 99},
		{Labels: map[string]string{}, Timestamp: ts(0), Value: 7},
		{Labels: map[string]string{}, Timestamp: ts(1), Value: 50},
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryInstantURL(srv.URL, "{} | rate()", map[string]string{
		"start": fixtureStartUnix,
		"end":   fixtureEndUnix,
	})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var body tempo.MetricsQueryInstantResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Series) != 1 {
		t.Fatalf("expected 1 series, got %d", len(body.Series))
	}
	if body.Series[0].Value != 7 {
		t.Errorf("expected first-sample value 7 (ts=10:00), got %v", body.Series[0].Value)
	}
}

// TestMetricsQueryInstant_EmptyResult — when CH returns zero rows, the
// envelope is `{series: []}` (not `null`). Matches the range handler's
// behaviour so Grafana renders "no data" rather than a JSON-shape error.
func TestMetricsQueryInstant_EmptyResult(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: nil}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryInstantURL(srv.URL, "{} | rate()", map[string]string{
		"start": fixtureStartUnix,
		"end":   fixtureEndUnix,
	})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var body tempo.MetricsQueryInstantResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Series == nil {
		t.Fatalf("expected non-nil empty Series slice, got nil (JSON null)")
	}
	if len(body.Series) != 0 {
		t.Fatalf("expected 0 series, got %d", len(body.Series))
	}
}

// TestMetricsQueryInstant_QueryParamAliases — Tempo's
// ParseQueryInstantRequest accepts both `q` and `query` for the TraceQL
// expression (`query` is the prom-compat alias Grafana still emits on
// some panels). Both forms must reach the handler.
func TestMetricsQueryInstant_QueryParamAliases(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"q", "query"} {
		key := key
		t.Run("param="+key, func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{samples: []chclient.Sample{{
				Labels:    map[string]string{},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
				Value:     1.0,
			}}}
			srv := newServer(q, "v1.0.0-test")
			t.Cleanup(srv.Close)

			vals := url.Values{}
			vals.Set(key, "{} | rate()")
			vals.Set("start", fixtureStartUnix)
			vals.Set("end", fixtureEndUnix)
			u := srv.URL + "/api/metrics/query?" + vals.Encode()
			resp, err := http.Get(u)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
			}
		})
	}
}

// TestMetricsQueryInstant_BadInputs covers the 4xx surface: missing or
// malformed `q` / `start` / `end`, and a non-metric TraceQL query.
func TestMetricsQueryInstant_BadInputs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		params  map[string]string
		wantSub string
	}{
		{
			name:    "missing_q",
			params:  map[string]string{"start": fixtureStartUnix, "end": fixtureEndUnix},
			wantSub: "missing 'q'",
		},
		{
			name:    "missing_start",
			params:  map[string]string{"q": "{} | rate()", "end": fixtureEndUnix},
			wantSub: "required",
		},
		{
			name:    "missing_end",
			params:  map[string]string{"q": "{} | rate()", "start": fixtureStartUnix},
			wantSub: "required",
		},
		{
			name: "malformed_time",
			params: map[string]string{
				"q":     "{} | rate()",
				"start": "not-a-time",
				"end":   fixtureEndUnix,
			},
			wantSub: "time",
		},
		{
			name: "end_before_start",
			params: map[string]string{
				"q":     "{} | rate()",
				"start": fixtureEndUnix,
				"end":   fixtureStartUnix,
			},
			wantSub: "before",
		},
		{
			name: "parse_error",
			params: map[string]string{
				"q":     "this is not traceql {{{",
				"start": fixtureStartUnix,
				"end":   fixtureEndUnix,
			},
			wantSub: "",
		},
		{
			name: "non_metric_query",
			params: map[string]string{
				// Bare spanset with no metrics pipeline — lowers to a
				// Filter(Scan), not a MetricsAggregate.
				"q":     `{ resource.service.name = "frontend" }`,
				"start": fixtureStartUnix,
				"end":   fixtureEndUnix,
			},
			wantSub: "metrics-pipeline",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{}
			srv := newServer(q, "v1.0.0-test")
			t.Cleanup(srv.Close)

			vals := url.Values{}
			for k, v := range tc.params {
				vals.Set(k, v)
			}
			u := srv.URL + "/api/metrics/query?" + vals.Encode()

			resp, err := http.Get(u)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode < 400 || resp.StatusCode >= 500 {
				t.Fatalf("expected 4xx, got status=%d body=%s",
					resp.StatusCode, readBody(t, resp))
			}
			var er tempo.ErrorResponse
			body := readBody(t, resp)
			if err := json.Unmarshal([]byte(body), &er); err != nil {
				t.Fatalf("decode: %v body=%s", err, body)
			}
			if !er.Error {
				t.Errorf("expected error=true, got %+v", er)
			}
			if tc.wantSub != "" && !strings.Contains(er.Message, tc.wantSub) {
				t.Errorf("error message missing %q: got %q", tc.wantSub, er.Message)
			}
		})
	}
}

// TestMetricsQueryInstant_CHFailure — a CH-side error surfaces as 502
// plus the Tempo error envelope, matching the range handler.
func TestMetricsQueryInstant_CHFailure(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: errors.New("clickhouse: connection refused")}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryInstantURL(srv.URL, "{} | rate()", map[string]string{
		"start": fixtureStartUnix,
		"end":   fixtureEndUnix,
	})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 body=%s", resp.StatusCode, readBody(t, resp))
	}
	var er tempo.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !er.Error || !strings.Contains(er.Message, "connection refused") {
		t.Errorf("expected error envelope with upstream message; got %+v", er)
	}
}
