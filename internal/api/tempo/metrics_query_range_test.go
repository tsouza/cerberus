package tempo_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
)

// metricsQueryRangeURL builds the test URL with proper escaping for
// the TraceQL `q` parameter — TraceQL queries always contain `{`/`}`,
// quotes, and pipes, so url.QueryEscape is mandatory.
func metricsQueryRangeURL(base, q string, params map[string]string) string {
	vals := url.Values{}
	vals.Set("q", q)
	for k, v := range params {
		vals.Set(k, v)
	}
	return base + "/api/metrics/query_range?" + vals.Encode()
}

// TestMetricsQueryRange_SingleSeriesNoGroupBy — a bare `| rate()` over
// the full spans table returns a single series (no labels). Three
// anchor rows in, three samples out, sorted by timestamp ascending.
func TestMetricsQueryRange_SingleSeriesNoGroupBy(t *testing.T) {
	t.Parallel()

	ts := func(min int) time.Time {
		return time.Date(2026, 5, 12, 10, min, 0, 0, time.UTC)
	}
	q := &stubQuerier{samples: []chclient.Sample{
		// Intentionally out of order — handler must sort within each series.
		{MetricName: "", Labels: map[string]string{}, Timestamp: ts(2), Value: 2.0},
		{MetricName: "", Labels: map[string]string{}, Timestamp: ts(0), Value: 0.5},
		{MetricName: "", Labels: map[string]string{}, Timestamp: ts(1), Value: 1.5},
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL, "{} | rate()", map[string]string{
		"start": "1747044000", // 2025-05-12T10:00:00Z
		"end":   "1747044180", // +3m
		"step":  "60s",
	})

	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	var body tempo.MetricsQueryRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Series) != 1 {
		t.Fatalf("expected 1 series, got %d: %+v", len(body.Series), body)
	}
	s := body.Series[0]
	if len(s.Labels) != 0 {
		t.Errorf("expected zero labels for ungrouped query, got %+v", s.Labels)
	}
	if len(s.Samples) != 3 {
		t.Fatalf("expected 3 samples, got %d", len(s.Samples))
	}
	// Sorted ascending: 0.5, 1.5, 2.0 in that order.
	for i, want := range []float64{0.5, 1.5, 2.0} {
		if s.Samples[i].Value != want {
			t.Errorf("sample[%d].Value = %v, want %v", i, s.Samples[i].Value, want)
		}
	}
	if s.Samples[0].TimestampMs >= s.Samples[1].TimestampMs ||
		s.Samples[1].TimestampMs >= s.Samples[2].TimestampMs {
		t.Errorf("samples not sorted ascending by timestamp: %+v", s.Samples)
	}

	// The handler should have asked the matrix-shape SQL emitter (an
	// arrayJoin fanout). Probe a few hallmark substrings to confirm
	// we're hitting emitRangeWindowMetrics and not a Sprintf fallback.
	assertSQLContains(t, q.lastSQL, "arrayJoin")
	assertSQLContains(t, q.lastSQL, "anchor_ts")
}

// TestMetricsQueryRange_MultiSeriesGroupBy — `| count_over_time() by
// (service.name)` returns one series per unique service. Labels
// surface as {key,value} pairs in the response.
func TestMetricsQueryRange_MultiSeriesGroupBy(t *testing.T) {
	t.Parallel()

	ts := func(min int) time.Time {
		return time.Date(2026, 5, 12, 10, min, 0, 0, time.UTC)
	}
	q := &stubQuerier{samples: []chclient.Sample{
		{Labels: map[string]string{"resource.service.name": "frontend"}, Timestamp: ts(0), Value: 12},
		{Labels: map[string]string{"resource.service.name": "frontend"}, Timestamp: ts(1), Value: 18},
		{Labels: map[string]string{"resource.service.name": "backend"}, Timestamp: ts(0), Value: 3},
		{Labels: map[string]string{"resource.service.name": "backend"}, Timestamp: ts(1), Value: 5},
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL,
		"{} | count_over_time() by (resource.service.name)",
		map[string]string{
			"start": "1747044000",
			"end":   "1747044120",
			"step":  "60s",
		})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	var body tempo.MetricsQueryRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Series) != 2 {
		t.Fatalf("expected 2 series, got %d: %+v", len(body.Series), body)
	}

	// Each series should have exactly one label entry whose Key is the
	// by(...) attribute name (TraceQL: resource.service.name).
	byService := map[string]tempo.MetricsSeries{}
	for _, s := range body.Series {
		if len(s.Labels) != 1 {
			t.Errorf("expected 1 label per series, got %+v", s.Labels)
			continue
		}
		if s.Labels[0].Key != "resource.service.name" {
			t.Errorf("expected label key 'resource.service.name', got %q", s.Labels[0].Key)
		}
		byService[s.Labels[0].Value] = s
	}
	if _, ok := byService["frontend"]; !ok {
		t.Errorf("missing 'frontend' series: %+v", body.Series)
	}
	if _, ok := byService["backend"]; !ok {
		t.Errorf("missing 'backend' series: %+v", body.Series)
	}
	if len(byService["frontend"].Samples) != 2 {
		t.Errorf("expected 2 samples for frontend, got %d", len(byService["frontend"].Samples))
	}
}

// TestMetricsQueryRange_EmptyResult — when CH returns zero rows the
// response is `{series: []}` (not `null`).
func TestMetricsQueryRange_EmptyResult(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: nil}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL, "{} | rate()", map[string]string{
		"start": "1747044000",
		"end":   "1747044060",
		"step":  "30s",
	})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var body tempo.MetricsQueryRangeResponse
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

// TestMetricsQueryRange_BadInputs covers the 4xx surface: missing or
// malformed `q` / `start` / `end` / `step`, and a non-metric TraceQL
// query (one that lowers to a Scan/Filter rather than a
// MetricsAggregate).
func TestMetricsQueryRange_BadInputs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		params  map[string]string
		wantSub string
	}{
		{
			name:    "missing_q",
			params:  map[string]string{"start": "1747044000", "end": "1747044060", "step": "30s"},
			wantSub: "missing 'q'",
		},
		{
			name:    "missing_step",
			params:  map[string]string{"q": "{} | rate()", "start": "1747044000", "end": "1747044060"},
			wantSub: "missing 'step'",
		},
		{
			name:    "missing_start",
			params:  map[string]string{"q": "{} | rate()", "end": "1747044060", "step": "30s"},
			wantSub: "required",
		},
		{
			name:    "missing_end",
			params:  map[string]string{"q": "{} | rate()", "start": "1747044000", "step": "30s"},
			wantSub: "required",
		},
		{
			name: "malformed_step",
			params: map[string]string{
				"q":     "{} | rate()",
				"start": "1747044000",
				"end":   "1747044060",
				"step":  "five-minutes",
			},
			wantSub: "step",
		},
		{
			name: "zero_step",
			params: map[string]string{
				"q":     "{} | rate()",
				"start": "1747044000",
				"end":   "1747044060",
				"step":  "0s",
			},
			wantSub: "step",
		},
		{
			name: "malformed_time",
			params: map[string]string{
				"q":     "{} | rate()",
				"start": "not-a-time",
				"end":   "1747044060",
				"step":  "30s",
			},
			wantSub: "time",
		},
		{
			name: "end_before_start",
			params: map[string]string{
				"q":     "{} | rate()",
				"start": "1747044060",
				"end":   "1747044000",
				"step":  "30s",
			},
			wantSub: "before",
		},
		{
			name: "parse_error",
			params: map[string]string{
				"q":     "this is not traceql {{{",
				"start": "1747044000",
				"end":   "1747044060",
				"step":  "30s",
			},
			wantSub: "",
		},
		{
			name: "non_metric_query",
			params: map[string]string{
				// Bare spanset with no metrics pipeline — lowers to a
				// Filter(Scan), not a MetricsAggregate.
				"q":     `{ resource.service.name = "frontend" }`,
				"start": "1747044000",
				"end":   "1747044060",
				"step":  "30s",
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
			u := srv.URL + "/api/metrics/query_range?" + vals.Encode()

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

// TestMetricsQueryRange_CHFailure — a CH-side error surfaces as 502 +
// the Tempo error envelope.
func TestMetricsQueryRange_CHFailure(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: errors.New("clickhouse: connection refused")}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL, "{} | rate()", map[string]string{
		"start": "1747044000",
		"end":   "1747044060",
		"step":  "30s",
	})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", resp.StatusCode)
	}
	var er tempo.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !er.Error || !strings.Contains(er.Message, "connection refused") {
		t.Errorf("expected error envelope with upstream message; got %+v", er)
	}
}

// TestMetricsQueryRange_StepDurationForms — accepts integer seconds,
// float seconds, and Go duration strings interchangeably. Matches the
// PromQL handler's tolerance so Grafana's Tempo datasource (which can
// send either shape) interoperates.
func TestMetricsQueryRange_StepDurationForms(t *testing.T) {
	t.Parallel()

	for _, step := range []string{"30s", "0.5m", "30", "1m"} {
		step := step
		t.Run(fmt.Sprintf("step=%s", step), func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{samples: []chclient.Sample{{
				Labels:    map[string]string{},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
				Value:     1.0,
			}}}
			srv := newServer(q, "v1.0.0-test")
			t.Cleanup(srv.Close)
			u := metricsQueryRangeURL(srv.URL, "{} | rate()", map[string]string{
				"start": "1747044000",
				"end":   "1747044060",
				"step":  step,
			})
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
