package chplan

// StructuralOp identifies a TraceQL-style spanset relation.
//
//	StructuralChild     — `A > B`  : A is the direct parent of B  (return B rows)
//	StructuralDescendant — `A >> B`: A is an ancestor of B         (return B rows)
//	StructuralParent    — `A < B`  : A is the direct child of B   (return B rows)
//	StructuralAncestor  — `A << B` : A is a descendant of B        (return B rows)
//	StructuralSibling   — `A ~ B`  : A and B share the same parent (return B rows)
//
// The negated variants invert the predicate: `A !> B` returns B rows
// for which *no* span in A is the direct parent of B. The set of
// negated ops mirrors the positive set one-for-one:
//
//	StructuralNotChild      — `A !> B`  : no A is parent of B           (return B rows)
//	StructuralNotParent     — `A !< B`  : no A is child of B            (return B rows)
//	StructuralNotDescendant — `A !>> B` : no A is ancestor of B         (return B rows)
//	StructuralNotAncestor   — `A !<< B` : no A is descendant of B       (return B rows)
//	StructuralNotSibling    — `A !~ B`  : no A shares B's parent        (return B rows)
//
// The union variants return rows from *both* sides that participate
// in the relation — the matched A spans plus the matched B spans, all
// projected through the same column shape:
//
//	StructuralUnionChild      — `A &> B`
//	StructuralUnionParent     — `A &< B`
//	StructuralUnionDescendant — `A &>> B`
//	StructuralUnionAncestor   — `A &<< B`
//	StructuralUnionSibling    — `A &~ B`
//
// Direct parent-child (`>` / `<`) and sibling (`~`) emit as a single
// INNER JOIN on (TraceID, SpanID/ParentSpanID). Recursive forms
// (`>>` / `<<`) walk the parent chain via a CH `WITH RECURSIVE` CTE
// — see internal/chsql/structural_join.go for the emission strategy.
//
// Negated ops reuse the relation predicate but swap the outer join
// for a `LEFT ANTI JOIN` (direct case) or apply the closure with the
// same anti-join shape (recursive case). Union ops emit the positive
// relation twice — once projecting R.*, once projecting L.* — joined
// by `UNION DISTINCT`.
//
// Multi-hop chains (`a > b > c`) already fall out of the binary node
// shape: the lowering produces `StructuralJoin{Left: a, Right:
// StructuralJoin{Left: b, Right: c}}` by recursing into LHS/RHS
// SpansetOperation nodes.
type StructuralOp string

const (
	StructuralChild      StructuralOp = ">"
	StructuralParent     StructuralOp = "<"
	StructuralDescendant StructuralOp = ">>"
	StructuralAncestor   StructuralOp = "<<"
	StructuralSibling    StructuralOp = "~"

	StructuralNotChild      StructuralOp = "!>"
	StructuralNotParent     StructuralOp = "!<"
	StructuralNotDescendant StructuralOp = "!>>"
	StructuralNotAncestor   StructuralOp = "!<<"
	StructuralNotSibling    StructuralOp = "!~"

	StructuralUnionChild      StructuralOp = "&>"
	StructuralUnionParent     StructuralOp = "&<"
	StructuralUnionDescendant StructuralOp = "&>>"
	StructuralUnionAncestor   StructuralOp = "&<<"
	StructuralUnionSibling    StructuralOp = "&~"
)

// IsNegated reports whether op is one of the `!...` structural
// variants (the predicate is negated; emit-time picks LEFT ANTI JOIN).
func (op StructuralOp) IsNegated() bool {
	switch op {
	case StructuralNotChild, StructuralNotParent,
		StructuralNotDescendant, StructuralNotAncestor,
		StructuralNotSibling:
		return true
	}
	return false
}

// IsUnion reports whether op is one of the `&...` structural variants
// (return rows from both sides that participate in the relation).
func (op StructuralOp) IsUnion() bool {
	switch op {
	case StructuralUnionChild, StructuralUnionParent,
		StructuralUnionDescendant, StructuralUnionAncestor,
		StructuralUnionSibling:
		return true
	}
	return false
}

// Positive returns the base structural relation underlying op — i.e.
// strips the `!` or `&` prefix. The result is one of StructuralChild,
// StructuralParent, StructuralDescendant, StructuralAncestor, or
// StructuralSibling. For an already-positive op the input is returned
// unchanged. Used by the emitter so negated/union variants can share
// the predicate-shape helpers with their positive counterparts.
func (op StructuralOp) Positive() StructuralOp {
	switch op {
	case StructuralNotChild, StructuralUnionChild:
		return StructuralChild
	case StructuralNotParent, StructuralUnionParent:
		return StructuralParent
	case StructuralNotDescendant, StructuralUnionDescendant:
		return StructuralDescendant
	case StructuralNotAncestor, StructuralUnionAncestor:
		return StructuralAncestor
	case StructuralNotSibling, StructuralUnionSibling:
		return StructuralSibling
	}
	return op
}

// StructuralJoin produces the rows from `Right` whose spans satisfy the
// requested structural relation with a span in `Left`. Both sides
// produce span rows from otel_traces (or a derived projection thereof);
// the join key uses TraceID + (Span/Parent)ID columns named in the
// schema.
//
// MaxDepth bounds the parent-chain walk for recursive ops (`>>` / `<<`):
// a positive value caps the recursion at that many levels. 0 leaves the
// cap to the emitter, which applies a safety default
// (chsql.defaultStructuralRecursionDepth) — the recursive CTE is never
// emitted unbounded, so a span-id cycle degrades to a partial closure
// instead of erroring with CH code 306. For an acyclic trace shallower
// than the cap the walk still terminates at the natural fixpoint, so
// the cap is invisible in the common case. The optimizer may set this
// from a configured ceiling. For the direct ops (`>` / `<` / `~`) the
// field is ignored: those always emit a single-level INNER JOIN.
//
// ExtraProjectionColumns lists non-key columns the emitter should re-
// expose as bare-name aliases in the wrap subquery (in addition to the
// three join keys). The emitter renders each as `R.<col> AS <col>`
// (and likewise for L on union variants) so an outer consumer can
// reference them as either bare names or qualified through the wrap.
// CH 25.8's analyzer drops `R.*`-introduced columns from outer-scope
// resolution when the JOIN's L and R sides have colliding column
// names — `R.SpanName` and bare `SpanName` both fail to resolve
// against `R.* EXCEPT (...)` in a wrap subquery. Listing the columns
// here keeps them resolvable. Empty falls back to the legacy `R.*
// EXCEPT (TraceId, SpanId, ParentSpanId)` shape — used by tests that
// construct StructuralJoin directly without populating the field.
type StructuralJoin struct {
	Left, Right Node
	Op          StructuralOp

	TraceIDColumn      string
	SpanIDColumn       string
	ParentSpanIDColumn string

	ExtraProjectionColumns []string

	MaxDepth int

	// TimestampColumn names the spans Timestamp column the recursive
	// closure's step scan is partition-pruned on. It is set together with
	// WindowStartNano / WindowEndNano by the /api/search lowering window
	// stamp; left "" on every other path (metrics, spec/property
	// harnesses, direct-join tests), in which case the emitter renders the
	// step scan without a request-window predicate (byte-identical to the
	// pre-window output). When set, it also arms the emit-time fail-closed
	// guard (chsql.requireSpansScanWindow): a step scan that opted into
	// windowing but reaches emit with a zero window is rejected rather than
	// scanning full retention.
	TimestampColumn string

	// WindowStartNano / WindowEndNano (unix nanoseconds, 0 = unset) carry the
	// /api/search request window the recursive closure's step scan of the spans
	// table is bounded by, so ClickHouse can partition-prune it instead of
	// reading full retention to satisfy the inert `t.TraceId IN (<seed>)`
	// membership (the seed-id IN never prunes partitions). They are set
	// alongside TimestampColumn on the search path only; every other path leaves
	// them 0 and the step scan stays unwindowed. The window bounds which spans
	// the walk traverses — at the window edge a trace whose intermediate spans
	// fall outside [start,end] degrades to a partial closure, the same residual
	// approximation boundedRootScopeFrag already accepts to keep the gate cheap.
	WindowStartNano int64
	WindowEndNano   int64

	// TraceIDRestriction, when non-empty, confines every physical spans scan
	// of the closure (the anchor seed, the recursive step `t`, and the R
	// leaf) to this literal on-disk TraceId set so ClickHouse's idx_trace_id
	// bloom filter granule-prunes the wide fetch. It is set only by the
	// /api/search two-phase orchestrator between phase A (narrow rank -> the
	// top-N TraceIds) and phase B (wide fetch of just those traces); every
	// other path leaves it nil, so the emitted SQL — and thus the result — is
	// byte-identical off the two-phase path. The restriction is a pure
	// pruning bound: correctness comes from the closure, so restricting to a
	// subset of traces only shrinks reads, never changes the per-trace rows.
	TraceIDRestriction []string

	// CandidatePrefilter, when true, restricts the recursive anchor seed to
	// traces that appear on BOTH sides of the relation (the L-intersect-R
	// candidate set), so a sparse selective query walks the closure over only
	// the traces that can possibly match instead of every trace the ancestor
	// side seeds. It is set by lowering only when BOTH sides are cheap
	// selective Filter(Scan) leaves (see traceql.isCheapSelectiveLeaf); a
	// bare `{}` (dense) side or a derived/structural side (a left-associative
	// `A>>B>>C` chain) leaves it false so a dense query never regresses and a
	// chain never re-executes its inner closure. Semantics-preserving: a
	// trace with no R span can never produce a descendant/ancestor match.
	CandidatePrefilter bool
}

func (*StructuralJoin) planNode() {}

func (j *StructuralJoin) Children() []Node { return []Node{j.Left, j.Right} }

func (j *StructuralJoin) Equal(other Node) bool {
	o, ok := other.(*StructuralJoin)
	if !ok {
		return false
	}
	if j.Op != o.Op {
		return false
	}
	if j.TraceIDColumn != o.TraceIDColumn ||
		j.SpanIDColumn != o.SpanIDColumn ||
		j.ParentSpanIDColumn != o.ParentSpanIDColumn {
		return false
	}
	if j.MaxDepth != o.MaxDepth {
		return false
	}
	if j.TimestampColumn != o.TimestampColumn ||
		j.WindowStartNano != o.WindowStartNano ||
		j.WindowEndNano != o.WindowEndNano {
		return false
	}
	if j.CandidatePrefilter != o.CandidatePrefilter {
		return false
	}
	if len(j.ExtraProjectionColumns) != len(o.ExtraProjectionColumns) {
		return false
	}
	for i := range j.ExtraProjectionColumns {
		if j.ExtraProjectionColumns[i] != o.ExtraProjectionColumns[i] {
			return false
		}
	}
	if len(j.TraceIDRestriction) != len(o.TraceIDRestriction) {
		return false
	}
	for i := range j.TraceIDRestriction {
		if j.TraceIDRestriction[i] != o.TraceIDRestriction[i] {
			return false
		}
	}
	return j.Left.Equal(o.Left) && j.Right.Equal(o.Right)
}
