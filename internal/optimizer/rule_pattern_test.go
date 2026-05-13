package optimizer_test

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// TestPatternRule_IdentityNoop wires a PatternRule that matches any Filter
// and returns nil (no change). The driver should produce a tree
// structurally equal to the input.
func TestPatternRule_IdentityNoop(t *testing.T) {
	t.Parallel()

	identity := &optimizer.PatternRule{
		RuleName:  "identity-filter",
		Match:     optimizer.Kind(optimizer.KindFilter),
		Transform: func(_ optimizer.Bindings) chplan.Node { return nil },
	}

	input := &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "MetricName"},
			Right: &chplan.LitString{V: "up"},
		},
	}

	out := optimizer.New(identity).Run(context.Background(), input)
	if !out.Equal(input) {
		t.Fatalf("identity rule mutated the plan: got %#v, want %#v", out, input)
	}
}

// TestPatternRule_DropLimit defines a rule that matches a Limit with a
// captured child input and replaces the Limit with its input — i.e. the
// Limit is stripped from the tree.
func TestPatternRule_DropLimit(t *testing.T) {
	t.Parallel()

	dropLimit := &optimizer.PatternRule{
		RuleName: "drop-limit",
		Match: optimizer.WithChildren(
			optimizer.Kind(optimizer.KindLimit),
			optimizer.Capture("input", optimizer.Any()),
		),
		Transform: func(b optimizer.Bindings) chplan.Node {
			in, ok := b.Get("input")
			if !ok {
				return nil
			}
			return in
		},
	}

	inner := &chplan.Scan{Table: "otel_metrics_gauge"}
	input := &chplan.Limit{Input: inner, Count: 10}

	out := optimizer.New(dropLimit).Run(context.Background(), input)
	if !out.Equal(inner) {
		t.Fatalf("drop-limit produced %#v, want %#v", out, inner)
	}
}

// TestPatternRule_DropLimit_Nested confirms the rule fires inside a
// larger tree — the existing driver walks the whole tree, so PatternRule
// gets its per-node match at every depth.
func TestPatternRule_DropLimit_Nested(t *testing.T) {
	t.Parallel()

	dropLimit := &optimizer.PatternRule{
		RuleName: "drop-limit",
		Match: optimizer.WithChildren(
			optimizer.Kind(optimizer.KindLimit),
			optimizer.Capture("input", optimizer.Any()),
		),
		Transform: func(b optimizer.Bindings) chplan.Node {
			in, _ := b.Get("input")
			return in
		},
	}

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	input := &chplan.Filter{
		Input: &chplan.Limit{Input: scan, Count: 10},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "MetricName"},
			Right: &chplan.LitString{V: "up"},
		},
	}

	out := optimizer.New(dropLimit).Run(context.Background(), input)

	got, ok := out.(*chplan.Filter)
	if !ok {
		t.Fatalf("expected Filter at root, got %T", out)
	}
	if !got.Input.Equal(scan) {
		t.Fatalf("Filter.Input = %#v, want Scan after Limit was dropped", got.Input)
	}
}

// TestPatternRule_NilFieldsNoop guards against accidental nil-deref when
// a malformed PatternRule (missing Match or Transform) ends up in a
// driver — it should be a no-op rather than crash.
func TestPatternRule_NilFieldsNoop(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}

	for _, r := range []*optimizer.PatternRule{
		{RuleName: "no-match"},
		{RuleName: "no-transform", Match: optimizer.Any()},
		{RuleName: "no-match-only-transform", Transform: func(optimizer.Bindings) chplan.Node { return scan }},
	} {
		out, changed := r.Apply(scan)
		if changed {
			t.Fatalf("%s: reported change on degenerate rule", r.RuleName)
		}
		if out != chplan.Node(scan) {
			t.Fatalf("%s: returned %v, want input unchanged", r.RuleName, out)
		}
	}
}

// TestPatternRule_ImplementsRule is a compile-time check that PatternRule
// satisfies the existing Rule interface — i.e. the existing driver picks
// it up with no plumbing changes.
func TestPatternRule_ImplementsRule(t *testing.T) {
	t.Parallel()
	var _ optimizer.Rule = (*optimizer.PatternRule)(nil)
}
