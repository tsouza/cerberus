package chsql

import (
	"fmt"
	"strconv"

	"github.com/tsouza/cerberus/internal/chplan"
)

// nestedSetPathElemHalf is the zero-padded decimal width of each half of
// a DFS-path element: 20 digits of the span's start timestamp in
// nanoseconds, and 20 digits of sipHash64(SpanId).
const nestedSetPathElemHalf = 20

// nestedSetPathElemWidth is the byte width of one DFS-path element: the
// two nestedSetPathElemHalf halves concatenated. Fixed-width elements
// make plain string comparison of concatenated paths equivalent to
// lexicographic tuple comparison, and length(path) / width recovers the
// span's depth.
const nestedSetPathElemWidth = 2 * nestedSetPathElemHalf

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
//	    SELECT `TraceId`, `SpanId`, `ParentSpanId`, <elem> AS `_path`,
//	           0 AS `_depth`
//	      FROM `otel_traces`
//	     WHERE `ParentSpanId` = '' AND `TraceId` IN <trace-scope>
//	           -- <trace-scope> = traceScopeFrag(input): a cheap
//	           -- superset of the input's trace ids, NOT a re-render
//	           -- of <input> (see traceScopeFrag)
//	    UNION ALL
//	    SELECT t.`TraceId`, t.`SpanId`, t.`ParentSpanId`,
//	           concat(c.`_path`, <elem(t)>), c.`_depth` + 1
//	      FROM `otel_traces` AS t
//	      INNER JOIN `_cerberus_ns_paths` AS c
//	        ON t.`TraceId` = c.`TraceId` AND t.`ParentSpanId` = c.`SpanId`
//	     WHERE c.`_depth` < <cap>
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
	// When the search returns only the N newest traces, bound the numbering
	// walk to exactly those N: rank the root spans by Timestamp (descending,
	// ties by TraceId ascending — the order /api/search's TruncateSummaries
	// keeps) and keep the top N. The lowering only sets TraceLimit when the
	// plan guarantees each returned trace's root is in the result, so this
	// root-Timestamp ranking equals the result-min-Timestamp ranking the
	// handler applies — the kept traces are numbered byte-identically; the
	// excess is dropped.
	//
	// In the bounded case the numbering scope is the SELF-CONTAINED top-N
	// frag (boundedRootScopeFrag), NOT traceScopeFrag(n.Input): the same
	// lowering that sets TraceLimit also pushes a BoundedTraceScope gate into
	// every leaf of n.Input (the structural-union row source), so deriving the
	// scope from n.Input here would (a) re-render the gate it embeds — an
	// emit cycle — and (b) be redundant. The leaf gate emits this exact same
	// frag, so the numbering and the row source see a byte-identical trace
	// set, which is load-bearing: a mismatch would strand kept rows at the
	// 0/0/0 LEFT-JOIN default. Under the gating precondition the dropped
	// `IN traceScopeFrag(input)` conjunct was a no-op anyway (the bare-root
	// union arm puts every root trace in that scope).
	var scope Frag
	if n.TraceLimit > 0 {
		scope = boundedRootScopeFrag(n.SpansTable, n.TraceIDColumn, n.ParentSpanIDColumn, n.TimestampColumn, n.TraceLimit, n.WindowStartNano, n.WindowEndNano)
	} else {
		scope, err = e.traceScopeFrag(n.Input, n.TraceIDColumn)
		if err != nil {
			return err
		}
	}

	numbering := buildNestedSetNumbering(n, scope)

	aliasedNS := func(nsCol, outCol string) Frag {
		// The `ns` qualifier is backtick-quoted (Qual/QualIdent), matching
		// the legacy emitter; the bare-qualifier qualColFrag is only for
		// the L / R / m single-letter aliases elsewhere.
		return As(Call("ifNull", Qual("ns", nsCol), InlineLit(0)), outCol)
	}
	onClause := And(
		spanIDPairFrag("m", n.TraceIDColumn, "ns", n.TraceIDColumn),
		spanIDPairFrag("m", n.SpanIDColumn, "ns", n.SpanIDColumn),
	)

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
		// One fixed-width element = zero-padded ns-timestamp ++
		// zero-padded sipHash64(SpanId). Each half is nestedSetPathElemHalf
		// digits; the two halves concatenate to nestedSetPathElemWidth.
		padded := func(inner Frag) Frag {
			return Call("leftPad", Call("toString", inner), InlineLit(nestedSetPathElemHalf), InlineLit("0"))
		}
		return Call(
			"concat",
			padded(Call("toUnixTimestamp64Nano", optQualColFrag(qual, n.TimestampColumn))),
			padded(Call("sipHash64", optQualColFrag(qual, n.SpanIDColumn))),
		)
	}

	// Anchor: the root spans (ParentSpanId = '') of every trace the
	// input touches. The trace-id IN scope keeps the walk off
	// unrelated traces; the walk itself is NOT time-bounded (Tempo
	// numbers whole traces at ingest, search window notwithstanding).
	// `_depth` seeds at 0 and increments one per child level; the
	// recursive step bounds it (`c._depth < <cap>`) so a span-id cycle
	// degrades to a partial numbering instead of erroring with CH code
	// 306 (see structuralDepthBoundFrag / defaultStructuralRecursionDepth).
	anchor := NewQuery().
		Select(
			Col(n.TraceIDColumn),
			Col(n.SpanIDColumn),
			Col(n.ParentSpanIDColumn),
			As(pathElem(""), "_path"),
			verbatim("0 AS `_depth`"),
		).
		From(Col(n.SpansTable)).
		Where(
			Eq(Col(n.ParentSpanIDColumn), InlineLit("")),
			// scope already carries its own parens; InSubquery adds none,
			// giving `<TraceId> IN (SELECT …)` with a single paren pair.
			InSubquery(Col(n.TraceIDColumn), scope),
		)

	// Recursive step: append one path element per child level and carry
	// `_depth + 1`. The `c._depth < <cap>` bound (shared with the
	// structural-join recursion via structuralDepthBoundFrag) keeps a
	// cyclic parent chain (ParentSpanId looping back into its own
	// ancestry — clock skew / instrumentation bug / OTLP span-id reuse)
	// from running the CTE past CH's max_recursive_cte_evaluation_depth
	// and erroring 306. For an acyclic trace shallower than the cap the
	// walk still terminates at the natural fixpoint (no span left whose
	// ParentSpanId matches a numbered SpanId), so the bound is invisible
	// — the numbering stays byte-identical to the pre-cap output.
	step := NewQuery().
		Select(
			qualColFrag("t", n.TraceIDColumn),
			qualColFrag("t", n.SpanIDColumn),
			qualColFrag("t", n.ParentSpanIDColumn),
			// `c.\`_path\`` is the recursive CTE's synthetic path column,
			// referenced bare-qualified (alias `c`, backtick-quoted
			// `_path`) — emitter-pinned, so verbatim; concat composes.
			Call("concat", verbatim("c.`_path`"), pathElem("t")),
			verbatim("c.`_depth` + 1 AS `_depth`"),
		).
		From(aliasedFrag(Col(n.SpansTable), "t")).
		Join(
			InnerJoin,
			aliasedFrag(verbatim("`_cerberus_ns_paths`"), "c"),
			And(
				spanIDPairFrag("t", n.TraceIDColumn, "c", n.TraceIDColumn),
				Eq(qualColFrag("t", n.ParentSpanIDColumn), qualColFrag("c", n.SpanIDColumn)),
			),
		).
		Where(structuralDepthBoundFrag(0))

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
	// `(SELECT <traceIDCol> FROM <sub>)` — the leaf-scope exact form.
	return NewQuery().Select(Col(traceIDCol)).From(sub).Frag(), nil
}

// boundedRootScopeFrag renders the N newest root-bearing traces directly from
// the spans table — the set /api/search returns when it keeps the top N:
//
//	(SELECT `TraceId`
//	   FROM `otel_traces`
//	  WHERE `ParentSpanId` = ''
//	  GROUP BY `TraceId`
//	  ORDER BY min(`Timestamp`) DESC, `TraceId` ASC
//	  LIMIT <limit>)
//
// `min(Timestamp)` over a trace's ROOT spans is the root's start time, and
// the DESC / TraceId-ASC order matches sortSummariesStartDesc. This
// APPROXIMATES the set TruncateSummaries keeps, which ranks by
// StartTimeUnixNano = min(Timestamp) over ALL matched result rows (root +
// matched descendants; toTraceSummaries). The two ranking keys coincide when
// the root is each trace's earliest matched span — true absent intra-trace
// clock skew — so for the common case the kept set is exactly
// TruncateSummaries'. Under skew (a matched descendant timestamped before its
// root) the N-th boundary trace may differ; that residual is accepted to keep
// the gate cheap — ranking by the true result-min would require materialising
// the full result, i.e. the OOM this bound exists to prevent. The GROUP BY +
// LIMIT over the narrow root scan keeps bounded memory (a partial top-N sort),
// and the recursive closures it gates only ever process those N traces.
//
// It is SELF-CONTAINED — no `TraceId IN <input scope>` conjunct — for two
// reasons. (1) It is referenced from two places that must agree byte-for-byte:
// the NestedSetAnnotate numbering anchor scope AND every leaf-scan gate
// (chplan.BoundedTraceScope) pushed into the row source; a self-contained frag
// makes those references identical by construction, so numbering and row
// source see the same trace set with no chance of drift stranding rows at the
// 0/0/0 LEFT-JOIN default. (2) Referencing the input scope from a gate pushed
// INTO that input's leaves would be an emit cycle. Dropping the conjunct is a
// no-op under the gating precondition (inputGuaranteesRootInResult): the
// bare-root union arm puts every root-bearing trace in the input scope, so
// `ParentSpanId=” AND TraceId IN <input scope>` selected exactly the same
// roots as `ParentSpanId=”` alone. Determinism (the `TraceId ASC` tie-break
// makes the order total) is load-bearing: both references are evaluated
// independently by ClickHouse and must yield the same N-boundary. QueryBuilder.Frag
// already parenthesises the SELECT, so each `TraceId IN (...)` stays a single
// subquery.
// startNano / endNano (when non-zero) restrict the ranking to roots whose start
// time falls in the request window, so the result is the newest-N roots IN the
// window rather than the newest-N ever — without this a historical-window
// search gates the row source to globally-newest roots outside the window and
// returns nothing (#1109 GAP-3 / structure-tab rank-in-window). The window
// conjuncts mirror tsBound on the traceql side (`Timestamp >=/<=
// fromUnixTimestamp64Nano(nano)`), and because both call sites pass the SAME
// nanos the numbering scope and leaf gate stay byte-identical.
func boundedRootScopeFrag(spansTable, traceIDCol, parentCol, tsCol string, limit, startNano, endNano int64) Frag {
	where := []Frag{Eq(Col(parentCol), InlineLit(""))}
	if startNano != 0 {
		where = append(where, Gte(Col(tsCol), Call("fromUnixTimestamp64Nano", InlineLit(startNano))))
	}
	if endNano != 0 {
		where = append(where, Lte(Col(tsCol), Call("fromUnixTimestamp64Nano", InlineLit(endNano))))
	}
	return NewQuery().
		Select(Col(traceIDCol)).
		From(Col(spansTable)).
		Where(where...).
		GroupBy(Col(traceIDCol)).
		OrderBy(Call("min", Col(tsCol)), true).
		OrderBy(Col(traceIDCol), false).
		Limit(limit).
		Frag()
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
	return Paren(UnionAll(l, r)), nil
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

// optQualColFrag is the Frag form of writeOptQualCol: `<qual>.<col>`
// (both backtick-quoted) when qual is non-empty, bare backtick-quoted
// `<col>` otherwise.
func optQualColFrag(qual, col string) Frag {
	if qual == "" {
		return Col(col)
	}
	return Qual(qual, col)
}

// quoteIdent backtick-quotes a column name for use inside verbatim
// fragments (the QueryBuilder slots normally do this via Ident).
func quoteIdent(name string) string {
	b := &Builder{}
	b.Ident(name)
	sql, _ := b.Build()
	return sql
}
