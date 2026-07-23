package migrateinventory

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// syntheticTSDB is a fixed /api/v1/status/tsdb payload whose arrays are
// deliberately out of value order, so the test proves the client re-ranks
// rather than trusting the server's ordering.
const syntheticTSDB = `{
  "status": "success",
  "data": {
    "headStats": {
      "numSeries": 508,
      "numLabelPairs": 1234,
      "chunkCount": 937,
      "minTime": 1700000000000,
      "maxTime": 1700003600000
    },
    "seriesCountByMetricName": [
      {"name": "http_requests_total", "value": 300},
      {"name": "node_cpu_seconds_total", "value": 900},
      {"name": "go_gc_duration_seconds", "value": 120}
    ],
    "labelValueCountByLabelName": [
      {"name": "instance", "value": 40},
      {"name": "__name__", "value": 210},
      {"name": "job", "value": 12}
    ],
    "memoryInBytesByLabelName": [
      {"name": "__name__", "value": 24000},
      {"name": "instance", "value": 8000}
    ]
  }
}`

// tsdbServer answers /api/v1/status/tsdb with the synthetic payload and the
// optional enrichment endpoints, so the probe can be driven over real HTTP.
func tsdbServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case statusTSDBPath:
			_, _ = w.Write([]byte(syntheticTSDB))
		case metricNamesPath:
			_, _ = w.Write([]byte(`{"status":"success","data":["http_requests_total","node_cpu_seconds_total","go_gc_duration_seconds"]}`))
		case metadataPath:
			_, _ = w.Write([]byte(`{"status":"success","data":{"http_requests_total":[{"type":"counter"}],"node_cpu_seconds_total":[{"type":"counter"}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestProbe_RanksTopN drives the full probe over httptest and asserts the
// ranking (highest series/value/memory first), the head stats, and the
// enrichment totals.
func TestProbe_RanksTopN(t *testing.T) {
	srv := tsdbServer(t)
	inv, err := NewClient(srv.URL).Probe(context.Background(), Options{Top: 2})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	if inv.Head.NumSeries != 508 || inv.Head.ChunkCount != 937 {
		t.Errorf("head = %+v, want numSeries 508 / chunks 937", inv.Head)
	}

	// Top 2 metrics by series, ranked descending: node_cpu (900) then http (300).
	if len(inv.TopMetricsBySeries) != 2 {
		t.Fatalf("top metrics = %d, want 2 (truncated to --top)", len(inv.TopMetricsBySeries))
	}
	if inv.TopMetricsBySeries[0].Name != "node_cpu_seconds_total" || inv.TopMetricsBySeries[0].Value != 900 {
		t.Errorf("rank #1 metric = %+v, want node_cpu_seconds_total=900", inv.TopMetricsBySeries[0])
	}
	if inv.TopMetricsBySeries[1].Name != "http_requests_total" {
		t.Errorf("rank #2 metric = %+v, want http_requests_total", inv.TopMetricsBySeries[1])
	}

	// Top 2 labels by value cardinality: __name__ (210) then instance (40).
	if inv.TopLabelsByValues[0].Name != "__name__" || inv.TopLabelsByValues[0].Value != 210 {
		t.Errorf("rank #1 label = %+v, want __name__=210", inv.TopLabelsByValues[0])
	}

	// Memory ranking: __name__ (24000) first.
	if inv.TopLabelsByMemory[0].Name != "__name__" || inv.TopLabelsByMemory[0].Value != 24000 {
		t.Errorf("rank #1 label-memory = %+v, want __name__=24000", inv.TopLabelsByMemory[0])
	}

	if inv.MetricNameTotal != 3 {
		t.Errorf("metric-name total = %d, want 3", inv.MetricNameTotal)
	}
	if inv.MetadataMetricTotal != 2 {
		t.Errorf("metadata total = %d, want 2", inv.MetadataMetricTotal)
	}
	if len(inv.Notes) != 0 {
		t.Errorf("expected no notes when all endpoints answer, got %v", inv.Notes)
	}
}

// TestProbe_404OnStatusIsError: a source that 404s /status/tsdb is a hard error
// (the caller exits non-zero), not a silently empty inventory.
func TestProbe_404OnStatusIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	_, err := NewClient(srv.URL).Probe(context.Background(), Options{Top: 10})
	if err == nil {
		t.Fatal("Probe should error when /status/tsdb 404s")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should name the 404 status, got: %v", err)
	}
}

// TestProbe_EnrichmentFailuresAreCounted: when the mandatory status endpoint
// answers but the optional enrichments 404, the probe still succeeds and every
// missing enrichment is COUNTED as a note (never silently dropped) with its
// total left at the -1 "not obtained" sentinel.
func TestProbe_EnrichmentFailuresAreCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == statusTSDBPath {
			_, _ = w.Write([]byte(syntheticTSDB))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	inv, err := NewClient(srv.URL).Probe(context.Background(), Options{Top: 10})
	if err != nil {
		t.Fatalf("Probe should succeed on status alone, got: %v", err)
	}
	if inv.MetricNameTotal != -1 || inv.MetadataMetricTotal != -1 {
		t.Errorf("failed enrichments should stay at -1, got names=%d metadata=%d",
			inv.MetricNameTotal, inv.MetadataMetricTotal)
	}
	if len(inv.Notes) != 2 {
		t.Errorf("both enrichment failures should be counted as notes, got %d: %v", len(inv.Notes), inv.Notes)
	}
}

// TestProbe_StatusErrorBodyIsError: a 200 whose JSON status is "error" is
// surfaced, not treated as an empty-but-valid inventory.
func TestProbe_StatusErrorBodyIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == statusTSDBPath {
			_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"limit too high"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	_, err := NewClient(srv.URL).Probe(context.Background(), Options{Top: 10})
	if err == nil || !strings.Contains(err.Error(), "limit too high") {
		t.Errorf("Probe should surface the source error body, got: %v", err)
	}
}

// TestOptionsValidate pins the input guards: top must be positive; a set window
// must be a valid duration.
func TestOptionsValidate(t *testing.T) {
	if err := (Options{Top: 0}).Validate(); err == nil {
		t.Error("Top=0 should be rejected")
	}
	if err := (Options{Top: 10, Window: "banana"}).Validate(); err == nil {
		t.Error("an unparseable window should be rejected")
	}
	if err := (Options{Top: 10, Window: "1h"}).Validate(); err != nil {
		t.Errorf("Top=10, Window=1h should be valid, got: %v", err)
	}
}

// TestProbe_SendsLimit asserts the probe passes --top through as the TSDB
// `limit` query param so the server returns enough entries to rank.
func TestProbe_SendsLimit(t *testing.T) {
	var gotLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == statusTSDBPath {
			gotLimit = r.URL.Query().Get("limit")
			_, _ = w.Write([]byte(syntheticTSDB))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	if _, err := NewClient(srv.URL).Probe(context.Background(), Options{Top: 25}); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if gotLimit != "25" {
		t.Errorf("limit query param = %q, want 25", gotLimit)
	}
}

// TestWriteJSON pins the JSON shape callers depend on: the ranked arrays and the
// enrichment sentinels round-trip.
func TestWriteJSON(t *testing.T) {
	inv := Inventory{
		Source:              "http://prom:9090",
		Top:                 2,
		Head:                HeadStats{NumSeries: 508},
		TopMetricsBySeries:  []NameValue{{Name: "node_cpu_seconds_total", Value: 900}},
		MetricNameTotal:     3,
		MetadataMetricTotal: -1,
		Notes:               []string{"metadata total unavailable"},
	}
	var buf strings.Builder
	if err := inv.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var back Inventory
	if err := json.Unmarshal([]byte(buf.String()), &back); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if back.Head.NumSeries != 508 || len(back.TopMetricsBySeries) != 1 ||
		back.MetricNameTotal != 3 || back.MetadataMetricTotal != -1 || len(back.Notes) != 1 {
		t.Errorf("round-tripped inventory lost fields: %+v", back)
	}
}

// TestWriteText pins the honesty framing and the ranked tables appear in the
// human report.
func TestWriteText(t *testing.T) {
	srv := tsdbServer(t)
	inv, err := NewClient(srv.URL).Probe(context.Background(), Options{Top: 50, Window: "1h"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	var buf strings.Builder
	if err := inv.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"RANK RISK",                           // honesty framing
		"NOT predict cerberus's exact memory", // does not predict cerberus memory
		"node_cpu_seconds_total",
		"top 50 metrics by series count",
		"head block",
		"Observation window (operator context): 1h",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text report missing %q, got:\n%s", want, out)
		}
	}
}

// TestWriteText_EmptyHeadNoGarbageSpan pins the empty-head guard: an empty TSDB
// head reports sentinel bounds (MinTime = math.MaxInt64, MaxTime = math.MinInt64,
// so MinTime > MaxTime). The head-span line must be OMITTED rather than printing
// garbage year-292-billion timestamps. A populated head still prints its span.
func TestWriteText_EmptyHeadNoGarbageSpan(t *testing.T) {
	empty := Inventory{
		Source: "http://src", Top: 5, MetricNameTotal: -1, MetadataMetricTotal: -1,
		Head: HeadStats{NumSeries: 0, MinTime: math.MaxInt64, MaxTime: math.MinInt64},
	}
	var eb strings.Builder
	if err := empty.WriteText(&eb); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if strings.Contains(eb.String(), "head span:") {
		t.Errorf("empty head must not print a head-span line, got:\n%s", eb.String())
	}

	populated := Inventory{
		Source: "http://src", Top: 5, MetricNameTotal: -1, MetadataMetricTotal: -1,
		Head: HeadStats{NumSeries: 10, MinTime: 1_700_000_000_000, MaxTime: 1_700_003_600_000},
	}
	var pb strings.Builder
	if err := populated.WriteText(&pb); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if !strings.Contains(pb.String(), "head span:") {
		t.Errorf("populated head must print a head-span line, got:\n%s", pb.String())
	}
}

// TestReadCappedBody pins the response-body cap: a body within the limit is
// returned whole, and a body past the limit errors rather than buffering an
// unbounded stream into memory.
func TestReadCappedBody(t *testing.T) {
	const limit = 16
	got, err := readCappedBody(strings.NewReader("small"), limit)
	if err != nil {
		t.Fatalf("under-limit read errored: %v", err)
	}
	if string(got) != "small" {
		t.Errorf("under-limit body = %q, want %q", got, "small")
	}

	oversize := strings.Repeat("x", limit+1)
	if _, err := readCappedBody(strings.NewReader(oversize), limit); err == nil {
		t.Errorf("over-limit read should error, got nil for a %d-byte body (cap %d)", len(oversize), limit)
	}
}
