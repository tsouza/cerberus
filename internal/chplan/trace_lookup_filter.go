package chplan

// TraceLookupKeyColumn is the column every [TraceLookupFilter] lookup
// relation names its trace id. It is deliberately not the spans table's own
// trace-id column name: the lookups are joined beside Input, and a key column
// sharing Input's trace-id name would make a bare reference to that column in
// Predicate ambiguous between the two sides of the join.
const TraceLookupKeyColumn = "_cerb_lookup_trace_id"

// TraceLookupFilter keeps the rows of Input that satisfy Predicate, where
// Predicate reads, besides Input's own columns, the value columns of one or
// more per-trace lookup relations. Each lookup is LEFT JOINed onto every
// Input row on the trace id, so a row sees the lookup values of its OWN
// trace:
//
//	SELECT m.* FROM (<Input>) AS m
//	LEFT JOIN (<Lookups[0]>) AS l0 ON m.<trace id> = l0.<TraceLookupKeyColumn>
//	…
//	WHERE <Predicate>
//
// It answers a comparison between a trace-level value and a per-span one —
// TraceQL's `{ rootName = name }` — which no membership test over a
// plan-time set of traces can express: the right-hand side differs per span.
//
// Contract on each lookup relation:
//   - at most one row per trace, keyed by a column named TraceLookupKeyColumn;
//   - its value columns carry names no other lookup and no Input column use,
//     so Predicate can reference them bare.
//
// A trace absent from a lookup gets the LEFT JOIN default for that lookup's
// values (NULL under join_use_nulls=1); Predicate is responsible for the
// value such a trace must compare as.
//
// The output row type is Input's: the lookup columns are read by Predicate
// and never projected, so a consumer that combines this relation with
// another span relation positionally (a UNION arm) sees the same columns as
// it would from a plain [Filter].
//
// The trace id of an Input row is the column carrying [RoleTraceID] in
// Input's row type.
type TraceLookupFilter struct {
	Input     Node
	Lookups   []Node
	Predicate Expr
}

func (*TraceLookupFilter) planNode() {}

// Children returns Input followed by every lookup relation, in order.
func (f *TraceLookupFilter) Children() []Node {
	return append([]Node{f.Input}, f.Lookups...)
}

func (f *TraceLookupFilter) Equal(other Node) bool {
	o, ok := other.(*TraceLookupFilter)
	if !ok || len(f.Lookups) != len(o.Lookups) || !f.Predicate.Equal(o.Predicate) {
		return false
	}
	for i := range f.Lookups {
		if !f.Lookups[i].Equal(o.Lookups[i]) {
			return false
		}
	}
	return f.Input.Equal(o.Input)
}
