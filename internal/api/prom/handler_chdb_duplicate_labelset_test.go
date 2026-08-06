//go:build chdb

// chDB-backed wire-level pins for the duplicate-labelset abort a
// name-dropping range function raises when two input series that differed
// only by `__name__` collapse onto one label set (#1811).
//
// Reference Prometheus refuses that query rather than answering it:
// `Matrix.ContainsSameLabelset()` → `ev.errorf("vector cannot contain
// metrics with the same labelset")` (promql/engine.go:2295, and the three
// sibling sites at :1531 / :1552 / :4109). Cerberus reduces in ClickHouse,
// where `GROUP BY Attributes` merges the two series by construction — so
// without a guard the query returns a plausible-looking single row for a
// question upstream declines to answer.
//
// The guard is planted in SQL, at the inner name-drop boundary, rather
// than on the result path. That placement is forced by the nested shape
// TestQueryRange_Subquery_RateInner_DuplicateLabelset_ChDB drives: the
// inner window has already merged the two series before any row reaches
// Go, so a Go-side "did two output series share a label set" check would
// see one row and find nothing wrong. Upstream errors at the inner call
// for the same reason.
//
// Every assertion here is at the wire: status code, errorType and message
// text, which is what Grafana surfaces as an error rather than as an empty
// or silently-wrong panel.

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
	"github.com/tsouza/cerberus/internal/chplan"
)

// dupLabelsetSeed writes `cpu_temp` and `gpu_temp` one sample per minute
// across [start-15m, end], under the caller-supplied per-metric attribute
// maps. Passing the SAME map for both is the collision; passing disjoint
// maps is the control.
func dupLabelsetSeed(t *testing.T, start, end time.Time, cpuAttrs, gpuAttrs string) string {
	t.Helper()
	const (
		seedLead  = 15 * time.Minute
		seedEvery = time.Minute
	)
	rows := ""
	value := 0
	for ts := start.Add(-seedLead); !ts.After(end); ts = ts.Add(seedEvery) {
		lit := ts.Format("2006-01-02 15:04:05.000000000")
		if rows != "" {
			rows += ",\n    "
		}
		rows += fmt.Sprintf(
			"('cpu_temp', '', '', %[2]s, toDateTime64('%[1]s', 9), %[4]d.0),\n    "+
				"('gpu_temp', '', '', %[3]s, toDateTime64('%[1]s', 9), %[5]d.0)",
			lit, cpuAttrs, gpuAttrs, 40+value, 70+value,
		)
		value++
	}
	return metaShapedMetricsDDL + `
INSERT INTO otel_metrics_gauge (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ` + rows + ";"
}

// assertDuplicateLabelsetRejected pins the whole wire contract of the
// abort: 422 (a query-level fault, not a 502 blaming ClickHouse), the
// execution errorType upstream uses for evaluation failures, upstream's
// verbatim message — which is what makes the rejection-parity comparison
// against reference Prometheus meaningful — and no leaked ClickHouse
// stack frame, which would mean the abort was forwarded rather than
// translated.
func assertDuplicateLabelsetRejected(t *testing.T, body string, status int, query string) {
	t.Helper()
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("%s: status: got %d, want %d; body=%s",
			query, status, http.StatusUnprocessableEntity, body)
	}
	var parsed prom.Response
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("%s: unmarshal: %v; body=%s", query, err, body)
	}
	if parsed.Status != "error" {
		t.Fatalf("%s: status: got %q, want %q; body=%s", query, parsed.Status, "error", body)
	}
	if parsed.ErrorType != prom.ErrExecution {
		t.Fatalf("%s: errorType: got %q, want %q; body=%s",
			query, parsed.ErrorType, prom.ErrExecution, body)
	}
	if !strings.Contains(parsed.Error, chplan.DuplicateLabelsetMessage) {
		t.Fatalf("%s: error: got %q, want it to name %q; body=%s",
			query, parsed.Error, chplan.DuplicateLabelsetMessage, body)
	}
	if strings.Contains(parsed.Error, "DB::Exception") {
		t.Errorf("%s: error leaks the ClickHouse exception: %q", query, parsed.Error)
	}
}

// getBody issues a GET and returns the status and body together, so a
// rejection can be asserted without a helper that pre-checks for 200.
func getBody(t *testing.T, reqURL string) (int, string) {
	t.Helper()
	resp, err := http.Get(reqURL)
	if err != nil {
		t.Fatalf("GET %s: %v", reqURL, err)
	}
	return resp.StatusCode, readBody(t, resp)
}

// TestQuery_RateMultiName_DuplicateLabelset_ChDB is #1811's simplest
// shape: one name-dropping range function over a selector spanning two
// metrics that share an attribute set. Upstream rejects it; so must
// cerberus.
func TestQuery_RateMultiName_DuplicateLabelset_ChDB(t *testing.T) {
	start, end, _ := subqueryNameWindow()
	srv, _ := newChDBServer(t, dupLabelsetSeed(t, start, end,
		"map('host', 'a')", "map('host', 'a')"))

	const query = `rate({__name__=~"cpu_temp|gpu_temp"}[5m])`
	status, body := getBody(t, fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
		srv.URL, url.QueryEscape(query), end.Unix()))
	assertDuplicateLabelsetRejected(t, body, status, query)
}

// TestQueryRange_Subquery_RateInner_DuplicateLabelset_ChDB is the shape
// that decided where the guard lives. `last_over_time` reads the merged
// output of the inner `rate` window, so by the time any row exists the two
// source series are already one and their identity is unrecoverable. The
// abort has to fire at the INNER call — which is exactly where upstream
// raises it.
func TestQueryRange_Subquery_RateInner_DuplicateLabelset_ChDB(t *testing.T) {
	start, end, step := subqueryNameWindow()
	srv, _ := newChDBServer(t, dupLabelsetSeed(t, start, end,
		"map('host', 'a')", "map('host', 'a')"))

	const query = `last_over_time(rate({__name__=~"cpu_temp|gpu_temp"}[5m])[10m:1m])`
	status, body := getBody(t, fmt.Sprintf(
		"%s/api/v1/query_range?query=%s&start=%d&end=%d&step=%d",
		srv.URL, url.QueryEscape(query), start.Unix(), end.Unix(), int(step.Seconds()),
	))
	assertDuplicateLabelsetRejected(t, body, status, query)
}

// TestQuery_RateMultiName_DistinctLabelsets_ChDB is the other half of the
// boundary, and the half that makes the guard a match for upstream rather
// than merely stricter than it: the SAME multi-metric name-dropping query
// over series whose attribute sets differ must still answer, with one
// nameless series per source series. Erroring here would be the same class
// of bug in the other direction.
func TestQuery_RateMultiName_DistinctLabelsets_ChDB(t *testing.T) {
	start, end, _ := subqueryNameWindow()
	srv, _ := newChDBServer(t, dupLabelsetSeed(t, start, end,
		"map('host', 'a')", "map('host', 'b')"))

	const query = `rate({__name__=~"cpu_temp|gpu_temp"}[5m])`
	status, body := getBody(t, fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
		srv.URL, url.QueryEscape(query), end.Unix()))
	if status != http.StatusOK {
		t.Fatalf("%s: status: got %d, want 200; body=%s", query, status, body)
	}

	var parsed queryResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, body)
	}
	rawResult, _ := json.Marshal(parsed.Data.Result)
	var vec []prom.VectorSample
	if err := json.Unmarshal(rawResult, &vec); err != nil {
		t.Fatalf("decode vector: %v; body=%s", err, body)
	}
	if len(vec) != 2 {
		t.Fatalf("%s: got %d series, want 2 (host=a and host=b are distinct label sets): %+v",
			query, len(vec), vec)
	}
	seen := map[string]bool{}
	for _, s := range vec {
		if name, ok := s.Metric["__name__"]; ok {
			t.Errorf("%s: __name__ %q survived rate (full metric: %+v)", query, name, s.Metric)
		}
		seen[s.Metric["host"]] = true
	}
	for _, want := range []string{"a", "b"} {
		if !seen[want] {
			t.Errorf("%s: expected a series with host=%q, got %v", query, want, seen)
		}
	}
}
