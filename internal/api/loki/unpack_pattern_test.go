package loki

import (
	"reflect"
	"testing"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
)

// The `| unpack` extraction cases that used to live here — the
// canonical Promtail payload, the `_extracted` collision suffix, the
// non-object and malformed error stamps, the empty line, the
// no-`_entry` no-op, key sanitisation and the non-string skip — now
// drive the SQL-side extraction instead, because that is where the
// stage is modelled. They are the `| unpack` rows of the shared
// awkward-line corpus in
// internal/logql/stage_extraction_chdb_test.go, which executes every
// modelled parser stage against chDB and asserts its extracted
// key/value set.

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

	// `| unpack` is modelled in SQL, so by the time the Go-side pipeline
	// runs the row already carries the unpacked line and the labels that
	// stage extracted. The pattern step composes on top of that.
	line := "GET /healthz"
	gotLine, gotLabels, _ := tx(line, 0, map[string]string{"job": "api", "pod": "web-1"})
	if gotLine != line {
		t.Errorf("pattern must not rewrite the line; got %q", gotLine)
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
