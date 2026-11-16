package chplan

// TraceLookupKeyColumn is the column every [TraceLookupFilter] lookup
// relation names its trace id. It is deliberately not the spans table's own
// trace-id column name: the lookups are joined beside Input, and a key column
// sharing Input's trace-id name would make a bare reference to that column in
// Predicate ambiguous between the two sides of the join.
const TraceLookupKeyColumn = "_cerb_lookup_trace_id"

// TraceLookupFilter joins one or more per-trace lookup relations onto every
// row of Input and keeps the rows that satisfy Predicate. Predicate reads,
// besides Input's own columns, the lookups' value columns. Each lookup is LEFT
// JOINed onto every Input row on the trace id, so a row sees the lookup values
// of its OWN trace:
//
//	SELECT m.* [, l0.<value>, …] FROM (<Input>) AS m
//	LEFT JOIN (<Lookups[0]>) AS l0 ON m.<trace id> = l0.<TraceLookupKeyColumn>
//	…
//	[WHERE <Predicate>]
//
// It answers a comparison between a trace-level value and a per-span one —
// TraceQL's `{ rootName = name }` — which no membership test over a
// plan-time set of traces can express: the right-hand side differs per span.
// With ExposeLookups set it also hands each row its trace's lookup values
// as columns, for a consumer that reads the per-trace value as a per-span
// VALUE rather than in a predicate: a group key (`by(rootName)`), an
// aggregate argument (`avg(traceDuration)`), a compare() selection.
//
// Contract on each lookup relation:
//   - at most one row per trace, keyed by a column named TraceLookupKeyColumn;
//   - its value columns carry names no other lookup and no Input column use,
//     so Predicate and the consumer above can reference them bare;
//   - its row type is closed, so its value columns are known.
//
// A trace absent from a lookup gets the LEFT JOIN default for that lookup's
// values (NULL under join_use_nulls=1); the expression reading the value is
// responsible for the value such a trace must compare or group as.
//
// The output row type is Input's, followed — only when ExposeLookups is set —
// by every lookup's value columns (LookupValueColumns). Without ExposeLookups
// the lookup columns are read by Predicate and never projected, so a consumer
// that combines this relation with another span relation positionally (a
// UNION arm) sees the same columns as it would from a plain [Filter].
//
// Predicate is optional when ExposeLookups is set: the node then filters
// nothing and only annotates. The emitter rejects a node with neither.
//
// The trace id of an Input row is the column carrying [RoleTraceID] in
// Input's row type.
type TraceLookupFilter struct {
	Input         Node
	Lookups       []Node
	Predicate     Expr
	ExposeLookups bool
}

func (*TraceLookupFilter) planNode() {}

// Children returns Input followed by every lookup relation, in order.
func (f *TraceLookupFilter) Children() []Node {
	return append([]Node{f.Input}, f.Lookups...)
}

func (f *TraceLookupFilter) Equal(other Node) bool {
	o, ok := other.(*TraceLookupFilter)
	if !ok || len(f.Lookups) != len(o.Lookups) || f.ExposeLookups != o.ExposeLookups ||
		(f.Predicate == nil) != (o.Predicate == nil) {
		return false
	}
	if f.Predicate != nil && !f.Predicate.Equal(o.Predicate) {
		return false
	}
	for i := range f.Lookups {
		if !f.Lookups[i].Equal(o.Lookups[i]) {
			return false
		}
	}
	return f.Input.Equal(o.Input)
}

// LookupValueColumns returns the value columns of every lookup relation, in
// Lookups order (LookupValueColumnsOf each). ok is false when any lookup's
// columns are unknown.
func (f *TraceLookupFilter) LookupValueColumns() (columns []Column, ok bool) {
	for _, lookup := range f.Lookups {
		values, ok := LookupValueColumnsOf(lookup)
		if !ok {
			return nil, false
		}
		columns = append(columns, values...)
	}
	return columns, true
}

// LookupValueColumnsOf returns one lookup relation's value columns: its
// row-type columns other than TraceLookupKeyColumn. ok is false for a nil
// lookup and for one whose row type is open, which cannot say which columns
// it provides.
func LookupValueColumnsOf(lookup Node) (columns []Column, ok bool) {
	if lookup == nil {
		return nil, false
	}
	rowType := lookup.RowType()
	if rowType.Open {
		return nil, false
	}
	for _, c := range rowType.Columns {
		if c.Name != TraceLookupKeyColumn {
			columns = append(columns, c)
		}
	}
	return columns, true
}
