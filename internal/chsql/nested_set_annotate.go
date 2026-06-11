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
//	     WHERE `ParentSpanId` = '' AND `TraceId` IN (SELECT `TraceId` FROM (<input>))
//	    UNION ALL
//	    SELECT t.`TraceId`, t.`SpanId`, t.`ParentSpanId`, concat(c.`_path`, <elem(t)>)
//	      FROM `otel_traces` AS t
//	      INNER JOIN `_cerberus_ns_paths` AS c
//	        ON t.`TraceId` = c.`TraceId` AND t.`ParentSpanId` = c.`SpanId`
//	  )
//	  SELECT `TraceId`, `_node`.2 AS `SpanId`,
//	         <2·pre − depth>                          AS `_ns_left`,
//	         <left + 2·subtreeSize − 1>               AS `_ns_right`,
//	         <−1 for roots, else parent's left bound> AS `_ns_parent`
//	  FROM (SELECT `TraceId`,
//	               arraySort(groupArray((`_path`, `SpanId`, `ParentSpanId`))) AS `_nodes`,
//	               arrayMap(x -> x.1, `_nodes`) AS `_paths`
//	          FROM `_cerberus_ns_paths` GROUP BY `TraceId`)
//	  ARRAY JOIN `_nodes` AS `_node`, arrayEnumerate(`_nodes`) AS `_pre`
//	) AS ns ON m.`TraceId` = ns.`TraceId` AND m.`SpanId` = ns.`SpanId`
//
// The arithmetic: with `pre` the 1-based DFS pre-order rank inside the
// trace (paths sort lexicographically; fixed-width elements make the
// sort a true tree pre-order), `depth` the path element count, and
// `size` the subtree span count (path-prefix count),
//
//	left  = 2·pre − depth          (entries before it: pre−1; exits
//	                                before it: pre−1 − (depth−1))
//	right = left + 2·size − 1      (the subtree consumes 2·size slots)
//	parent.left = 2·parentPre − (depth−1), parentPre by path prefix
//
// which reproduces Tempo's two-pointer walk without materialising the
// event sequence.
//
// The <input> subquery is rendered twice (FROM arm + the trace-id
// scope of the CTE anchor). ClickHouse does not share subquery results
// across those two sites, so inputs that are themselves expensive
// (structural joins) are evaluated twice — correctness-first; the
// numbering walk itself is bounded by the spans of the matched traces.
func (e *emitter) emitNestedSetAnnotate(n *chplan.NestedSetAnnotate) error {
	if n.SpansTable == "" || n.TraceIDColumn == "" || n.SpanIDColumn == "" ||
		n.ParentSpanIDColumn == "" || n.TimestampColumn == "" {
		return fmt.Errorf("%w: NestedSetAnnotate column names unset", ErrUnsupported)
	}

	inputSub, err := e.subqueryFrag(n.Input)
	if err != nil {
		return err
	}

	numbering := buildNestedSetNumbering(n, inputSub)

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
// sorted DFS paths into (SpanId, left, right, parent) rows.
func buildNestedSetNumbering(n *chplan.NestedSetAnnotate, inputSub Frag) *QueryBuilder {
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
			b.writeSQL(" IN (SELECT ")
			b.Ident(n.TraceIDColumn)
			b.writeSQL(" FROM ")
			inputSub(b)
			b.writeSQL(")")
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
	// depth = path length in elements; recomputed inline at each use
	// site (CH disallows alias references inside sibling SELECT
	// expressions on some analyzer paths).
	depth := "intDiv(length(`_node`.1), " + w + ")"

	perTrace := NewQuery().
		Select(
			Col(n.TraceIDColumn),
			verbatim("arraySort(groupArray((`_path`, "+quoteIdent(n.SpanIDColumn)+", "+quoteIdent(n.ParentSpanIDColumn)+"))) AS `_nodes`"),
			verbatim("arrayMap(x -> x.1, `_nodes`) AS `_paths`"),
		).
		From(verbatim("`_cerberus_ns_paths`")).
		GroupBy(Col(n.TraceIDColumn))

	return NewQuery().
		WithRecursive("_cerberus_ns_paths", anchor, step).
		Select(
			Col(n.TraceIDColumn),
			func(b *Builder) {
				b.writeSQL("`_node`.2 AS ")
				b.Ident(n.SpanIDColumn)
			},
			verbatim("toInt64(2 * `_pre` - "+depth+") AS `_ns_left`"),
			verbatim("toInt64(2 * `_pre` - "+depth+" + 2 * arrayCount(p -> startsWith(p, `_node`.1), `_paths`) - 1) AS `_ns_right`"),
			verbatim("if(`_node`.3 = '', toInt64(-1), toInt64(2 * indexOf(`_paths`, substring(`_node`.1, 1, length(`_node`.1) - "+w+")) - "+depth+" + 1)) AS `_ns_parent`"),
		).
		From(func(b *Builder) {
			b.writeSQL("(")
			sql, args := perTrace.Build()
			b.writeSQL(sql)
			b.args = append(b.args, args...)
			b.writeSQL(") ARRAY JOIN `_nodes` AS `_node`, arrayEnumerate(`_nodes`) AS `_pre`")
		})
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
