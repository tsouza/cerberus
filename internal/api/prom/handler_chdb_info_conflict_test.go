//go:build chdb

// chDB-backed end-to-end coverage for PromQL `info()`'s multi-info-metric
// merge: the conflicting-label abort, and the non-conflicting merge it
// must not disturb.
//
// The reference engine merges the data labels of every matched info
// series onto one output sample, and fails the query outright when two of
// them contribute the same label with different values
// (promql/info.go::combineWithInfoVector: `conflicting label: %s`).
// Cerberus does the merge in SQL, where the natural fold concatenates the
// per-metric key/value arrays — a repeated key lands in the result Map
// twice and ClickHouse's subscript silently keeps one of them, so the
// query answers 200 OK with an arbitrary one of the two values.
//
// These tests pin the observable contract at the wire: a conflict is a
// 422 execution error naming the conflict, not a 200 carrying a coin
// flip, and not a 502 blaming the backend.

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chsql"
)

// infoConflictSeedTime is the instant both the seed and the query use, so
// the bare-selector staleness window covers every seeded sample.
var infoConflictSeedTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// infoConflictSeed writes one base series and two info metrics sharing
// the `job` identity label. versionOnBuildInfo decides whether the second
// info metric collides with the first on `version` or contributes a
// disjoint `revision`.
//
// The histogram table is created because a `__name__` regex selector fans
// the scan out across the classic-histogram companion families; it stays
// empty, it just has to exist.
func infoConflictSeed(buildInfoLabels string) string {
	ts := infoConflictSeedTime.Format("2006-01-02 15:04:05.000000000")
	return gaugeDDL + `
CREATE TABLE otel_metrics_histogram (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64)
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);` + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge VALUES
    ('up',          map('job', 'api'),                       toDateTime64('%s', 9), 1.0),
    ('target_info', map('job', 'api', 'version', '1.2.3'),   toDateTime64('%s', 9), 1.0),
    ('build_info',  %s,                                      toDateTime64('%s', 9), 1.0);`,
		ts, ts, buildInfoLabels, ts)
}

// infoConflictQuery merges every `*_info` metric onto `up`, which is the
// shape that makes two info metrics meet on one base sample.
const infoConflictQuery = `info(up, {__name__=~".+_info"})`

// TestInfo_ConflictingLabel_ChDB drives the conflicting case end to end.
// `target_info` and `build_info` both carry `version`, with different
// values, so the reference engine fails the query.
func TestInfo_ConflictingLabel_ChDB(t *testing.T) {
	seed := infoConflictSeed(`map('job', 'api', 'version', '4.5.6')`)
	srv, _ := newChDBServer(t, seed)

	resp, err := http.Get(fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
		srv.URL, url.QueryEscape(infoConflictQuery), infoConflictSeedTime.Unix()))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)

	// Upstream classifies this as a query-level execution failure, not a
	// backend one: 422, errorType=execution. A 502 would be cerberus
	// blaming ClickHouse for a fault in the query.
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want %d; body=%s",
			resp.StatusCode, http.StatusUnprocessableEntity, body)
	}

	var parsed prom.Response
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, body)
	}
	if parsed.Status != "error" {
		t.Fatalf("status: got %q, want %q; body=%s", parsed.Status, "error", body)
	}
	if parsed.ErrorType != prom.ErrExecution {
		t.Fatalf("errorType: got %q, want %q; body=%s", parsed.ErrorType, prom.ErrExecution, body)
	}
	if !strings.Contains(parsed.Error, chsql.InfoConflictingLabelMessage) {
		t.Fatalf("error: got %q, want it to name %q; body=%s",
			parsed.Error, chsql.InfoConflictingLabelMessage, body)
	}
	// A ClickHouse stack frame leaking to the client would mean the abort
	// was forwarded rather than translated.
	if strings.Contains(parsed.Error, "DB::Exception") {
		t.Errorf("error leaks the ClickHouse exception: %q", parsed.Error)
	}
}

// TestInfo_NonConflictingMerge_ChDB is the other half of the contract:
// the guard must not fire on the common case, where the matched info
// metrics contribute disjoint data labels and the reference engine unions
// them onto one sample.
func TestInfo_NonConflictingMerge_ChDB(t *testing.T) {
	seed := infoConflictSeed(`map('job', 'api', 'revision', 'abc123')`)
	srv, _ := newChDBServer(t, seed)

	resp, err := http.Get(fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
		srv.URL, url.QueryEscape(infoConflictQuery), infoConflictSeedTime.Unix()))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", resp.StatusCode, body)
	}

	var parsed queryResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success; body=%s", parsed.Status, body)
	}

	rawResult, _ := json.Marshal(parsed.Data.Result)
	var vec []prom.VectorSample
	if err := json.Unmarshal(rawResult, &vec); err != nil {
		t.Fatalf("unmarshal result: %v; body=%s", err, body)
	}
	if len(vec) != 1 {
		t.Fatalf("series count: got %d, want 1; body=%s", len(vec), body)
	}
	// One sample carrying the union of both info metrics' data labels.
	for k, want := range map[string]string{
		"__name__": "up",
		"job":      "api",
		"version":  "1.2.3",
		"revision": "abc123",
	} {
		if got := vec[0].Metric[k]; got != want {
			t.Errorf("label %q: got %q, want %q; metric=%v", k, got, want, vec[0].Metric)
		}
	}
}
