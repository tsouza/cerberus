package loki

import (
	"reflect"
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"
)

// TestUnpack_ExtractsLabelsAndEntry — the canonical Promtail `pack`
// payload: a JSON object whose `_entry` key is the original line and
// whose remaining string keys are added as labels.
func TestUnpack_ExtractsLabelsAndEntry(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr(`{job="api"} | unpack`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if tx == nil {
		t.Fatalf("expected non-nil transform for unpack")
	}

	line := `{"_entry":"original log line","pod":"web-1","container":"app"}`
	gotLine, gotLabels := tx(line, map[string]string{"job": "api"})

	if gotLine != "original log line" {
		t.Errorf("line: got %q, want %q", gotLine, "original log line")
	}
	want := map[string]string{"job": "api", "pod": "web-1", "container": "app"}
	if !reflect.DeepEqual(gotLabels, want) {
		t.Errorf("labels mismatch\n got %#v\nwant %#v", gotLabels, want)
	}
}

// TestUnpack_DuplicateSuffix — a parser-extracted label that collides
// with a stream label gets the `_extracted` suffix (matches Loki's
// disambiguation contract).
func TestUnpack_DuplicateSuffix(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr(`{job="api"} | unpack`)
	tx, _ := postProcessExtract(expr)

	line := `{"_entry":"l","job":"shadowed"}`
	_, gotLabels := tx(line, map[string]string{"job": "api"})

	if gotLabels["job"] != "api" {
		t.Errorf("stream label `job` should be preserved; got %q", gotLabels["job"])
	}
	if gotLabels["job_extracted"] != "shadowed" {
		t.Errorf("clashing key should land under `job_extracted`; got %q",
			gotLabels["job_extracted"])
	}
}

// TestUnpack_NonJSONLeavesLineAlone — a non-JSON line silently passes
// through. Loki sets `__error__=JSONParserErr` in that case; cerberus
// uses silent-fallback semantics (matching its line_format /
// label_format style).
func TestUnpack_NonJSONLeavesLineAlone(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr(`{job="api"} | unpack`)
	tx, _ := postProcessExtract(expr)

	gotLine, gotLabels := tx("not json", map[string]string{"job": "api"})
	if gotLine != "not json" {
		t.Errorf("line should pass through; got %q", gotLine)
	}
	if !reflect.DeepEqual(gotLabels, map[string]string{"job": "api"}) {
		t.Errorf("labels should be unchanged; got %#v", gotLabels)
	}
}

// TestUnpack_SkipsNonStringValues — Promtail's pack only ever emits
// string-valued keys, but real-world JSON has numbers and objects;
// unpack skips those (Loki's contract).
func TestUnpack_SkipsNonStringValues(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr(`{job="api"} | unpack`)
	tx, _ := postProcessExtract(expr)

	line := `{"_entry":"l","count":42,"nested":{"x":1},"pod":"web-1"}`
	_, gotLabels := tx(line, map[string]string{"job": "api"})

	if _, ok := gotLabels["count"]; ok {
		t.Errorf("non-string `count` should be skipped; got %q", gotLabels["count"])
	}
	if _, ok := gotLabels["nested"]; ok {
		t.Errorf("object-valued `nested` should be skipped")
	}
	if gotLabels["pod"] != "web-1" {
		t.Errorf("string-valued `pod` should be extracted; got %q", gotLabels["pod"])
	}
}

// TestPattern_NamedCaptures — the canonical case: each named segment
// in the pattern becomes a label. `<_>` segments are skipped.
func TestPattern_NamedCaptures(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr(`{job="api"} | pattern "<ip> <_> <method> <path>"`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if tx == nil {
		t.Fatalf("expected non-nil transform")
	}

	gotLine, gotLabels := tx(`10.0.0.1 - GET /index.html`, map[string]string{"job": "api"})
	if gotLine != `10.0.0.1 - GET /index.html` {
		t.Errorf("pattern shouldn't rewrite line; got %q", gotLine)
	}
	want := map[string]string{
		"job":    "api",
		"ip":     "10.0.0.1",
		"method": "GET",
		"path":   "/index.html",
	}
	if !reflect.DeepEqual(gotLabels, want) {
		t.Errorf("labels mismatch\n got %#v\nwant %#v", gotLabels, want)
	}
}

// TestPattern_EmptyLineNoExtraction — Loki's Matcher returns nil for
// empty input. Cerberus mirrors that: no captures means labels pass
// through untouched.
func TestPattern_EmptyLineNoExtraction(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr(`{job="api"} | pattern "<ip> <_> <method> <path>"`)
	tx, _ := postProcessExtract(expr)

	gotLine, gotLabels := tx("", map[string]string{"job": "api"})
	if gotLine != "" {
		t.Errorf("line should pass through; got %q", gotLine)
	}
	if !reflect.DeepEqual(gotLabels, map[string]string{"job": "api"}) {
		t.Errorf("labels should be unchanged; got %#v", gotLabels)
	}
}

// TestPattern_PartialCapture — Loki's matcher emits whatever it could
// capture even on a malformed line (matches its `if i == -1` fallback
// path that returns up to the end as the last capture). Cerberus
// mirrors that. The point of the test is to pin the contract, not to
// promise full-line validation.
func TestPattern_PartialCapture(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr(`{job="api"} | pattern "<a> <b> <c>"`)
	tx, _ := postProcessExtract(expr)

	// Only one space-separated token survives: the matcher captures
	// `one` for `<a>` and stops (no more space-literals to anchor on),
	// returning a single capture per Matches' early-return.
	_, gotLabels := tx("one", map[string]string{"job": "api"})
	if got := gotLabels["a"]; got != "one" {
		t.Errorf("first capture should be `one`; got %q", got)
	}
	if _, ok := gotLabels["b"]; ok {
		t.Errorf("unmatched `b` should not be present; got %q", gotLabels["b"])
	}
}

// TestPattern_DuplicateSuffix — a capture that collides with a stream
// label gets `_extracted` suffixed, same as unpack and Loki's contract.
func TestPattern_DuplicateSuffix(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr(`{job="api"} | pattern "<job> <_>"`)
	tx, _ := postProcessExtract(expr)

	_, gotLabels := tx("other rest", map[string]string{"job": "api"})
	if gotLabels["job"] != "api" {
		t.Errorf("stream label `job` should be preserved; got %q", gotLabels["job"])
	}
	if gotLabels["job_extracted"] != "other" {
		t.Errorf("capture clash should land in `job_extracted`; got %q",
			gotLabels["job_extracted"])
	}
}

// TestPattern_BadPatternStringIsParseError — the upstream Loki parser
// rejects malformed patterns at ParseExpr-time. Cerberus relies on
// that and never observes a malformed pattern itself; the test pins
// the contract.
func TestPattern_BadPatternStringIsParseError(t *testing.T) {
	t.Parallel()

	// A pattern with no captures is invalid per Loki's grammar.
	if _, err := syntax.ParseExpr(`{job="api"} | pattern "just literal"`); err == nil {
		t.Fatalf("expected ParseExpr to reject capture-less pattern; got nil")
	}
}

// TestPatternUnpackCompose — `| unpack | pattern` chains: unpack
// rewrites the line from `_entry`, pattern then runs against the new
// line. Mirrors the line_format/decolorize compose test.
func TestPatternUnpackCompose(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr(`{job="api"} | unpack | pattern "<method> <path>"`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	line := `{"_entry":"GET /healthz","pod":"web-1"}`
	gotLine, gotLabels := tx(line, map[string]string{"job": "api"})
	if gotLine != "GET /healthz" {
		t.Errorf("line should be the unpacked _entry; got %q", gotLine)
	}
	want := map[string]string{
		"job":    "api",
		"pod":    "web-1",
		"method": "GET",
		"path":   "/healthz",
	}
	if !reflect.DeepEqual(gotLabels, want) {
		t.Errorf("compose mismatch\n got %#v\nwant %#v", gotLabels, want)
	}
}
