package loki

import (
	"maps"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/logql"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
	"github.com/tsouza/cerberus/internal/schema"
)

// metadataFoldStart is the base instant the fixtures below stamp their
// rows at; each row is offset by whole seconds from it so the entry
// order is stable regardless of map iteration.
var metadataFoldStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// sampleWithMetadata builds one log-stream cursor row. `labels` is the
// interned per-series map (SHARED between rows of the same series, as
// the real cursor shares it) and `metadata` the per-line structured
// metadata the fifth projection column carries.
func sampleWithMetadata(offset time.Duration, line string, labels, metadata map[string]string) chclient.Sample {
	return chclient.Sample{
		MetricName: line,
		Labels:     labels,
		Timestamp:  metadataFoldStart.Add(offset),
		Metadata:   metadata,
	}
}

// TestBuildRangeData_StructuredMetadataWidensStreamIdentity pins issue
// #1684: reference Loki carries structured metadata inside the entry's
// label set, so two lines of one stream that differ only in a metadata
// value are TWO streams. Cerberus previously widened stream identity
// for exactly one metadata key (`detected_level`, spliced in by the SQL
// wrap) and collapsed every distinct value of any other key into a
// single stream.
//
// Pre-fix this test fails with `len(streams) = 1`: all three rows share
// the same interned `Labels` map, so `format.CanonicalKey` returns one
// group no matter what `request_id` says.
func TestBuildRangeData_StructuredMetadataWidensStreamIdentity(t *testing.T) {
	t.Parallel()

	const query = `{job="api"}`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	series := map[string]string{"job": "api"}
	samples := []chclient.Sample{
		sampleWithMetadata(0, "a", series, map[string]string{"request_id": "req-1"}),
		sampleWithMetadata(time.Second, "b", series, map[string]string{"request_id": "req-2"}),
		sampleWithMetadata(2*time.Second, "c", series, map[string]string{"request_id": "req-3"}),
	}

	data, err := buildRangeData(expr, samples, metadataFoldStart, metadataFoldStart.Add(time.Minute),
		time.Minute, schema.DefaultOTelLogs(), 100, directionBackward, false)
	if err != nil {
		t.Fatalf("buildRangeData: %v", err)
	}
	streams, ok := data.Result.([]Stream)
	if !ok {
		t.Fatalf("Result = %T, want []Stream", data.Result)
	}
	if len(streams) != len(samples) {
		t.Fatalf("len(streams) = %d, want %d — one stream per distinct structured-metadata value",
			len(streams), len(samples))
	}
	seen := map[string]bool{}
	for _, s := range streams {
		if s.Stream["job"] != "api" {
			t.Errorf("stream %v lost the indexed label job=api", s.Stream)
		}
		id := s.Stream["request_id"]
		if id == "" {
			t.Errorf("stream %v carries no request_id — the metadata key did not reach stream identity", s.Stream)
		}
		if seen[id] {
			t.Errorf("request_id %q appears in two streams", id)
		}
		seen[id] = true
		if len(s.Values) != 1 {
			t.Errorf("stream %v has %d values, want 1", s.Stream, len(s.Values))
		}
	}
}

// TestBuildRangeData_CategorizeLabelsKeepsNarrowIdentity is the control
// side: with the `categorize-labels` encoding flag the client asked for
// the metadata split OUT of the labels (upstream's
// CategorizeLabelsIterator), so identity must stay keyed on the indexed
// labels alone and all three rows collapse into ONE stream.
func TestBuildRangeData_CategorizeLabelsKeepsNarrowIdentity(t *testing.T) {
	t.Parallel()

	const query = `{job="api"}`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	series := map[string]string{"job": "api"}
	samples := []chclient.Sample{
		sampleWithMetadata(0, "a", series, map[string]string{"request_id": "req-1"}),
		sampleWithMetadata(time.Second, "b", series, map[string]string{"request_id": "req-2"}),
		sampleWithMetadata(2*time.Second, "c", series, map[string]string{"request_id": "req-3"}),
	}

	data, err := buildRangeData(expr, samples, metadataFoldStart, metadataFoldStart.Add(time.Minute),
		time.Minute, schema.DefaultOTelLogs(), 100, directionBackward, true)
	if err != nil {
		t.Fatalf("buildRangeData: %v", err)
	}
	streams, ok := data.Result.([]Stream)
	if !ok {
		t.Fatalf("Result = %T, want []Stream", data.Result)
	}
	if len(streams) != 1 {
		t.Fatalf("len(streams) = %d, want 1 — categorize-labels keys identity on indexed labels alone", len(streams))
	}
	if _, ok := streams[0].Stream["request_id"]; ok {
		t.Errorf("stream %v folded request_id into the labels despite categorize-labels", streams[0].Stream)
	}
	if len(streams[0].Values) != len(samples) {
		t.Fatalf("stream has %d values, want %d", len(streams[0].Values), len(samples))
	}
	for _, v := range streams[0].Values {
		if v.Metadata["request_id"] == "" {
			t.Errorf("value %v lost its structured-metadata third tuple element", v)
		}
	}
}

// TestBuildRangeData_MetadataFoldRunsBeforePipelineStages pins the
// ORDERING half of the fix: the fold happens before
// [applyLineTransform], so a `| drop` stage sees the folded key and
// removes it — exactly as upstream's LabelsBuilder does, where a
// metadata key is projectable like any other. Fold the other way round
// (after the transform) and `| drop request_id` would be a no-op and
// the three rows would stay three streams.
func TestBuildRangeData_MetadataFoldRunsBeforePipelineStages(t *testing.T) {
	t.Parallel()

	const query = `{job="api"} | drop request_id`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	series := map[string]string{"job": "api"}
	samples := []chclient.Sample{
		sampleWithMetadata(0, "a", series, map[string]string{"request_id": "req-1"}),
		sampleWithMetadata(time.Second, "b", series, map[string]string{"request_id": "req-2"}),
	}

	data, err := buildRangeData(expr, samples, metadataFoldStart, metadataFoldStart.Add(time.Minute),
		time.Minute, schema.DefaultOTelLogs(), 100, directionBackward, false)
	if err != nil {
		t.Fatalf("buildRangeData: %v", err)
	}
	streams, ok := data.Result.([]Stream)
	if !ok {
		t.Fatalf("Result = %T, want []Stream", data.Result)
	}
	if len(streams) != 1 {
		t.Fatalf("len(streams) = %d, want 1 — `| drop request_id` removes the folded key again", len(streams))
	}
	if _, ok := streams[0].Stream["request_id"]; ok {
		t.Errorf("stream %v kept request_id through `| drop request_id`", streams[0].Stream)
	}
}

// TestFoldStructuredMetadata_LeavesInternedLabelsUntouched pins the
// read-only contract on [chclient.LogRow.Labels]: the cursor interns
// ONE map per series and every row of that series aliases it, so a fold
// that wrote into it would corrupt every other row (and every other
// query sharing the cursor). The fold must allocate.
func TestFoldStructuredMetadata_LeavesInternedLabelsUntouched(t *testing.T) {
	t.Parallel()

	interned := map[string]string{"job": "api"}
	before := maps.Clone(interned)
	rows := []chclient.LogRow{
		{Timestamp: metadataFoldStart, Line: "a", Labels: interned, Metadata: map[string]string{"request_id": "req-1"}},
		{Timestamp: metadataFoldStart, Line: "b", Labels: interned, Metadata: map[string]string{"request_id": "req-2"}},
	}

	got := foldStructuredMetadata(rows, false)

	if !maps.Equal(interned, before) {
		t.Errorf("interned label map mutated: got %v, want %v", interned, before)
	}
	for i, r := range got {
		if r.Labels["request_id"] != r.Metadata["request_id"] {
			t.Errorf("row %d: labels[request_id] = %q, want %q", i, r.Labels["request_id"], r.Metadata["request_id"])
		}
	}
}

// TestFoldStructuredMetadata_IndexedLabelWinsCollision pins the merge
// precedence: an indexed label always beats a metadata entry of the
// same name. The SQL layer has already resolved `detected_level` to its
// normalised lowercase form in the identity map, so re-folding the raw
// `LogAttributes` entry over it would undo that normalisation and
// re-split a stream reference Loki keeps whole.
func TestFoldStructuredMetadata_IndexedLabelWinsCollision(t *testing.T) {
	t.Parallel()

	rows := []chclient.LogRow{{
		Timestamp: metadataFoldStart,
		Line:      "a",
		Labels:    map[string]string{"job": "api", "detected_level": "warn"},
		Metadata:  map[string]string{"detected_level": "WARNING"},
	}}

	got := foldStructuredMetadata(rows, false)

	if got[0].Labels["detected_level"] != "warn" {
		t.Errorf("labels[detected_level] = %q, want %q (the indexed label wins)",
			got[0].Labels["detected_level"], "warn")
	}
}

// TestFoldStructuredMetadata_NormalisesDottedMetadataKeys pins that a
// folded key goes through the same Loki/Prom label grammar the response
// labels do ([normalizeMetadata]), so an OTel-dotted structured-metadata
// key becomes a well-formed label rather than an unqueryable
// `http.status_code`-shaped one, and an empty-valued key never widens
// identity at all.
func TestFoldStructuredMetadata_NormalisesDottedMetadataKeys(t *testing.T) {
	t.Parallel()

	rows := []chclient.LogRow{{
		Timestamp: metadataFoldStart,
		Line:      "a",
		Labels:    map[string]string{"job": "api"},
		Metadata:  map[string]string{"http.status_code": "500", "blank": ""},
	}}

	got := foldStructuredMetadata(rows, false)

	if got[0].Labels["http_status_code"] != "500" {
		t.Errorf("labels[http_status_code] = %q, want %q", got[0].Labels["http_status_code"], "500")
	}
	if _, ok := got[0].Labels["blank"]; ok {
		t.Errorf("empty-valued metadata key reached the label set: %v", got[0].Labels)
	}
}

// TestBuildRangeData_MetricQueryIgnoresMetadataFold pins that the fold
// is scoped to the `streams` result type. A metric query's rows never
// reach [foldStructuredMetadata] — its series identity is decided in
// SQL by the RangeWindow grouping key — so the matrix pivot must be
// untouched by the presence of per-line metadata.
func TestBuildRangeData_MetricQueryIgnoresMetadataFold(t *testing.T) {
	t.Parallel()

	const query = `count_over_time({job="api"}[5m])`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	if !logql.IsMetricQuery(expr) {
		t.Fatalf("fixture invalid: %q is not a metric query", query)
	}
	series := map[string]string{"job": "api"}
	samples := []chclient.Sample{
		{Labels: series, Timestamp: metadataFoldStart, Value: 1, Metadata: map[string]string{"request_id": "req-1"}},
		{Labels: series, Timestamp: metadataFoldStart.Add(time.Minute), Value: 2, Metadata: map[string]string{"request_id": "req-2"}},
	}

	data, err := buildRangeData(expr, samples, metadataFoldStart, metadataFoldStart.Add(time.Minute),
		time.Minute, schema.DefaultOTelLogs(), 100, directionBackward, false)
	if err != nil {
		t.Fatalf("buildRangeData: %v", err)
	}
	if data.ResultType != "matrix" {
		t.Fatalf("ResultType = %q, want %q", data.ResultType, "matrix")
	}
	rows, ok := data.Result.([]MatrixSample)
	if !ok {
		t.Fatalf("Result = %T, want []MatrixSample", data.Result)
	}
	if len(rows) != 1 {
		t.Fatalf("len(matrix) = %d, want 1 — metadata must not split a metric series", len(rows))
	}
	if _, ok := rows[0].Metric["request_id"]; ok {
		t.Errorf("metric series %v gained a structured-metadata label", rows[0].Metric)
	}
}
