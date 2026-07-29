package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestEmitAggregate_ShadowingGroupKeyNamesItsAlias pins the ClickHouse
// alias-resolution rule that governs how a GROUP BY key is rendered.
//
// ClickHouse resolves an identifier inside GROUP BY against the SELECT
// aliases BEFORE the FROM columns. So a key projected as
// `mapSort(Attributes) AS Attributes` cannot be re-rendered as
// `GROUP BY mapSort(Attributes)`: the inner `Attributes` binds to the
// alias, the key becomes `mapSort(mapSort(Attributes))`, it no longer
// matches the SELECT expression, and the query dies with
//
//	Code: 215. DB::Exception: Column 'Attributes' is not under aggregate
//	function and not in GROUP BY keys
//
// Naming the alias is unambiguous — it denotes exactly the projected
// expression — so an aliased key is always emitted as its alias. This is
// the shape the histogram series-identity keys produce once their Map is
// canonicalised, which is why it has its own regression pin.
func TestEmitAggregate_ShadowingGroupKeyNamesItsAlias(t *testing.T) {
	t.Parallel()

	plan := &chplan.Aggregate{
		Input: &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
		GroupBy: []chplan.Expr{
			&chplan.FuncCall{
				Name: "mapSort",
				Args: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			},
		},
		GroupByAliases: []string{"Attributes"},
		AggFuncs: []chplan.AggFunc{{
			Name: "argMax",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: "Count"},
				&chplan.ColumnRef{Name: "TimeUnix"},
			},
			Alias: "Count",
		}},
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "mapSort(`Attributes`) AS `Attributes`") {
		t.Errorf("SELECT list lost the canonicalising projection\nsql=%s", sql)
	}
	if !strings.Contains(sql, "GROUP BY `Attributes`") {
		t.Errorf("GROUP BY must name the alias\nsql=%s", sql)
	}
	if strings.Contains(sql, "GROUP BY mapSort(") {
		t.Errorf("GROUP BY re-rendered a self-shadowing expression\nsql=%s", sql)
	}
}

// TestEmitAggregate_UnaliasedGroupKeyKeepsItsExpression is the other half
// of the rule: with no alias to name, the key can only be the expression
// itself.
func TestEmitAggregate_UnaliasedGroupKeyKeepsItsExpression(t *testing.T) {
	t.Parallel()

	plan := &chplan.Aggregate{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		GroupBy: []chplan.Expr{
			&chplan.FuncCall{
				Name: "mapSort",
				Args: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			},
		},
		AggFuncs: []chplan.AggFunc{{
			Name:  "sum",
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: "Value"}},
			Alias: "Value",
		}},
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "GROUP BY mapSort(`Attributes`)") {
		t.Errorf("un-aliased key must group by its expression\nsql=%s", sql)
	}
}
