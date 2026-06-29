package loki

import (
	"reflect"
	"testing"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
)

// TestDropLabels_SingleName — the canonical case: a single bare-name
// drop removes only that key from the output label map. The other
// stream labels survive unchanged.
func TestDropLabels_SingleName(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr(`{job="api"} | drop env`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if tx == nil {
		t.Fatalf("expected non-nil transform for drop query")
	}

	gotLine, gotLabels := tx("hello", 0, map[string]string{
		"job": "api",
		"env": "prod",
		"pod": "web-1",
	})
	if gotLine != "hello" {
		t.Errorf("line should pass through; got %q", gotLine)
	}
	want := map[string]string{"job": "api", "pod": "web-1"}
	if !reflect.DeepEqual(gotLabels, want) {
		t.Errorf("labels mismatch\n got %#v\nwant %#v", gotLabels, want)
	}
}

// TestDropLabels_MultipleNames — `| drop a, b` removes every named key
// at once. Unmentioned labels survive.
func TestDropLabels_MultipleNames(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr(`{job="api"} | drop env, pod`)
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	_, got := tx("l", 0, map[string]string{
		"job": "api",
		"env": "prod",
		"pod": "web-1",
		"app": "x",
	})
	want := map[string]string{"job": "api", "app": "x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("labels mismatch\n got %#v\nwant %#v", got, want)
	}
}

// TestDropLabels_Matcher — `| drop foo="v"` only drops the label when
// its value matches the matcher. Mirrors Loki's `log.dropLabelMatches`
// contract. A non-matching value leaves the label intact.
func TestDropLabels_Matcher(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr(`{job="api"} | drop env="prod"`)
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	// value matches → dropped
	_, gotDrop := tx("l", 0, map[string]string{
		"job": "api",
		"env": "prod",
	})
	if _, ok := gotDrop["env"]; ok {
		t.Errorf("env=prod should be dropped; got %#v", gotDrop)
	}

	// value differs → preserved
	_, gotKeep := tx("l", 0, map[string]string{
		"job": "api",
		"env": "stg",
	})
	if gotKeep["env"] != "stg" {
		t.Errorf("env=stg should be preserved; got %#v", gotKeep)
	}
}

// TestDropLabels_AbsentNameIsNoOp — dropping a label that isn't in the
// input is a silent no-op (Loki's contract). The output equals the
// input.
func TestDropLabels_AbsentNameIsNoOp(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr(`{job="api"} | drop nope`)
	tx, _ := postProcessExtract(expr)

	_, got := tx("l", 0, map[string]string{"job": "api"})
	want := map[string]string{"job": "api"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("absent-name drop should be no-op\n got %#v\nwant %#v", got, want)
	}
}

// TestKeepLabels_SingleName — `| keep job` projects the output down to
// the single named label. Every other key drops.
func TestKeepLabels_SingleName(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr(`{job="api"} | keep job`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if tx == nil {
		t.Fatalf("expected non-nil transform for keep query")
	}

	gotLine, got := tx("hello", 0, map[string]string{
		"job": "api",
		"env": "prod",
		"pod": "web-1",
	})
	if gotLine != "hello" {
		t.Errorf("line should pass through; got %q", gotLine)
	}
	want := map[string]string{"job": "api"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("labels mismatch\n got %#v\nwant %#v", got, want)
	}
}

// TestKeepLabels_MultipleNames — `| keep a, b` keeps every listed key
// when present in the input; unlisted keys are dropped.
func TestKeepLabels_MultipleNames(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr(`{job="api"} | keep job, env`)
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	_, got := tx("l", 0, map[string]string{
		"job": "api",
		"env": "prod",
		"pod": "web-1",
		"app": "x",
	})
	want := map[string]string{"job": "api", "env": "prod"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("labels mismatch\n got %#v\nwant %#v", got, want)
	}
}

// TestKeepLabels_Matcher — `| keep env="prod"` only keeps the label
// when its value matches; non-matching values are dropped (mirrors
// Loki's KeepLabels.Process).
func TestKeepLabels_Matcher(t *testing.T) {
	t.Parallel()

	expr, _ := syntax.ParseExpr(`{job="api"} | keep env="prod"`)
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	// value matches → preserved (but only env survives)
	_, gotMatch := tx("l", 0, map[string]string{
		"job": "api",
		"env": "prod",
	})
	if gotMatch["env"] != "prod" {
		t.Errorf("env=prod should survive; got %#v", gotMatch)
	}
	if _, ok := gotMatch["job"]; ok {
		t.Errorf("job should not survive a keep without it; got %#v", gotMatch)
	}

	// value differs → dropped
	_, gotMiss := tx("l", 0, map[string]string{
		"job": "api",
		"env": "stg",
	})
	if _, ok := gotMiss["env"]; ok {
		t.Errorf("env=stg should not survive keep env=\"prod\"; got %#v", gotMiss)
	}
}

// TestDropKeep_Compose — `| drop` followed by another stage sees the
// projected label set. Mirrors the compose tests for unpack/pattern.
func TestDropKeep_Compose(t *testing.T) {
	t.Parallel()

	// `| drop env` then `| line_format` template references only the
	// surviving labels.
	expr, err := syntax.ParseExpr(`{job="api"} | drop env | line_format "[{{.job}}] {{__line__}}"`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	gotLine, gotLabels := tx("hi", 0, map[string]string{
		"job": "api",
		"env": "prod",
	})
	if gotLine != "[api] hi" {
		t.Errorf("line: got %q, want %q", gotLine, "[api] hi")
	}
	if _, ok := gotLabels["env"]; ok {
		t.Errorf("env should be dropped before line_format runs; got %#v", gotLabels)
	}
}

// TestDropLabels_ExtractOK — pins that `| drop` is recognised by
// postProcessExtract (i.e. the handler-side dispatch wires the
// projection through). The SQL-equivalence contract is pinned in
// internal/logql/drop_keep_test.go TestLowerDropKeep_NoSQLImpact.
func TestDropLabels_ExtractOK(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr(`{job="api"} | drop env`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := postProcessExtract(expr); err != nil {
		t.Fatalf("postProcessExtract: %v", err)
	}
}
