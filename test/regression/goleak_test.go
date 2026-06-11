package regression

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// Layer 11 — goroutine-leak detector tests. Each test opens a real
// httptest.Server, drives N requests through one handler entrypoint,
// closes the server, and asserts goleak.VerifyNone(t) finds no leaked
// goroutines.
//
// Why this lives in test/regression (not in each api/* package): the
// runtime goroutine inventory at test start is package-local — running
// goleak inside api/prom catches leaks the prom package itself owns,
// but the cross-package tail of stuck OTel exporters / WebSocket
// upgrade goroutines is easier to see when the test exists in a
// standalone package that doesn't import them transitively.
//
// goleak ignores known persistent runtime goroutines (e.g. the Go HTTP
// idle-conn cleaner) via opts.

// goleakOpts excludes the few intermittent goroutines that don't
// constitute a real leak — the http.DefaultTransport idle-conn
// goroutine and OTel's noop tracer background tasks.
func goleakOpts() []goleak.Option {
	return []goleak.Option{
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
		goleak.IgnoreTopFunction("net/http.(*Transport).getConn"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		// Some tests use httptest.Server which spawns its own conn-tracking
		// goroutines that linger briefly after Close — ignore the standard
		// tail.
		goleak.IgnoreCurrent(),
	}
}

// promStub is a minimal prom.Querier returning canned data — no
// goroutines of its own.
type promStub struct{ samples []chclient.Sample }

func (s *promStub) Query(_ context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	return s.samples, nil
}

func (s *promStub) QueryCursor(_ context.Context, _ string, _ ...any) (chclient.Cursor, error) {
	return &goleakSliceCursor{samples: s.samples, idx: -1}, nil
}

func (s *promStub) QueryStrings(_ context.Context, _ string, _ ...any) ([]string, error) {
	return nil, nil
}

func (s *promStub) QueryLabelSets(_ context.Context, _ string, _ ...any) ([]map[string]string, error) {
	return nil, nil
}

func (s *promStub) QueryMetricMeta(_ context.Context, _, _ string, _ ...any) ([]chclient.MetricMetaRow, error) {
	return nil, nil
}

func (s *promStub) QueryExemplars(_ context.Context, _ string, _ ...any) ([]chclient.ExemplarRow, error) {
	return nil, nil
}

type goleakSliceCursor struct {
	samples []chclient.Sample
	idx     int
	cur     chclient.Sample
}

func (c *goleakSliceCursor) Next() bool {
	c.idx++
	if c.idx >= len(c.samples) {
		return false
	}
	c.cur = c.samples[c.idx]
	return true
}
func (c *goleakSliceCursor) Sample() chclient.Sample { return c.cur }
func (c *goleakSliceCursor) Err() error              { return nil }
func (c *goleakSliceCursor) Close() error            { return nil }

func TestNoGoroutineLeak_PromQuery(t *testing.T) {
	defer goleak.VerifyNone(t, goleakOpts()...)

	h := prom.New(&promStub{samples: []chclient.Sample{
		{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: time.Now(), Value: 1},
	}}, schema.DefaultOTelMetrics(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 50 {
		resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func TestNoGoroutineLeak_PromQueryRange(t *testing.T) {
	defer goleak.VerifyNone(t, goleakOpts()...)

	start := time.Unix(1717995600, 0).UTC()
	end := start.Add(2 * time.Minute)
	h := prom.New(&promStub{samples: []chclient.Sample{
		{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: start, Value: 1},
		{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: end, Value: 2},
	}}, schema.DefaultOTelMetrics(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	url := fmt.Sprintf("%s/api/v1/query_range?query=up&start=%d&end=%d&step=60",
		srv.URL, start.Unix(), end.Unix())
	for range 50 {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func TestNoGoroutineLeak_PromLabels(t *testing.T) {
	defer goleak.VerifyNone(t, goleakOpts()...)

	h := prom.New(&promStub{}, schema.DefaultOTelMetrics(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 50 {
		resp, err := http.Get(srv.URL + "/api/v1/labels")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
	}
}

func TestNoGoroutineLeak_PromSeries(t *testing.T) {
	defer goleak.VerifyNone(t, goleakOpts()...)

	h := prom.New(&promStub{}, schema.DefaultOTelMetrics(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 30 {
		resp, err := http.Get(srv.URL + `/api/v1/series?match%5B%5D=up`)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
	}
}

func TestNoGoroutineLeak_PromMetadata(t *testing.T) {
	defer goleak.VerifyNone(t, goleakOpts()...)

	h := prom.New(&promStub{}, schema.DefaultOTelMetrics(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 30 {
		resp, err := http.Get(srv.URL + "/api/v1/metadata")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
	}
}

// --- Loki ----------------------------------------------------------------

type lokiStub struct{ samples []chclient.Sample }

func (s *lokiStub) Query(_ context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	return s.samples, nil
}

func (s *lokiStub) QueryStrings(_ context.Context, _ string, _ ...any) ([]string, error) {
	return nil, nil
}

func (s *lokiStub) QueryDetectedFieldRows(_ context.Context, _ string, _ ...any) ([]chclient.DetectedFieldRow, error) {
	return nil, nil
}

func (s *lokiStub) QueryTimestampedLines(_ context.Context, _ string, _ ...any) ([]chclient.TimestampedLine, error) {
	return nil, nil
}

func (s *lokiStub) QueryIndexStats(_ context.Context, _ string, _ ...any) (chclient.IndexStatsRow, error) {
	return chclient.IndexStatsRow{}, nil
}

func (s *lokiStub) QueryIndexVolume(_ context.Context, _ string, _ ...any) ([]chclient.IndexVolumeRow, error) {
	return nil, nil
}

func (s *lokiStub) QueryLabelSets(_ context.Context, _ string, _ ...any) ([]map[string]string, error) {
	return nil, nil
}

var _ loki.Querier = (*lokiStub)(nil)

func TestNoGoroutineLeak_LokiQuery(t *testing.T) {
	defer goleak.VerifyNone(t, goleakOpts()...)

	h := loki.New(&lokiStub{}, schema.DefaultOTelLogs(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 30 {
		resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D`)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
	}
}

func TestNoGoroutineLeak_LokiLabels(t *testing.T) {
	defer goleak.VerifyNone(t, goleakOpts()...)

	h := loki.New(&lokiStub{}, schema.DefaultOTelLogs(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 30 {
		resp, err := http.Get(srv.URL + "/loki/api/v1/labels")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
	}
}

func TestNoGoroutineLeak_LokiIndexStats(t *testing.T) {
	defer goleak.VerifyNone(t, goleakOpts()...)

	h := loki.New(&lokiStub{}, schema.DefaultOTelLogs(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 30 {
		resp, err := http.Get(srv.URL + `/loki/api/v1/index/stats?query=%7Bjob%3D%22api%22%7D`)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
	}
}

// --- Tempo ---------------------------------------------------------------

type tempoStub struct{ samples []chclient.Sample }

func (s *tempoStub) Query(_ context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	return s.samples, nil
}

func (s *tempoStub) QueryStrings(_ context.Context, _ string, _ ...any) ([]string, error) {
	return nil, nil
}

var _ tempo.Querier = (*tempoStub)(nil)

func TestNoGoroutineLeak_TempoSearch(t *testing.T) {
	defer goleak.VerifyNone(t, goleakOpts()...)

	h := tempo.New(&tempoStub{}, schema.DefaultOTelTraces(), "v1.0.0", slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 30 {
		resp, err := http.Get(srv.URL + `/api/search?q=%7B%7D`)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
	}
}

func TestNoGoroutineLeak_TempoTraceByID(t *testing.T) {
	defer goleak.VerifyNone(t, goleakOpts()...)

	h := tempo.New(&tempoStub{}, schema.DefaultOTelTraces(), "v1.0.0", slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Drive both the v1 (bare-trace) and v2 (TraceByIDResponse
	// envelope) endpoints — since the v2 envelope fix they are
	// distinct handler funcs sharing serveTraceByID, and the goleak
	// net must cover every mounted entrypoint.
	for _, path := range []string{"/api/traces/abc123", "/api/v2/traces/abc123"} {
		for range 30 {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			_ = resp.Body.Close()
		}
	}
}

// TestNoGoroutineLeak_TempoMetricsQueryInstant exercises the instant
// variant of the TraceQL metrics pipeline — the handler shares the
// matrix-shape RangeWindow plan with /api/metrics/query_range but
// runs it over a single bucket (step=end-start). A regression here
// would surface as a goroutine spawned per request by the metrics
// engine path that the handler then forgets to tear down.
func TestNoGoroutineLeak_TempoMetricsQueryInstant(t *testing.T) {
	defer goleak.VerifyNone(t, goleakOpts()...)

	h := tempo.New(&tempoStub{}, schema.DefaultOTelTraces(), "v1.0.0", slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 30 {
		u := fmt.Sprintf("%s/api/metrics/query?q=%%7B%%7D%%20%%7C%%20rate()&start=%d&end=%d",
			srv.URL, time.Now().Add(-1*time.Hour).Unix(), time.Now().Unix())
		resp, err := http.Get(u)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
	}
}

// TestNoGoroutineLeak_UnderError — drives requests against a failing CH
// stub so the error path is the one exercised. Different goroutine
// graph than the happy path (no streaming cursor open, immediate
// envelope write); pinned separately.
func TestNoGoroutineLeak_UnderError(t *testing.T) {
	defer goleak.VerifyNone(t, goleakOpts()...)

	failQ := &failingProm{err: errors.New("ch chaos")}
	h := prom.New(failQ, schema.DefaultOTelMetrics(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 30 {
		resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
	}
}

type failingProm struct {
	err error
}

func (f *failingProm) Query(_ context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	return nil, f.err
}

func (f *failingProm) QueryCursor(_ context.Context, _ string, _ ...any) (chclient.Cursor, error) {
	return nil, f.err
}

func (f *failingProm) QueryStrings(_ context.Context, _ string, _ ...any) ([]string, error) {
	return nil, f.err
}

func (f *failingProm) QueryLabelSets(_ context.Context, _ string, _ ...any) ([]map[string]string, error) {
	return nil, f.err
}

func (f *failingProm) QueryMetricMeta(_ context.Context, _, _ string, _ ...any) ([]chclient.MetricMetaRow, error) {
	return nil, f.err
}

func (f *failingProm) QueryExemplars(_ context.Context, _ string, _ ...any) ([]chclient.ExemplarRow, error) {
	return nil, f.err
}

// TestNoGoroutineLeak_ConcurrentRequests — parallel requests must not
// leak. Catches connection-pool leaks more reliably than serial.
func TestNoGoroutineLeak_ConcurrentRequests(t *testing.T) {
	defer goleak.VerifyNone(t, goleakOpts()...)

	h := prom.New(&promStub{samples: []chclient.Sample{{MetricName: "up", Value: 1}}},
		schema.DefaultOTelMetrics(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}()
	}
	wg.Wait()
}
