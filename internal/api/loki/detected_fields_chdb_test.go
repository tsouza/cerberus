//go:build chdb

// chDB-backed end-to-end coverage for /loki/api/v1/detected_fields.
// The default (untagged) test lane exercises the handler against a
// stubQuerier that hand-feeds canned (Body, LogAttributes,
// ResourceAttributes) rows; this file round-trips the same flow through
// real ClickHouse semantics: the handler emits the three-column peek
// SQL, chDB executes it against a seeded otel_logs table (Map columns
// included), and the response is decoded back exactly as Grafana's
// Logs Drilldown reads it — top-level `fields`, no envelope.

package loki_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
)

// detectedFieldsLogsDDL is the minimal otel_logs projection
// buildDetectedFieldsSQL touches: the three selected columns plus the
// Timestamp ordering key. Engine = Memory — the peek SQL is a straight
// SELECT ... ORDER BY Timestamp DESC LIMIT N with no optimizer passes.
const detectedFieldsLogsDDL = `CREATE TABLE otel_logs (
    Timestamp DateTime64(9),
    Body String,
    LogAttributes Map(String, String),
    ResourceAttributes Map(String, String)
) ENGINE = Memory;`

// TestDetectedFields_ChDB_Roundtrip seeds otel_logs with logfmt bodies
// + LogAttributes structured metadata, exercises the handler end-to-end
// through chDB, and asserts the consumer-decoded response carries:
//   - the logfmt-parsed fields (status / latency) with parser tags,
//   - the structured-metadata field (detected_level) with nil parsers,
//   - cardinalities that match the seeded distinct-value counts,
//   - the selector predicate + time bounds applied in CH (an
//     out-of-window row and a non-matching stream are excluded).
func TestDetectedFields_ChDB_Roundtrip(t *testing.T) {
	base := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	const tsFmt = "2006-01-02 15:04:05.000"

	type seedRow struct {
		dt    time.Duration
		body  string
		level string
		job   string
	}
	seedRows := []seedRow{
		{0 * time.Second, "status=200 latency=5ms", "info", "api"},
		{1 * time.Second, "status=500 latency=22ms", "error", "api"},
		{2 * time.Second, "status=200 latency=9ms", "info", "api"},
		// Different stream — the {job="api"} selector must exclude it.
		{3 * time.Second, "status=301 latency=2ms", "warn", "web"},
		// Outside the request window — the time bound must exclude it.
		{2 * time.Hour, "status=418 latency=1ms", "debug", "api"},
	}

	var inserts strings.Builder
	inserts.WriteString("INSERT INTO otel_logs (Timestamp, Body, LogAttributes, ResourceAttributes) VALUES\n")
	for i, row := range seedRows {
		ts := base.Add(row.dt).Format(tsFmt)
		comma := ","
		if i == len(seedRows)-1 {
			comma = ";"
		}
		fmt.Fprintf(&inserts,
			"    (toDateTime64('%s', 9), '%s', map('detected_level', '%s'), map('job', '%s'))%s\n",
			ts, row.body, row.level, row.job, comma)
	}

	srv, _ := newChDBPatternsServer(t, detectedFieldsLogsDDL+inserts.String())

	startUnix := base.Add(-1 * time.Minute).Unix()
	endUnix := base.Add(1 * time.Minute).Unix()
	url := fmt.Sprintf(
		`%s/loki/api/v1/detected_fields?query=%%7Bjob%%3D%%22api%%22%%7D&start=%d&end=%d`,
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

	// Decode exactly as the consumer does: bare top-level fields.
	var out loki.DetectedFieldsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byLabel := map[string]loki.DetectedField{}
	for _, f := range out.Fields {
		byLabel[f.Label] = f
	}

	status, ok := byLabel["status"]
	if !ok {
		t.Fatalf("status missing from fields: %+v", out.Fields)
	}
	if status.Type != "int" {
		t.Errorf("status.type=%q want int", status.Type)
	}
	// In-window api rows carry status ∈ {200, 500}: the web stream's
	// 301 and the out-of-window 418 must not contribute.
	if status.Cardinality != 2 {
		t.Errorf("status.cardinality=%d want 2 (selector + window applied in CH)", status.Cardinality)
	}
	if len(status.Parsers) != 1 || status.Parsers[0] != "logfmt" {
		t.Errorf("status.parsers=%v want [logfmt]", status.Parsers)
	}

	latency, ok := byLabel["latency"]
	if !ok {
		t.Fatalf("latency missing from fields: %+v", out.Fields)
	}
	if latency.Type != "duration" {
		t.Errorf("latency.type=%q want duration", latency.Type)
	}

	level, ok := byLabel["detected_level"]
	if !ok {
		t.Fatalf("detected_level missing from fields: %+v", out.Fields)
	}
	if level.Parsers != nil {
		t.Errorf("detected_level.parsers=%v want nil (structured metadata)", level.Parsers)
	}
	if level.Cardinality != 2 {
		t.Errorf("detected_level.cardinality=%d want 2 (info+error)", level.Cardinality)
	}
}
