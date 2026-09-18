package attrmap_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/attrmap"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestAt_BindsTheKeyAsAnArgument pins the one property every fragment here
// inherits: the Map key travels as a positional argument, never as SQL text.
func TestAt_BindsTheKeyAsAnArgument(t *testing.T) {
	t.Parallel()

	sql, args := chsql.Render(attrmap.At("Attributes", "service.name"))
	if sql != "`Attributes`[?]" {
		t.Errorf("At = %q, want the bare subscript with a ? placeholder", sql)
	}
	if !reflect.DeepEqual(args, []any{"service.name"}) {
		t.Errorf("args = %#v, want the key bound once", args)
	}
	if strings.Contains(sql, "service.name") {
		t.Errorf("the key leaked into the SQL text: %q", sql)
	}
}

func TestDistinctAt_WrapsTheSubscript(t *testing.T) {
	t.Parallel()

	sql, args := chsql.Render(attrmap.DistinctAt("ResourceAttributes", "k8s.pod.name"))
	if sql != "DISTINCT `ResourceAttributes`[?]" {
		t.Errorf("DistinctAt = %q", sql)
	}
	if !reflect.DeepEqual(args, []any{"k8s.pod.name"}) {
		t.Errorf("args = %#v", args)
	}
}

// TestNotEmpty_BindsBothTheKeyAndTheSentinel pins that the empty-string
// sentinel is a bound argument too, so the predicate carries no literal.
func TestNotEmpty_BindsBothTheKeyAndTheSentinel(t *testing.T) {
	t.Parallel()

	sql, args := chsql.Render(attrmap.NotEmpty("Attributes", "job"))
	if sql != "`Attributes`[?] != ?" {
		t.Errorf("NotEmpty = %q", sql)
	}
	if !reflect.DeepEqual(args, []any{"job", ""}) {
		t.Errorf("args = %#v, want the key then the empty sentinel", args)
	}
}

// TestCollapsedValues_OneProjectionForEveryCandidate pins the shape that
// makes a label-values lookup one scan regardless of how many spellings it
// checks: every candidate becomes one array element of a single
// arrayJoin(arrayFilter(...)) projection, the sentinel filter is bound
// before the elements, and no candidate appears in the SQL text.
func TestCollapsedValues_OneProjectionForEveryCandidate(t *testing.T) {
	t.Parallel()

	keys := []string{"k8s_pod_name", "k8s.pod_name", "k8s_pod.name", "k8s.pod.name"}
	sql, args := chsql.Render(attrmap.CollapsedValues("ResourceAttributes", keys))

	want := "arrayJoin(arrayFilter(v -> v != ?, [`ResourceAttributes`[?], `ResourceAttributes`[?], `ResourceAttributes`[?], `ResourceAttributes`[?]]))"
	if sql != want {
		t.Errorf("CollapsedValues =\n %s\nwant\n %s", sql, want)
	}
	wantArgs := []any{""}
	for _, k := range keys {
		wantArgs = append(wantArgs, k)
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %#v, want %#v", args, wantArgs)
	}
	if strings.Count(sql, "arrayJoin") != 1 {
		t.Errorf("a collapsed projection has exactly one arrayJoin; got %q", sql)
	}
	for _, k := range keys {
		if strings.Contains(sql, k) {
			t.Errorf("candidate %q leaked into the SQL text: %q", k, sql)
		}
	}
}

// TestCollapsedValues_OneCandidateIsStillOneProjection pins the degenerate
// input: a single spelling renders the same shape with a one-element array,
// so callers need no special case for it.
func TestCollapsedValues_OneCandidateIsStillOneProjection(t *testing.T) {
	t.Parallel()

	sql, args := chsql.Render(attrmap.CollapsedValues("Attributes", []string{"job"}))
	if want := "arrayJoin(arrayFilter(v -> v != ?, [`Attributes`[?]]))"; sql != want {
		t.Errorf("CollapsedValues = %q, want %q", sql, want)
	}
	if !reflect.DeepEqual(args, []any{"", "job"}) {
		t.Errorf("args = %#v", args)
	}
}
