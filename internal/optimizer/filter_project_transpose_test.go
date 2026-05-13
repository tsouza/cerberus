package optimizer_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// TestFilterProjectTranspose_Passthrough fires the rule on
// `Filter(Project([X, Y], Scan), X = "v")` and expects
// `Project([X, Y], Filter(Scan, X = "v"))`.
func TestFilterProjectTranspose_Passthrough(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	pred := &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "MetricName"},
		Right: &chplan.LitString{V: "up"},
	}
	input := &chplan.Filter{
		Input: &chplan.Project{
			Input: scan,
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "MetricName"}},
				{Expr: &chplan.ColumnRef{Name: "Value"}},
			},
		},
		Predicate: pred,
	}

	expected := &chplan.Project{
		Input: &chplan.Filter{
			Input:     scan,
			Predicate: pred,
		},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "MetricName"}},
			{Expr: &chplan.ColumnRef{Name: "Value"}},
		},
	}

	out := optimizer.New(optimizer.FilterProjectTranspose()).Run(input)
	if !out.Equal(expected) {
		t.Fatalf("FilterProjectTranspose did not fire:\n got: %#v\nwant: %#v", out, expected)
	}
}

// TestFilterProjectTranspose_Aliased confirms the rule still fires when
// the Project carries an alias matching the column name (a no-op rename
// from CH's perspective).
func TestFilterProjectTranspose_Aliased(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	pred := &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "MetricName"},
		Right: &chplan.LitString{V: "up"},
	}
	input := &chplan.Filter{
		Input: &chplan.Project{
			Input: scan,
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "MetricName"}, Alias: "MetricName"},
				{Expr: &chplan.ColumnRef{Name: "Value"}},
			},
		},
		Predicate: pred,
	}

	out := optimizer.New(optimizer.FilterProjectTranspose()).Run(input)
	proj, ok := out.(*chplan.Project)
	if !ok {
		t.Fatalf("expected Project at root, got %T", out)
	}
	if _, ok := proj.Input.(*chplan.Filter); !ok {
		t.Fatalf("expected Filter under Project, got %T", proj.Input)
	}
}

// TestFilterProjectTranspose_BlockedByRename keeps the Filter above the
// Project when the predicate references a renamed alias — the source
// column name doesn't exist below the Project.
func TestFilterProjectTranspose_BlockedByRename(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	input := &chplan.Filter{
		Input: &chplan.Project{
			Input: scan,
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "MetricName"}, Alias: "metric"},
			},
		},
		// Predicate references the *alias* `metric`, which does not
		// exist below the Project.
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "metric"},
			Right: &chplan.LitString{V: "up"},
		},
	}

	out := optimizer.New(optimizer.FilterProjectTranspose()).Run(input)
	if !out.Equal(input) {
		t.Fatalf("rule fired despite alias-renamed reference:\n got: %#v", out)
	}
}

// TestFilterProjectTranspose_BlockedByComputed keeps the Filter when
// the Project introduces a computed column the predicate then mentions.
func TestFilterProjectTranspose_BlockedByComputed(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	input := &chplan.Filter{
		Input: &chplan.Project{
			Input: scan,
			Projections: []chplan.Projection{
				// computed column `doubled = Value * 2`
				{
					Expr: &chplan.Binary{
						Op:    chplan.OpMul,
						Left:  &chplan.ColumnRef{Name: "Value"},
						Right: &chplan.LitInt{V: 2},
					},
					Alias: "doubled",
				},
				{Expr: &chplan.ColumnRef{Name: "MetricName"}},
			},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  &chplan.ColumnRef{Name: "doubled"},
			Right: &chplan.LitInt{V: 10},
		},
	}

	out := optimizer.New(optimizer.FilterProjectTranspose()).Run(input)
	if !out.Equal(input) {
		t.Fatalf("rule fired despite computed-column reference:\n got: %#v", out)
	}
}

// TestFilterProjectTranspose_BlockedByStarProject is conservative: an
// empty Projections slice (`SELECT *`) declines the rewrite. The
// emitter renders it as a no-op subselect anyway, so the rule's
// abstention has no practical cost.
func TestFilterProjectTranspose_BlockedByStarProject(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	input := &chplan.Filter{
		Input: &chplan.Project{Input: scan},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "MetricName"},
			Right: &chplan.LitString{V: "up"},
		},
	}

	out := optimizer.New(optimizer.FilterProjectTranspose()).Run(input)
	if !out.Equal(input) {
		t.Fatalf("rule fired on SELECT-* Project:\n got: %#v", out)
	}
}

// TestFilterProjectTranspose_NoMatchOnOtherShape leaves a Filter
// directly over a Scan alone — the pattern requires Filter(Project(...))
// specifically.
func TestFilterProjectTranspose_NoMatchOnOtherShape(t *testing.T) {
	t.Parallel()

	input := &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "MetricName"},
			Right: &chplan.LitString{V: "up"},
		},
	}

	out := optimizer.New(optimizer.FilterProjectTranspose()).Run(input)
	if !out.Equal(input) {
		t.Fatalf("rule fired on Filter(Scan): %#v", out)
	}
}
