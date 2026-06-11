package chsql

import (
	"fmt"
	"strconv"

	"github.com/tsouza/cerberus/internal/chplan"
)

// nestedSetPathElemWidth is the byte width of one DFS-path element:
// 20 zero-padded decimal digits of the span's start timestamp in
// nanoseconds + 20 zero-padded decimal digits of sipHash64(SpanId).
// Fixed-width elements make plain string comparison of concatenated
// paths equivalent to lexicographic tuple comparison, and
// length(path) / width recovers the span's depth.
const nestedSetPathElemWidth = 40

// emitNestedSetAnnotate renders a chplan.NestedSetAnnotate: the input
// rows LEFT JOINed against a per-trace nested-set numbering computed
// on the fly from the span table's (TraceId, SpanId, ParentSpanId)
// adjacency.
//
// Semantics contract — matches reference Tempo's ingest-time
// assignNestedSetModelBoundsAndServiceStats
// (tempodb/encoding/vparquet4/nested_set_model.go) exactly, except for
// sibling order (see below):
//
//   - DFS interval numbering per trace, counter starting at 1; entering
//     a span assigns its left bound, leaving assigns its right bound.
//   - nestedSetParent = the parent span's LEFT bound ("the left bound
//     of the parent serves as numeric span ID" — upstream comment);
//     root spans carry -1.
//   - The counter continues across multiple roots in the same trace.
//   - Spans not reachable from any root (broken traces) and traces
//     with no root at all stay 0/0/0 — exactly the zero values Tempo
//     returns for unnumbered spans (LEFT JOIN miss → Int64 default 0,
//     made explicit with ifNull for join_use_nulls=1 deployments).
//
// Sibling order: Tempo numbers siblings in ingest order, which the
// OTel-CH schema does not record. Cerberus orders siblings by
// (Timestamp, sipHash64(SpanId)) — deterministic, start-time-first —
// which produces a valid nested-set numbering of the same tree; only
// the relative positions of same-parent siblings can differ from
// reference, never the containment relations consumers (Grafana
// Traces Drilldown's structure tab) derive from the bounds.
//
// One divergence is deliberate: traces carrying >1 span with the same
// SpanId (Zipkin shared-span pairs) get a best-effort numbering here,
// whereas Tempo either pairs them by span kind (exactly 2 copies) or
// zeroes the whole trace (3+). The OTel-CH exporter never produces
// duplicate SpanIds within a trace, so the case is unreachable on the
// schemas cerberus targets.
//
// Rendered shape:
//
//	SELECT m.*,
//	       ifNull(ns.`_ns_left`, 0)   AS `__cerberus_ns_left`,
//	       ifNull(ns.`_ns_right`, 0)  AS `__cerberus_ns_right`,
//	       ifNull(ns.`_ns_parent`, 0) AS `__cerberus_ns_parent`
//	FROM (<input>) AS m
//	LEFT JOIN (
//	  WITH RECURSIVE `_cerberus_ns_paths` AS (
//	    SELECT `TraceId`, `SpanId`, `ParentSpanId`, <elem> AS `_path`
//	      FROM `otel_traces`
//	     WHERE `ParentSpanId` = '' AND `TraceId` IN <trace-scope>
//	           -- <trace-scope> = traceScopeFrag(input): a cheap
//	           -- superset of the input's trace ids, NOT a re-render
//	           -- of <input> (see traceScopeFrag)
//	    UNION ALL
//	    SELECT t.`TraceId`, t.`SpanId`, t.`ParentSpanId`, concat(c.`_path`, <elem(t)>)
//	      FROM `otel_traces` AS t
//	      INNER JOIN `_cerberus_ns_paths` AS c
//	        ON t.`TraceId` = c.`TraceId` AND t.`ParentSpanId` = c.`SpanId`
//	  )
//	  SELECT `TraceId`, `SpanId`,
//	         <rank of entry event>                       AS `_ns_left`,
//	         <rank of exit event>                        AS `_ns_right`,
//	         <−1 for roots, else parent's entry rank>    AS `_ns_parent`
//	  FROM (<per-span entry/exit/parent-lookup events, ranked by two
//	         window passes — see buildNestedSetNumbering>)
//	  GROUP BY `TraceId`, `SpanId`
//	) AS ns ON m.`TraceId` = ns.`TraceId` AND m.`SpanId` = ns.`SpanId`
//
// The numbering: each span contributes an entry event (key `_path`)
// and an exit event (key `_path` + '~'); lexicographic key order over
// the digit-only fixed-width paths is exactly the DFS entry/exit
// event order, so the per-trace running count of those events IS the
// nested-set position — left = rank(entry), right = rank(exit),
// parent = rank(parent's entry), materialising Tempo's two-pointer
// walk as an event sequence (linear in span count; see
// buildNestedSetNumbering for the full derivation and why join-free /
// single-CTE-reference / non-quadratic are each load-bearing).
//
// The <input> subquery is rendered exactly ONCE (the FROM arm). The
// trace-id scope of the CTE anchor deliberately does NOT re-render it:
// ClickHouse evaluates every reference to a subquery — including a
// WITH CTE, which 24.8 inlines per use site — independently, so a
// second rendering of an expensive input (structural joins carry
// their own recursive closures; mixed `||` unions dedup wide rows)
// doubles its evaluation and blew past the #756 per-query memory cap
// on the Traces Drilldown structure-tab query. Instead the anchor is
// scoped by traceScopeFrag: a cheap plan-derived SUPERSET of the
// input's trace ids (joins / unions decompose into their leaf scans).
// Extra traces in the scope only add rows to the numbering side that
// the LEFT JOIN never matches — output rows are identical, and the
// walk stays bounded by the leaf scans' matched traces.
func (e *emitter) emitNestedSetAnnotate(n *chplan.NestedSetAnnotate) error {
	if n.SpansTable == "" || n.TraceIDColumn == "" || n.SpanIDColumn == "" ||
		n.ParentSpanIDColumn == "" || n.TimestampColumn == "" {
		return fmt.Errorf("%w: NestedSetAnnotate column names unset", ErrUnsupported)
	}

	inputSub, err := e.subqueryFrag(n.Input)
	if err != nil {
		return err
	}
	scope, err := e.traceScopeFrag(n.Input, n.TraceIDColumn)
	if err != nil {
		return err
	}

	numbering := buildNestedSetNumbering(n, scope)

	aliasedNS := func(nsCol, outCol string) Frag {
		return func(b *Builder) {
			b.writeSQL("ifNull(")
			b.QualIdent("ns", nsCol)
			b.writeSQL(", 0) AS ")
			b.Ident(outCol)
		}
	}
	onClause := func(b *Builder) {
		spanIDPairFrag("m", n.TraceIDColumn, "ns", n.TraceIDColumn)(b)
		b.writeSQL(" AND ")
		spanIDPairFrag("m", n.SpanIDColumn, "ns", n.SpanIDColumn)(b)
	}

	sb := NewQuery().
		Select(
			verbatim("m.*"),
			aliasedNS("_ns_left", chplan.NestedSetLeftColumn),
			aliasedNS("_ns_right", chplan.NestedSetRightColumn),
			aliasedNS("_ns_parent", chplan.NestedSetParentColumn),
		).
		From(aliasedFrag(inputSub, "m")).
		Join(LeftJoin, aliasedFrag(numbering.Frag(), "ns"), onClause)
	e.emitSelect(sb)
	return nil
}

// buildNestedSetNumbering assembles the numbering subquery: the
// recursive path CTE plus the per-trace ARRAY JOIN that converts
// sorted DFS paths into (SpanId, left, right, parent) rows. scope is
// a parenthesised single-column SELECT (from traceScopeFrag) bounding
// which traces the walk numbers.
func buildNestedSetNumbering(n *chplan.NestedSetAnnotate, scope Frag) *QueryBuilder {
	// One fixed-width path element for the span row `qual` qualifies
	// (empty qual = bare column references in the anchor SELECT).
	pathElem := func(qual string) Frag {
		return func(b *Builder) {
			b.writeSQL("concat(leftPad(toString(toUnixTimestamp64Nano(")
			writeOptQualCol(b, qual, n.TimestampColumn)
			b.writeSQL(")), 20, '0'), leftPad(toString(sipHash64(")
			writeOptQualCol(b, qual, n.SpanIDColumn)
			b.writeSQL(")), 20, '0'))")
		}
	}

	// Anchor: the root spans (ParentSpanId = '') of every trace the
	// input touches. The trace-id IN scope keeps the walk off
	// unrelated traces; the walk itself is NOT time-bounded (Tempo
	// numbers whole traces at ingest, search window notwithstanding).
	anchor := NewQuery().
		Select(
			Col(n.TraceIDColumn),
			Col(n.SpanIDColumn),
			Col(n.ParentSpanIDColumn),
			As(pathElem(""), "_path"),
		).
		From(Col(n.SpansTable)).
		Where(func(b *Builder) {
			b.Ident(n.ParentSpanIDColumn)
			b.writeSQL(" = '' AND ")
			b.Ident(n.TraceIDColumn)
			b.writeSQL(" IN ")
			scope(b)
		})

	// Recursive step: append one path element per child level.
	step := NewQuery().
		Select(
			qualColFrag("t", n.TraceIDColumn),
			qualColFrag("t", n.SpanIDColumn),
			qualColFrag("t", n.ParentSpanIDColumn),
			func(b *Builder) {
				b.writeSQL("concat(c.`_path`, ")
				pathElem("t")(b)
				b.writeSQL(")")
			},
		).
		From(aliasedFrag(Col(n.SpansTable), "t")).
		Join(
			InnerJoin,
			aliasedFrag(verbatim("`_cerberus_ns_paths`"), "c"),
			func(b *Builder) {
				spanIDPairFrag("t", n.TraceIDColumn, "c", n.TraceIDColumn)(b)
				b.writeSQL(" AND ")
				writeSideCol(b, "t", n.ParentSpanIDColumn)
				b.writeSQL(" = ")
				writeSideCol(b, "c", n.SpanIDColumn)
			},
		)

	w := strconv.Itoa(nestedSetPathElemWidth)

	// The numbering is computed as the rank of each span's ENTRY and
	// EXIT event in the per-trace DFS event sequence — literally
	// Tempo's two-pointer walk, recovered from the sorted paths:
	//
	//   - entry event key  = `_path`
	//   - exit  event key  = concat(`_path`, '~')
	//
	// Paths are digit-only (zero-padded decimal), and '~' (0x7E) sorts
	// after every digit, so lexicographic order of the event keys IS
	// the DFS event order: a node's exit key sorts after every entry /
	// exit key of its subtree (longer digit prefix < '~') and before
	// its parent's exit and its later siblings' entries. The running
	// count of entry/exit events (window sum per trace) is therefore
	// exactly the nested-set position: rank(entry) = left, rank(exit)
	// = right.
	//
	// The parent's left bound rides the same pass: each non-root span
	// adds a PARENT-LOOKUP event keyed by its parent's path, ordered
	// just after that parent's entry event (`_etype` 1 between entry 0
	// and exit 2; lookup events do not advance the running count). A
	// second window (first_value over the (trace, key) partition)
	// hands every lookup event the entry rank of the node that owns
	// the key — i.e. the parent's left bound. Roots emit no lookup
	// event (the empty-key filter) and resolve to -1 in the final
	// aggregation.
	//
	// Shape rationale (all three constraints are load-bearing):
	//   - `_cerberus_ns_paths` is referenced exactly ONCE. CH 24.8
	//     re-evaluates a recursive CTE at every reference (verified:
	//     3 references = 3× read_rows), so any join-back / multi-read
	//     formulation re-runs the whole recursion.
	//   - No per-node scan of the trace's path list. Both the previous
	//     ARRAY JOIN shape (per-trace `_paths` array replicated onto
	//     every span row) and an arrayMap/arrayCount reformulation
	//     (captured `_paths` replicated per lambda element) are
	//     quadratic per trace; at compose-smoke scale (~150k
	//     self-telemetry spans) ArrayJoinTransform / FUNCTION arrayMap
	//     attempted single 1 GiB chunks and tripped the #756 cap.
	//   - Event rows are narrow (ids + key + type) and everything is
	//     a window sort + hash aggregation: linear in span count.
	events := NewQuery().
		Select(
			Col(n.TraceIDColumn),
			Col(n.SpanIDColumn),
			verbatim("`_ev`.1 AS `_ekey`"),
			verbatim("`_ev`.2 AS `_etype`"),
		).
		From(verbatim("`_cerberus_ns_paths` ARRAY JOIN arrayFilter(e -> NOT (e.2 = 1 AND e.1 = ''), [(`_path`, 0), (concat(`_path`, '~'), 2), (substring(`_path`, 1, length(`_path`) - " + w + "), 1)]) AS `_ev`"))

	ranked := NewQuery().
		Select(
			Col(n.TraceIDColumn),
			Col(n.SpanIDColumn),
			Col("_ekey"),
			Col("_etype"),
			verbatim("sum(`_etype` != 1) OVER (PARTITION BY "+quoteIdent(n.TraceIDColumn)+" ORDER BY `_ekey` ASC, `_etype` ASC ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS `_erank`"),
		).
		From(events.Frag())

	keyed := NewQuery().
		Select(
			Col(n.TraceIDColumn),
			Col(n.SpanIDColumn),
			Col("_etype"),
			Col("_erank"),
			verbatim("first_value(`_erank`) OVER (PARTITION BY "+quoteIdent(n.TraceIDColumn)+", `_ekey` ORDER BY `_etype` ASC ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS `_keyrank`"),
		).
		From(ranked.Frag())

	return NewQuery().
		WithRecursive("_cerberus_ns_paths", anchor, step).
		Select(
			Col(n.TraceIDColumn),
			Col(n.SpanIDColumn),
			verbatim("toInt64(maxIf(`_erank`, `_etype` = 0)) AS `_ns_left`"),
			verbatim("toInt64(maxIf(`_erank`, `_etype` = 2)) AS `_ns_right`"),
			verbatim("if(countIf(`_etype` = 1) = 0, toInt64(-1), toInt64(maxIf(`_keyrank`, `_etype` = 1))) AS `_ns_parent`"),
		).
		From(keyed.Frag()).
		GroupBy(Col(n.TraceIDColumn), Col(n.SpanIDColumn))
}

// traceScopeFrag derives the numbering walk's trace-id scope from the
// annotate input plan: a Frag rendering a parenthesised SELECT whose
// single column covers a SUPERSET of the trace ids in n's output —
// without re-evaluating the expensive interior of the plan.
//
// Superset is sufficient: the scope only bounds which traces the
// recursive walk numbers, and the outer LEFT JOIN discards numbering
// rows for traces the input never produced — the annotated output is
// row-for-row identical for any scope ⊇ traces(input). That freedom
// is what lets the derivation skip the input's expensive interior:
//
//   - SetOperation — every output row comes from one of the two arms,
//     so traces(out) ⊆ traces(L) ∪ traces(R): UNION ALL the arm scopes
//     (set-semantics IN makes duplicates free), skipping the
//     wide-row UNION DISTINCT / INTERSECT dedup.
//   - StructuralJoin — every emitted span row originates in one of the
//     two arms (which arm depends on the op: `>>` keeps R matches,
//     negated forms keep the non-matching side, union-prefixed forms
//     keep both), so traces(out) ⊆ traces(L) ∪ traces(R) for every op:
//     UNION ALL the arm scopes, skipping the recursive closure walk
//     entirely — the second closure evaluation is what tipped the
//     Drilldown structure-tab query past the per-query memory cap.
//   - Project that passes TraceIDColumn through bare — projection
//     never changes the trace set: recurse into the input.
//   - Limit — dropping the limit only widens the set: recurse.
//   - anything else (Filter over Scan is the leaf shape) — exact:
//     `(SELECT <tid> FROM <node>)`, same as the input re-render the
//     scope replaces, but on plans where it is just a pruned scan.
func (e *emitter) traceScopeFrag(n chplan.Node, traceIDCol string) (Frag, error) {
	switch t := n.(type) {
	case *chplan.SetOperation:
		return e.unionTraceScopeFrag(t.Left, t.Right, traceIDCol)
	case *chplan.StructuralJoin:
		return e.unionTraceScopeFrag(t.Left, t.Right, traceIDCol)
	case *chplan.Limit:
		return e.traceScopeFrag(t.Input, traceIDCol)
	case *chplan.Project:
		if projectsBareColumn(t, traceIDCol) {
			return e.traceScopeFrag(t.Input, traceIDCol)
		}
	}
	sub, err := e.subqueryFrag(n)
	if err != nil {
		return nil, err
	}
	return func(b *Builder) {
		b.writeSQL("(SELECT ")
		b.Ident(traceIDCol)
		b.writeSQL(" FROM ")
		sub(b)
		b.writeSQL(")")
	}, nil
}

// unionTraceScopeFrag renders `(<scope(left)> UNION ALL <scope(right)>)`
// — the union of the two arms' trace-id scopes.
func (e *emitter) unionTraceScopeFrag(left, right chplan.Node, traceIDCol string) (Frag, error) {
	l, err := e.traceScopeFrag(left, traceIDCol)
	if err != nil {
		return nil, err
	}
	r, err := e.traceScopeFrag(right, traceIDCol)
	if err != nil {
		return nil, err
	}
	return func(b *Builder) {
		b.writeSQL("(")
		l(b)
		b.writeSQL(" UNION ALL ")
		r(b)
		b.writeSQL(")")
	}, nil
}

// projectsBareColumn reports whether p projects col through unchanged:
// a bare unqualified ColumnRef of that name, unaliased or aliased to
// itself.
func projectsBareColumn(p *chplan.Project, col string) bool {
	for _, proj := range p.Projections {
		ref, ok := proj.Expr.(*chplan.ColumnRef)
		if !ok || ref.Name != col || ref.Qualifier != "" {
			continue
		}
		if proj.Alias == "" || proj.Alias == col {
			return true
		}
	}
	return false
}

// writeOptQualCol writes `<qual>.<col>` when qual is non-empty, bare
// `<col>` otherwise.
func writeOptQualCol(b *Builder, qual, col string) {
	if qual == "" {
		b.Ident(col)
		return
	}
	b.QualIdent(qual, col)
}

// quoteIdent backtick-quotes a column name for use inside verbatim
// fragments (the QueryBuilder slots normally do this via Ident).
func quoteIdent(name string) string {
	b := &Builder{}
	b.Ident(name)
	sql, _ := b.Build()
	return sql
}
