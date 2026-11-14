package tempo

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This is the metrics-family counterpart of
// scan_resource_bound_search_conformance_test.go: it drives real TraceQL
// metrics queries through the REAL handler pipeline — the
// /api/metrics/query_range and /api/metrics/query routes, parse -> lower ->
// RangeWindow -> optimize -> emit — and asserts every one answers 200, so none
// trips the spans-scan chokepoint (chplan.ScanResourceBoundViolation), which
// descends a metrics node's per-trace subqueries and lookups with a fresh
// context. The family covers each trace-scoped intrinsic shape that lowers to
// a per-trace subquery (a membership IN, a TraceLookupFilter lookup, a
// compare() selection) under every metrics function family, beside plain
// metrics shapes whose only spans scan is the input spine.

// metricsConformanceStep is the step of every range request below.
const metricsConformanceStep = time.Minute

// metricsConformanceFamily is the metrics family the chokepoint must pass.
var metricsConformanceFamily = []string{
	`{ } | rate()`,
	`{ kind = server } | count_over_time() by (resource.service.name)`,
	`{ } | quantile_over_time(duration, .9)`,
	`{ } | histogram_over_time(duration)`,
	`{ status = error } | rate() by (span.http.route)`,
	`{ rootName = name } | rate()`,
	`{ rootServiceName = resource.service.name } | count_over_time()`,
	`{ traceDuration > duration } | rate()`,
	`{ traceDuration > 1s } | rate()`,
	`{ rootName != "x" } | count_over_time()`,
	`{ rootServiceName = "checkout" } | rate() by (span.http.route)`,
	`{ rootName = name && traceDuration > 1s } | rate()`,
	`{ rootName = name || traceDuration > 2s } | rate()`,
	`{ rootName = name } | quantile_over_time(duration, .9)`,
	`{ rootName = name } | histogram_over_time(duration)`,
	`{ rootName = name } | rate() by (resource.service.name)`,
	`{ trace:duration > 100ms } | count_over_time()`,
	`{ trace:rootService = "x" } | rate()`,
	`{ rootName = name } | compare({ traceDuration > 1s })`,
	`{ } | compare({ rootServiceName = "checkout" })`,
}

// metricsConformanceGet sends q to route over [start, end] and returns the
// status and body.
func metricsConformanceGet(t *testing.T, srv *httptest.Server, route, q string, start, end time.Time) (int, string) {
	t.Helper()
	vals := url.Values{}
	vals.Set("q", q)
	vals.Set("start", strconv.FormatInt(start.Unix(), 10))
	vals.Set("end", strconv.FormatInt(end.Unix(), 10))
	vals.Set("step", metricsConformanceStep.String())
	resp, err := http.Get(srv.URL + route + "?" + vals.Encode())
	if err != nil {
		t.Fatalf("GET %s %s: %v", route, q, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", route, q, err)
	}
	return resp.StatusCode, string(body)
}

// TestMetricsFamilyPassesTheSpansScanChokepoint pins that every query of the
// metrics family emits on both metrics routes without a resource-bound
// rejection.
func TestMetricsFamilyPassesTheSpansScanChokepoint(t *testing.T) {
	t.Parallel()
	h := New(&capturingQuerier{}, tracesSchema(), "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	start := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	for _, route := range []string{"/api/metrics/query_range", "/api/metrics/query"} {
		for _, q := range metricsConformanceFamily {
			status, body := metricsConformanceGet(t, srv, route, q, start, end)
			if status != http.StatusOK {
				t.Errorf("%s %s: status %d, want 200\n  %s", route, q, status, body)
			}
			if strings.Contains(body, "no resource bound") {
				t.Errorf("%s %s: tripped the spans-scan chokepoint\n  %s", route, q, body)
			}
		}
	}
}
