package traceql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestGroupsPerSpanset pins that only a GROUP BY on the trace-id column under
// lowerAggregate's alias, alone or followed by a by() group key under the
// GroupKey alias, counts as a spanset aggregate: any other key, column or
// alias names a cohort a `TraceId IN (…)` or `(TraceId, key) IN (…)`
// semi-join cannot reproduce.
func TestGroupsPerSpanset(t *testing.T) {
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
		{"trace id and group key", []chplan.Expr{col(traceID), col("SpanName")}, []string{aggTraceIDAlias, groupKeyAlias}, true},
		{"trace id and span id (coalesce)", []chplan.Expr{col(traceID), col("SpanId")}, []string{traceID, "SpanId"}, false},
		{"second key under another alias", []chplan.Expr{col(traceID), col("SpanName")}, []string{aggTraceIDAlias, otherAlias}, false},
		{"three keys", []chplan.Expr{col(traceID), col("SpanName"), col("SpanKind")}, []string{aggTraceIDAlias, groupKeyAlias, otherAlias}, false},
		{"no key", nil, nil, false},
		{"second key, one alias", []chplan.Expr{col(traceID), col("SpanName")}, []string{aggTraceIDAlias}, false},
		{"one key, second alias", []chplan.Expr{col(traceID)}, []string{aggTraceIDAlias, otherAlias}, false},
		{"other column under the trace-id alias", []chplan.Expr{col("SpanName")}, []string{aggTraceIDAlias}, false},
		{"trace id under another alias", []chplan.Expr{col(traceID)}, []string{otherAlias}, false},
		{"non-column key", []chplan.Expr{&chplan.LitString{V: traceID}}, []string{aggTraceIDAlias}, false},
	} {
		agg := &chplan.Aggregate{GroupBy: tc.groupBy, GroupByAliases: tc.aliases}
		if got := groupsPerSpanset(agg, traceID); got != tc.want {
			t.Errorf("%s: groupsPerSpanset = %v, want %v", tc.name, got, tc.want)
		}
	}
}
