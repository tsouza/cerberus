//go:build integration

package chserver

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// The Loki compatibility lane's driver sends its comparison queries this many
// at a time, and gives each request this long to answer: the `-parallelism`
// and `-timeout` defaults .github/scripts/run-loki-compatibility.mjs passes it.
// test/regression's TestThrottledUnwrapProbeMirrorsTheLokiCompatBudget keeps
// the two equal.
const (
	compatDriverParallelism = 8
	compatRequestTimeout    = 30 * time.Second
)

// throttledUnwrapImage runs the ClickHouse version the Loki compatibility lane
// runs.
const throttledUnwrapImage = "clickhouse/clickhouse-server:26.8.36.3-alpine"

// throttledUnwrapNanoCPUs bounds the server to a quarter of one CPU. The share
// is calibrated against two lowerings of throttledUnwrapQuery: one that
// recomputed each per-row value at every read (an 82 KB statement) left most
// of compatDriverParallelism concurrent requests unanswered at
// compatRequestTimeout on every run at this share, and one that computes each
// value once (an 18 KB statement) answered all of them in under half of it.
const throttledUnwrapNanoCPUs = 250_000_000 // a quarter of one CPU

// throttledUnwrapQuery is a range aggregation over an unwrapped duration, from
// the Loki compatibility corpus: a logfmt parse, a label filter, and the
// duration conversion each read the same per-row values.
const throttledUnwrapQuery = `sum by (detected_level) (sum_over_time({cluster="cluster-1", container="container-1", datacenter="dc1", env="production", namespace="namespace-3", pod="pod-7", region="us-east-1", service="mimir", service_name="mimir"} | logfmt | duration != "" | unwrap duration(duration) [5m]))`

// The query_range window the lane evaluates the query over: one day at a
// one-minute step.
var (
	throttledUnwrapStart = time.Date(2026, time.May, 11, 0, 0, 0, 0, time.UTC)
	throttledUnwrapEnd   = throttledUnwrapStart.Add(24 * time.Hour)
)

const throttledUnwrapStepSeconds = 60

// throttledUnwrapLogs are the seeded log lines, all in one stream the
// selector matches and all inside one step's range window, with the sum
// each detected_level series must reach.
var throttledUnwrapLogs = []struct {
	level    string
	duration string
}{
	{"info", "1.5s"},
	{"info", "250ms"},
	{"error", "2s"},
}

var throttledUnwrapWant = map[string]float64{"info": 1.75, "error": 2}

// throttledUnwrapSeedOffset places the seeded logs an hour into the window.
const throttledUnwrapSeedOffset = time.Hour

// TestThrottledUnwrapAnswersWithinTheCompatTimeout drives the compatibility
// lane's unwrap query through the Loki handler and the data-plane client
// against a CPU-bounded server, compatDriverParallelism requests at once, and
// requires every request to return the right answer within
// compatRequestTimeout.
func TestThrottledUnwrapAnswersWithinTheCompatTimeout(t *testing.T) {
	ctx := context.Background()
	s := startServer(ctx, t, throttledUnwrapImage,
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.NanoCPUs = throttledUnwrapNanoCPUs
		}))
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Logs}); err != nil {
		t.Fatalf("apply the logs schema: %v", err)
	}
	seedThrottledUnwrap(ctx, t, s)

	client := s.client(t, adminUser, adminPassword, chclient.Config{MaxOpenConns: compatDriverParallelism})
	// The handler lowers over the default OTel logs schema, as the lane's
	// cerberus does, so it sends the statement the lane sends. The per-query
	// settings and resource ceilings production configures on top are not
	// what the statement's cost depends on, and are left at their zero values.
	mux := http.NewServeMux()
	loki.New(client, schema.DefaultOTelLogs(), nil).Mount(mux)

	path := "/loki/api/v1/query_range?" + url.Values{
		"query": {throttledUnwrapQuery},
		"start": {strconv.FormatInt(throttledUnwrapStart.UnixNano(), 10)},
		"end":   {strconv.FormatInt(throttledUnwrapEnd.UnixNano(), 10)},
		"step":  {strconv.Itoa(throttledUnwrapStepSeconds)},
	}.Encode()

	var wg sync.WaitGroup
	for i := range compatDriverParallelism {
		wg.Go(func() {
			reqCtx, cancel := context.WithTimeout(ctx, compatRequestTimeout)
			defer cancel()
			start := time.Now()
			got, err := throttledUnwrapAnswer(reqCtx, mux, path)
			elapsed := time.Since(start)
			if err != nil {
				t.Errorf("request %d after %s: %v", i, elapsed.Round(time.Millisecond), err)
				return
			}
			if !maps.Equal(got, throttledUnwrapWant) {
				t.Errorf("request %d: peak per detected_level = %v; want %v", i, got, throttledUnwrapWant)
			}
			t.Logf("request %d answered in %s", i, elapsed.Round(time.Millisecond))
		})
	}
	wg.Wait()
}

// seedThrottledUnwrap seeds throttledUnwrapLogs into the stream the query's
// selector names, one millisecond apart.
func seedThrottledUnwrap(ctx context.Context, t *testing.T, s *server) {
	t.Helper()
	resource := map[string]string{
		"cluster": "cluster-1", "container": "container-1", "datacenter": "dc1",
		"env": "production", "namespace": "namespace-3", "pod": "pod-7",
		"region": "us-east-1", "service": "mimir", "service_name": "mimir",
		"service.name": "mimir",
	}
	base := throttledUnwrapStart.Add(throttledUnwrapSeedOffset)
	for i, l := range throttledUnwrapLogs {
		s.exec(ctx, t, `INSERT INTO otel_logs (Timestamp, ServiceName, Body, ResourceAttributes, LogAttributes, SeverityText) VALUES (?, ?, ?, ?, ?, ?)`,
			base.Add(time.Duration(i)*time.Millisecond), "mimir",
			fmt.Sprintf("level=%s duration=%s msg=done", l.level, l.duration),
			resource, map[string]string{"detected_level": l.level}, l.level)
	}
}

// throttledUnwrapAnswer issues GET path against mux under ctx and returns each
// detected_level series' peak value. A request that outlives ctx fails.
func throttledUnwrapAnswer(ctx context.Context, mux http.Handler, path string) (map[string]float64, error) {
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("no answer within %s: %w", compatRequestTimeout, err)
	}
	if rec.Code != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		return nil, fmt.Errorf("decode: %w (%s)", err, rec.Body.String())
	}
	peaks := map[string]float64{}
	for _, series := range body.Data.Result {
		level := series.Metric["detected_level"]
		for _, v := range series.Values {
			s, _ := v[1].(string)
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return nil, fmt.Errorf("series %v: value %v: %w", series.Metric, v[1], err)
			}
			peaks[level] = max(peaks[level], f)
		}
	}
	return peaks, nil
}
