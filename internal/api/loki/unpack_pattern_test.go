package loki

import (
	"reflect"
	"testing"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
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
	gotLine, gotLabels, _ := tx(line, 0, map[string]string{"job": "api"})

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
	_, gotLabels, _ := tx(line, 0, map[string]string{"job": "api"})

	if gotLabels["job"] != "api" {
		t.Errorf("stream label `job` should be preserved; got %q", gotLabels["job"])
	}
	if gotLabels["job_extracted"] != "shadowed" {
		t.Errorf("clashing key should land under `job_extracted`; got %q",
			gotLabels["job_extracted"])
	}
}

// TestUnpack_NonJSONStampsParserError — a payload that is not a JSON
// object keeps the line but is an ERROR: Loki's UnpackParser stamps
// `__error__="JSONParserErr"` and an `__error_details__` sentinel. The
// details text is byte-compared because callers filter on it, and
// because swallowing the pair inverts `| __error__=""` — the whole of
// issue #1447.
func TestUnpack_NonJSONStampsParserError(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr(`{job="api"} | unpack`)
	tx, _ := postProcessExtract(expr)

	for _, line := range []string{"not json", `[1,2]`, `"a string"`, `42`} {
		gotLine, gotLabels, _ := tx(line, 0, map[string]string{"job": "api"})
		if gotLine != line {
			t.Errorf("line should pass through; got %q, want %q", gotLine, line)
		}
		want := map[string]string{
			"job":               "api",
			"__error__":         "JSONParserErr",
			"__error_details__": "expecting json object(6), but it is not",
		}
		if !reflect.DeepEqual(gotLabels, want) {
			t.Errorf("labels for %q\n got %#v\nwant %#v", line, gotLabels, want)
		}
	}
}

// TestUnpack_EmptyLineIsNotAnError — upstream returns early on a
// zero-length line BEFORE the `{` check, so no error is stamped. Folding
// this into the non-object branch would mark every empty line broken.
func TestUnpack_EmptyLineIsNotAnError(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr(`{job="api"} | unpack`)
	tx, _ := postProcessExtract(expr)

	gotLine, gotLabels, _ := tx("", 0, map[string]string{"job": "api"})
	if gotLine != "" {
		t.Errorf("line should stay empty; got %q", gotLine)
	}
	if !reflect.DeepEqual(gotLabels, map[string]string{"job": "api"}) {
		t.Errorf("labels should be unchanged; got %#v", gotLabels)
	}
}

// TestUnpack_MalformedObjectStampsParserError — a line that opens with
// `{` but does not parse carries the underlying decoder's message
// through to `__error_details__`, exactly as upstream passes
// jsonparser's error to addErrLabel.
func TestUnpack_MalformedObjectStampsParserError(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr(`{job="api"} | unpack`)
	tx, _ := postProcessExtract(expr)

	const line = `{"_entry":"l","pod":`
	gotLine, gotLabels, _ := tx(line, 0, map[string]string{"job": "api"})
	if gotLine != line {
		t.Errorf("malformed line should pass through unmodified; got %q", gotLine)
	}
	if gotLabels["__error__"] != "JSONParserErr" {
		t.Errorf("__error__ = %q, want JSONParserErr", gotLabels["__error__"])
	}
	if gotLabels["__error_details__"] == "" {
		t.Error("__error_details__ should carry the decoder's message")
	}
	if _, ok := gotLabels["pod"]; ok {
		t.Error("labels collected before the failure must be discarded")
	}
}

// TestUnpack_NoPackedEntryExtractsNothing — upstream flushes its label
// buffer only when a top-level string `_entry` key was found. A
// well-formed object without one is a complete no-op. Extracting the
// labels anyway invents series identity out of an ordinary JSON log
// line, which is the silently-wrong shape from the other direction.
func TestUnpack_NoPackedEntryExtractsNothing(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr(`{job="api"} | unpack`)
	tx, _ := postProcessExtract(expr)

	const line = `{"pod":"web-1","container":"app"}`
	gotLine, gotLabels, _ := tx(line, 0, map[string]string{"job": "api"})
	if gotLine != line {
		t.Errorf("line should pass through; got %q", gotLine)
	}
	if !reflect.DeepEqual(gotLabels, map[string]string{"job": "api"}) {
		t.Errorf("no labels should be extracted without `_entry`; got %#v", gotLabels)
	}
}

// TestUnpack_SanitizesLabelKeys — upstream runs every extracted key
// through sanitizeLabelKey, so a key that is not a valid label name
// arrives normalised rather than verbatim.
func TestUnpack_SanitizesLabelKeys(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr(`{job="api"} | unpack`)
	tx, _ := postProcessExtract(expr)

	const line = `{"_entry":"l","pod-name":"web-1","9lives":"cat"}`
	_, gotLabels, _ := tx(line, 0, map[string]string{"job": "api"})
	if gotLabels["pod_name"] != "web-1" {
		t.Errorf("`pod-name` should sanitise to `pod_name`; got %#v", gotLabels)
	}
	if gotLabels["_9lives"] != "cat" {
		t.Errorf("leading-digit key should gain the `_` prefix; got %#v", gotLabels)
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
	_, gotLabels, _ := tx(line, 0, map[string]string{"job": "api"})

	if _, ok := gotLabels["count"]; ok {
		t.Errorf("non-string `count` should be dropped; got %q", gotLabels["count"])
	}
	if _, ok := gotLabels["nested"]; ok {
		t.Errorf("object-valued `nested` should be dropped")
	}
	if gotLabels["pod"] != "web-1" {
		t.Errorf("string-valued `pod` should be extracted; got %q", gotLabels["pod"])
	}
}

// TestPattern_NamedCaptures — the canonical case: each named segment
// in the pattern becomes a label. `<_>` segments are dropped.
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

	gotLine, gotLabels, _ := tx(`10.0.0.1 - GET /index.html`, 0, map[string]string{"job": "api"})
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

	gotLine, gotLabels, _ := tx("", 0, map[string]string{"job": "api"})
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
	_, gotLabels, _ := tx("one", 0, map[string]string{"job": "api"})
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

	_, gotLabels, _ := tx("other rest", 0, map[string]string{"job": "api"})
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
	gotLine, gotLabels, _ := tx(line, 0, map[string]string{"job": "api"})
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
