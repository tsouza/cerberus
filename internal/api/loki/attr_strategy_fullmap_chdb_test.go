//go:build chdb

// chDB-backed end-to-end coverage for cerberus issue #3063 point 2: the
// ad-hoc discovery-endpoint query builders (/labels, /series,
// /detected_labels, /detected_fields, /label/<name>/values) never route
// through chplan/engine.emitForHead, so they need their own JSON-aware
// rendering (attr_strategy.go's attrMapFrag / distinctAttrKeysFrag,
// chsql's Builder.MapAt fix) gated on the SAME Handler.AttrStrategies the
// normal query path resolves. Each test here runs the SAME HTTP request
// against a Map-typed otel_logs table and a logically equivalent
// JSON-typed one, and asserts the decoded responses agree — genuine chDB
// execution through the real HTTP handler, not a hand-built comparison
// string.
package loki_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// fullMapDiscoverySeedBase is the fixed anchor every seed row in this file
// is offset from, so request URLs can frame [start, end] precisely.
var fullMapDiscoverySeedBase = time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

const fullMapDiscoveryTSFmt = "2006-01-02 15:04:05.000"

// fullMapDiscoveryMapDDL / fullMapDiscoveryJSONDDL seed logically
// equivalent otel_logs tables — one Map(String,String), one JSON — with:
//
//   - two rows on stream job=api (service.name=svc-a), one on job=web
//     (service.name=svc-b) — /series and /detected_labels must group these
//     into the same two/one distinct stream identities on both tables;
//   - an http.status_code attribute present on the api rows only, so
//     /labels' key set and /label/http.status_code/values' value set both
//     exercise a dotted OTel key end to end;
//   - one row outside the request window, excluded by the time bound on
//     both tables identically.
const fullMapDiscoveryMapDDL = `CREATE TABLE %[5]s (
    Timestamp DateTime64(9),
    Body String,
    LogAttributes Map(String, String),
    ResourceAttributes Map(String, String)
) ENGINE = Memory;

INSERT INTO %[5]s (Timestamp, Body, LogAttributes, ResourceAttributes) VALUES
    (toDateTime64('%[1]s', 9), 'line one', map(), map('job', 'api', 'service.name', 'svc-a', 'http.status_code', '200')),
    (toDateTime64('%[2]s', 9), 'line two', map(), map('job', 'api', 'service.name', 'svc-a', 'http.status_code', '500')),
    (toDateTime64('%[3]s', 9), 'line three', map(), map('job', 'web', 'service.name', 'svc-b')),
    (toDateTime64('%[4]s', 9), 'line outside window', map(), map('job', 'api', 'service.name', 'svc-a', 'http.status_code', '404'));
`

const fullMapDiscoveryJSONDDL = `CREATE TABLE %[5]s (
    Timestamp DateTime64(9),
    Body String,
    LogAttributes JSON,
    ResourceAttributes JSON
) ENGINE = Memory;

INSERT INTO %[5]s (Timestamp, Body, LogAttributes, ResourceAttributes) VALUES
    (toDateTime64('%[1]s', 9), 'line one', '{}', '{"job":"api","service.name":"svc-a","http.status_code":"200"}'),
    (toDateTime64('%[2]s', 9), 'line two', '{}', '{"job":"api","service.name":"svc-a","http.status_code":"500"}'),
    (toDateTime64('%[3]s', 9), 'line three', '{}', '{"job":"web","service.name":"svc-b"}'),
    (toDateTime64('%[4]s', 9), 'line outside window', '{}', '{"job":"api","service.name":"svc-a","http.status_code":"404"}');
`

// newDiscoveryChDBServer boots a real chDB-backed Handler against ddl
// (formatted with the four seed-row timestamps plus a distinct table
// name), with AttrStrategies wired exactly as cmd/cerberus's boot path
// wires it for a JSON-typed schema — nil for a Map-typed table
// (attr_strategy.go's attrMapFrag / distinctAttrKeysFrag's own no-op
// default, identical to every call site that predates cerberus issue
// #3063).
//
// tableName MUST differ between the Map and JSON variant a single test
// function boots: chdb-go shares one engine (and one default database)
// across the whole test process (chclienttest.Client.Seed's own doc), so
// two servers built back to back against the SAME table name have the
// second's `CREATE OR REPLACE TABLE` silently swap the first's
// already-built httptest.Server onto a table of the OTHER physical type
// before either has served a single request — exactly the false failure
// this comment exists to head off.
func newDiscoveryChDBServer(t *testing.T, ddlTemplate, tableName string, jsonStrategy bool) *httptest.Server {
	t.Helper()
	ddl := fmt.Sprintf(
		ddlTemplate,
		fullMapDiscoverySeedBase.Format(fullMapDiscoveryTSFmt),
		fullMapDiscoverySeedBase.Add(1*time.Second).Format(fullMapDiscoveryTSFmt),
		fullMapDiscoverySeedBase.Add(2*time.Second).Format(fullMapDiscoveryTSFmt),
		fullMapDiscoverySeedBase.Add(2*time.Hour).Format(fullMapDiscoveryTSFmt),
		tableName,
	)
	c := chclienttest.NewChDB(t)
	c.Seed(t, ddl)
	s := schema.DefaultOTelLogs()
	s.LogsTable = tableName
	h := loki.New(c, s, nil)
	if jsonStrategy {
		h.AttrStrategies = chsql.AttrStrategies{
			s.ResourceAttributesColumn: chsql.AttrStrategyJSON,
			s.AttributesColumn:         chsql.AttrStrategyJSON,
		}
		h.Lang.AttrStrategies = h.AttrStrategies
	}
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newMapDiscoveryServer(t *testing.T) *httptest.Server {
	return newDiscoveryChDBServer(t, fullMapDiscoveryMapDDL, "otel_logs_fullmap_map", false)
}

func newJSONDiscoveryServer(t *testing.T) *httptest.Server {
	return newDiscoveryChDBServer(t, fullMapDiscoveryJSONDDL, "otel_logs_fullmap_json", true)
}

func discoveryWindowParams() (startUnix, endUnix int64) {
	return fullMapDiscoverySeedBase.Add(-1 * time.Minute).Unix(), fullMapDiscoverySeedBase.Add(1 * time.Minute).Unix()
}

func decodeStringSliceResponse(t *testing.T, resp *http.Response) []string {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sort.Strings(out.Data)
	return out.Data
}

// TestLabels_JSONStrategy_ChDB pins /loki/api/v1/labels: the JSON-typed
// table's key set (via distinctAttrKeysFrag's JSONAllPaths branch) must
// match the Map-typed table's (.keys subcolumn branch) exactly, dotted
// OTel key (http.status_code) included.
func TestLabels_JSONStrategy_ChDB(t *testing.T) {
	startUnix, endUnix := discoveryWindowParams()
	url := func(base string) string {
		return fmt.Sprintf("%s/loki/api/v1/labels?start=%d&end=%d", base, startUnix, endUnix)
	}

	mapSrv := newMapDiscoveryServer(t)
	jsonSrv := newJSONDiscoveryServer(t)

	mapResp, err := http.Get(url(mapSrv.URL))
	if err != nil {
		t.Fatalf("GET map: %v", err)
	}
	jsonResp, err := http.Get(url(jsonSrv.URL))
	if err != nil {
		t.Fatalf("GET json: %v", err)
	}

	mapGot := decodeStringSliceResponse(t, mapResp)
	jsonGot := decodeStringSliceResponse(t, jsonResp)

	if len(mapGot) == 0 {
		t.Fatal("Map-typed /labels returned no keys — seed or window is broken, test proves nothing")
	}
	if fmt.Sprint(mapGot) != fmt.Sprint(jsonGot) {
		t.Errorf("/labels: Map = %v, JSON = %v, want equal", mapGot, jsonGot)
	}
}

// TestLabelValues_JSONStrategy_ChDB pins
// /loki/api/v1/label/http_status_code/values (the dotted
// http.status_code key, normalised to its underscored wire form) end to
// end: the JSON-typed table must return the same distinct value set as
// the Map-typed one, with the out-of-window row's 404 excluded on both.
func TestLabelValues_JSONStrategy_ChDB(t *testing.T) {
	startUnix, endUnix := discoveryWindowParams()
	url := func(base string) string {
		return fmt.Sprintf("%s/loki/api/v1/label/http_status_code/values?start=%d&end=%d", base, startUnix, endUnix)
	}

	mapSrv := newMapDiscoveryServer(t)
	jsonSrv := newJSONDiscoveryServer(t)

	mapResp, err := http.Get(url(mapSrv.URL))
	if err != nil {
		t.Fatalf("GET map: %v", err)
	}
	jsonResp, err := http.Get(url(jsonSrv.URL))
	if err != nil {
		t.Fatalf("GET json: %v", err)
	}

	mapGot := decodeStringSliceResponse(t, mapResp)
	jsonGot := decodeStringSliceResponse(t, jsonResp)

	want := []string{"200", "500"}
	if fmt.Sprint(mapGot) != fmt.Sprint(want) {
		t.Fatalf("Map label values = %v, want %v (seed/window sanity)", mapGot, want)
	}
	if fmt.Sprint(jsonGot) != fmt.Sprint(want) {
		t.Errorf("JSON label values = %v, want %v", jsonGot, want)
	}
}

// TestSeries_JSONStrategy_ChDB pins /loki/api/v1/series: the JSON-typed
// table's distinct label-set enumeration (attrMapFrag's
// JSONAttrMapReconstruction branch feeding canonicalLabelsFrag's GROUP BY)
// must report the same three distinct streams the Map-typed table does —
// http_status_code is a stream-identity (ResourceAttributes) label in the
// shared seed, so the two in-window api rows' differing status codes
// genuinely form two separate streams, plus the one web stream.
func TestSeries_JSONStrategy_ChDB(t *testing.T) {
	startUnix, endUnix := discoveryWindowParams()
	url := func(base string) string {
		return fmt.Sprintf("%s/loki/api/v1/series?start=%d&end=%d", base, startUnix, endUnix)
	}

	mapSrv := newMapDiscoveryServer(t)
	jsonSrv := newJSONDiscoveryServer(t)

	mapResp, err := http.Get(url(mapSrv.URL))
	if err != nil {
		t.Fatalf("GET map: %v", err)
	}
	jsonResp, err := http.Get(url(jsonSrv.URL))
	if err != nil {
		t.Fatalf("GET json: %v", err)
	}
	defer mapResp.Body.Close()
	defer jsonResp.Body.Close()

	if mapResp.StatusCode != http.StatusOK {
		t.Fatalf("map status=%d", mapResp.StatusCode)
	}
	if jsonResp.StatusCode != http.StatusOK {
		t.Fatalf("json status=%d", jsonResp.StatusCode)
	}

	var mapOut, jsonOut struct {
		Data []map[string]string `json:"data"`
	}
	if err := json.NewDecoder(mapResp.Body).Decode(&mapOut); err != nil {
		t.Fatalf("decode map: %v", err)
	}
	if err := json.NewDecoder(jsonResp.Body).Decode(&jsonOut); err != nil {
		t.Fatalf("decode json: %v", err)
	}

	if len(mapOut.Data) != 3 {
		t.Fatalf("Map /series returned %d streams, want 3 (seed/window sanity): %+v", len(mapOut.Data), mapOut.Data)
	}
	if len(jsonOut.Data) != 3 {
		t.Errorf("JSON /series returned %d streams, want 3: %+v", len(jsonOut.Data), jsonOut.Data)
	}

	keyOf := func(m map[string]string) string {
		return fmt.Sprint(m["job"], "|", m["service_name"], "|", m["http_status_code"])
	}
	mapKeys := map[string]bool{}
	for _, m := range mapOut.Data {
		mapKeys[keyOf(m)] = true
	}
	for _, m := range jsonOut.Data {
		if !mapKeys[keyOf(m)] {
			t.Errorf("JSON /series stream %+v has no Map-side counterpart", m)
		}
	}
}

// TestDetectedLabels_JSONStrategy_ChDB pins /loki/api/v1/detected_labels:
// per-key cardinality derived from the JSON-typed table's reconstructed
// label sets must match the Map-typed table's.
func TestDetectedLabels_JSONStrategy_ChDB(t *testing.T) {
	startUnix, endUnix := discoveryWindowParams()
	url := func(base string) string {
		return fmt.Sprintf("%s/loki/api/v1/detected_labels?start=%d&end=%d", base, startUnix, endUnix)
	}

	mapSrv := newMapDiscoveryServer(t)
	jsonSrv := newJSONDiscoveryServer(t)

	mapResp, err := http.Get(url(mapSrv.URL))
	if err != nil {
		t.Fatalf("GET map: %v", err)
	}
	jsonResp, err := http.Get(url(jsonSrv.URL))
	if err != nil {
		t.Fatalf("GET json: %v", err)
	}
	defer mapResp.Body.Close()
	defer jsonResp.Body.Close()

	var mapOut, jsonOut struct {
		DetectedLabels []loki.DetectedLabel `json:"detectedLabels"`
	}
	if err := json.NewDecoder(mapResp.Body).Decode(&mapOut); err != nil {
		t.Fatalf("decode map: %v", err)
	}
	if err := json.NewDecoder(jsonResp.Body).Decode(&jsonOut); err != nil {
		t.Fatalf("decode json: %v", err)
	}

	byLabel := func(in []loki.DetectedLabel) map[string]uint64 {
		out := map[string]uint64{}
		for _, l := range in {
			out[l.Label] = l.Cardinality
		}
		return out
	}
	mapByLabel := byLabel(mapOut.DetectedLabels)
	jsonByLabel := byLabel(jsonOut.DetectedLabels)

	if mapByLabel["job"] != 2 {
		t.Fatalf("Map job cardinality = %d, want 2 (seed/window sanity)", mapByLabel["job"])
	}
	for _, key := range []string{"job", "service_name"} {
		if mapByLabel[key] != jsonByLabel[key] {
			t.Errorf("detected_labels[%s]: Map cardinality=%d, JSON cardinality=%d, want equal", key, mapByLabel[key], jsonByLabel[key])
		}
	}
}

// detectedFieldsJSONSeedDDL / detectedFieldsMapSeedDDL seed a MINIMAL,
// dedicated otel_logs shape for /detected_fields specifically: unlike
// /labels, /series and /detected_labels (which read the ResourceAttributes
// STREAM-identity map), /detected_fields mines its field set from the
// BODY (logfmt/json-parsed) and the LogAttributes STRUCTURED-METADATA map
// (detected_fields.go's stream_labels/log_attributes projections feed
// detected_level plus whatever structured metadata a row carries) — a
// stream-identity attribute like the shared seed's http.status_code is
// never surfaced there, so this test needs its own seed rather than
// reusing fullMapDiscoveryMapDDL/JSONDDL.
const detectedFieldsMapSeedDDL = `CREATE TABLE %[2]s (
    Timestamp DateTime64(9),
    Body String,
    LogAttributes Map(String, String),
    ResourceAttributes Map(String, String)
) ENGINE = Memory;

INSERT INTO %[2]s (Timestamp, Body, LogAttributes, ResourceAttributes) VALUES
    (toDateTime64('%[1]s', 9), 'line one', map('request_id', 'r1'), map('job', 'api')),
    (toDateTime64('%[1]s', 9), 'line two', map('request_id', 'r2'), map('job', 'api'));
`

const detectedFieldsJSONSeedDDL = `CREATE TABLE %[2]s (
    Timestamp DateTime64(9),
    Body String,
    LogAttributes JSON,
    ResourceAttributes JSON
) ENGINE = Memory;

INSERT INTO %[2]s (Timestamp, Body, LogAttributes, ResourceAttributes) VALUES
    (toDateTime64('%[1]s', 9), 'line one', '{"request_id":"r1"}', '{"job":"api"}'),
    (toDateTime64('%[1]s', 9), 'line two', '{"request_id":"r2"}', '{"job":"api"}');
`

// newDetectedFieldsServer boots a dedicated chDB-backed Handler for
// detectedFieldsMapSeedDDL/JSONSeedDDL — see their doc comment for why
// this test does not reuse newMapDiscoveryServer/newJSONDiscoveryServer.
func newDetectedFieldsServer(t *testing.T, ddlTemplate, tableName string, jsonStrategy bool) *httptest.Server {
	t.Helper()
	ddl := fmt.Sprintf(ddlTemplate, fullMapDiscoverySeedBase.Format(fullMapDiscoveryTSFmt), tableName)
	c := chclienttest.NewChDB(t)
	c.Seed(t, ddl)
	s := schema.DefaultOTelLogs()
	s.LogsTable = tableName
	h := loki.New(c, s, nil)
	if jsonStrategy {
		h.AttrStrategies = chsql.AttrStrategies{
			s.ResourceAttributesColumn: chsql.AttrStrategyJSON,
			s.AttributesColumn:         chsql.AttrStrategyJSON,
		}
		h.Lang.AttrStrategies = h.AttrStrategies
	}
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestDetectedFields_JSONStrategy_ChDB pins /loki/api/v1/detected_fields'
// stream_labels / log_attributes projections (attrMapFrag) against a
// JSON-typed LogAttributes/ResourceAttributes pair: the structured field
// set derived from them (request_id, a LogAttributes structured-metadata
// key) must match the Map-typed baseline.
func TestDetectedFields_JSONStrategy_ChDB(t *testing.T) {
	startUnix, endUnix := discoveryWindowParams()
	url := func(base string) string {
		return fmt.Sprintf(`%s/loki/api/v1/detected_fields?query=%%7Bjob%%3D%%22api%%22%%7D&start=%d&end=%d`, base, startUnix, endUnix)
	}

	mapSrv := newDetectedFieldsServer(t, detectedFieldsMapSeedDDL, "otel_logs_detfields_map", false)
	jsonSrv := newDetectedFieldsServer(t, detectedFieldsJSONSeedDDL, "otel_logs_detfields_json", true)

	mapResp, err := http.Get(url(mapSrv.URL))
	if err != nil {
		t.Fatalf("GET map: %v", err)
	}
	jsonResp, err := http.Get(url(jsonSrv.URL))
	if err != nil {
		t.Fatalf("GET json: %v", err)
	}
	defer mapResp.Body.Close()
	defer jsonResp.Body.Close()

	if mapResp.StatusCode != http.StatusOK {
		t.Fatalf("map status=%d", mapResp.StatusCode)
	}
	if jsonResp.StatusCode != http.StatusOK {
		t.Fatalf("json status=%d", jsonResp.StatusCode)
	}

	var mapOut, jsonOut loki.DetectedFieldsResponse
	if err := json.NewDecoder(mapResp.Body).Decode(&mapOut); err != nil {
		t.Fatalf("decode map: %v", err)
	}
	if err := json.NewDecoder(jsonResp.Body).Decode(&jsonOut); err != nil {
		t.Fatalf("decode json: %v", err)
	}

	byLabel := func(in []loki.DetectedField) map[string]loki.DetectedField {
		out := map[string]loki.DetectedField{}
		for _, f := range in {
			out[f.Label] = f
		}
		return out
	}
	mapFields := byLabel(mapOut.Fields)
	jsonFields := byLabel(jsonOut.Fields)

	mapReq, ok := mapFields["request_id"]
	if !ok || mapReq.Cardinality != 2 {
		t.Fatalf("Map request_id field = %+v (ok=%v), want cardinality 2 (seed sanity)", mapReq, ok)
	}
	jsonReq, ok := jsonFields["request_id"]
	if !ok {
		t.Fatalf("JSON detected_fields missing request_id: %+v", jsonOut.Fields)
	}
	if jsonReq.Cardinality != mapReq.Cardinality {
		t.Errorf("request_id cardinality: Map=%d JSON=%d, want equal", mapReq.Cardinality, jsonReq.Cardinality)
	}
}
