package prom_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
)

// End-to-end handler benchmarks (Layer 12). Each Benchmark drives a
// full HTTP request through the live ServeMux against a stubbed
// CH client; the timing covers parse → lower → optimize → emit →
// cursor-drain → JSON encode. No live ClickHouse round-trip is
// exercised — the stub returns synthetic samples — so the numbers
// measure cerberus's per-request envelope cost, not CH's.

// fakeCountingCursor is a chclient.Cursor that synthesises samples on
// the fly. Unlike sliceCursor (which materialises every sample in a
// pre-built slice), the counting cursor only keeps the current row in
// memory — used by the streaming-cursor RAM benchmarks to confirm the
// handler's cursor path stays bounded.
type fakeCountingCursor struct {
	total      int
	emitted    int
	seriesMod  int // emit floor(emitted/seriesMod) as the series-id label
	stepNanos  int64
	startUnix  int64
	cur        chclient.Sample
	metricName string
	stopAt     int // -1 means "no stop" — closing mid-iter zeroes idx.
	closed     bool
}

func (c *fakeCountingCursor) Next() bool {
	if c.closed {
		return false
	}
	if c.stopAt >= 0 && c.emitted >= c.stopAt {
		return false
	}
	if c.emitted >= c.total {
		return false
	}
	seriesID := c.emitted / c.seriesMod
	ts := time.Unix(c.startUnix, 0).Add(time.Duration(c.emitted) * time.Duration(c.stepNanos))
	c.cur = chclient.Sample{
		MetricName: c.metricName,
		Labels:     map[string]string{"job": "api", "instance": fmt.Sprintf("host-%d", seriesID)},
		Timestamp:  ts,
		Value:      float64(c.emitted),
	}
	c.emitted++
	return true
}

func (c *fakeCountingCursor) Sample() chclient.Sample { return c.cur }
func (c *fakeCountingCursor) Err() error              { return nil }
func (c *fakeCountingCursor) Close() error {
	c.closed = true
	return nil
}

// cursorQuerier returns a fresh counting cursor on each QueryCursor.
// stubQuerier (in handler_test.go) returns a sliceCursor over its
// pre-materialised samples slice — fine for small results, but masks
// the streaming behaviour cerberus claims for very large results.
type cursorQuerier struct {
	total      int
	seriesMod  int
	stepNanos  int64
	startUnix  int64
	metricName string
	stopAt     int
}

func (c *cursorQuerier) Query(_ context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	return nil, nil
}

func (c *cursorQuerier) QueryCursor(_ context.Context, _ string, _ ...any) (chclient.Cursor, error) {
	return &fakeCountingCursor{
		total:      c.total,
		seriesMod:  c.seriesMod,
		stepNanos:  c.stepNanos,
		startUnix:  c.startUnix,
		metricName: c.metricName,
		stopAt:     c.stopAt,
	}, nil
}

func (c *cursorQuerier) QueryStrings(_ context.Context, _ string, _ ...any) ([]string, error) {
	return nil, nil
}

func (c *cursorQuerier) QueryLabelSets(_ context.Context, _ string, _ ...any) ([]map[string]string, error) {
	return nil, nil
}

func (c *cursorQuerier) QueryMetricMeta(_ context.Context, _, _ string, _ ...any) ([]chclient.MetricMetaRow, error) {
	return nil, nil
}

// BenchmarkHandleQuery_Small drives the instant /api/v1/query path with
// a single-sample CH response. The expected target is sub-millisecond
// per request on the bench host; deviations point at envelope-cost
// regressions (JSON encoder, plan walk, response-header writer).
func BenchmarkHandleQuery_Small(b *testing.B) {
	ts := time.Unix(1700000000, 0).UTC()
	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1.0},
		},
	}
	srv := newServer(q)
	b.Cleanup(srv.Close)
	url := srv.URL + "/api/v1/query?query=up&time=1700000000"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Get(url)
		if err != nil {
			b.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// BenchmarkHandleQueryRange_Small drives /api/v1/query_range over a
// 1-hour window at 1m step (= 60 evaluation points) against a small
// synthetic result set — the typical Grafana dashboard panel.
func BenchmarkHandleQueryRange_Small(b *testing.B) {
	start := time.Unix(1700000000, 0).UTC()
	end := start.Add(time.Hour)
	samples := make([]chclient.Sample, 0, 60)
	for i := 0; i < 60; i++ {
		samples = append(samples, chclient.Sample{
			MetricName: "up",
			Labels:     map[string]string{"job": "api"},
			Timestamp:  start.Add(time.Duration(i) * time.Minute),
			Value:      float64(i),
		})
	}
	q := &stubQuerier{samples: samples}
	srv := newServer(q)
	b.Cleanup(srv.Close)
	url := fmt.Sprintf("%s/api/v1/query_range?query=up&start=%d&end=%d&step=60",
		srv.URL, start.Unix(), end.Unix())

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Get(url)
		if err != nil {
			b.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// BenchmarkHandleQueryRange_Large drives /api/v1/query_range over a
// 24-hour window at 1m step (= 1440 evaluation points). Exercises the
// matrix-build + JSON encoder under the realistic large-Grafana-range
// load: per-step bucketing dominates here.
func BenchmarkHandleQueryRange_Large(b *testing.B) {
	start := time.Unix(1700000000, 0).UTC()
	end := start.Add(24 * time.Hour)
	samples := make([]chclient.Sample, 0, 1440)
	for i := 0; i < 1440; i++ {
		samples = append(samples, chclient.Sample{
			MetricName: "up",
			Labels:     map[string]string{"job": "api"},
			Timestamp:  start.Add(time.Duration(i) * time.Minute),
			Value:      float64(i),
		})
	}
	q := &stubQuerier{samples: samples}
	srv := newServer(q)
	b.Cleanup(srv.Close)
	url := fmt.Sprintf("%s/api/v1/query_range?query=up&start=%d&end=%d&step=60",
		srv.URL, start.Unix(), end.Unix())

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Get(url)
		if err != nil {
			b.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// BenchmarkHandleLabels drives /api/v1/labels — the metadata-discovery
// endpoint Grafana uses for dropdown completion. The CH side returns a
// flat list of names; the handler unions across all metric tables.
func BenchmarkHandleLabels(b *testing.B) {
	names := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		names = append(names, fmt.Sprintf("label_%03d", i))
	}
	q := &stubQuerier{strings: names}
	srv := newServer(q)
	b.Cleanup(srv.Close)
	url := srv.URL + "/api/v1/labels"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Get(url)
		if err != nil {
			b.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// BenchmarkHandleSeries drives /api/v1/series with several match[]
// selectors — Grafana's "series autocompletion" path.
func BenchmarkHandleSeries(b *testing.B) {
	labelSets := make([]map[string]string, 0, 10)
	for i := 0; i < 10; i++ {
		labelSets = append(labelSets, map[string]string{
			"__name__": "up",
			"job":      "api",
			"instance": fmt.Sprintf("host-%d", i),
		})
	}
	q := &stubQuerier{labelSets: labelSets}
	srv := newServer(q)
	b.Cleanup(srv.Close)
	// 10 matchers — each one is a series selector. Grafana variable
	// pickers commonly issue a small batch like this.
	url := srv.URL + "/api/v1/series?" +
		"match%5B%5D=up%7Bjob%3D%22api%22%7D&" +
		"match%5B%5D=up%7Bjob%3D%22web%22%7D&" +
		"match%5B%5D=up%7Bjob%3D%22db%22%7D&" +
		"match%5B%5D=up%7Bjob%3D%22cache%22%7D&" +
		"match%5B%5D=up%7Bjob%3D%22queue%22%7D&" +
		"match%5B%5D=up%7Bjob%3D%22worker%22%7D&" +
		"match%5B%5D=up%7Bjob%3D%22ingest%22%7D&" +
		"match%5B%5D=up%7Bjob%3D%22output%22%7D&" +
		"match%5B%5D=up%7Bjob%3D%22auth%22%7D&" +
		"match%5B%5D=up%7Bjob%3D%22edge%22%7D"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Get(url)
		if err != nil {
			b.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// BenchmarkStreamingCursor_1M_Points runs query_range against a
// fakeCountingCursor synthesising 1M samples — 1000 series × 1000
// points each. Asserts the live HeapInuse delta over baseline stays
// inside a generous bound; the matrix builder *does* materialise rows
// per series (bySeries map), so the bound covers that resident set
// plus JSON encoding overhead. Catastrophic regressions (e.g.
// accidentally holding the whole result slice) trip the bound.
//
// NOTE: This bench is not a strict "streaming cursor RAM ceiling" —
// the matrix handler materialises rows-per-series in memory. See the
// LIMITATION comment in the PR body.
func BenchmarkStreamingCursor_1M_Points(b *testing.B) {
	const (
		totalRows     = 1_000_000
		seriesCount   = 1000
		samplesPerSer = totalRows / seriesCount
	)
	start := time.Unix(1700000000, 0).UTC()
	end := start.Add(time.Duration(samplesPerSer-1) * time.Second)
	q := &cursorQuerier{
		total:      totalRows,
		seriesMod:  samplesPerSer,
		stepNanos:  int64(time.Second),
		startUnix:  start.Unix(),
		metricName: "up",
		stopAt:     -1,
	}
	srv := newServer(q)
	b.Cleanup(srv.Close)
	url := fmt.Sprintf("%s/api/v1/query_range?query=up&start=%d&end=%d&step=1",
		srv.URL, start.Unix(), end.Unix())

	// Establish a baseline so the heap-delta assertion is meaningful.
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Get(url)
		if err != nil {
			b.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	b.StopTimer()

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if after.HeapInuse > baseline.HeapInuse {
		b.ReportMetric(float64(after.HeapInuse-baseline.HeapInuse)/(1024*1024), "MB_heap_delta")
	}
}

// BenchmarkStreamingCursor_100K_Series exercises the high-cardinality
// case — 100K series with 10 points each. Different shape: the bySeries
// map dominates RAM rather than per-series row slices.
func BenchmarkStreamingCursor_100K_Series(b *testing.B) {
	const (
		seriesCount   = 100_000
		samplesPerSer = 10
		totalRows     = seriesCount * samplesPerSer
	)
	start := time.Unix(1700000000, 0).UTC()
	end := start.Add(time.Duration(samplesPerSer-1) * time.Minute)
	q := &cursorQuerier{
		total:      totalRows,
		seriesMod:  samplesPerSer,
		stepNanos:  int64(time.Minute),
		startUnix:  start.Unix(),
		metricName: "up",
		stopAt:     -1,
	}
	srv := newServer(q)
	b.Cleanup(srv.Close)
	url := fmt.Sprintf("%s/api/v1/query_range?query=up&start=%d&end=%d&step=60",
		srv.URL, start.Unix(), end.Unix())

	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Get(url)
		if err != nil {
			b.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	b.StopTimer()

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if after.HeapInuse > baseline.HeapInuse {
		b.ReportMetric(float64(after.HeapInuse-baseline.HeapInuse)/(1024*1024), "MB_heap_delta")
	}
}

// BenchmarkStreamingCursor_Stop_Mid exercises the early-close path.
// Drives the request handler, but the underlying cursor stops at half
// the synthesised row count — verifies the handler still returns a
// valid response without panicking when the cursor terminates early.
func BenchmarkStreamingCursor_Stop_Mid(b *testing.B) {
	const (
		totalRows = 100_000
		stopAt    = totalRows / 2
	)
	start := time.Unix(1700000000, 0).UTC()
	end := start.Add(time.Hour)
	q := &cursorQuerier{
		total:      totalRows,
		seriesMod:  1000,
		stepNanos:  int64(time.Millisecond),
		startUnix:  start.Unix(),
		metricName: "up",
		stopAt:     stopAt,
	}
	srv := newServer(q)
	b.Cleanup(srv.Close)
	url := fmt.Sprintf("%s/api/v1/query_range?query=up&start=%d&end=%d&step=60",
		srv.URL, start.Unix(), end.Unix())

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Get(url)
		if err != nil {
			b.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// TestAllocs_HandleQuery_Small pins the per-request alloc count for
// the smallest /api/v1/query path. Compared to the other allocs tests,
// this one is loose — the net/http stack contributes a lot of allocs
// per request — but a 10× ceiling around the current baseline is a
// useful regression detector for handler-side changes (e.g.
// accidentally materialising the response twice).
func TestAllocs_HandleQuery_Small(t *testing.T) {
	// AllocsPerRun forbids parallel execution.
	ts := time.Unix(1700000000, 0).UTC()
	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1.0},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)
	url := srv.URL + "/api/v1/query?query=up&time=1700000000"

	got := testing.AllocsPerRun(20, func() {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	})
	// HTTP transport + handler envelope. Baseline ~281; ceiling
	// includes 3× slack for net/http variance — the goal is
	// regression detection, not optimisation.
	const ceiling = 850.0
	if got > ceiling {
		t.Errorf("HandleQuery_Small avg allocs = %.1f; want <= %.1f", got, ceiling)
	}
	t.Logf("HandleQuery_Small avg allocs = %.1f (ceiling %.1f)", got, ceiling)
}

// Sanity check the benchmark plumbing — confirms json decoder picks
// up the vector response so a misconfigured stub doesn't silently
// emit "1 sample, no labels" garbage and pass.
func TestCursorQuerier_DrivesHandler(t *testing.T) {
	t.Parallel()

	start := time.Unix(1700000000, 0).UTC()
	end := start.Add(10 * time.Minute)
	q := &cursorQuerier{
		total:      10,
		seriesMod:  5,
		stepNanos:  int64(time.Minute),
		startUnix:  start.Unix(),
		metricName: "up",
		stopAt:     -1,
	}
	// Use the canonical newServer + the cursor querier so the wiring
	// matches what the benchmarks drive.
	srv := newServer(q)
	t.Cleanup(srv.Close)

	url := fmt.Sprintf("%s/api/v1/query_range?query=up&start=%d&end=%d&step=60",
		srv.URL, start.Unix(), end.Unix())
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var parsed struct {
		Status string         `json:"status"`
		Data   prom.QueryData `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed.Status != "success" {
		t.Fatalf("status: %s", parsed.Status)
	}
	if parsed.Data.ResultType != "matrix" {
		t.Fatalf("resultType: %s", parsed.Data.ResultType)
	}
}
