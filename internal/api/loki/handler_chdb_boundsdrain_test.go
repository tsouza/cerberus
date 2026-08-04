//go:build chdb

// chDB-backed bounds-drain coverage for the Loki LogQL metric range path
// (/loki/api/v1/query_range over a range-aggregation query like
// count_over_time). Folds into the shared chclienttest.RunBoundsDrain
// harness (internal/chclienttest/boundsdrain.go) as the Loki row #1515
// flagged missing: the PromQL query_range row (handler_chdb_boundsdrain_test.go
// in package prom_test) and the Tempo /api/search row
// (search_trace_limit_chdb_test.go in package tempo_test) were the only two
// entrypoints wired before this file.
//
// count_over_time shares the SAME chplan.RangeWindow plan node — and the
// SAME chsql emitter (internal/chsql/range_window.go) — that PromQL's
// query_range uses. The emitter GROUPs BY (series, anchor_ts) in SQL, so
// ClickHouse returns one row per (series, step anchor) regardless of how
// many raw log lines fall inside each step's window; toMatrixStepGrid then
// does a trivial row->point copy with NO further collapsing (see its
// doc comment in handler.go). That means a regression that stopped the
// emitter from aggregating server-side would show up here as EXTRA points
// landing at the same (series, anchor) timestamp — this test seeds a raw
// log-line density many times the (series × step) output bound and asserts
// the drain tracks the bound, not the raw density, exactly the O(output)-
// not-O(input) property OOM #1 (Tempo) and OOM #2 (PromQL) both violated.
package loki_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// lokiDrainLogsDDL is the otel_logs projection the LogQL range-aggregation
// lowering touches: SeverityText + LogAttributes feed the synthesized
// detected_level label (folded into the series-identity ResourceAttributes
// map by the emitter), and ServiceName participates in the service_name
// coalesce chain alongside ResourceAttributes. Engine = Memory keeps the
// seed fast; the range-aggregation SQL does its own GROUP BY regardless of
// table engine.
const lokiDrainLogsDDL = `CREATE TABLE otel_logs (
    Timestamp DateTime64(9),
    Body String,
    SeverityText LowCardinality(String) DEFAULT '',
    ResourceAttributes Map(String, String),
    ServiceName String DEFAULT '',
    LogAttributes Map(String, String)
) ENGINE = Memory;`

// Bounds-drain seed scale for the Loki metric range path. Mirrors the
// PromQL row's shape (internal/api/prom/handler_chdb_boundsdrain_test.go):
// the INPUT axis (raw log lines) is made to dwarf the OUTPUT axis
// (series × step) so a bounded drain (O(output)) and an unbounded one
// (O(input)) produce wildly different counts.
//
//   - lokiDrainServiceCount distinct series (the cardinality axis),
//   - lokiDrainStepCount step anchors across the request window (the
//     output axis),
//   - lokiDrainLinesPerWindow raw log lines seeded inside EACH per-step
//     window (the input-density axis).
//
// The query's range == step (both lokiDrainStep) keeps step windows
// non-overlapping (tumbling, not sliding), so each raw line contributes to
// exactly one anchor bucket — output bound is exactly
// lokiDrainServiceCount × lokiDrainStepCount, one row per (series, anchor).
const (
	lokiDrainServiceCount   = 20
	lokiDrainStepCount      = 13
	lokiDrainLinesPerWindow = 6
	lokiDrainStep           = time.Minute
)

// seedManyServicesDenseWindows plants lokiDrainServiceCount services, each
// carrying lokiDrainLinesPerWindow log lines inside every per-step window
// across lokiDrainStepCount anchors. Raw row count is therefore
// lokiDrainServiceCount × lokiDrainStepCount × lokiDrainLinesPerWindow (the
// input axis), while the count_over_time range-aggregation collapses to one
// row per (series, anchor) server-side (the output axis) — see the package
// doc comment above.
func seedManyServicesDenseWindows(start time.Time) (ddl string, fullSeed int64) {
	const tsFmt = "2006-01-02 15:04:05.000000000"
	rows := make([]string, 0, lokiDrainServiceCount*lokiDrainStepCount*lokiDrainLinesPerWindow)
	for s := 0; s < lokiDrainServiceCount; s++ {
		svc := fmt.Sprintf("svc-%03d", s)
		for step := 0; step < lokiDrainStepCount; step++ {
			anchor := start.Add(time.Duration(step) * lokiDrainStep)
			// Spread lokiDrainLinesPerWindow lines through the seconds
			// leading up to (and including) the anchor, all inside the
			// current tumbling window (anchor-1m, anchor] and clear of the
			// PREVIOUS window's boundary.
			for k := 0; k < lokiDrainLinesPerWindow; k++ {
				ts := anchor.Add(-time.Duration(k) * time.Second).Format(tsFmt)
				rows = append(rows, fmt.Sprintf(
					"    (toDateTime64('%s', 9), 'line', map('service_name', '%s'))",
					ts, svc,
				))
			}
		}
	}
	ddl = lokiDrainLogsDDL + "\nINSERT INTO otel_logs (Timestamp, Body, ResourceAttributes) VALUES\n" +
		strings.Join(rows, ",\n") + ";"
	return ddl, int64(len(rows))
}

// boundsDrainLokiServer builds a chDB-backed Loki handler + httptest server,
// returning the handler so the caller can install the drain hook before
// driving /loki/api/v1/query_range.
func boundsDrainLokiServer(t *testing.T, ddl string) (*loki.Handler, *httptest.Server) {
	t.Helper()
	c := chclienttest.NewChDB(t)
	c.Seed(t, ddl)
	h := loki.New(c, schema.DefaultOTelLogs(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return h, srv
}

// lokiRangeMatrixResponse decodes the /loki/api/v1/query_range envelope for
// a metric-form (matrix) result — QueryData.Result is `any` in production,
// so the test pins the concrete shape it expects back.
type lokiRangeMatrixResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string              `json:"resultType"`
		Result     []loki.MatrixSample `json:"result"`
	} `json:"data"`
}

// lokiRangeDrainCase builds the Loki /loki/api/v1/query_range bounds-drain
// row. It seeds many services with dense per-window log lines, installs the
// drain hook, drives a count_over_time query at a fixed (range == step)
// tumbling window, and returns the streaming-drain count plus the full seed
// for the harness's two assertions.
func lokiRangeDrainCase() chclienttest.BoundsDrainCase {
	return chclienttest.BoundsDrainCase{
		Name:        "loki/query_range/count_over_time",
		OutputBound: int64(lokiDrainServiceCount * lokiDrainStepCount),
		Run: func(t *testing.T) (drain, fullSeed int64) {
			start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
			end := start.Add(time.Duration(lokiDrainStepCount-1) * lokiDrainStep)

			ddl, full := seedManyServicesDenseWindows(start)
			h, srv := boundsDrainLokiServer(t, ddl)

			var got int64
			h.SetOnQueryRangeDrain(func(n int64) { got = n })

			q := fmt.Sprintf(`count_over_time({service_name=~"svc-.*"}[%s])`, formatLogQLDuration(lokiDrainStep))
			reqURL := fmt.Sprintf("%s/loki/api/v1/query_range?query=%s&start=%d&end=%d&step=%s",
				srv.URL, url.QueryEscape(q), start.Unix(), end.Unix(), formatLogQLDuration(lokiDrainStep))

			resp, err := http.Get(reqURL)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var out lokiRangeMatrixResponse
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if out.Data.ResultType != "matrix" {
				t.Fatalf("resultType = %q, want matrix", out.Data.ResultType)
			}

			// Anti-vacuous shape check: every seeded service must appear, else
			// a selector/window bug could silently zero the drain and the
			// bound would pass for the wrong reason (the same guard the
			// PromQL row applies to its matrix).
			if len(out.Data.Result) != lokiDrainServiceCount {
				t.Fatalf("got %d series, want %d — the output matrix is not the (series × step) shape the bound names",
					len(out.Data.Result), lokiDrainServiceCount)
			}

			return got, full
		},
	}
}

// formatLogQLDuration renders a time.Duration in the compact unit LogQL's
// duration literal grammar accepts (e.g. "1m"), matching how Grafana's Loki
// datasource and the range-mode fixtures spell range/step literals.
func formatLogQLDuration(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}

// TestBoundsDrain_LokiQueryRange_ChDB is the Loki row for the shared
// bounds-drain gate (chclienttest.RunBoundsDrain): it seeds at scale, drives
// count_over_time end to end against a real chDB drain, and the harness
// asserts the drain is O(output) (<= OutputBound × fudge) AND a real
// reduction below the full seed. Falsifiability: neutering the SQL-side
// per-(series, anchor) GROUP BY in the shared chsql RangeWindow emitter (the
// same emitter PromQL's query_range depends on) would turn the measured
// drain from lokiDrainServiceCount × lokiDrainStepCount toward the full seed
// (× lokiDrainLinesPerWindow), failing the bound assertion.
func TestBoundsDrain_LokiQueryRange_ChDB(t *testing.T) {
	chclienttest.RunBoundsDrain(t, []chclienttest.BoundsDrainCase{
		lokiRangeDrainCase(),
	})
}
