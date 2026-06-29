package loki

import (
	"testing"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
)

// TestLineFormat_LabelInterpolation — the canonical case: a `| line_format`
// template references a stream label and the transform substitutes it.
func TestLineFormat_LabelInterpolation(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr(`{job="api"} | line_format "[{{.job}}] {{__line__}}"`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if tx == nil {
		t.Fatalf("expected non-nil transform for line_format query")
	}

	got, _ := tx("hello world", 0, map[string]string{"job": "api"})
	want := "[api] hello world"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestDecolorize_StripsAnsi — `| decolorize` removes ANSI escape
// sequences. Common case: terminal red `[31m` + reset `[0m` wrapping
// "ERROR".
func TestDecolorize_StripsAnsi(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr(`{job="api"} | decolorize`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if tx == nil {
		t.Fatalf("expected non-nil transform for decolorize query")
	}

	got, _ := tx("\x1b[31mERROR\x1b[0m: oops", 0, nil)
	want := "ERROR: oops"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestLineFormat_Compose — multiple post-fetch stages compose
// left-to-right. decolorize then line_format wraps the cleaned text.
func TestLineFormat_Compose(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr(`{job="api"} | decolorize | line_format "[{{.job}}] {{__line__}}"`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	got, _ := tx("\x1b[31mERROR\x1b[0m: oops", 0, map[string]string{"job": "api"})
	want := "[api] ERROR: oops"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestLineFormat_NoTransform — a query with no post-fetch stages
// returns nil; toStreamsWithTransform takes the identity path.
func TestLineFormat_NoTransform(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr(`{job="api"} |= "error"`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if tx != nil {
		t.Errorf("expected nil transform for non-line_format query; got non-nil")
	}
}

// TestLineFormat_BadTemplate — malformed Go-template syntax surfaces
// as a parse error, NOT a silent no-op.
func TestLineFormat_BadTemplate(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr(`{job="api"} | line_format "{{ .unclosed"`)
	if err != nil {
		t.Fatalf("parse logql: %v", err)
	}
	if _, err := postProcessExtract(expr); err == nil {
		t.Errorf("expected template-parse error; got nil")
	}
}
