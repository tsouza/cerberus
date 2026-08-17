package optimizer_test

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// TestFilterAggregateTranspose_GroupKey fires on
// `Filter(Aggregate(Scan, GROUP BY job), job = "api")`
// and expects the Filter to land under the Aggregate.
func TestFilterAggregateTranspose_GroupKey(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	pred := &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "job"},
		Right: &chplan.LitString{V: "api"},
	}
	input := &chplan.Filter{
		Input: &chplan.Aggregate{
			Input:   scan,
			GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "job"}},
			AggFuncs: []chplan.AggFunc{
				{Fn: chplan.FnSum, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "sum_value"},
			},
		},
		Predicate: pred,
	}

	expected := &chplan.Aggregate{
		Input: &chplan.Filter{
			Input:     scan,
			Predicate: pred,
		},
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "job"}},
		AggFuncs: []chplan.AggFunc{
			{Fn: chplan.FnSum, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "sum_value"},
		},
	}

	out := optimizer.New(optimizer.FilterAggregateTranspose()).Run(context.Background(), input)
	if !out.Equal(expected) {
		t.Fatalf("FilterAggregateTranspose did not fire:\n got: %#v\nwant: %#v", out, expected)
	}
}

// TestFilterAggregateTranspose_BlockedByAggOutput leaves the Filter
// above when the predicate touches the aggregate output column (here
// `sum_value`), which doesn't exist below the Aggregate.
func TestFilterAggregateTranspose_BlockedByAggOutput(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	input := &chplan.Filter{
		Input: &chplan.Aggregate{
			Input:   scan,
			GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "job"}},
			AggFuncs: []chplan.AggFunc{
				{Fn: chplan.FnSum, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "sum_value"},
			},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  &chplan.ColumnRef{Name: "sum_value"},
			Right: &chplan.LitFloat{V: 0},
		},
	}

	out := optimizer.New(optimizer.FilterAggregateTranspose()).Run(context.Background(), input)
	if !out.Equal(input) {
		t.Fatalf("rule fired despite aggregate-output reference:\n got: %#v", out)
	}
}

// TestFilterAggregateTranspose_BlockedByRenamedKey leaves the Filter
// above when the matched group key has been renamed via GroupByAliases
// and the predicate references the alias.
func TestFilterAggregateTranspose_BlockedByRenamedKey(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	input := &chplan.Filter{
		Input: &chplan.Aggregate{
			Input:          scan,
			GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "job"}},
			GroupByAliases: []string{"renamed_job"},
			AggFuncs: []chplan.AggFunc{
				{Fn: chplan.FnCount, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "n"},
			},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "renamed_job"},
			Right: &chplan.LitString{V: "api"},
		},
	}

	out := optimizer.New(optimizer.FilterAggregateTranspose()).Run(context.Background(), input)
	if !out.Equal(input) {
		t.Fatalf("rule fired despite renamed group key:\n got: %#v", out)
	}
}

// TestFilterAggregateTranspose_BlockedByComputedGroupKey skips Aggregates
// whose group keys are not bare ColumnRefs (e.g. `GROUP BY substr(X, 1)`).
func TestFilterAggregateTranspose_BlockedByComputedGroupKey(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	input := &chplan.Filter{
		Input: &chplan.Aggregate{
			Input: scan,
			GroupBy: []chplan.Expr{
				&chplan.FuncCall{
					Fn:   chplan.FnSubstring,
					Args: []chplan.Expr{&chplan.ColumnRef{Name: "MetricName"}, &chplan.LitInt{V: 1}},
				},
			},
			AggFuncs: []chplan.AggFunc{
				{Fn: chplan.FnCount, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "n"},
			},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "MetricName"},
			Right: &chplan.LitString{V: "up"},
		},
	}

	out := optimizer.New(optimizer.FilterAggregateTranspose()).Run(context.Background(), input)
	if !out.Equal(input) {
		t.Fatalf("rule fired despite computed group key:\n got: %#v", out)
	}
}

// TestFilterAggregateTranspose_MixedBareAndComputedGroupKey verifies that a
// GroupBy mixing a bare key (`job`) with a computed one
// (`substr(MetricName, 1)`) still allows the rewrite via the bare subset:
// the computed entry contributes no passthrough name and is simply skipped,
// it does not abort the whole passthrough set. This pins the "skip, don't
// abort" policy (see #2285) against a case both existing
// BlockedByComputedGroupKey tests miss — they use a single-entry GroupBy,
// where "skip down to empty" and "abort the whole set" happen to coincide.
func TestFilterAggregateTranspose_MixedBareAndComputedGroupKey(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	pred := &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "job"},
		Right: &chplan.LitString{V: "api"},
	}
	computedKey := &chplan.FuncCall{
		Fn:   chplan.FnSubstring,
		Args: []chplan.Expr{&chplan.ColumnRef{Name: "MetricName"}, &chplan.LitInt{V: 1}},
	}
	groupBy := []chplan.Expr{&chplan.ColumnRef{Name: "job"}, computedKey}
	aggFuncs := []chplan.AggFunc{
		{Fn: chplan.FnCount, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "n"},
	}
	input := &chplan.Filter{
		Input: &chplan.Aggregate{
			Input:    scan,
			GroupBy:  groupBy,
			AggFuncs: aggFuncs,
		},
		Predicate: pred,
	}

	expected := &chplan.Aggregate{
		Input: &chplan.Filter{
			Input:     scan,
			Predicate: pred,
		},
		GroupBy:  groupBy,
		AggFuncs: aggFuncs,
	}

	out := optimizer.New(optimizer.FilterAggregateTranspose()).Run(context.Background(), input)
	if !out.Equal(expected) {
		t.Fatalf("FilterAggregateTranspose did not fire on mixed bare/computed GroupBy:\n got: %#v\nwant: %#v", out, expected)
	}
}

// TestFilterAggregateTranspose_AliasMatchesName allows the rewrite when
// the alias matches the underlying column name — that's a no-op rename.
func TestFilterAggregateTranspose_AliasMatchesName(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	input := &chplan.Filter{
		Input: &chplan.Aggregate{
			Input:          scan,
			GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "job"}},
			GroupByAliases: []string{"job"},
			AggFuncs: []chplan.AggFunc{
				{Fn: chplan.FnCount, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "n"},
			},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "job"},
			Right: &chplan.LitString{V: "api"},
		},
	}

	out := optimizer.New(optimizer.FilterAggregateTranspose()).Run(context.Background(), input)
	agg, ok := out.(*chplan.Aggregate)
	if !ok {
		t.Fatalf("expected Aggregate at root, got %T", out)
	}
	if _, ok := agg.Input.(*chplan.Filter); !ok {
		t.Fatalf("expected Filter under Aggregate, got %T", agg.Input)
	}
}

// TestFilterAggregateTranspose_NoMatchOnOtherShape leaves Filter(Scan)
// alone.
func TestFilterAggregateTranspose_NoMatchOnOtherShape(t *testing.T) {
	t.Parallel()

	input := &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "MetricName"},
			Right: &chplan.LitString{V: "up"},
		},
	}

	out := optimizer.New(optimizer.FilterAggregateTranspose()).Run(context.Background(), input)
	if !out.Equal(input) {
		t.Fatalf("rule fired on Filter(Scan): %#v", out)
	}
}
