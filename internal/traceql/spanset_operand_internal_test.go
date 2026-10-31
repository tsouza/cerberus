package traceql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestGroupsOnTraceID pins that only a single-key GROUP BY on the trace-id
// column under lowerAggregate's alias counts as per-trace: a second key, a
// different column, or a different alias each name a cohort a
// `TraceId IN (…)` semi-join cannot reproduce.
func TestGroupsOnTraceID(t *testing.T) {
	t.Parallel()
	traceID := schema.DefaultOTelTraces().TraceIDColumn
	col := func(name string) chplan.Expr { return &chplan.ColumnRef{Name: name} }
	const otherAlias = "_cerb_other_alias"

	for _, tc := range []struct {
		name    string
		groupBy []chplan.Expr
		aliases []string
		want    bool
	}{
		{"trace id under its alias", []chplan.Expr{col(traceID)}, []string{aggTraceIDAlias}, true},
		{"second key, one alias", []chplan.Expr{col(traceID), col("SpanName")}, []string{aggTraceIDAlias}, false},
		{"one key, second alias", []chplan.Expr{col(traceID)}, []string{aggTraceIDAlias, otherAlias}, false},
		{"other column under the trace-id alias", []chplan.Expr{col("SpanName")}, []string{aggTraceIDAlias}, false},
		{"trace id under another alias", []chplan.Expr{col(traceID)}, []string{otherAlias}, false},
		{"non-column key", []chplan.Expr{&chplan.LitString{V: traceID}}, []string{aggTraceIDAlias}, false},
	} {
		agg := &chplan.Aggregate{GroupBy: tc.groupBy, GroupByAliases: tc.aliases}
		if got := groupsOnTraceID(agg, traceID); got != tc.want {
			t.Errorf("%s: groupsOnTraceID = %v, want %v", tc.name, got, tc.want)
		}
	}
}
