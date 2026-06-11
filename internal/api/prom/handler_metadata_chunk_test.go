package prom_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
)

// maxQuerySizeBound is the safe ceiling the chunked combined-query
// builders must stay under. ClickHouse's default `max_query_size` is
// 256KB (262144 bytes); PR #790's un-chunked fan-in blew past it at
// position 262124 on the metrics-explorer broad probe. The chunk cap
// (maxMetricCandidatesPerQuery) is sized so any single combined query
// renders well under this — we assert 200KB to keep a comfortable margin
// for wider schemas.
const maxQuerySizeBound = 200 * 1024

// recordingQuerier captures every rendered SQL string the handler issues
// so a test can assert (a) round-trip counts and (b) per-query SQL size.
// It backs reads with the embedded stub samples so the handler completes
// a normal 200 response.
type recordingQuerier struct {
	samples []chclient.Sample

	querySQLs   []string
	stringsSQLs []string
}

func (r *recordingQuerier) Query(_ context.Context, sql string, _ ...any) ([]chclient.Sample, error) {
	r.querySQLs = append(r.querySQLs, sql)
	return r.samples, nil
}

func (r *recordingQuerier) QueryCursor(_ context.Context, _ string, _ ...any) (chclient.Cursor, error) {
	return newSliceCursor(r.samples), nil
}

func (r *recordingQuerier) QueryStrings(_ context.Context, sql string, _ ...any) ([]string, error) {
	r.stringsSQLs = append(r.stringsSQLs, sql)
	return nil, nil
}

func (r *recordingQuerier) QueryLabelSets(_ context.Context, _ string, _ ...any) ([]map[string]string, error) {
	return nil, nil
}

func (r *recordingQuerier) QueryMetricMeta(_ context.Context, _, _ string, _ ...any) ([]chclient.MetricMetaRow, error) {
	return nil, nil
}

func (r *recordingQuerier) QueryExemplars(_ context.Context, _ string, _ ...any) ([]chclient.ExemplarRow, error) {
	return nil, nil
}

var _ prom.Querier = (*recordingQuerier)(nil)

// largeMatchValues builds n distinct bare metric-name match[] selectors —
// the shape the metrics-explorer "every published metric" probe sends in
// one request. Each name carries rewritable underscores so it also drives
// the dotted-candidate fan-out, mirroring the production blowup.
func largeMatchValues(n int) url.Values {
	v := url.Values{}
	for i := 0; i < n; i++ {
		v.Add("match[]", fmt.Sprintf("metric_explorer_probe_series_%d", i))
	}
	return v
}

func maxLen(sqls []string) int {
	m := 0
	for _, s := range sqls {
		if len(s) > m {
			m = len(s)
		}
	}
	return m
}

// TestHandleSeries_ChunksUnderMaxQuerySize is the regression pin for the
// PR #790 follow-up: a broad /api/v1/series request (hundreds of match[]
// selectors, like the metrics-explorer probe) must NOT render a single
// combined query that exceeds ClickHouse's max_query_size. Before the
// chunk cap, the un-bounded UNION-ALL crossed 262124 bytes and CH rejected
// it with `code: 62 … Max query size exceeded`.
//
// The test asserts:
//   - every rendered combined query stays under maxQuerySizeBound, AND
//   - the request fans into MORE than one bounded query (chunking kicked
//     in past the cap), but FAR fewer than the per-matcher count the
//     pre-#790 fan-out would have issued.
func TestHandleSeries_ChunksUnderMaxQuerySize(t *testing.T) {
	t.Parallel()

	// Enough distinct matchers to exceed the chunk cap several times over.
	const matcherCount = 1500
	rec := &recordingQuerier{
		samples: []chclient.Sample{
			{MetricName: "metric_explorer_probe_series_0", Labels: map[string]string{"job": "api"}},
		},
	}
	srv := newServer(rec)
	t.Cleanup(srv.Close)

	form := largeMatchValues(matcherCount)
	resp, err := http.PostForm(srv.URL+"/api/v1/series", form)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	if len(rec.querySQLs) == 0 {
		t.Fatalf("no /series queries recorded")
	}

	// (a) Every combined query stays under the max_query_size ceiling.
	if got := maxLen(rec.querySQLs); got >= maxQuerySizeBound {
		t.Errorf("largest combined /series SQL = %d bytes, want < %d (max_query_size guard)",
			got, maxQuerySizeBound)
	}

	// (b) Chunking kicked in: more than one bounded query, but the
	// round-trip count is a small multiple of ⌈N/cap⌉ — vastly fewer than
	// the matcherCount round-trips the pre-#790 per-variant fan-out issued.
	if len(rec.querySQLs) < 2 {
		t.Errorf("expected chunking to fan into >1 query for %d matchers, got %d",
			matcherCount, len(rec.querySQLs))
	}
	if len(rec.querySQLs) >= matcherCount {
		t.Errorf("chunked round-trips (%d) should be far fewer than per-matcher count (%d)",
			len(rec.querySQLs), matcherCount)
	}

	// Each combined query must be a parameterized UNION-ALL of lowered
	// SELECTs — the literals bind through `?` placeholders, not inlined
	// into the SQL text (that's what keeps the text small under the cap).
	for _, s := range rec.querySQLs {
		if !strings.Contains(s, "?") {
			t.Errorf("combined /series SQL has no `?` placeholder — values inlined, not parameterized:\n%s", s[:min(len(s), 400)])
		}
	}
}

// TestHandleSeries_SmallRequestSingleRoundTrip pins the #790 win: a typical
// small request (a handful of matchers, well under the chunk cap) still
// collapses to exactly ONE combined round-trip — chunking does not
// regress the common case.
func TestHandleSeries_SmallRequestSingleRoundTrip(t *testing.T) {
	t.Parallel()

	rec := &recordingQuerier{
		samples: []chclient.Sample{
			{MetricName: "up", Labels: map[string]string{"job": "api"}},
		},
	}
	srv := newServer(rec)
	t.Cleanup(srv.Close)

	form := url.Values{}
	for _, j := range []string{"api", "web", "db", "cache", "queue"} {
		form.Add("match[]", fmt.Sprintf("up{job=%q}", j))
	}
	resp, err := http.PostForm(srv.URL+"/api/v1/series", form)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if len(rec.querySQLs) != 1 {
		t.Errorf("small /series request round-trips = %d, want 1 (chunking must not regress the common case)",
			len(rec.querySQLs))
	}
}

// TestHandleLabelValuesMatched_ChunksUnderMaxQuerySize mirrors the /series
// guard for the matched /api/v1/label/<name>/values path: a broad match[]
// set there also fans into chunked combined queries that stay under the
// max_query_size ceiling.
func TestHandleLabelValuesMatched_ChunksUnderMaxQuerySize(t *testing.T) {
	t.Parallel()

	const matcherCount = 1500
	rec := &recordingQuerier{}
	srv := newServer(rec)
	t.Cleanup(srv.Close)

	// /api/v1/label/{name}/values is GET-only; pass the broad match[] set
	// as the query string.
	form := largeMatchValues(matcherCount)
	resp, err := http.Get(srv.URL + "/api/v1/label/job/values?" + form.Encode())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if len(rec.stringsSQLs) == 0 {
		t.Fatalf("no label/values queries recorded")
	}
	if got := maxLen(rec.stringsSQLs); got >= maxQuerySizeBound {
		t.Errorf("largest combined label/values SQL = %d bytes, want < %d", got, maxQuerySizeBound)
	}
	if len(rec.stringsSQLs) < 2 {
		t.Errorf("expected chunking to fan into >1 query for %d matchers, got %d",
			matcherCount, len(rec.stringsSQLs))
	}
}
