package chsql

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitNodeSQL renders a single plan node through a fresh emitter.
func emitNodeSQL(t *testing.T, n chplan.Node) string {
	t.Helper()
	sql, _, err := Emit(context.Background(), n)
	if err != nil {
		t.Fatalf("Emit(%T): unexpected error: %v", n, err)
	}
	return sql
}

// TestMutation_MetricsAggregate_GroupByAliasesShorterThanGroupBy pins that the
// SELECT-list loop treats GroupByAliases as OPTIONAL per group key: chplan
// permits an unaliased group, so a plan carrying group keys and no aliases at
// all must render them bare rather than indexing off the end of the slice.
//
// Kills range_window.go's `i < len(selectedAliases)` CONDITIONALS_BOUNDARY
// (`i <= len(...)`): with one group key and zero aliases the mutated guard is
// `0 <= 0`, so it reads selectedAliases[0] out of an empty slice and panics.
func TestMutation_MetricsAggregate_GroupByAliasesShorterThanGroupBy(t *testing.T) {
	t.Parallel()

	sql := emitNodeSQL(t, &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpRate,
		GroupBy:    []chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}},
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	})
	if !strings.Contains(sql, "ServiceName") {
		t.Fatalf("unaliased group key must still be projected, got %q", sql)
	}
}

// TestMutation_MetricsAggregate_GroupByAliasesProjected pins the other half of
// the same guard: when an alias IS supplied for a group key, the SELECT list
// must carry `<expr> AS <alias>` — that alias is the name the GROUP BY below
// refers to, so dropping it makes the query reference an unknown identifier.
//
// Kills range_window.go's `i < len(selectedAliases)` CONDITIONALS_NEGATION
// (`i >= len(...)`), which inverts the guard so the alias is only picked up for
// indexes past the end of the slice — i.e. never — and every group key renders
// bare.
func TestMutation_MetricsAggregate_GroupByAliasesProjected(t *testing.T) {
	t.Parallel()

	sql := emitNodeSQL(t, &chplan.MetricsAggregate{
		Op:             chplan.MetricsOpRate,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}},
		GroupByAliases: []string{"svc"},
		ValueAlias:     "Value",
		Inner:          &chplan.Scan{Table: "otel_traces"},
	})
	if !strings.Contains(sql, "AS `svc`") {
		t.Fatalf("aliased group key must render `<expr> AS `svc``, got %q", sql)
	}
}

// TestMutation_GroupKeyFrags_EveryAliasedKeySurvives pins that groupKeyFrags
// walks the WHOLE group-by list. Each aliased key contributes its alias to the
// GROUP BY; a truncated list silently groups by a prefix of the keys, which
// merges series that should stay apart.
//
// Kills range_window.go groupKeyFrags' INVERT_LOOPCTRL (`continue` -> `break`):
// the mutant stops at the first aliased key, so a two-key group-by renders a
// one-key GROUP BY.
func TestMutation_GroupKeyFrags_EveryAliasedKeySurvives(t *testing.T) {
	t.Parallel()

	frags := groupKeyFrags(
		[]chplan.Expr{&chplan.ColumnRef{Name: "a"}, &chplan.ColumnRef{Name: "b"}},
		[]string{"ka", "kb"},
	)
	if len(frags) != 2 {
		t.Fatalf("expected one frag per group key, got %d", len(frags))
	}
	for i, want := range []string{"`ka`", "`kb`"} {
		if got := renderFragToSQL(frags[i]); got != want {
			t.Fatalf("frag %d: expected %q, got %q", i, want, got)
		}
	}
}

// TestMutation_GroupKeyFrags_UnaliasedKeyFallsBackToExpr pins the fall-through
// arm: a key with no alias (or an empty one) groups by the expression itself.
// Without this the loop-control assertion above could be satisfied by a
// groupKeyFrags that always took the alias branch.
func TestMutation_GroupKeyFrags_UnaliasedKeyFallsBackToExpr(t *testing.T) {
	t.Parallel()

	frags := groupKeyFrags(
		[]chplan.Expr{&chplan.ColumnRef{Name: "a"}, &chplan.ColumnRef{Name: "b"}},
		[]string{"", "kb"},
	)
	if len(frags) != 2 {
		t.Fatalf("expected one frag per group key, got %d", len(frags))
	}
	if got := renderFragToSQL(frags[0]); got != "`a`" {
		t.Fatalf("empty alias must group by the expression, got %q", got)
	}
	if got := renderFragToSQL(frags[1]); got != "`kb`" {
		t.Fatalf("non-empty alias must group by the alias, got %q", got)
	}
}

// TestMutation_RequireRecollapseEmittable_NeedsBothStateAndMerge pins that the
// deferred re-collapse requires the COMPLETE -State/-Merge pair. Half a pair is
// as unusable as none: the partial-aggregate column produced by -State can only
// be folded back by its matching -Merge, so a func carrying one without the
// other must be rejected up front rather than emitting SQL that cannot resolve.
//
// Kills range_window_native.go's `StateFn == "" || MergeFn == ""`
// INVERT_LOGICAL (`&&`), which only rejects a func missing BOTH halves and
// waves through either half-pair.
func TestMutation_RequireRecollapseEmittable_NeedsBothStateAndMerge(t *testing.T) {
	t.Parallel()

	r := &chplan.RangeWindowNative{
		Func:       "rate",
		GroupBy:    []chplan.Expr{&chplan.ColumnRef{Name: "a"}},
		Recollapse: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "a"}, Alias: "ka"}},
	}
	for _, agg := range []nativeTSGridAgg{
		{Fn: "timeSeriesRateToGrid", StateFn: "someState", MergeFn: ""},
		{Fn: "timeSeriesRateToGrid", StateFn: "", MergeFn: "someMerge"},
		{Fn: "timeSeriesRateToGrid"},
	} {
		_, err := requireRecollapseEmittable(r, agg)
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("agg %+v: an incomplete -State/-Merge pair must be rejected, got err=%v", agg, err)
		}
	}
}

// TestMutation_RequireRecollapseEmittable_AcceptsCompletePair pins the success
// arm so the rejection assertion above cannot pass by rejecting everything.
func TestMutation_RequireRecollapseEmittable_AcceptsCompletePair(t *testing.T) {
	t.Parallel()

	keys, err := requireRecollapseEmittable(
		&chplan.RangeWindowNative{
			Func:       "rate",
			GroupBy:    []chplan.Expr{&chplan.ColumnRef{Name: "a"}},
			Recollapse: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "a"}, Alias: "ka"}},
		},
		nativeTSGridAgg{Fn: "timeSeriesRateToGrid", StateFn: "someState", MergeFn: "someMerge"},
	)
	if err != nil {
		t.Fatalf("a complete -State/-Merge pair must be accepted, got %v", err)
	}
	if len(keys) != 1 || keys[0] != "a" {
		t.Fatalf("expected the GroupBy column names to be returned, got %v", keys)
	}
}
