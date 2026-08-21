package chsql

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestMutation_EmitAggregate_HavingWithDropEmptyIsRejected pins the guard that
// refuses `Having` on the no-group + DropEmptyOnNoGroup shape. That shape does
// not render as a plain aggregate at all: emitAggregateNoGroup wraps it in a
// `count() > 0` guard layer, and the wrapper has nowhere to hang a HAVING
// clause — accepting the combination would silently DROP the predicate, so a
// `sum(x) > 5`-style filter would stop filtering.
//
// Kills emit_node.go:519:39 CONDITIONALS_NEGATION (`len(a.GroupBy) == 0` ->
// `!= 0`): the mutated guard fires only for GROUPED aggregates, so this
// ungrouped plan sails past it into emitAggregateNoGroup and emits SQL.
func TestMutation_EmitAggregate_HavingWithDropEmptyIsRejected(t *testing.T) {
	t.Parallel()

	_, _, err := Emit(context.Background(), &chplan.Aggregate{
		Input:              &chplan.Scan{Table: "otel_traces"},
		AggFuncs:           []chplan.AggFunc{{Fn: chplan.FnCount, Args: []chplan.Expr{&chplan.LitInt{V: 1}}, Alias: "Value"}},
		Having:             &chplan.Binary{Op: chplan.OpGt, Left: &chplan.ColumnRef{Name: "Value"}, Right: &chplan.LitInt{V: 0}},
		DropEmptyOnNoGroup: true,
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Having combined with the no-group DropEmptyOnNoGroup shape must be rejected, got %v", err)
	}
}

// TestMutation_EmitAggregate_HavingWithGroupKeysIsAccepted pins the complement
// so the rejection above cannot pass by rejecting every Having: a GROUPED
// aggregate renders a real GROUP BY, so HAVING has somewhere to attach and
// DropEmptyOnNoGroup is simply inert (the flag only speaks about the no-group
// case).
//
// Also kills emit_node.go:519:39 CONDITIONALS_NEGATION from the other side:
// the mutated guard rejects exactly this plan.
func TestMutation_EmitAggregate_HavingWithGroupKeysIsAccepted(t *testing.T) {
	t.Parallel()

	sql := emitNodeSQL(t, &chplan.Aggregate{
		Input:              &chplan.Scan{Table: "otel_traces"},
		GroupBy:            []chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}},
		GroupByAliases:     []string{"svc"},
		AggFuncs:           []chplan.AggFunc{{Fn: chplan.FnCount, Args: []chplan.Expr{&chplan.LitInt{V: 1}}, Alias: "Value"}},
		Having:             &chplan.Binary{Op: chplan.OpGt, Left: &chplan.ColumnRef{Name: "Value"}, Right: &chplan.LitInt{V: 0}},
		DropEmptyOnNoGroup: true,
	})
	if !strings.Contains(sql, "GROUP BY") || !strings.Contains(sql, "HAVING") {
		t.Fatalf("a grouped aggregate with Having must render both GROUP BY and HAVING, got %q", sql)
	}
}
