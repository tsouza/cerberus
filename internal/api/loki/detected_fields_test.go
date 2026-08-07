package loki_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// getDetectedFields issues the request and decodes the body EXACTLY as
// the consumer (Grafana's Logs Drilldown / Loki datasource) does: the
// upstream wire shape is the BARE logproto.DetectedFieldsResponse —
// top-level `fields` + `limit`, no {status, data} envelope.
func getDetectedFields(t *testing.T, url string) loki.DetectedFieldsResponse {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var out loki.DetectedFieldsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func fieldsByLabel(fields []loki.DetectedField) map[string]loki.DetectedField {
	byLabel := map[string]loki.DetectedField{}
	for _, f := range fields {
		byLabel[f.Label] = f
	}
	return byLabel
}

// TestDetectedFields_JSON exercises the JSON detection branch. Rows
// with JSON bodies yield typed fields tagged with the "json" parser
// and their original jsonPath.
func TestDetectedFields_JSON(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{detectedRows: []chclient.DetectedFieldRow{
		{
			Line:       `{"status":200,"path":"/api","ok":true}`,
			JSONFields: map[string]string{"status": "200", "path": "/api", "ok": "true"},
		},
		{
			Line:       `{"status":404,"path":"/api","ok":false}`,
			JSONFields: map[string]string{"status": "404", "path": "/api", "ok": "false"},
		},
	}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	out := getDetectedFields(t, srv.URL+
		`/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D`)

	byLabel := fieldsByLabel(out.Fields)
	if got := byLabel["status"].Type; got != "int" {
		t.Errorf("status.type=%q want int", got)
	}
	if got := byLabel["ok"].Type; got != "boolean" {
		t.Errorf("ok.type=%q want boolean", got)
	}
	if got := byLabel["path"].Type; got != "string" {
		t.Errorf("path.type=%q want string", got)
	}
	if got := byLabel["status"].Cardinality; got != 2 {
		t.Errorf("status.cardinality=%d want 2", got)
	}
	for _, label := range []string{"status", "path", "ok"} {
		f := byLabel[label]
		if len(f.Parsers) != 1 || f.Parsers[0] != "json" {
			t.Errorf("%s.parsers=%v want [json]", label, f.Parsers)
		}
		if len(f.JSONPath) != 1 || f.JSONPath[0] != label {
			t.Errorf("%s.jsonPath=%v want [%s]", label, f.JSONPath, label)
		}
	}
	if out.Limit == 0 {
		t.Errorf("limit not echoed alongside non-empty fields: %+v", out)
	}

	// SQL sanity: the peek window selects all three field sources,
	// ordered by Timestamp DESC + LIMIT.
	lastSQL := q.LastSQL()
	for _, col := range []string{"`Body`", "`LogAttributes`", "`ResourceAttributes`"} {
		if !strings.Contains(lastSQL, col) {
			t.Errorf("missing %s projection: %q", col, lastSQL)
		}
	}
	if !strings.Contains(lastSQL, "ORDER BY `Timestamp` DESC") {
		t.Errorf("missing ORDER BY DESC: %q", lastSQL)
	}
	if !strings.Contains(lastSQL, "LIMIT 1000") {
		t.Errorf("missing default LIMIT 1000: %q", lastSQL)
	}
}

// TestDetectedFields_ClickHouseQueryLog pins #1888: against the compose
// stack's `{service_name="clickhouse"}` logs — whose Body is a raw SQL
// query string and whose LogAttributes carry the useful query_log
// columns — cerberus's detected_fields must (a) surface the real
// structured-metadata keys (duration / read_bytes / query_id), and (b)
// NOT advertise `_` / `_method` / `_id` fields that no LogQL query can
// ever produce.
//
// The body below is the SQL text from the issue's repro with the `=`
// UNSPACED (`(method='GET')`) — the shape that makes the difference. A
// Loki-shaped Go decoder reads `(method` as a key, sanitises it to
// `_method` and advertises it; the query path's `| logfmt` lowering is
// ClickHouse's extractKeyValuePairs, which never yields that name. The
// peek row therefore carries EMPTY LogfmtFields / JSONFields — what
// ClickHouse actually returns for this body — so the test fails the
// moment the handler re-derives fields in Go instead of reading the
// CH-evaluated extraction.
func TestDetectedFields_ClickHouseQueryLog(t *testing.T) {
	t.Parallel()

	sqlBody := `SELECT count() FROM otel_logs WHERE (method='GET') AND ` +
		`(_id=5) SETTINGS index_granularity=8192, max_threads=4`
	q := &stubQuerier{detectedRows: []chclient.DetectedFieldRow{
		{
			Line: sqlBody,
			Attributes: map[string]string{
				"duration":   "12ms",
				"read_bytes": "4096",
				"query_id":   "abc-123",
			},
		},
	}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	out := getDetectedFields(t, srv.URL+
		`/loki/api/v1/detected_fields?query=%7Bservice_name%3D%22clickhouse%22%7D`)

	byLabel := fieldsByLabel(out.Fields)
	// Real structured-metadata keys surface as fields with no parser
	// (they come from LogAttributes, not a line parse).
	for _, label := range []string{"duration", "read_bytes", "query_id"} {
		f, ok := byLabel[label]
		if !ok {
			t.Errorf("expected structured-metadata field %q to surface; fields=%v", label, out.Fields)
			continue
		}
		if len(f.Parsers) != 0 {
			t.Errorf("%s.parsers=%v want [] (structured metadata, not parsed)", label, f.Parsers)
		}
	}
	if got := byLabel["duration"].Type; got != "duration" {
		t.Errorf("duration.type=%q want duration", got)
	}
	// No phantom keys derived from the SQL Body. ClickHouse extracted
	// nothing from it, so the advertised set is the structured metadata
	// and nothing else — in particular no `_`-prefixed sanitiser output.
	for k := range byLabel {
		if strings.HasPrefix(k, "_") {
			t.Errorf("phantom field advertised from SQL body: %q (fields=%v)", k, out.Fields)
		}
	}
	if len(byLabel) != 3 {
		t.Errorf("fields=%v want exactly the 3 structured-metadata keys", out.Fields)
	}
}

// TestDetectedFields_SQLProjectsQueryPathExtraction is the structural
// half of #1888: the peek SQL must project the VERY expressions the
// LogQL lowering emits for `| logfmt` and `| json`, so the advertised
// inventory cannot drift from what a query can read back. Comparing
// rendered SQL — rather than re-listing the expected function calls —
// means any change to either side that is not mirrored on the other
// fails here.
func TestDetectedFields_SQLProjectsQueryPathExtraction(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{detectedRows: []chclient.DetectedFieldRow{{Line: "x"}}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	_ = getDetectedFields(t, srv.URL+
		`/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D`)

	s := schema.DefaultOTelLogs()
	for _, tc := range []struct {
		name string
		expr chplan.Expr
	}{
		{"logfmt", logql.LogfmtParsedLabels(s)},
		{"json", logql.JSONParsedLabels(s)},
	} {
		want := renderExpr(t, tc.expr)
		if !strings.Contains(q.LastSQL(), want) {
			t.Errorf("peek SQL does not project the %s parser-stage extraction\n want: %s\n  sql: %s",
				tc.name, want, q.LastSQL())
		}
	}
}

// renderExpr renders a chplan expression to the SQL text the emitter
// would produce for it, by building a one-projection SELECT and
// stripping the keyword.
func renderExpr(t *testing.T, e chplan.Expr) string {
	t.Helper()
	sqlStr, _ := chsql.NewQuery().
		Select(func(b *chsql.Builder) {
			if err := b.Expr(e); err != nil {
				t.Fatalf("render expr: %v", err)
			}
		}).
		Build()
	return strings.TrimPrefix(sqlStr, "SELECT ")
}

// TestDetectedFields_Logfmt covers the logfmt fallback branch:
// free-form key=value lines tagged with the "logfmt" parser, no
// jsonPath.
func TestDetectedFields_Logfmt(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{detectedRows: []chclient.DetectedFieldRow{
		{
			Line: `level=info method=GET status=200 duration=12ms`,
			LogfmtFields: map[string]string{
				"level": "info", "method": "GET", "status": "200", "duration": "12ms",
			},
		},
		{
			Line: `level=error method=POST status=500 duration=1s`,
			LogfmtFields: map[string]string{
				"level": "error", "method": "POST", "status": "500", "duration": "1s",
			},
		},
	}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	out := getDetectedFields(t, srv.URL+
		`/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D`)

	byLabel := fieldsByLabel(out.Fields)
	if got := byLabel["duration"].Type; got != "duration" {
		t.Errorf("duration.type=%q want duration", got)
	}
	if got := byLabel["status"].Type; got != "int" {
		t.Errorf("status.type=%q want int", got)
	}
	if got := byLabel["level"].Type; got != "string" {
		t.Errorf("level.type=%q want string", got)
	}
	f := byLabel["status"]
	if len(f.Parsers) != 1 || f.Parsers[0] != "logfmt" {
		t.Errorf("status.parsers=%v want [logfmt]", f.Parsers)
	}
	if len(f.JSONPath) != 0 {
		t.Errorf("status.jsonPath=%v want empty for logfmt", f.JSONPath)
	}
}

// TestDetectedFields_StructuredMetadata — LogAttributes entries (the
// OTel-CH structured-metadata analogue) are reported as fields with a
// NULL parser list, exactly as upstream Loki reports fields that come
// from structured metadata rather than line parsing.
func TestDetectedFields_StructuredMetadata(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{detectedRows: []chclient.DetectedFieldRow{
		{Line: `plain text line`, Attributes: map[string]string{"detected_level": "info"}},
		{Line: `another plain line`, Attributes: map[string]string{"detected_level": "error"}},
	}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	out := getDetectedFields(t, srv.URL+
		`/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D`)

	byLabel := fieldsByLabel(out.Fields)
	f, ok := byLabel["detected_level"]
	if !ok {
		t.Fatalf("detected_level missing from fields: %+v", out.Fields)
	}
	if f.Parsers != nil {
		t.Errorf("detected_level.parsers=%v want nil (structured metadata)", f.Parsers)
	}
	if f.Type != "string" {
		t.Errorf("detected_level.type=%q want string", f.Type)
	}
	if f.Cardinality != 2 {
		t.Errorf("detected_level.cardinality=%d want 2", f.Cardinality)
	}
}

// TestDetectedFields_MetadataAndParsedMerge — a key present both in
// LogAttributes AND parseable from the line accumulates the parser
// list (upstream merges the two sources into one field entry).
func TestDetectedFields_MetadataAndParsedMerge(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{detectedRows: []chclient.DetectedFieldRow{
		{
			Line:         `level=info msg=ok`,
			Attributes:   map[string]string{"level": "info"},
			LogfmtFields: map[string]string{"level": "info", "msg": "ok"},
		},
	}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	out := getDetectedFields(t, srv.URL+
		`/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D`)

	byLabel := fieldsByLabel(out.Fields)
	f, ok := byLabel["level"]
	if !ok {
		t.Fatalf("level missing from fields: %+v", out.Fields)
	}
	if len(f.Parsers) != 1 || f.Parsers[0] != "logfmt" {
		t.Errorf("level.parsers=%v want [logfmt] after metadata+parsed merge", f.Parsers)
	}
}

// TestDetectedFields_StreamLabelCollision — a parsed key that shadows
// a stream label (ResourceAttributes) is renamed `<key>_extracted`,
// mirroring upstream Loki's labels-builder collision policy.
func TestDetectedFields_StreamLabelCollision(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{detectedRows: []chclient.DetectedFieldRow{
		{
			Line:     `service_name=other msg=hello`,
			Resource: map[string]string{"service_name": "api"},
			// ClickHouse applied the collision rename in the peek SQL —
			// the same mapApply the `| logfmt` lowering emits.
			LogfmtFields: map[string]string{"service_name_extracted": "other", "msg": "hello"},
		},
	}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	out := getDetectedFields(t, srv.URL+
		`/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D`)

	byLabel := fieldsByLabel(out.Fields)
	if _, ok := byLabel["service_name_extracted"]; !ok {
		t.Errorf("expected service_name_extracted for stream-label collision, got: %+v", out.Fields)
	}
	if _, ok := byLabel["service_name"]; ok {
		t.Errorf("raw service_name must not be reported as a detected field (it is a stream label): %+v", out.Fields)
	}
}

// TestDetectedFields_TypeFollowsLastRow — upstream re-detects the type
// per record and lets the LAST processed record win; there is no
// merge-to-string collapse.
func TestDetectedFields_TypeFollowsLastRow(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{detectedRows: []chclient.DetectedFieldRow{
		{Line: `v=12`, LogfmtFields: map[string]string{"v": "12"}},
		{Line: `v=hello`, LogfmtFields: map[string]string{"v": "hello"}},
	}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	out := getDetectedFields(t, srv.URL+
		`/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D`)

	byLabel := fieldsByLabel(out.Fields)
	if got := byLabel["v"].Type; got != "string" {
		t.Errorf("v.type=%q want string (last row wins)", got)
	}
	if got := byLabel["v"].Cardinality; got != 2 {
		t.Errorf("v.cardinality=%d want 2", got)
	}
}

// TestDetectedFields_LimitCapsDistinctFields — `limit` caps the number
// of DISTINCT fields tracked (upstream stops admitting new field names
// once the cap is hit; existing ones keep accumulating).
func TestDetectedFields_LimitCapsDistinctFields(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{detectedRows: []chclient.DetectedFieldRow{
		{
			Line:       `{"a":1,"b":2,"c":3}`,
			JSONFields: map[string]string{"a": "1", "b": "2", "c": "3"},
		},
	}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	out := getDetectedFields(t, srv.URL+
		`/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D&limit=2`)

	if len(out.Fields) != 2 {
		t.Fatalf("fields=%d want 2 (limit cap): %+v", len(out.Fields), out.Fields)
	}
	if out.Limit != 2 {
		t.Errorf("limit echo=%d want 2", out.Limit)
	}
}

// TestDetectedFields_Empty — no rows returned → upstream omits both
// `fields` and `limit` (omitempty), so the body decodes to the zero
// response. The consumer renders this as "no fields", which is correct
// for genuinely empty data.
func TestDetectedFields_Empty(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{detectedRows: nil}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	out := getDetectedFields(t, srv.URL+
		`/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D`)
	if len(out.Fields) != 0 {
		t.Fatalf("fields=%+v want []", out.Fields)
	}
	if out.Limit != 0 {
		t.Fatalf("limit=%d want omitted on empty fields", out.Limit)
	}
}

// TestDetectedFields_BadInput — missing or broken parameters → 400.
func TestDetectedFields_BadInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
	}{
		{"missing query", `/loki/api/v1/detected_fields?start=1&end=2`},
		{"bad query", `/loki/api/v1/detected_fields?query=%7Bnot+a+selector`},
		{"bad line_limit", `/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D&line_limit=-1`},
		// limits are bounded to MaxInt32 at parse time (ParseUint
		// bitSize 31) so neither the wire uint32 nor a 32-bit int can
		// ever wrap (CodeQL go/incorrect-integer-conversion on PR
		// #774). Pin both the int32 and the uint32 boundary.
		{"limit above int32", `/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D&limit=2147483648`},
		{"limit above uint32", `/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D&limit=4294967296`},
		{"line_limit above int32", `/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D&line_limit=2147483648`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + tc.url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", resp.StatusCode)
			}
		})
	}
}
