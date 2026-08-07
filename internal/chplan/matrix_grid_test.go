package chplan_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// matrixGridColumns is the canonical OTel-CH spelling of the four sample
// columns, written out rather than imported so this package's tests stay
// independent of internal/schema.
var matrixGridColumns = chplan.SampleColumns{
	MetricName: "MetricName",
	Attributes: "Attributes",
	Timestamp:  "TimeUnix",
	Value:      "Value",
}

func groupKey(names ...string) ([]chplan.Expr, []string) {
	exprs := make([]chplan.Expr, 0, len(names))
	for _, n := range names {
		exprs = append(exprs, &chplan.ColumnRef{Name: n})
	}
	return exprs, names
}

// TestAggregatePreservesMatrixGrid covers both directions of the predicate,
// because a classifier that only ever answers one way is indistinguishable
// from a constant. The two shapes below are the two real ones: the
// duplicate-labelset guard's Aggregate, which re-keys on the cell it was
// handed, and a PromQL `sum by (job)`, which folds it.
func TestAggregatePreservesMatrixGrid(t *testing.T) {
	for _, tc := range []struct {
		name string
		agg  *chplan.Aggregate
		want bool
	}{
		{
			// The duplicate-labelset guard over `label_replace(rate(m[5m]), …)`:
			// keys on the whole label set and on the grid anchor, aggregates
			// only the value.
			name: "guard_keys_on_attributes_and_anchor",
			agg: aggregateOn(
				[]string{matrixGridColumns.Attributes, chplan.RangeWindowAnchorColumn, matrixGridColumns.Timestamp},
				chplan.AggFunc{
					Name:  "any",
					Args:  []chplan.Expr{&chplan.ColumnRef{Name: matrixGridColumns.Value}},
					Alias: matrixGridColumns.Value,
				},
			),
			want: true,
		},
		{
			// `sum by (job) (rate(m[5m]))`: the label set is reduced to one
			// extracted key under a synthetic alias, and the step axis is
			// republished as `bucket_ts` off the schema timestamp — neither
			// name is one this predicate accepts.
			name: "promql_aggregation_folds_series_and_republishes_step",
			agg: aggregateOn(
				[]string{"gkey_0", "bucket_ts"},
				chplan.AggFunc{
					Name:  "sum",
					Args:  []chplan.Expr{&chplan.ColumnRef{Name: matrixGridColumns.Value}},
					Alias: matrixGridColumns.Value,
				},
			),
			want: false,
		},
		{
			// The series axis survives but the anchor does not: the grid
			// column is gone from the output scope, so a consumer that read
			// `anchor_ts` off this node would select an identifier it never
			// exposed.
			name: "attributes_without_anchor",
			agg: aggregateOn(
				[]string{matrixGridColumns.Attributes, matrixGridColumns.Timestamp},
				chplan.AggFunc{
					Name:  "any",
					Args:  []chplan.Expr{&chplan.ColumnRef{Name: matrixGridColumns.Value}},
					Alias: matrixGridColumns.Value,
				},
			),
			want: false,
		},
		{
			// The anchor survives but every series is folded onto one row per
			// step, so the node no longer emits one row per (series, anchor).
			name: "anchor_without_attributes",
			agg: aggregateOn(
				[]string{chplan.RangeWindowAnchorColumn},
				chplan.AggFunc{
					Name:  "sum",
					Args:  []chplan.Expr{&chplan.ColumnRef{Name: matrixGridColumns.Value}},
					Alias: matrixGridColumns.Value,
				},
			),
			want: false,
		},
		{
			// An Aggregate that groups on both columns but names NEITHER in
			// its alias list has collapsed them out of its output scope. The
			// emitter renders the raw expressions, so nothing downstream can
			// reference them by name.
			name: "keys_on_both_but_exposes_no_aliases",
			agg: &chplan.Aggregate{
				Input: &chplan.Scan{Table: "otel_metrics_gauge"},
				GroupBy: []chplan.Expr{
					&chplan.ColumnRef{Name: matrixGridColumns.Attributes},
					&chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn},
				},
				AggFuncs: []chplan.AggFunc{{
					Name:  "any",
					Args:  []chplan.Expr{&chplan.ColumnRef{Name: matrixGridColumns.Value}},
					Alias: matrixGridColumns.Value,
				}},
			},
			want: false,
		},
		{
			// An alias with no matching GroupBy entry names nothing: the two
			// slices are parallel, and the emitter aliases position by
			// position, so a trailing alias is never rendered.
			name: "alias_beyond_the_group_key",
			agg: &chplan.Aggregate{
				Input:   &chplan.Scan{Table: "otel_metrics_gauge"},
				GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: matrixGridColumns.Attributes}},
				GroupByAliases: []string{
					matrixGridColumns.Attributes,
					chplan.RangeWindowAnchorColumn,
				},
				AggFuncs: []chplan.AggFunc{{
					Name:  "any",
					Args:  []chplan.Expr{&chplan.ColumnRef{Name: matrixGridColumns.Value}},
					Alias: matrixGridColumns.Value,
				}},
			},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chplan.AggregatePreservesMatrixGrid(tc.agg, matrixGridColumns); got != tc.want {
				t.Errorf("AggregatePreservesMatrixGrid = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAggregatePreservesMatrixGrid_ReadsConfiguredAttributesName pins that
// the predicate reads the Attributes name it is handed rather than the OTel
// default. A schema override renames the column, and an Aggregate keyed on
// the renamed column must still be recognised — while the default spelling,
// which names nothing in that schema, must not be.
func TestAggregatePreservesMatrixGrid_ReadsConfiguredAttributesName(t *testing.T) {
	renamed := chplan.SampleColumns{
		MetricName: "name",
		Attributes: "labels",
		Timestamp:  "ts",
		Value:      "val",
	}
	onRenamed := aggregateOn(
		[]string{"labels", chplan.RangeWindowAnchorColumn},
		chplan.AggFunc{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: "val"}}, Alias: "val"},
	)
	if !chplan.AggregatePreservesMatrixGrid(onRenamed, renamed) {
		t.Error("an Aggregate keyed on the CONFIGURED attributes column must preserve the grid")
	}
	if chplan.AggregatePreservesMatrixGrid(onRenamed, matrixGridColumns) {
		t.Error("keying on `labels` must not satisfy a schema whose attributes column is `Attributes`")
	}

	// A schema that maps the attributes column away leaves no series
	// identity to key on, so no Aggregate can preserve the grid.
	noAttrs := renamed
	noAttrs.Attributes = ""
	emptyKeyed := aggregateOn(
		[]string{"", chplan.RangeWindowAnchorColumn},
		chplan.AggFunc{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: "val"}}, Alias: "val"},
	)
	if chplan.AggregatePreservesMatrixGrid(emptyKeyed, noAttrs) {
		t.Error("an empty attributes column names nothing; no Aggregate can expose it")
	}
}

func aggregateOn(aliases []string, aggs ...chplan.AggFunc) *chplan.Aggregate {
	groupBy, groupAliases := groupKey(aliases...)
	return &chplan.Aggregate{
		Input:          &chplan.Scan{Table: "otel_metrics_gauge"},
		GroupBy:        groupBy,
		GroupByAliases: groupAliases,
		AggFuncs:       aggs,
	}
}
