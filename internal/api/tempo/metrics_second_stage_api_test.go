package tempo_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
)

// TestMetricsQueryRange_TopKSecondStage pins the wiring fixed by the
// showcase-traceql wave: `{} | rate() by (...) | topk(N)` lowers to a
// chplan.MetricsSecondStage wrapping the aggregate, and before the
// handler learned to peel + re-apply the wrap around the RangeWindow,
// unwrapMetricsAggregate failed on the wrapper type and the endpoint
// returned 400 "is not a TraceQL metrics-pipeline expression" for
// every second-stage query — even though the chsql emit support had
// landed in #437. The matrix path must stamp the per-anchor partition
// (`LIMIT 3 BY anchor_ts`) so topk selects per timestamp, matching
// Tempo's processTopK semantics.
func TestMetricsQueryRange_TopKSecondStage(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	q := &stubQuerier{samples: []chclient.Sample{
		{Labels: map[string]string{"service.name": "api"}, Timestamp: ts, Value: 2.0},
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL,
		`{} | rate() by (resource.service.name) | topk(3)`,
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
	if len(body.Series) != 1 {
		t.Fatalf("expected 1 series, got %d: %+v", len(body.Series), body)
	}

	// The matrix-shape SQL must carry the per-anchor LIMIT BY: a global
	// `LIMIT 3` would return three (series, anchor) rows total instead
	// of the top-3 series at every step.
	var matrixSQL string
	for _, sql := range q.queriedSQLs {
		if strings.Contains(sql, "LIMIT 3") {
			matrixSQL = sql
			break
		}
	}
	if matrixSQL == "" {
		t.Fatalf("no executed SQL carried the topk LIMIT; queried: %v", q.queriedSQLs)
	}
	if !strings.Contains(matrixSQL, "LIMIT 3 BY `anchor_ts`") &&
		!strings.Contains(matrixSQL, "LIMIT 3 BY anchor_ts") {
		t.Fatalf("topk matrix SQL lacks the per-anchor partition:\n%s", matrixSQL)
	}
	if !strings.Contains(matrixSQL, "ORDER BY `Value` DESC") {
		t.Fatalf("topk matrix SQL lacks ORDER BY Value DESC:\n%s", matrixSQL)
	}
}

// TestMetricsQueryRange_ThresholdSecondStage — the `| > N` metrics
// filter re-applies as a WHERE over the windowed aggregate's Value.
func TestMetricsQueryRange_ThresholdSecondStage(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: []chclient.Sample{}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL,
		`{} | rate() by (resource.service.name) > 0.05`,
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
	var found bool
	for _, sql := range q.queriedSQLs {
		if strings.Contains(sql, "WHERE (`Value` > toFloat64(?))") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no executed SQL carried the threshold WHERE; queried: %v", q.queriedSQLs)
	}
}

// TestMetricsQueryInstant_TopKSecondStage — the instant path applies
// topk with a global LIMIT (no PartitionBy): one anchor, one selection.
func TestMetricsQueryInstant_TopKSecondStage(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: []chclient.Sample{}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := srv.URL + "/api/metrics/query?q=" +
		"%7B%7D%20%7C%20rate()%20by%20(resource.service.name)%20%7C%20topk(2)" +
		"&start=" + fixtureStartUnix + "&end=" + fixtureEndUnix

	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	if !strings.Contains(q.lastSQL, "ORDER BY `Value` DESC LIMIT 2") {
		t.Fatalf("instant topk SQL lacks the global LIMIT:\n%s", q.lastSQL)
	}
	if strings.Contains(q.lastSQL, "LIMIT 2 BY") {
		t.Fatalf("instant topk SQL must not partition by anchor:\n%s", q.lastSQL)
	}
}

// TestMetricsQueryRange_SecondStageOverQuantileRejected pins the
// documented boundary: quantiles fold from bucket rows Go-side, so a
// SQL-side rank/threshold would operate on bucket counts. 422 with a
// descriptive message, never a wrong answer.
func TestMetricsQueryRange_SecondStageOverQuantileRejected(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL,
		`{} | quantile_over_time(duration, .9) | topk(3)`,
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
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422; body=%s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "second-stage topk over quantile_over_time is unsupported") {
		t.Fatalf("422 body lacks the boundary message: %s", body)
	}
}
