package chplan

// UnionAll concatenates the row streams of two or more sibling subtrees
// without deduplication. Lowers to a `(SELECT …) UNION ALL (SELECT …)
// [UNION ALL (SELECT …)]` chain — every arm renders as a parenthesised
// SELECT so the union remains binding-safe regardless of arm shape.
//
// Used by the PromQL classic-histogram companion-suffix routing where a
// `<base>_count` / `<base>_sum` reference can resolve against EITHER:
//
//   - the OTel-CH histogram row (a single row written under the bare
//     `<base>` name with `Count` / `Sum` columns), the convention the
//     OTel exporter uses; OR
//   - the OTel-CH sum row (a row written under the suffixed
//     `<base>_count` / `<base>_sum` name), the shape hostmetrics /
//     sqlquery emitters produce for counts that aren't actually
//     histogram companions (`system_cpu_logical_count`,
//     `system_processes_count`, `system_filesystem_inodes_count`, …).
//
// The two arms produce non-overlapping rows by construction — each
// scan-side MetricName filter only admits rows from one physical layout —
// so UNION ALL is correct: no row appears in both arms, and a `DISTINCT`
// would be a wasteful no-op. The two physical tables have different
// column shapes (histogram lacks `Value`; sum lacks `Count`), so the
// existing single-`merge()` UnionTables path can't fan them; each arm
// owns its own Project that synthesises the canonical Sample-row
// quadruple (MetricName, Attributes, TimeUnix, Value).
//
// Every Inputs element MUST project the same output column shape
// — the chsql emitter relies on positional column matching across
// UNION ALL arms (ClickHouse's behaviour). The PromQL lowering
// guarantees this by feeding each arm through the same canonical
// Sample-row Project.
type UnionAll struct {
	// Inputs lists the per-arm subtrees in stable left-to-right order.
	// Empty / single-arm UnionAlls are an invariant violation — the
	// emitter rejects them so the lowering cannot accidentally produce
	// a degenerate union.
	Inputs []Node
}

func (*UnionAll) planNode() {}

// Children returns the per-arm subtrees in stable order. Matches the
// Node interface's depth-first visitor contract.
func (u *UnionAll) Children() []Node {
	out := make([]Node, len(u.Inputs))
	copy(out, u.Inputs)
	return out
}

// Equal reports structural equality with another UnionAll. Order is
// significant: `(A) UNION ALL (B)` and `(B) UNION ALL (A)` produce the
// same multiset, but the emitted SQL byte-stream differs, so the
// equality is positional to match the canonical fixture-comparison
// semantics every other plan node uses.
func (u *UnionAll) Equal(other Node) bool {
	o, ok := other.(*UnionAll)
	if !ok {
		return false
	}
	if len(u.Inputs) != len(o.Inputs) {
		return false
	}
	for i := range u.Inputs {
		if !u.Inputs[i].Equal(o.Inputs[i]) {
			return false
		}
	}
	return true
}
