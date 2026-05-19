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
// the TraceQL `q` parameter — TraceQL queries always contain `{` / `}`,
// quotes, and pipes, so url.QueryEscape is mandatory.
func metricsQueryRangeURL(base, q string, params map[string]string) string {
	vals := url.Values{}
	vals.Set("q", q)
	for k, v := range params {
		vals.Set(k, v)
	}
	return base + "/api/metrics/query_range?" + vals.Encode()
}

// fixtureStart / fixtureEnd are the canonical test-time bookends used
// across handler tests in this package; matches handler_test.go's
// 2026-05-12T10:00:00Z anchor so a future seed swap doesn't have to
// touch every test.
const (
	fixtureStartUnix = "1778580000" // 2026-05-12T10:00:00Z
	fixtureEndUnix   = "1778580180" // +3m
)

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
		"start": fixtureStartUnix,
		"end":   fixtureEndUnix,
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
// (resource.service.name)` returns one series per unique service.
// Labels surface as {key,value} pairs in the response, ordered to match
// the by(...) attribute list.
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
			"start": fixtureStartUnix,
			"end":   fixtureEndUnix,
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
// response is `{series: []}` (not `null`). Grafana's Tempo datasource
// short-circuits on a `null` series array, which would surface as a
// dashboard "no data" badge for a healthy gateway with no spans yet.
func TestMetricsQueryRange_EmptyResult(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: nil}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL, "{} | rate()", map[string]string{
		"start": fixtureStartUnix,
		"end":   fixtureEndUnix,
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
			params:  map[string]string{"start": fixtureStartUnix, "end": fixtureEndUnix, "step": "30s"},
			wantSub: "missing 'q'",
		},
		{
			name:    "missing_step",
			params:  map[string]string{"q": "{} | rate()", "start": fixtureStartUnix, "end": fixtureEndUnix},
			wantSub: "missing 'step'",
		},
		{
			name:    "missing_start",
			params:  map[string]string{"q": "{} | rate()", "end": fixtureEndUnix, "step": "30s"},
			wantSub: "required",
		},
		{
			name:    "missing_end",
			params:  map[string]string{"q": "{} | rate()", "start": fixtureStartUnix, "step": "30s"},
			wantSub: "required",
		},
		{
			name: "malformed_step",
			params: map[string]string{
				"q":     "{} | rate()",
				"start": fixtureStartUnix,
				"end":   fixtureEndUnix,
				"step":  "five-minutes",
			},
			wantSub: "step",
		},
		{
			name: "zero_step",
			params: map[string]string{
				"q":     "{} | rate()",
				"start": fixtureStartUnix,
				"end":   fixtureEndUnix,
				"step":  "0s",
			},
			wantSub: "step",
		},
		{
			name: "malformed_time",
			params: map[string]string{
				"q":     "{} | rate()",
				"start": "not-a-time",
				"end":   fixtureEndUnix,
				"step":  "30s",
			},
			wantSub: "time",
		},
		{
			name: "end_before_start",
			params: map[string]string{
				"q":     "{} | rate()",
				"start": fixtureEndUnix,
				"end":   fixtureStartUnix,
				"step":  "30s",
			},
			wantSub: "before",
		},
		{
			name: "parse_error",
			params: map[string]string{
				"q":     "this is not traceql {{{",
				"start": fixtureStartUnix,
				"end":   fixtureEndUnix,
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
				"start": fixtureStartUnix,
				"end":   fixtureEndUnix,
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
// the Tempo error envelope so Grafana renders the right "data source
// error" UI rather than a generic 5xx page.
func TestMetricsQueryRange_CHFailure(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: errors.New("clickhouse: connection refused")}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL, "{} | rate()", map[string]string{
		"start": fixtureStartUnix,
		"end":   fixtureEndUnix,
		"step":  "30s",
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

// TestMetricsQueryRange_LabelWireShape pins the tempopb KeyValue +
// AnyValue wire shape for `{"labels":[...]}`. Grafana's Tempo datasource
// (and any other consumer parsing through `gogo/protobuf/jsonpb` against
// `pkg/tempopb/common/v1.KeyValue`) requires the typed AnyValue envelope
// `{"key":"k","value":{"stringValue":"v"}}` — the flat string form
// cerberus used to emit (`{"key":"k","value":"v"}`) silently round-trips
// to an empty AnyValue on the consumer side. EF #398 caught this in the
// tempo-compatibility harness; this test pins the fixed wire shape so a
// future refactor can't quietly regress it.
func TestMetricsQueryRange_LabelWireShape(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: []chclient.Sample{{
		Labels:    map[string]string{"resource.service.name": "frontend"},
		Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
		Value:     1.0,
	}}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL,
		"{} | count_over_time() by (resource.service.name)",
		map[string]string{
			"start": fixtureStartUnix,
			"end":   fixtureEndUnix,
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
	// Read raw body so we assert wire shape without going through the
	// MarshalJSON we just defined (i.e. the assertion is on the bytes
	// Grafana actually receives).
	raw := readBody(t, resp)
	wantSub := `"value":{"stringValue":"frontend"}`
	if !strings.Contains(raw, wantSub) {
		t.Fatalf("response missing tempopb AnyValue shape %q\nbody=%s", wantSub, raw)
	}
	// And the legacy flat shape MUST NOT appear.
	badSub := `"value":"frontend"`
	if strings.Contains(raw, badSub) {
		t.Errorf("response still emits legacy flat label shape %q\nbody=%s", badSub, raw)
	}

	// Decode through MetricsQueryRangeResponse and verify the in-process
	// struct still surfaces `frontend` so handler callers stay ergonomic.
	var body tempo.MetricsQueryRangeResponse
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode tempopb shape: %v", err)
	}
	if len(body.Series) != 1 || len(body.Series[0].Labels) != 1 {
		t.Fatalf("unexpected shape: %+v", body)
	}
	if body.Series[0].Labels[0].Key != "resource.service.name" ||
		body.Series[0].Labels[0].Value != "frontend" {
		t.Errorf("decoded label = %+v, want {resource.service.name, frontend}",
			body.Series[0].Labels[0])
	}

	// Round-trip: also tolerate the legacy flat shape so an old consumer
	// pushing data into the type (or an older replay fixture) doesn't
	// break the decoder side of the contract.
	legacyJSON := []byte(`{"series":[{"labels":[{"key":"k","value":"v"}],"samples":[]}]}`)
	var legacy tempo.MetricsQueryRangeResponse
	if err := json.Unmarshal(legacyJSON, &legacy); err != nil {
		t.Fatalf("decode legacy flat shape: %v", err)
	}
	if len(legacy.Series) != 1 || len(legacy.Series[0].Labels) != 1 ||
		legacy.Series[0].Labels[0].Key != "k" || legacy.Series[0].Labels[0].Value != "v" {
		t.Errorf("legacy flat decode = %+v, want {k, v}", legacy.Series[0].Labels)
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
				"start": fixtureStartUnix,
				"end":   fixtureEndUnix,
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

// TestMetricsQueryRange_ExemplarsEnvelope pins the wire-shape contract
// that every MetricsSeries emits `"exemplars": []` even before cerberus
// populates them. Grafana's Tempo datasource expects the field, so
// omitting it (or rendering it as null) destabilises the envelope. See
// EF #398 for the broader Tempo metrics shape parity work.
//
// Sub-test guard: feed a stub that returns the matrix-shape sample but
// NO exemplar samples (samplesBySQL with an `exemplar_trace_id` needle
// returns nil). The empty-array envelope contract still holds.
func TestMetricsQueryRange_ExemplarsEnvelope(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		samples: []chclient.Sample{{
			Labels:    map[string]string{},
			Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
			Value:     1.0,
		}},
		samplesBySQL: map[string][]chclient.Sample{
			"exemplar_trace_id": nil,
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL, "{} | rate()", map[string]string{
		"start": fixtureStartUnix,
		"end":   fixtureEndUnix,
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
	raw := readBody(t, resp)
	if !strings.Contains(raw, `"exemplars":[]`) {
		t.Fatalf("response missing empty exemplars array\nbody=%s", raw)
	}
}

// TestMetricsQueryRange_ExemplarsPopulated exercises the end-to-end
// data path now that the exemplars query landed: the handler fires
// BOTH a matrix-shape query (anchor-fanout / Sample projection) and an
// exemplars-shape query (argMax over `exemplar_trace_id` /
// `exemplar_span_id`), then merges them so each MetricsSeries surfaces
// trace-anchored Exemplar entries.
//
// Stub layout: the matrix branch returns one sample per anchor; the
// exemplars branch returns a Sample whose Labels map carries the
// `trace:id`, `span:id`, and the by(...) alias values — same shape
// chsql.EmitMetricsExemplars projects via the outer `map(...) AS
// Attributes` column. attachExemplars keys each exemplar against its
// matching series via the by(...) label canonical key.
//
// Note: the stubbed Labels map keys use the Tempo-canonical wire form
// (`resource.service.name`) rather than the SQL-side alias
// (`service.name`). The chsql emitter projects the inner SELECT with
// `ResourceAttributes['service.name'] AS service.name` (the SQL alias
// drops the scope prefix to keep column names compact), but the outer
// matrix and exemplar projections key the `Attributes` map by the
// scope-prefixed display name so the wire shape matches upstream
// Tempo's metrics-query response. attachExemplars matches each
// exemplar to its parent series via canonical label-set hash, so both
// the matrix branch and exemplars branch must agree on the key form.
func TestMetricsQueryRange_ExemplarsPopulated(t *testing.T) {
	t.Parallel()

	ts := func(min int) time.Time {
		return time.Date(2026, 5, 12, 10, min, 0, 0, time.UTC)
	}
	q := &stubQuerier{
		samples: []chclient.Sample{
			// Matrix branch — one series, two anchors. Labels are keyed
			// by the Tempo-canonical scope-prefixed wire name
			// (`resource.service.name`), matching what
			// wrapMetricsForSample projects via the outer `Attributes`
			// map.
			{
				Labels:    map[string]string{"resource.service.name": "frontend"},
				Timestamp: ts(0),
				Value:     12,
			},
			{
				Labels:    map[string]string{"resource.service.name": "frontend"},
				Timestamp: ts(1),
				Value:     18,
			},
		},
		samplesBySQL: map[string][]chclient.Sample{
			// Exemplars branch — one trace-anchored sample per anchor,
			// carrying the trace:id + span:id pair attachExemplars
			// surfaces under Exemplar.TraceID / SpanID. The by(...)
			// display key (`resource.service.name`) lets
			// attachExemplars match the exemplar back to its parent
			// series by canonical label-set hash.
			"exemplar_trace_id": {
				{
					Labels: map[string]string{
						"resource.service.name": "frontend",
						"trace:id":              "0123456789abcdef0123456789abcdef",
						"span:id":               "0011223344556677",
					},
					Timestamp: ts(0),
					Value:     1,
				},
				{
					Labels: map[string]string{
						"resource.service.name": "frontend",
						"trace:id":              "fedcba9876543210fedcba9876543210",
						"span:id":               "aabbccddeeff0011",
					},
					Timestamp: ts(1),
					Value:     1,
				},
			},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL,
		"{} | count_over_time() by (resource.service.name)",
		map[string]string{
			"start": fixtureStartUnix,
			"end":   fixtureEndUnix,
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

	// The handler must have issued BOTH the matrix-shape query and the
	// exemplars-shape query (recorded via samplesBySQL needle).
	var matrixFired, exemplarFired bool
	for _, sql := range q.queriedSQLs {
		if strings.Contains(sql, "exemplar_trace_id") {
			exemplarFired = true
		} else if strings.Contains(sql, "arrayJoin") {
			matrixFired = true
		}
	}
	if !matrixFired {
		t.Errorf("expected a matrix-shape (arrayJoin fanout) query to fire; saw %d SQL statements", len(q.queriedSQLs))
	}
	if !exemplarFired {
		t.Errorf("expected an exemplars-shape (argMax over exemplar_trace_id) query to fire; saw %d SQL statements", len(q.queriedSQLs))
	}

	var body tempo.MetricsQueryRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Series) != 1 {
		t.Fatalf("expected 1 series, got %d: %+v", len(body.Series), body)
	}
	got := body.Series[0]
	if len(got.Samples) != 2 {
		t.Errorf("expected 2 samples, got %d", len(got.Samples))
	}
	if len(got.Exemplars) != 2 {
		t.Fatalf("expected 2 exemplars, got %d: %+v", len(got.Exemplars), got.Exemplars)
	}

	// Exemplars must be sorted ascending by timestamp and carry the
	// stubbed (TraceID, SpanID) pairs verbatim — attachExemplars
	// reads `trace:id` / `span:id` off Sample.Labels and projects them
	// to the Exemplar.{TraceID,SpanID} fields plus the labels slice.
	if got.Exemplars[0].Timestamp >= got.Exemplars[1].Timestamp {
		t.Errorf("exemplars not sorted ascending: %+v", got.Exemplars)
	}
	wantTraceIDs := []string{
		"0123456789abcdef0123456789abcdef",
		"fedcba9876543210fedcba9876543210",
	}
	wantSpanIDs := []string{
		"0011223344556677",
		"aabbccddeeff0011",
	}
	for i, want := range wantTraceIDs {
		if got.Exemplars[i].TraceID != want {
			t.Errorf("exemplar[%d].TraceID = %q, want %q", i, got.Exemplars[i].TraceID, want)
		}
	}
	for i, want := range wantSpanIDs {
		if got.Exemplars[i].SpanID != want {
			t.Errorf("exemplar[%d].SpanID = %q, want %q", i, got.Exemplars[i].SpanID, want)
		}
	}

	// And the Exemplar.Labels slice must carry the trace:id + span:id
	// MetricsLabel entries so consumers binding to the labels array
	// (rather than the typed scalar fields) see the same data.
	for i, ex := range got.Exemplars {
		if len(ex.Labels) != 2 {
			t.Errorf("exemplar[%d].Labels: got %d entries, want 2: %+v", i, len(ex.Labels), ex.Labels)
			continue
		}
		if ex.Labels[0].Key != "trace:id" || ex.Labels[0].Value != wantTraceIDs[i] {
			t.Errorf("exemplar[%d].Labels[0] = %+v, want {trace:id, %q}", i, ex.Labels[0], wantTraceIDs[i])
		}
		if ex.Labels[1].Key != "span:id" || ex.Labels[1].Value != wantSpanIDs[i] {
			t.Errorf("exemplar[%d].Labels[1] = %+v, want {span:id, %q}", i, ex.Labels[1], wantSpanIDs[i])
		}
	}
}

// TestMetricsQueryRange_ResourceLabelWireShape conforms cerberus's
// metrics-query response to upstream Tempo's wire shape for
// resource-scoped group-by labels.
//
// Upstream Tempo emits the full scope-prefixed form
// `resource.service.name` on the response Labels list for a
// `by (resource.service.name)` clause (see grafana/tempo
// `pkg/traceql.Attribute.String` and `engine_metrics.go::labelsFor`;
// the upstream integration test `integration/api/query_range_test.go`
// pins `label.Key == "resource.res_attr"` for the equivalent query).
//
// Cerberus's SQL emitter aliases the inner SELECT column as the bare
// path (`AS service.name`) to keep CH column names short, but the
// matrix-shape outer wrap projects the `Attributes` map with the
// Tempo-canonical scope-prefixed key — so the chclient.Sample.Labels
// the handler decodes carries `resource.service.name`, and the JSON
// envelope mirrors that. This test feeds a stub Sample whose Labels
// map uses the wire-canonical key (matching what the matrix SQL would
// produce post-wrap) and asserts the JSON `labels[].key` reads
// `resource.service.name`.
func TestMetricsQueryRange_ResourceLabelWireShape(t *testing.T) {
	t.Parallel()

	ts := func(min int) time.Time {
		return time.Date(2026, 5, 12, 10, min, 0, 0, time.UTC)
	}
	q := &stubQuerier{samples: []chclient.Sample{
		// The matrix outer SELECT emits `map('resource.service.name', toString(service.name), ...)`
		// — see wrapMetricsForSample. The decoded Sample.Labels map is
		// therefore keyed by the scope-prefixed wire form, not the bare
		// SQL alias. The stub mirrors that contract.
		{Labels: map[string]string{"resource.service.name": "frontend"}, Timestamp: ts(0), Value: 12},
		{Labels: map[string]string{"resource.service.name": "frontend"}, Timestamp: ts(1), Value: 18},
		{Labels: map[string]string{"resource.service.name": "backend"}, Timestamp: ts(0), Value: 3},
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL,
		"{} | count_over_time() by (resource.service.name)",
		map[string]string{
			"start": fixtureStartUnix,
			"end":   fixtureEndUnix,
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

	raw := readBody(t, resp)
	// The Tempo-canonical wire key MUST appear verbatim (the assertion
	// is on the bytes Grafana / a tempopb consumer actually reads).
	wantSub := `"key":"resource.service.name"`
	if !strings.Contains(raw, wantSub) {
		t.Fatalf("response missing wire-canonical resource-scope key %q\nbody=%s", wantSub, raw)
	}
	// And the bare-alias form (the SQL-side alias) MUST NOT appear as a
	// response key — that would mean the wrap leaked the alias to the
	// wire instead of the scope-prefixed display name.
	badSub := `"key":"service.name"`
	if strings.Contains(raw, badSub) {
		t.Errorf("response surfaces bare SQL alias %q where the Tempo-canonical form is required\nbody=%s",
			badSub, raw)
	}

	// Decode the response through the typed struct and verify every
	// series's first (and only) label carries the prefixed key.
	var body tempo.MetricsQueryRangeResponse
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Series) != 2 {
		t.Fatalf("expected 2 series, got %d: %+v", len(body.Series), body)
	}
	for i, s := range body.Series {
		if len(s.Labels) != 1 {
			t.Errorf("series[%d]: expected 1 label, got %+v", i, s.Labels)
			continue
		}
		if s.Labels[0].Key != "resource.service.name" {
			t.Errorf("series[%d].Labels[0].Key = %q, want %q",
				i, s.Labels[0].Key, "resource.service.name")
		}
	}
}

