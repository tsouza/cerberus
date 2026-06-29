package loki

import (
	"reflect"
	"testing"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
)

// TestLabelFormat_Rename — `| label_format new=old` copies the old
// label's value under the new name and drops the source. Mirrors
// Loki's Rename semantics.
func TestLabelFormat_Rename(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr(`{job="api"} | label_format svc=job`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if tx == nil {
		t.Fatalf("expected non-nil transform for label_format query")
	}

	gotLine, gotLabels := tx("hello", 0, map[string]string{"job": "api", "env": "prod"})
	if gotLine != "hello" {
		t.Errorf("line should pass through, got %q", gotLine)
	}
	want := map[string]string{"svc": "api", "env": "prod"}
	if !reflect.DeepEqual(gotLabels, want) {
		t.Errorf("labels: got %v, want %v", gotLabels, want)
	}
}

// TestLabelFormat_RenameMissingSource — Loki silently skips a rename
// when the source label doesn't exist; cerberus mirrors that.
func TestLabelFormat_RenameMissingSource(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr(`{job="api"} | label_format svc=missing`)
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	_, labels := tx("hello", 0, map[string]string{"job": "api"})
	if _, ok := labels["svc"]; ok {
		t.Errorf("svc should not be set when source missing; got %v", labels)
	}
	if labels["job"] != "api" {
		t.Errorf("job should be untouched; got %v", labels)
	}
}

// TestLabelFormat_Template — `| label_format lvl=` + "`" + `{{.severity}}` + "`" — set
// the destination label to the templated value.
func TestLabelFormat_Template(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr("{job=\"api\"} | label_format lvl=`{{.severity}}-suffix`")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	_, labels := tx("hello", 0, map[string]string{"job": "api", "severity": "error"})
	if labels["lvl"] != "error-suffix" {
		t.Errorf("expected lvl=error-suffix, got %q (labels=%v)", labels["lvl"], labels)
	}
}

// TestLabelFormat_TemplateMissingKey — Loki uses `missingkey=zero`
// which renders an unset label as `<no value>`. cerberus mirrors
// that — silent fallback, not error.
func TestLabelFormat_TemplateMissingKey(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr("{job=\"api\"} | label_format lvl=`{{.absent}}`")
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	_, labels := tx("hello", 0, map[string]string{"job": "api"})
	if labels["lvl"] != "<no value>" {
		t.Errorf("expected <no value> sentinel, got %q", labels["lvl"])
	}
}

// TestLabelFormat_RenameThenLineFormat — composition: rename a label,
// then line_format references the new name. The line_format template
// MUST see the renamed label (post-format dot map).
func TestLabelFormat_RenameThenLineFormat(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr(`{job="api"} | label_format svc=job | line_format "[{{.svc}}] {{__line__}}"`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	gotLine, _ := tx("hello", 0, map[string]string{"job": "api"})
	want := "[api] hello"
	if gotLine != want {
		t.Errorf("got %q, want %q", gotLine, want)
	}
}

// TestLabelFormat_DoesNotMutateInput — the transform must allocate a
// fresh map; the original sample's labels stay unchanged. Without
// this, two samples sharing the same Labels reference would corrupt
// each other's data.
func TestLabelFormat_DoesNotMutateInput(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr(`{job="api"} | label_format svc=job`)
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	input := map[string]string{"job": "api", "env": "prod"}
	tx("hello", 0, input)
	if input["job"] != "api" {
		t.Errorf("input.job mutated to %q", input["job"])
	}
	if _, ok := input["svc"]; ok {
		t.Errorf("input.svc was added: %v", input)
	}
	if len(input) != 2 {
		t.Errorf("input length changed: %v", input)
	}
}

// TestLabelFormat_BadTemplate — a parse-time template error surfaces
// up rather than failing silently per row. Same shape as line_format.
func TestLabelFormat_BadTemplate(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr("{job=\"api\"} | label_format lvl=`{{ .unclosed`")
	if err != nil {
		t.Fatalf("parse logql: %v", err)
	}
	if _, err := postProcessExtract(expr); err == nil {
		t.Errorf("expected template-parse error; got nil")
	}
}
