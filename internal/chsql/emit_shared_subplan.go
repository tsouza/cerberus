package chsql

import (
	"strconv"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Shared-subplan hoisting: a plan node rendered as a sub-statement more than
// once in one statement is declared ONCE as a non-recursive CTE on the
// outermost statement and every rendering site references it by name.
//
// A plan is a DAG, not a tree: a lowering hands one chplan.Node to several
// parents (a mixed histogram/float `or` feeds both the histogram and the
// float arm of a rate() above it, for example), and several emitters render
// one child more than once inside their own shape. Inlining each rendering
// multiplies the node's SQL text by the number of paths that reach it, and a
// nested composition multiplies those factors together, so a small query can
// render past ClickHouse's max_query_size (a syntax-level byte limit) purely
// from repeated text. A CTE reference is a few bytes however large the body.
//
// ClickHouse inlines a non-recursive relational CTE at each reference, so
// hoisting dedupes the emitted TEXT and never the work: every reference still
// executes the body, exactly as the inlined copy did, which is why a
// reference still carries the body's physical-scan count.
//
// Emission runs in up to two passes (EmitCounted):
//
//   - Pass 1 renders normally, caching each renderNode key's FIRST rendering
//     (sql/args/physicalScans). A key rendered a second time is only marked
//     repeated (a pass-2 hoist candidate) when the cached first rendering is
//     at least [minSharedSubplanHoistBytes] — see that constant's doc — and
//     the second-and-later renderings then splice in a short reference
//     instead of the body, so the pass-1 text stays linear in the plan and
//     the emitted-SQL byte bound still judges roughly the final size. A
//     repeated key below the floor instead replays its cached first
//     rendering verbatim at every occurrence: byte-identical to inlining it
//     again, without paying to re-render it. A plan with no key at or above
//     the floor keeps pass 1's output unchanged, and e.shared.repeated stays
//     empty — the signal emitBound reads to decide whether pass 2 (and its
//     wrapping `WITH` statement) runs at all.
//   - Pass 2 runs only when pass 1 found a repeat at or above the floor. The
//     first rendering of a repeated key renders its body (nested repeated
//     keys inside it are themselves hoisted first, so a later CTE may
//     reference an earlier one) and registers the CTE; every rendering
//     returns the reference.
//
// A key is the node's identity plus the one piece of mutable emitter state a
// rendering reads (spansTable, reassigned by the whole-trace emitters), so
// two renderings share a CTE only when they would have rendered the same
// text. The CTE-name counter is the other state a rendering consumes; it
// only names CTEs INSIDE the body, and one body declares each name once.
//
// # Why hoisting is also gated on the body's own size
//
// A repeated key is not always the pathological case this file exists for.
// rate()/increase()'s own window arms and SearchTraceLimit's two arms each
// render one small child (typically a bare Scan+Filter+Project, a few
// hundred bytes to a couple KB) more than once by design — that is normal,
// already-shipping SQL shape, not the byte-blowup #3743 fixes. Hoisting
// EVERY repeated key regardless of size would rewrite those small,
// already-correct statements into a CTE form too, churning the entire
// golden corpus for a rewrite that saves only a few hundred bytes each.
// [minSharedSubplanHoistBytes] keeps hoisting scoped to the shapes that
// actually risk the statement-size bound: a deeply-nested composition (the
// mixed histogram/float `or` feeding both arms of an outer subquery
// function, cerberus issue #3743) whose repeated body is tens of KB, not a
// few hundred bytes. A repeated key whose body renders below the floor is
// left exactly as it rendered before this file existed — inlined at every
// occurrence — so the existing golden corpus for small, ordinary repeats
// stays byte-identical.
const minSharedSubplanHoistBytes = 8192

// sharedSubplanKey identifies one rendering of a plan node.
type sharedSubplanKey struct {
	node       chplan.Node
	spansTable string
}

// sharedSubplanRef is the reference a hoisted node renders to: its CTE name
// and the physical scans one reference executes (the body's own).
type sharedSubplanRef struct {
	name          string
	physicalScans int
}

// sharedSubplanCTE is one hoisted body, in declaration order.
type sharedSubplanCTE struct {
	name string
	body PreRenderedSQL
}

// sharedSubplanRendering is one key's first rendering, cached in pass 1 so a
// later occurrence below the hoist floor can replay it instead of
// re-rendering, and so a later occurrence at or above it knows the body
// bytes without a second render.
type sharedSubplanRendering struct {
	sql           string
	args          []any
	physicalScans int
}

// sharedSubplans is the per-statement hoisting state, shared by pointer
// between an emitter and its sub-emitters because their text is spliced into
// the same statement.
type sharedSubplans struct {
	// hoist is nil during pass 1 and holds pass 1's repeated keys in pass 2.
	hoist map[sharedSubplanKey]bool
	// firstRender caches every key's first-occurrence rendering from pass 1.
	firstRender map[sharedSubplanKey]sharedSubplanRendering
	// repeated records every key pass 1 rendered more than once with a
	// first rendering at or above minSharedSubplanHoistBytes.
	repeated map[sharedSubplanKey]bool
	// declared maps a pass-2 hoisted key to its reference once its body is
	// registered.
	declared map[sharedSubplanKey]sharedSubplanRef
	// ctes lists the pass-2 hoisted bodies in declaration order.
	ctes []sharedSubplanCTE
}

func newSharedSubplans(hoist map[sharedSubplanKey]bool) *sharedSubplans {
	return &sharedSubplans{
		hoist:       hoist,
		firstRender: map[sharedSubplanKey]sharedSubplanRendering{},
		repeated:    map[sharedSubplanKey]bool{},
		declared:    map[sharedSubplanKey]sharedSubplanRef{},
	}
}

// sharedSubplanCTEPrefix names a hoisted CTE; the emitter's CTE counter
// suffixes it so it cannot collide with any other CTE in the statement.
const sharedSubplanCTEPrefix = "_shared_subplan_"

// sharedSubplanRefSQL renders the sub-statement a hoisted reference splices
// in place of the body: `SELECT * FROM <name>`.
func sharedSubplanRefSQL(name string) (string, []any, error) {
	return NewQuery().Select(Star()).From(BareIdent(name)).subquerySQL()
}

// renderShared is renderNode's hoisting front: it returns handled=false when
// n is rendered inline as before, and the reference rendering otherwise.
func (e *emitter) renderShared(n chplan.Node) (sql string, args []any, physicalScans int, handled bool, err error) {
	s := e.shared
	if s == nil {
		return "", nil, 0, false, nil
	}
	key := sharedSubplanKey{node: n, spansTable: e.spansTable}
	if s.hoist == nil {
		if cached, ok := s.firstRender[key]; ok {
			if len(cached.sql) >= minSharedSubplanHoistBytes {
				s.repeated[key] = true
				sql, args, err = sharedSubplanRefSQL(sharedSubplanCTEPrefix + strconv.Itoa(e.nextCTESeq()))
				return sql, args, 0, true, err
			}
			// Below the floor: replay the cached first rendering verbatim —
			// byte-identical to inlining it again, without re-rendering it.
			return cached.sql, cached.args, cached.physicalScans, true, nil
		}
		sql, args, physicalScans, renderErr := e.renderInline(n)
		if renderErr != nil {
			return "", nil, 0, true, renderErr
		}
		s.firstRender[key] = sharedSubplanRendering{sql: sql, args: args, physicalScans: physicalScans}
		return sql, args, physicalScans, true, nil
	}
	if !s.hoist[key] {
		return "", nil, 0, false, nil
	}
	ref, ok := s.declared[key]
	if !ok {
		bodySQL, bodyArgs, bodyScans, bodyErr := e.renderInline(n)
		if bodyErr != nil {
			return "", nil, 0, true, bodyErr
		}
		ref = sharedSubplanRef{name: sharedSubplanCTEPrefix + strconv.Itoa(e.nextCTESeq()), physicalScans: bodyScans}
		s.ctes = append(s.ctes, sharedSubplanCTE{
			name: ref.name,
			// The declaration executes nothing by itself — each reference
			// carries the body's scans — so it counts none.
			body: PreRenderedSQL{SQL: bodySQL, Args: bodyArgs},
		})
		s.declared[key] = ref
	}
	sql, args, err = sharedSubplanRefSQL(ref.name)
	return sql, args, ref.physicalScans, true, err
}

// withSharedSubplans declares every hoisted body on q, in registration
// order, so a body referencing an earlier hoisted CTE sees it declared first.
func (s *sharedSubplans) withSharedSubplans(q *QueryBuilder) *QueryBuilder {
	for _, c := range s.ctes {
		q.With(c.name, Subquery(c.body))
	}
	return q
}
