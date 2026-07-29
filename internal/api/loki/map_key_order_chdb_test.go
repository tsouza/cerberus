//go:build chdb

// Behavioural coverage for the ClickHouse Map key-order invariant across
// the Loki metadata endpoints. These run against real ClickHouse
// semantics (chDB) rather than a stubQuerier, because the bug they pin
// lives entirely inside CH's evaluation of a Map-typed grouping key —
// a canned-row stub can never exhibit it.
//
// A CH Map compares, groups and hashes POSITIONALLY over its
// (keys, values) arrays, so map('job','api','dc','eu') and
// map('dc','eu','job','api') are unequal despite carrying the same label
// set. The OTel-CH exporter preserves OTLP wire order and never sorts,
// so two deliveries of ONE logical stream routinely land under two
// physical key orders. Every seed below writes exactly that shape and
// asserts the endpoint reports one stream, not two.
//
// Delete the mapSort wrap from any of the four build*SQL sites and the
// matching test here fails on a wrong number — a doubled stream count, a
// halved byte volume, a dropped top-N entry — not on a shape mismatch.

package loki_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// keyOrderWindowBase anchors every seed in this file well in the past so
// wall-clock drift during the run can never move a row in or out of the
// request window.
var keyOrderWindowBase = time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

// keyOrderSeedTable is the minimal otel_logs projection these endpoints
// read: the matcher target (ResourceAttributes), the byte-volume source
// (Body) and the window predicate column (Timestamp). Engine = Memory
// mirrors patterns_chdb_test.go — none of the four queries depends on
// MergeTree ordering.
const keyOrderSeedTable = `CREATE TABLE otel_logs (
    Timestamp DateTime64(9),
    Body String,
    ResourceAttributes Map(String, String)
) ENGINE = Memory;`

// keyOrderSeedRow is one seeded log line: a body (whose length is the
// byte volume the endpoint sums) and the literal CH map() expression
// spelling out the PHYSICAL key order it is delivered under. Writing the
// map() call out longhand rather than building it from a Go map is the
// whole point — Go map iteration order is unspecified, so only a literal
// can pin a divergent physical order deterministically.
type keyOrderSeedRow struct {
	body   string
	mapSQL string
}

// labelSetRowCounter wraps the chDB querier and records how many rows
// ClickHouse actually returned for the label-set queries (/series,
// /detected_labels).
//
// This is the observable that finding C is about. Both endpoints fold
// their rows through order-insensitive Go-side passes
// (dedupeLabelSets keys on format.CanonicalKey, summariseDetectedLabels
// uses per-key sets), so the WIRE answer is correct whether or not the
// SQL collapses divergent key orders — a response-body assertion alone
// can never detect the wrap being deleted. The row count can: every
// duplicated split row is charged against the client's row drain budget
// (chclient.Client.QueryLabelSets calls drainBudgetExceeded per row), so
// a split label set can push a response that legitimately fits over
// CERBERUS_QUERY_MAX_SAMPLES and turn a valid query into a rejection.
type labelSetRowCounter struct {
	*chclienttest.Client
	rows int
}

func (c *labelSetRowCounter) QueryLabelSets(
	ctx context.Context, sql string, args ...any,
) ([]map[string]string, error) {
	out, err := c.Client.QueryLabelSets(ctx, sql, args...)
	c.rows = len(out)
	return out, err
}

// seedKeyOrderServer builds a chDB-backed Loki handler over otel_logs
// seeded with rows, one row per second from keyOrderWindowBase. The
// returned counter exposes the CH row count of the last label-set query.
func seedKeyOrderServer(t *testing.T, rows []keyOrderSeedRow) (*httptest.Server, *labelSetRowCounter) {
	t.Helper()
	const tsFmt = "2006-01-02 15:04:05.000"

	seed := keyOrderSeedTable
	for i, r := range rows {
		ts := keyOrderWindowBase.Add(time.Duration(i) * time.Second).Format(tsFmt)
		seed += fmt.Sprintf(
			"\nINSERT INTO otel_logs (Timestamp, Body, ResourceAttributes) VALUES"+
				" (toDateTime64('%s', 9), '%s', %s);",
			ts, r.body, r.mapSQL,
		)
	}

	c := chclienttest.NewChDB(t)
	c.Seed(t, seed)
	counter := &labelSetRowCounter{Client: c}
	h := loki.New(counter, schema.DefaultOTelLogs(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, counter
}

// keyOrderWindow returns the start/end unix seconds framing every seeded
// row with a minute of slack on each side.
func keyOrderWindow() (int64, int64) {
	return keyOrderWindowBase.Add(-time.Minute).Unix(),
		keyOrderWindowBase.Add(time.Hour).Unix()
}

// getJSON issues the request and decodes the body into out, failing the
// test on any transport / status / decode error.
func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status=%d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

// splitStreamSeed is the canonical shape every test in this file starts
// from: ONE logical stream {job=api, dc=eu} delivered under two physical
// key orders, ten body bytes each. A correct backend reports one stream
// carrying twenty bytes.
func splitStreamSeed() []keyOrderSeedRow {
	return []keyOrderSeedRow{
		{body: "0123456789", mapSQL: "map('job','api','dc','eu')"},
		{body: "0123456789", mapSQL: "map('dc','eu','job','api')"},
	}
}

// TestIndexVolume_ChDB_DivergentKeyOrderCollapses is finding A. The
// endpoint has NO Go-side re-aggregation — handleIndexVolume loops CH
// rows straight into VectorSample — so a split grouping key surfaces
// verbatim on the wire as two half-height bars in Grafana's logs-volume
// panel.
func TestIndexVolume_ChDB_DivergentKeyOrderCollapses(t *testing.T) {
	srv, _ := seedKeyOrderServer(t, splitStreamSeed())
	start, end := keyOrderWindow()

	var parsed struct {
		Status string         `json:"status"`
		Data   loki.QueryData `json:"data"`
	}
	getJSON(t, fmt.Sprintf(
		`%s/loki/api/v1/index/volume?query=%%7Bjob%%3D%%22api%%22%%7D&start=%d&end=%d`,
		srv.URL, start, end,
	), &parsed)

	raw, err := json.Marshal(parsed.Data.Result)
	if err != nil {
		t.Fatalf("re-marshal result: %v", err)
	}
	var samples []loki.VectorSample
	if err := json.Unmarshal(raw, &samples); err != nil {
		t.Fatalf("decode vector: %v", err)
	}

	if len(samples) != 1 {
		t.Fatalf("one logical stream delivered under two key orders must yield ONE "+
			"vector sample; got %d: %+v", len(samples), samples)
	}
	got := samples[0].Value[1]
	gotStr, ok := got.(string)
	if !ok {
		t.Fatalf("volume value must be a string-encoded count; got %T (%v)", got, got)
	}
	// Twenty: both deliveries of the one stream carry a ten-byte body,
	// and both must land in the same group. A split grouping key reports
	// ten — the silent half-size answer this endpoint used to give.
	const wantBytes = "20"
	if gotStr != wantBytes {
		t.Errorf("byte volume must sum across both key orders: got %q want %q", gotStr, wantBytes)
	}
	if job := samples[0].Metric["job"]; job != "api" {
		t.Errorf("metric labels lost job=api: %+v", samples[0].Metric)
	}
	if dc := samples[0].Metric["dc"]; dc != "eu" {
		t.Errorf("metric labels lost dc=eu: %+v", samples[0].Metric)
	}
}

// TestIndexVolume_ChDB_DivergentKeyOrderTopN is the ranking half of
// finding A. `ORDER BY bytes DESC LIMIT n` runs over whatever groups the
// GROUP BY produced, so a split stream is ranked by its halves — and a
// genuinely-top stream can fall out of the returned set entirely, not
// merely rank lower.
//
// The seed is engineered so the split decides membership: the true #1
// carries 24 bytes across two key orders (12 + 12) while a rival carries
// 18 in one order. Under a split key the rival's 18 outranks both 12s and
// LIMIT 1 returns the WRONG stream; under a canonical key the true #1's
// 24 wins.
func TestIndexVolume_ChDB_DivergentKeyOrderTopN(t *testing.T) {
	srv, _ := seedKeyOrderServer(t, []keyOrderSeedRow{
		{body: "012345678901", mapSQL: "map('job','api','dc','eu')"},
		{body: "012345678901", mapSQL: "map('dc','eu','job','api')"},
		{body: "012345678901234567", mapSQL: "map('job','api','dc','us')"},
	})
	start, end := keyOrderWindow()

	var parsed struct {
		Data loki.QueryData `json:"data"`
	}
	getJSON(t, fmt.Sprintf(
		`%s/loki/api/v1/index/volume?query=%%7Bjob%%3D%%22api%%22%%7D&start=%d&end=%d&limit=1`,
		srv.URL, start, end,
	), &parsed)

	raw, err := json.Marshal(parsed.Data.Result)
	if err != nil {
		t.Fatalf("re-marshal result: %v", err)
	}
	var samples []loki.VectorSample
	if err := json.Unmarshal(raw, &samples); err != nil {
		t.Fatalf("decode vector: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("limit=1 must return exactly one sample; got %d: %+v", len(samples), samples)
	}
	if dc := samples[0].Metric["dc"]; dc != "eu" {
		t.Fatalf("top-N picked the wrong stream: the 24-byte dc=eu stream outranks the "+
			"18-byte dc=us one, but only when its two key orders are grouped together; got %+v",
			samples[0].Metric)
	}
	const wantTopBytes = "24"
	if got := samples[0].Value[1]; got != any(wantTopBytes) {
		t.Errorf("top stream volume: got %v want %q", got, wantTopBytes)
	}
}

// TestIndexStats_ChDB_DivergentKeyOrderCountsOnce is finding B. `streams`
// is a scalar produced by uniqExact inside ClickHouse; no Go-side pass
// can recover a doubled count, so the SQL is the only place to fix it.
func TestIndexStats_ChDB_DivergentKeyOrderCountsOnce(t *testing.T) {
	srv, _ := seedKeyOrderServer(t, splitStreamSeed())
	start, end := keyOrderWindow()

	var stats loki.IndexStats
	getJSON(t, fmt.Sprintf(
		`%s/loki/api/v1/index/stats?query=%%7Bjob%%3D%%22api%%22%%7D&start=%d&end=%d`,
		srv.URL, start, end,
	), &stats)

	if stats.Streams != 1 {
		t.Errorf("one logical stream delivered under two key orders must count as ONE "+
			"stream; got streams=%d", stats.Streams)
	}
	// Entries and Bytes are row-level aggregates and are unaffected by
	// the key-order split — asserting them pins that the canonical wrap
	// changed ONLY the identity key and did not perturb the row set.
	if stats.Entries != 2 {
		t.Errorf("entries must count both rows; got %d", stats.Entries)
	}
	if stats.Bytes != 20 {
		t.Errorf("bytes must sum both rows; got %d", stats.Bytes)
	}
}

// TestSeries_ChDB_DivergentKeyOrderYieldsOneRow is finding C for
// /series. The wire answer is asserted for completeness, but the
// load-bearing assertion is the CH row count: dedupeLabelSets already
// makes the response correct either way, so only the row count can catch
// the wrap being deleted — and the row count is precisely what the
// client's drain budget is spent on.
func TestSeries_ChDB_DivergentKeyOrderYieldsOneRow(t *testing.T) {
	srv, counter := seedKeyOrderServer(t, splitStreamSeed())
	start, end := keyOrderWindow()

	var parsed struct {
		Status string              `json:"status"`
		Data   []map[string]string `json:"data"`
	}
	getJSON(t, fmt.Sprintf(
		`%s/loki/api/v1/series?match%%5B%%5D=%%7Bjob%%3D%%22api%%22%%7D&start=%d&end=%d`,
		srv.URL, start, end,
	), &parsed)

	if counter.rows != 1 {
		t.Errorf("ClickHouse must return ONE row for one logical stream — every split row "+
			"is charged against the client drain budget; got %d rows", counter.rows)
	}
	if len(parsed.Data) != 1 {
		t.Fatalf("one logical stream must return one label set; got %d: %+v",
			len(parsed.Data), parsed.Data)
	}
	if parsed.Data[0]["job"] != "api" || parsed.Data[0]["dc"] != "eu" {
		t.Errorf("label set lost keys: %+v", parsed.Data[0])
	}
}

// TestDetectedLabels_ChDB_DivergentKeyOrderCardinality is finding C for
// /detected_labels. summariseDetectedLabels folds values into per-key
// sets so the cardinalities are right whichever way the rows arrive; as
// with /series the CH row count is the assertion that actually detects
// the wrap being deleted.
func TestDetectedLabels_ChDB_DivergentKeyOrderCardinality(t *testing.T) {
	srv, counter := seedKeyOrderServer(t, splitStreamSeed())
	start, end := keyOrderWindow()

	var data loki.DetectedLabelsData
	getJSON(t, fmt.Sprintf(
		`%s/loki/api/v1/detected_labels?query=%%7Bjob%%3D%%22api%%22%%7D&start=%d&end=%d`,
		srv.URL, start, end,
	), &data)

	if counter.rows != 1 {
		t.Errorf("ClickHouse must return ONE row for one logical stream — every split row "+
			"is charged against the client drain budget; got %d rows", counter.rows)
	}

	got := map[string]uint64{}
	for _, dl := range data.DetectedLabels {
		got[dl.Label] = dl.Cardinality
	}
	for _, k := range []string{"job", "dc"} {
		if got[k] != 1 {
			t.Errorf("label %q carries one distinct value across the two key orders; got %d "+
				"(all labels: %+v)", k, got[k], data.DetectedLabels)
		}
	}
}

// TestIndexVolume_ChDB_TargetLabelsKeyOrder pins the projected branch of
// volumeGroupFrag. mapFilter preserves the SOURCE map's key order, so
// projecting {job, dc} out of two divergently-ordered maps yields two
// divergently-ordered projections — the targetLabels path needs the
// canonical wrap exactly as much as the bare-column path does.
func TestIndexVolume_ChDB_TargetLabelsKeyOrder(t *testing.T) {
	srv, _ := seedKeyOrderServer(t, []keyOrderSeedRow{
		{body: "0123456789", mapSQL: "map('job','api','dc','eu','pod','a')"},
		{body: "0123456789", mapSQL: "map('dc','eu','job','api','pod','b')"},
	})
	start, end := keyOrderWindow()

	var parsed struct {
		Data loki.QueryData `json:"data"`
	}
	getJSON(t, fmt.Sprintf(
		`%s/loki/api/v1/index/volume?query=%%7Bjob%%3D%%22api%%22%%7D&start=%d&end=%d`+
			`&targetLabels=job,dc&aggregateBy=labels`,
		srv.URL, start, end,
	), &parsed)

	raw, err := json.Marshal(parsed.Data.Result)
	if err != nil {
		t.Fatalf("re-marshal result: %v", err)
	}
	var samples []loki.VectorSample
	if err := json.Unmarshal(raw, &samples); err != nil {
		t.Fatalf("decode vector: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("projecting to {job,dc} must collapse the two pods into ONE row; got %d: %+v",
			len(samples), samples)
	}
	if _, ok := samples[0].Metric["pod"]; ok {
		t.Errorf("targetLabels projection must drop pod: %+v", samples[0].Metric)
	}
	const wantProjectedBytes = 20
	gotStr, ok := samples[0].Value[1].(string)
	if !ok {
		t.Fatalf("volume value must be a string-encoded count; got %T", samples[0].Value[1])
	}
	gotBytes, err := strconv.Atoi(gotStr)
	if err != nil {
		t.Fatalf("volume value %q is not numeric: %v", gotStr, err)
	}
	if gotBytes != wantProjectedBytes {
		t.Errorf("projected byte volume must sum both key orders: got %d want %d",
			gotBytes, wantProjectedBytes)
	}
}
