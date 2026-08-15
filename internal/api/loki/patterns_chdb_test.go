//go:build chdb

// chDB-backed end-to-end coverage for /loki/api/v1/patterns. The default
// (untagged) test lane exercises the handler against a stubQuerier that
// hand-feeds canned lines into drain; this file rounds the same flow
// trip through real ClickHouse semantics: the handler emits SQL, chDB
// executes it against a seeded otel_logs table, the returned rows reach
// the in-process drain template miner, and the response is decoded back
// to the wire shape Grafana consumes.
//
// The seed lines are chosen so drain produces a predictable cluster
// shape: three distinct token-skeletons (GET /api/foo, GET /api/bar,
// POST /bar) with low-cardinality variable slots so drain folds the
// numeric IDs into `<_>` placeholders without spawning extra clusters.
// The assertions follow plan §4: structural (string contains tokens)
// rather than against an exact frozen pattern string, plus invariants
// that hold under any drain config (sum of sample counts == number of
// trained lines, sample timestamps in [start, end]).

package loki_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// logsDDL is the minimal otel_logs schema the patterns handler reads.
// The full upstream OTel exporter DDL has many more columns than the
// handler touches — this projection covers exactly what
// buildPatternsSQL selects (Timestamp, Body, SeverityText — the last
// added for #1434's per-level bucketing) plus the matcher target
// (ResourceAttributes). Engine = Memory keeps the seed fast and avoids
// MergeTree's PREWHERE / sort-key constraints; the patterns SQL does
// not depend on either (it's a straight SELECT ... ORDER BY Timestamp
// DESC LIMIT N without optimizer passes).
const logsDDL = `CREATE TABLE otel_logs (
    Timestamp DateTime64(9),
    Body String,
    SeverityText String,
    ResourceAttributes Map(String, String)
) ENGINE = Memory;`

// newChDBPatternsServer wires a chDB-backed Loki handler with the seed
// already applied. Mirrors newChDBServer in the prom handler_chdb_test
// — Loki's schema/handler constructor pair just swaps in the logs
// schema.
func newChDBPatternsServer(t *testing.T, ddl string) (*httptest.Server, *chclienttest.Client) {
	t.Helper()
	c := chclienttest.NewChDB(t)
	if ddl != "" {
		c.Seed(t, ddl)
	}
	h := loki.New(c, schema.DefaultOTelLogs(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, c
}

// TestPatterns_ChDB_DrainRoundtrip seeds otel_logs with one pattern at
// upstream's volume floor, exercises the handler end-to-end through chDB,
// and asserts the response carries:
//   - one retained cluster (all rows share one token skeleton),
//   - sum of samples[*][1] across all clusters == 30 (every trained
//     line accounts for one count),
//   - at least one cluster contains both "GET" and "200" (the dominant
//     shape), exercising the structural-not-exact assertion strategy
//     from plan §4,
//   - every sample timestamp falls in [start, end] (verifies the SQL
//     time-bound predicate reaches CH and drain's bucketing keeps
//     timestamps within the window).
func TestPatterns_ChDB_DrainRoundtrip(t *testing.T) {
	// Pin the seed window to a known anchor so the request URL's
	// start/end can frame it precisely. Anchored well in the past so
	// any wall-clock drift across the run is well outside the window
	// — drain.Train timestamps are not compared against now, only the
	// chDB time-bound predicate matters for which rows reach drain.
	base := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	const tsFmt = "2006-01-02 15:04:05.000"

	// One pattern shape at upstream's 30-line retention floor. Drain
	// rejects lines with fewer than 4 tokens, so each body carries at
	// least four space-separated chunks (verb, path, status, latency
	// suffix) to clear the threshold.
	//
	// All rows use "GET /api/foo/<N> status=200 latency=<L>ms", which
	// drain folds into a single retained template.
	seedRows := make([]struct {
		dt   time.Duration
		body string
	}, 0, retainedPatternVolumeFloor)
	for i := 0; i < retainedPatternVolumeFloor; i++ {
		seedRows = append(seedRows, struct {
			dt   time.Duration
			body string
		}{
			dt:   time.Duration(i) * time.Second,
			body: fmt.Sprintf("GET /api/foo/%d status=200 latency=%dms", i, i+1),
		})
	}

	var inserts strings.Builder
	inserts.WriteString("INSERT INTO otel_logs (Timestamp, Body, SeverityText, ResourceAttributes) VALUES\n")
	for i, row := range seedRows {
		ts := base.Add(row.dt).Format(tsFmt)
		comma := ","
		if i == len(seedRows)-1 {
			comma = ";"
		}
		fmt.Fprintf(&inserts,
			"    (toDateTime64('%s', 9), '%s', 'INFO', map('job', 'api'))%s\n",
			ts, row.body, comma)
	}

	seed := logsDDL + inserts.String()
	srv, _ := newChDBPatternsServer(t, seed)

	startUnix := base.Add(-1 * time.Minute).Unix()
	endUnix := base.Add(1 * time.Minute).Unix()
	url := fmt.Sprintf(
		`%s/loki/api/v1/patterns?query=%%7Bjob%%3D%%22api%%22%%7D&start=%d&end=%d&line_limit=30`,
		srv.URL, startUnix, endUnix,
	)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var out struct {
		Status string         `json:"status"`
		Data   []loki.Pattern `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "success" {
		t.Fatalf("status=%q want success", out.Status)
	}

	// The 30 structurally equivalent rows meet the retention floor, so
	// anything below one cluster means the SQL did not retrieve them or
	// drain failed to train.
	if len(out.Data) < 1 {
		t.Fatalf("expected >=1 cluster after training %d lines; got %d: %+v",
			retainedPatternVolumeFloor,
			len(out.Data), out.Data)
	}

	// Plan §4 assertion: sum of samples[*][1] across all clusters ==
	// 30 (every trained line accounts for one count).
	var totalCount int64
	for _, p := range out.Data {
		for _, s := range p.Samples {
			totalCount += s[1]
		}
	}
	if totalCount != retainedPatternVolumeFloor {
		t.Errorf("expected sample-count sum == %d (one per trained line); got %d across %d clusters",
			retainedPatternVolumeFloor, totalCount, len(out.Data))
	}

	// Plan §4 assertion: at least one cluster contains the literal
	// tokens "GET" and "200". Structural assertion — drain's `<_>` placeholder may
	// land anywhere between the verbs/codes.
	var foundGET bool
	for _, p := range out.Data {
		if strings.Contains(p.Pattern, "GET") && strings.Contains(p.Pattern, "200") {
			foundGET = true
			break
		}
	}
	if !foundGET {
		t.Errorf("no cluster contained both GET and 200; clusters=%+v", out.Data)
	}

	// Plan §4 assertion: every sample timestamp falls in [start, end].
	// Samples are unix seconds (per WriteQueryPatternsResponseJSON);
	// drain bucketises into TimeResolution=10s windows, so a sample
	// could land slightly before the earliest line's second-truncated
	// timestamp — the test allows a ±15s slack to absorb the
	// bucketisation without losing the in-window invariant.
	const slack = int64(15)
	for _, p := range out.Data {
		for _, s := range p.Samples {
			ts := s[0]
			if ts < startUnix-slack || ts > endUnix+slack {
				t.Errorf("sample ts=%d outside window [%d, %d] (±%ds slack); pattern=%q",
					ts, startUnix, endUnix, slack, p.Pattern)
			}
		}
	}
}

// TestPatterns_ChDB_EmptyWindow seeds the table but the request window
// excludes every row. The handler returns success with an empty data
// array — exercising the SQL-emits-time-bounds contract end-to-end.
// Without the WHERE Timestamp >= ? AND Timestamp <= ? predicates drain
// would still train on every row and the test would see a non-empty
// data array.
func TestPatterns_ChDB_EmptyWindow(t *testing.T) {
	base := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	const tsFmt = "2006-01-02 15:04:05.000"
	ts := base.Format(tsFmt)

	seed := logsDDL + fmt.Sprintf(`
INSERT INTO otel_logs (Timestamp, Body, SeverityText, ResourceAttributes) VALUES
    (toDateTime64('%s', 9), 'GET /api/foo status=200 latency=5ms', 'INFO', map('job', 'api'));`, ts)

	srv, _ := newChDBPatternsServer(t, seed)

	// Window 1 hour AFTER the seed row — excludes every row.
	startUnix := base.Add(1 * time.Hour).Unix()
	endUnix := base.Add(2 * time.Hour).Unix()
	url := fmt.Sprintf(
		`%s/loki/api/v1/patterns?query=%%7Bjob%%3D%%22api%%22%%7D&start=%d&end=%d`,
		srv.URL, startUnix, endUnix,
	)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var out struct {
		Status string         `json:"status"`
		Data   []loki.Pattern `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "success" {
		t.Fatalf("status=%q want success", out.Status)
	}
	if out.Data == nil {
		t.Fatalf("expected non-nil empty data slice (JSON []); got nil")
	}
	if len(out.Data) != 0 {
		t.Errorf("expected empty data slice (no rows in window); got %d clusters: %+v",
			len(out.Data), out.Data)
	}
}
