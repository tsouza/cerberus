package testsql

import (
	"slices"
	"strings"
)

// chdbEOFSentinel is the spurious "empty row" error chdb-go's parquet
// driver returns instead of io.EOF at end-of-iteration (see chdb-go
// v1.11.0's chdb/driver/parquet.go: `return fmt.Errorf("empty row")`).
// It surfaces on rows.Err(), where every caller must ignore it.
const chdbEOFSentinel = "empty row"

// TolerantRowsErr swallows the [chdbEOFSentinel] end-of-iteration error
// so a chDB-backed `rows.Err()` reads the way a database/sql caller
// expects. Any other error is real and is returned unchanged.
func TolerantRowsErr(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), chdbEOFSentinel) {
		return nil
	}
	return err
}

// mapColumnNames is the conservative allow-list of OTel-CH Map column
// names the rewriter wraps in `toJSONString(...)` before a query is
// issued against chDB. There is no type information at this layer, so
// the wrap is a textual transform keyed off this list; extend it when a
// new Map column joins the schema or a new alias is projected for one.
//
// The list is the UNION of the two allow-lists this package replaced,
// because each entry names a column that is Map-typed in the OTel-CH
// schema on EVERY lane — a lane that never projects one simply never
// matches it, which costs nothing:
//
//   - Attributes / ResourceAttributes / ScopeAttributes / SpanAttributes /
//     LogAttributes are the OTel-CH source columns themselves.
//   - ResourceAttrs is the shortened alias the TXTAR fixtures' simplified
//     seed DDL declares for a resource map.
//   - ExemplarAttributes is the alias chsql.EmitQueryExemplars projects
//     for `Exemplars.FilteredAttributes`, a
//     Map(LowCardinality(String),String).
//   - labels / log_attributes / stream_labels are the aliases
//     loki.buildSeriesSQL, buildDetectedLabelsSQL, buildIndexVolumeSQL and
//     buildDetectedFieldsSQL project for `ResourceAttributes` /
//     `LogAttributes`. They are deliberately spelled differently from the
//     source column so the toJSONString wrap cannot shadow the raw map a
//     WHERE / GROUP BY predicate references — ClickHouse resolves those
//     identifiers against SELECT aliases before FROM columns.
//   - logfmt_fields / json_fields are the `| logfmt` / `| json` parser-stage
//     extractions loki.buildDetectedFieldsSQL projects alongside the peek
//     row — Map(String,String) like every other entry here.
var mapColumnNames = []string{
	"Attributes",
	"ExemplarAttributes",
	"LogAttributes",
	"ResourceAttributes",
	"ResourceAttrs",
	"ScopeAttributes",
	"SpanAttributes",
	"json_fields",
	"labels",
	"log_attributes",
	"logfmt_fields",
	"stream_labels",
}

// IsMapColumn reports whether name — a projection alias, already stripped
// of its backticks — is one of the known OTel-CH Map columns.
func IsMapColumn(name string) bool {
	return slices.Contains(mapColumnNames, name)
}

// NestMapOrderBy guards the Map-wrap output against an outer ORDER BY
// clause that still references a raw Map column — either subscripted
// (the `sort_by_label` / `sort_by_label_desc` lowering's `SELECT * FROM
// (<sub>) ORDER BY ` + "`Attributes`" + `['<label>']`) or passed whole
// to a function (the route-A streaming-matrix ordering
// engine.rangeSeriesOrderer wires up — see prom.lang.RangeSeriesOrder —
// which emits `SELECT * FROM (<sub>) ORDER BY mapSort(` +
// "`Attributes`" + `), TimeUnix`). After
// [ExpandStarProjection] + [RewriteMapProjections] rewrite the OUTER
// projection so the Map column is emitted as `toJSONString(Attributes)
// AS Attributes`, ANY reference to `Attributes` in the ORDER BY —
// subscripted or not — binds to that String-typed SELECT alias
// (ClickHouse resolves ORDER BY identifiers to SELECT aliases ahead of
// the source column), so a map subscript fails with `arrayElement …
// got 'String'` and mapSort(...) fails the same way on a String
// argument. Production never hits this — the live query path has no
// toJSONString wrap, so `SELECT * … ORDER BY mapSort(Attributes)` (or
// `Attributes[k]`) keeps `Attributes` a Map. The collision is purely an
// artefact of the test harness's parquet-Map workaround.
//
// This runs AFTER the wrap passes and pushes the ORDER BY one level
// below the wrapped projection: rewrite
//
//	SELECT <…>, toJSONString(Attributes) AS Attributes, <…>
//	  FROM (<sub>) ORDER BY `Attributes`['h']
//
// into
//
//	SELECT <…>, toJSONString(Attributes) AS Attributes, <…>
//	  FROM (SELECT * FROM (<sub>) ORDER BY `Attributes`['h'])
//
// The inner subquery sorts against the still-raw Map; the outer
// wrapped projection produces the wire shape. ClickHouse preserves the
// inner ORDER BY's row order through the outer projection (no outer
// ORDER BY / GROUP BY reshuffles it), so the pinned `expected_rows:`
// ordering survives.
//
// The transform is conservative: it fires only when the query is a
// `SELECT <projs> FROM (<single subquery>) ORDER BY …` (no WITH head)
// whose ORDER BY references a known Map column (subscripted or bare),
// and the FROM clause is exactly one parenthesised subquery. Every
// other shape passes through untouched.
func NestMapOrderBy(query string) string {
	q := strings.TrimSpace(query)
	head, tail := splitOuterSelect(q)
	if head == "" {
		return query
	}
	// Depth-track the FROM clause's own parenthesised subquery so an
	// ORDER BY NESTED inside it (the bottomk/topk-over-subquery shape's
	// own `ORDER BY Value LIMIT 1 BY anchor_ts`, or any other inner
	// sort) can never be mistaken for the OUTER query's trailing ORDER
	// BY — a naive `strings.Index(tail, " ORDER BY ")` finds whichever
	// occurs FIRST in the text, which is the inner one whenever the
	// subquery itself sorts, and — now that [orderByReferencesMapColumn]
	// matches a bare column name rather than only a `[`-subscript — the
	// (wrong) inner match's unbounded "rest of the query" tail routinely
	// contains an unrelated `` `Attributes` `` reference (a GROUP BY, a
	// SELECT list, …) further out, corrupting an otherwise-fine query.
	// Matching parens first, then requiring "ORDER BY" to immediately
	// follow the close paren, pins the search to the query's own
	// top-level clause.
	fromBody, trailer, ok := splitParenthesisedFrom(tail)
	if !ok {
		return query
	}
	upperTrailer := strings.ToUpper(trailer)
	if !strings.HasPrefix(upperTrailer, "ORDER BY ") {
		return query
	}
	orderBy := trailer[len("ORDER BY "):]
	if !orderByReferencesMapColumn(orderBy) {
		return query
	}
	return "SELECT " + head + " FROM (SELECT * FROM " + fromBody + " ORDER BY " + orderBy + ")"
}

// splitParenthesisedFrom expects tail in the shape ` FROM (<subquery>)<rest>`
// — the tail [splitOuterSelect] returns — and depth-tracks the parens to
// find the subquery's OWN matching close paren, however many ORDER BY /
// nested-subquery clauses live inside it. Returns the parenthesised
// subquery verbatim (INCLUDING its own parens) as fromBody, and whatever
// follows the matching close paren, TrimSpace'd, as trailer. ok is false
// when tail does not start with ` FROM (` or the parens never balance —
// the FROM clause is not a single bare subquery, the shape [NestMapOrderBy]
// requires.
func splitParenthesisedFrom(tail string) (fromBody, trailer string, ok bool) {
	const fromKeyword = " FROM "
	if !strings.HasPrefix(tail, fromKeyword+"(") {
		return "", "", false
	}
	body := tail[len(fromKeyword):] // starts at the opening '('
	depth := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return body[:i+1], strings.TrimSpace(body[i+1:]), true
			}
		}
	}
	return "", "", false
}

// orderByReferencesMapColumn reports whether an ORDER BY clause body
// references a known Map column by its backtick-quoted name — whether
// subscripted (e.g. "`Attributes`['handler'] DESC", the sort_by_label
// shape) or passed to a function (e.g. "mapSort(`Attributes`),
// `TimeUnix`", the route-A streaming-matrix ordering shape). Used by
// [NestMapOrderBy] to detect either collision shape: both bind the Map
// column's backtick-quoted identifier verbatim into the ORDER BY text,
// so a plain substring match on the quoted name catches every syntactic
// position it can appear in without needing a real SQL parser.
func orderByReferencesMapColumn(orderBy string) bool {
	for _, name := range mapColumnNames {
		if strings.Contains(orderBy, "`"+name+"`") {
			return true
		}
	}
	return false
}

// ExpandStarProjection rewrites a top-level `SELECT * FROM (SELECT
// <projs> FROM ...) ...` into `SELECT <alias-list> FROM (SELECT
// <projs> FROM ...) ...` so the subsequent [RewriteMapProjections]
// pass can wrap Map-typed columns in `toJSONString(...)`. cerberus's
// emitter sometimes hoists a star projection over a fully-aliased
// inner SELECT (e.g. the `Filter ... Project ...` lowering shape of
// `<scalar> < metric`); without expansion, the outer `*` carries the
// inner Map column through unwrapped and chdb-go's parquet driver
// panics with `could not cast to type: MAP`.
//
// The transform is conservative: it fires only when the outer
// projection is exactly `*` or a single qualified star `<t>.*` (the
// spanset-intersect shape `SELECT L.* FROM (<left>) AS L INNER JOIN
// (<right>) AS R ON …`, whose columns come from the FIRST subquery —
// the same one this function already borrows from), and the inner
// subquery starts with `SELECT ` (case-insensitive). Anything else
// passes through. tableCols is the fixture's own seed DDL (see
// [SeedTableColumns]) — a lower-cased table name -> declared column
// names in CREATE TABLE order — the fallback catalog [expandQualifiedStar]
// consults when the inner relation is a bare table scan with no
// projection list of its own to borrow from (#1431). May be nil, in
// which case that shape still bails as before. The inner subquery's
// projections are re-rendered as
// their aliases (preferring explicit `AS <alias>` over the implicit
// form), re-qualified with the star's own table alias so the JOIN
// shape stays unambiguous, which lets the outer SELECT name the
// columns and the Map-wrap pass do its work without touching the inner
// shape.
func ExpandStarProjection(query string, tableCols map[string][]string) string {
	// A `WITH <cte> AS (...) SELECT …` head (the vector-set-op CSE CTE,
	// or the structural-join WITH RECURSIVE closure) precedes the outer
	// SELECT — peel it so the projection split sees the real outer
	// SELECT, then re-prepend it on the rewritten result. The CTE
	// bodies keep their raw Map columns (consumed server-side); only
	// the outer projection needs the toJSONString wrap.
	withHead, body := stripWithHead(query)
	if withHead != "" {
		return withHead + expandStarProjectionWithCTEs(body, withHead, tableCols)
	}
	return expandStarProjectionWithCTEs(query, "", tableCols)
}

// expandStarProjectionWithCTEs is [ExpandStarProjection] with the
// enclosing `WITH <cte> AS (...)` definitions available for name
// discovery. The Tempo-compatible `&&` shape (chsql.intersectQuery)
// emits `WITH l AS (...), r AS (...) SELECT * FROM ((SELECT * FROM l)
// UNION ALL (SELECT * FROM r)) …`, so the outer star's columns are not
// borrowable from the inner FROM directly — the inner is a parenthesised
// UNION whose branches reference CTEs by bare identifier. Name discovery
// therefore peels the union to its first branch and, when that branch is
// `SELECT * FROM <cte-ident>`, resolves the identifier back to its CTE
// body in `withHead`. Everything else is delegated unchanged.
func expandStarProjectionWithCTEs(query, withHead string, tableCols map[string][]string) string {
	head, tail := splitOuterSelect(query)
	if head == "" {
		return query
	}
	star := strings.TrimSpace(head)
	if star != "*" {
		return expandQualifiedStar(query, tableCols)
	}
	rest := strings.TrimSpace(strings.TrimPrefix(tail, " FROM "))
	if !strings.HasPrefix(rest, "(") {
		return expandQualifiedStar(query, tableCols)
	}
	depth, end := 0, -1
	for i := 0; i < len(rest) && end < 0; i++ {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
	}
	if end < 0 {
		return expandQualifiedStar(query, tableCols)
	}
	inner := strings.TrimSpace(rest[1:end])
	// Peel a parenthesised UNION down to its leading branch, then
	// resolve a bare-CTE-reference branch (`SELECT * FROM <ident>`)
	// against the WITH head so the column names come from the CTE body.
	branch := peelUnionPrefix(inner)
	names := cteBranchAliases(branch, withHead, tableCols)
	if len(names) == 0 {
		return expandQualifiedStar(query, tableCols)
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "`" + n + "`"
	}
	return "SELECT " + strings.Join(quoted, ", ") + tail
}

// cteBranchAliases returns the projected column aliases of a UNION
// branch of the form `SELECT * FROM <cte-ident>` by looking the CTE up
// in withHead and borrowing its body's projection aliases. Returns nil
// for any shape it cannot canonically enumerate, so the caller falls
// back to the conservative [ExpandStarProjection] path.
func cteBranchAliases(branch, withHead string, tableCols map[string][]string) []string {
	bHead, bTail := splitOuterSelect(branch)
	if strings.TrimSpace(bHead) != "*" {
		return nil
	}
	ident := strings.TrimSpace(strings.TrimPrefix(bTail, " FROM "))
	// The FROM target must be a single bare identifier (the CTE name),
	// not a subquery or a further expression.
	if ident == "" || strings.ContainsAny(ident, " ,()`*") {
		return nil
	}
	body := cteBody(withHead, ident)
	if body == "" {
		return nil
	}
	// The CTE body is itself a `SELECT * FROM (SELECT <aliased projs>…)`
	// shape with no further WITH head; reuse the CTE-unaware star
	// expander for name discovery only.
	expanded := expandQualifiedStar(body, tableCols)
	eHead, _ := splitOuterSelect(expanded)
	if eHead == "" || strings.TrimSpace(eHead) == "*" {
		return nil
	}
	var names []string
	for _, p := range splitProjections(eHead) {
		expr, alias := splitAlias(p)
		if alias == "" {
			alias = mapColAlias(strings.TrimSpace(expr))
		}
		if alias == "" || alias == "*" || strings.ContainsAny(alias, "()`") {
			return nil
		}
		names = append(names, alias)
	}
	return names
}

// cteBody returns the parenthesised body of the named CTE from a
// `WITH a AS (...), b AS (...)` head, or "" if not found. The name match
// is anchored on the `<name> AS (` token at depth 0 within the head.
func cteBody(withHead, name string) string {
	needle := name + " AS ("
	idx := strings.Index(withHead, needle)
	if idx < 0 {
		return ""
	}
	open := idx + len(needle) - 1 // points at the '('
	depth, end := 0, -1
	for i := open; i < len(withHead) && end < 0; i++ {
		switch withHead[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
	}
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(withHead[open+1 : end])
}

// expandQualifiedStar is the CTE-unaware star expander: it rewrites a
// top-level `SELECT * FROM (SELECT <projs> …) …` (or the qualified
// `SELECT L.* FROM (<left>) AS L …` JOIN shape) into an explicit alias
// list borrowed from the inner subquery's projections, so
// [RewriteMapProjections] can wrap Map columns. It is the fallback path
// [expandStarProjectionWithCTEs] delegates to whenever the outer FROM is
// a borrowable subquery rather than a CTE-referencing UNION.
func expandQualifiedStar(query string, tableCols map[string][]string) string {
	head, tail := splitOuterSelect(query)
	if head == "" {
		return query
	}
	// `qual` is the star's table qualifier ("" for a bare `*`, "L" for
	// the intersect shape's `L.*`); it is re-attached to every borrowed
	// alias so the expanded projection stays unambiguous across the JOIN.
	star := strings.TrimSpace(head)
	qual := ""
	if star != "*" {
		base, ok := strings.CutSuffix(star, ".*")
		// A qualifier must be a single bare identifier — anything with a
		// space, comma, paren, backtick or further star is a projection
		// list or an expression, not the star shape this pass expands.
		if !ok || base == "" || strings.ContainsAny(base, " ,()`*") {
			return query
		}
		qual = base + "."
	}
	// `tail` starts with " FROM "; the next non-space token should be
	// `(` opening an inner subquery whose projection list we can
	// borrow. Bail out otherwise.
	rest := strings.TrimSpace(strings.TrimPrefix(tail, " FROM "))
	if !strings.HasPrefix(rest, "(") {
		return query
	}
	// Find the matching `)` for the subquery.
	depth := 0
	end := -1
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return query
	}
	inner := strings.TrimSpace(rest[1:end])
	// The inner subquery may itself project a star over a further
	// subquery (the spanset shape nests `SELECT * FROM (<aggregate>)
	// WHERE …` inside the JOIN arm). Expand it recursively for NAME
	// DISCOVERY only — the rewritten inner is never spliced back, tail
	// keeps the original subquery verbatim.
	inner = ExpandStarProjection(inner, tableCols)
	innerHead, innerTail := splitOuterSelect(inner)
	if innerHead == "" {
		return query
	}
	// The inner relation may itself be a BARE TABLE SCAN (`SELECT *
	// FROM <table> ...`, no subquery) rather than a subquery with its
	// own projection list to borrow from — exactly the shape
	// emitSearchTraceLimit's drain produces (`SELECT s.* FROM (SELECT *
	// FROM otel_traces ...) AS s ...`). The recursive ExpandStarProjection
	// call above cannot rewrite that inner star (there is no nested
	// subquery to name-discover against), so it comes back unchanged and
	// innerHead is still the literal "*". Resolve the table's own column
	// list from the fixture's seed DDL instead of bailing (#1431).
	if strings.TrimSpace(innerHead) == "*" {
		if names, ok := bareTableColumns(innerTail, tableCols); ok {
			aliases := make([]string, len(names))
			for i, n := range names {
				aliases[i] = qual + "`" + n + "`"
			}
			return "SELECT " + strings.Join(aliases, ", ") + tail
		}
		return query
	}
	innerProjs := splitProjections(innerHead)
	aliases := make([]string, 0, len(innerProjs))
	for _, p := range innerProjs {
		expr, alias := splitAlias(p)
		if alias == "" {
			alias = mapColAlias(strings.TrimSpace(expr))
		}
		// Bail when the inner projection is itself a star, a
		// function call, or anything else that doesn't reduce to a
		// stable column name. Returning the original query keeps
		// the existing Map-panic failure mode for shapes the
		// rewriter cannot canonically enumerate.
		if alias == "" || alias == "*" || strings.ContainsAny(alias, "()`") {
			return query
		}
		aliases = append(aliases, qual+"`"+alias+"`")
	}
	return "SELECT " + strings.Join(aliases, ", ") + tail
}

// bareTableColumns resolves the column list for a bare `SELECT * FROM
// <table> ...` inner relation — no subquery, so there is no projection
// list [expandQualifiedStar] can borrow names from — by looking <table>
// up in tableCols, the fixture's own seed DDL parsed by
// [SeedTableColumns]. Returns (nil, false) when tableCols is empty, the
// FROM target isn't a single bare (optionally backtick-quoted)
// identifier, or that identifier has no known column list — any of
// which sends the caller back to the original conservative bail.
func bareTableColumns(innerTail string, tableCols map[string][]string) ([]string, bool) {
	if len(tableCols) == 0 {
		return nil, false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(innerTail, " FROM "))
	table := firstToken(rest)
	table = strings.TrimSuffix(strings.TrimPrefix(table, "`"), "`")
	if table == "" {
		return nil, false
	}
	names, ok := tableCols[strings.ToLower(table)]
	if !ok || len(names) == 0 {
		return nil, false
	}
	return names, true
}

// SeedTableColumns parses a fixture's `-- seed --` script and returns a
// map from lower-cased table name to its declared column names, in
// CREATE TABLE order. It is the authoritative catalog
// [expandQualifiedStar] falls back to (via [bareTableColumns]) when the
// star's inner relation is a bare table scan rather than a subquery
// with a borrowable projection list — the DDL is the only place those
// names exist (#1431). Statements other than a `CREATE [OR REPLACE |
// TEMPORARY] TABLE [IF NOT EXISTS] <name> (...)` are ignored; a table
// with zero recognisable column definitions is omitted rather than
// mapped to an empty slice, so a lookup miss and an empty declaration
// both read as "unknown" to callers.
//
// MATERIALIZED and ALIAS columns are excluded from the list: a plain
// `SELECT *` never projects them (that is ClickHouse's own semantics,
// not a rewriter choice), so including one would hand
// [expandQualifiedStar] a column name the real query never returns —
// exactly the shape that broke the spanset_pipeline_intersect fixture
// (its MATERIALIZED SpanAttributes) during development of this fix.
func SeedTableColumns(seed string) map[string][]string {
	cols := map[string][]string{}
	for _, stmt := range SplitStatements(seed) {
		trimmed := stripLeadingNoise(stmt)
		rest, ok := createTableTail(trimmed)
		if !ok {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(firstToken(rest)))
		open := strings.IndexByte(stmt, '(')
		if open < 0 {
			continue
		}
		closeParen := matchParen(stmt, open)
		if closeParen < 0 {
			continue
		}
		defs := splitTopLevelCommas(stmt[open+1 : closeParen])
		names := make([]string, 0, len(defs))
		for _, d := range defs {
			d = strings.TrimSpace(d)
			cn := firstToken(d)
			if cn == "" || isGeneratedColumnDef(d) {
				continue
			}
			names = append(names, cn)
		}
		if len(names) > 0 {
			cols[name] = names
		}
	}
	return cols
}

// isGeneratedColumnDef reports whether a CREATE TABLE column definition
// declares a MATERIALIZED or ALIAS column — the two ClickHouse column
// kinds a bare `SELECT *` omits from its result set (DEFAULT columns,
// by contrast, ARE included). Matched the same way
// normalizeTracesColumnDef locates `MATERIALIZED`: a space-delimited
// token search, which is safe here because both keywords only ever
// appear as the modifier between a column's type and its generating
// expression, never as (part of) a bare identifier or type name.
func isGeneratedColumnDef(def string) bool {
	upper := " " + strings.ToUpper(def) + " "
	return strings.Contains(upper, " MATERIALIZED ") || strings.Contains(upper, " ALIAS ")
}

// RewriteMapProjections wraps every top-level SELECT projection whose
// alias names a known Map column (see [IsMapColumn]) in
// `toJSONString(...)`. Only the OUTERMOST SELECT is touched — subqueries
// and CTE bodies keep their Map columns raw, because ClickHouse consumes
// those server-side and never hands them to the driver.
//
// It exists because chdb-go's parquet driver cannot decode a `Map` cell
// into a Go string: an unwrapped Map column surfaces as NULL against a
// string scan destination ("converting NULL to string is unsupported") or
// panics outright ("could not cast to type: MAP"). Wrapping the
// projection makes the cell a JSON string the caller decodes back into a
// map, so the failure mode disappears without changing which rows the
// query returns.
//
// Recognised projection shapes:
//
//	`Attributes`                       → toJSONString(`Attributes`) AS `Attributes`
//	<expr> AS `Attributes`             → toJSONString(<expr>) AS `Attributes`
//	`Attributes` AS `Attributes`       → toJSONString(`Attributes`) AS `Attributes`
//	L.`Attributes`                     → toJSONString(L.`Attributes`) AS `Attributes`
//
// and, recursively, four statement shapes that hide the outer SELECT:
//
//   - a leading `WITH <cte-chain> ` head (the vector-set-op CSE CTE, the
//     structural-join `WITH RECURSIVE` closure, the emitter's
//     single-evaluation trace-scope binding) — peeled, its SELECT
//     rewritten, the head re-prepended verbatim;
//   - `SELECT * FROM (<plan>)`, the wrapper the emitter renders around a
//     plan needing an outer binding — the star forwards the inner names
//     and order unchanged, so rewriting one level down produces exactly
//     the same outer wire shape;
//   - `<arm> UNION ALL <arm> …`, the shape the fan-in metadata /series
//     path (internal/api/prom/metadata.go) renders — each arm's outer
//     SELECT is rewritten independently and the arms re-joined;
//   - a parenthesised branch list under any UNION glue
//     (`(SELECT …) UNION DISTINCT (SELECT …) …`), the n-way `||`
//     set-operation shape — each branch is rewritten in place.
//
// Anything else passes through untouched, which keeps the unwrapped-Map
// failure loud rather than silently mis-rewriting a shape this pass
// cannot canonically enumerate.
func RewriteMapProjections(query string) string {
	if head, body := stripWithHead(query); head != "" {
		return head + RewriteMapProjections(body)
	}
	if inner, ok := starOverSubquery(query); ok {
		return "SELECT * FROM (" + RewriteMapProjections(inner) + ")"
	}
	if arms, ok := splitTopLevelUnionAll(query); ok {
		for i, a := range arms {
			arms[i] = RewriteMapProjections(a)
		}
		return strings.Join(arms, " UNION ALL ")
	}
	head, tail := splitOuterSelect(query)
	if head == "" {
		// A UNION-ALL arm arrives wrapped in its own parens.
		if inner, ok := stripOuterParens(query); ok {
			return "(" + RewriteMapProjections(inner) + ")"
		}
		// Any other parenthesised-branch UNION glue (`UNION DISTINCT`).
		if rewritten, ok := rewriteUnionMapProjections(query); ok {
			return rewritten
		}
		return query
	}
	projs := splitProjections(head)
	for i, p := range projs {
		expr, alias := splitAlias(p)
		if alias == "" {
			alias = mapColAlias(strings.TrimSpace(expr))
		}
		if !IsMapColumn(alias) {
			continue
		}
		projs[i] = "toJSONString(" + expr + ") AS `" + alias + "`"
	}
	return "SELECT " + strings.Join(projs, ", ") + tail
}

// rewriteUnionMapProjections walks a top-level UNION query
// (`(SELECT ...) UNION DISTINCT (SELECT ...) UNION DISTINCT (...) ...`)
// and rewrites Map columns inside each parenthesised branch. Returns
// (rewritten, true) on success, ("", false) when the shape doesn't
// match the expected union form. Branches that don't parse as
// `SELECT ... FROM ...` are left alone.
func rewriteUnionMapProjections(query string) (string, bool) {
	query = strings.TrimSpace(query)
	if !strings.HasPrefix(query, "(") {
		return "", false
	}
	var out strings.Builder
	rewrote := false
	i := 0
	for i < len(query) {
		// Skip whitespace + UNION glue between branches.
		for i < len(query) && (query[i] == ' ' || query[i] == '\n' || query[i] == '\t' || query[i] == '\r') {
			out.WriteByte(query[i])
			i++
		}
		if i >= len(query) {
			break
		}
		if query[i] == '(' {
			// Find the matching `)` at depth 0.
			depth := 0
			end := -1
			for j := i; j < len(query); j++ {
				switch query[j] {
				case '(':
					depth++
				case ')':
					depth--
					if depth == 0 {
						end = j
					}
				}
				if end >= 0 {
					break
				}
			}
			if end < 0 {
				return "", false
			}
			inner := query[i+1 : end]
			rewrittenInner := RewriteMapProjections(strings.TrimSpace(inner))
			if rewrittenInner != strings.TrimSpace(inner) {
				rewrote = true
			}
			out.WriteByte('(')
			out.WriteString(rewrittenInner)
			out.WriteByte(')')
			i = end + 1
			continue
		}
		// Non-paren token (UNION DISTINCT, UNION ALL, etc.) — copy through.
		for i < len(query) && query[i] != '(' {
			out.WriteByte(query[i])
			i++
		}
	}
	if !rewrote {
		return "", false
	}
	return out.String(), true
}

// mapColAlias derives the implicit projection alias for a bare column
// reference. Handles both `\`Col\“ (unqualified) and `Q.\`Col\“
// (qualifier-prefixed, e.g. the `L.\`Attributes\“ form vector_join
// emits) so the surrounding Map-rewrite pass can recognise Attributes
// projected through the join's left / right side.
func mapColAlias(s string) string {
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		s = s[i+1:]
	}
	return unquoteBackticks(s)
}

// stripWithHead peels a leading `WITH <cte-chain> ` off query, returning
// (head, body) where head is the verbatim prefix up to — and excluding —
// the outer SELECT, and body is that SELECT. Returns ("", "") when query
// does not begin with `WITH ` (case-insensitive), so a bare SELECT falls
// through to the single-SELECT path.
//
// The outer SELECT is the first `SELECT ` keyword reached at paren depth
// 0: every CTE body is parenthesised, whether it is a relational
// `WITH x AS (SELECT …)`, a `WITH RECURSIVE x AS (…)`, or a scalar
// `WITH (SELECT …) AS x`, so their own SELECTs sit deeper and are
// skipped. Quoted regions are shielded so a `SELECT ` inside a string
// literal cannot be mistaken for the outer one.
func stripWithHead(query string) (head, body string) {
	if !strings.HasPrefix(strings.ToUpper(query), "WITH ") {
		return "", ""
	}
	const sel = "SELECT "
	depth := 0
	inStr := byte(0)
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case inStr != 0:
			if c == inStr {
				inStr = 0
			}
			continue
		case c == '\'' || c == '`':
			inStr = c
			continue
		case c == '(':
			depth++
			continue
		case c == ')':
			depth--
			continue
		}
		if depth != 0 || i == 0 || i+len(sel) > len(query) {
			continue
		}
		// Standalone keyword only — a `SELECT` that continues an identifier
		// is not the outer one.
		if !strings.EqualFold(query[i:i+len(sel)], sel) || !isSQLBreak(query[i-1]) {
			continue
		}
		return query[:i], query[i:]
	}
	return "", ""
}

// isSQLBreak reports whether c can precede a standalone SQL keyword.
func isSQLBreak(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ')'
}

// splitOuterSelect returns the (projection-list, rest) split of a
// `SELECT <projs> FROM ...` query. If the query doesn't start with
// SELECT or the FROM is missing at depth 0, returns ("", "").
func splitOuterSelect(query string) (head, tail string) {
	upper := strings.ToUpper(query)
	if !strings.HasPrefix(upper, "SELECT ") {
		return "", ""
	}
	rest := query[len("SELECT "):]
	depth := 0
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && i+6 <= len(rest) && strings.EqualFold(rest[i:i+6], " FROM ") {
			return rest[:i], rest[i:]
		}
	}
	return "", ""
}

// peelUnionPrefix strips leading `(...)` wrappers from a UNION-shaped
// query so the inner SELECT becomes visible. It handles the recursive
// `((SELECT ...) UNION DISTINCT (SELECT ...)) UNION DISTINCT (SELECT ...)`
// shape that cerberus emits for n-way `||` set operations. Used only by
// ProjectionCount so we can count the leading branch's columns;
// the RewriteMapProjections pass still operates on the unmodified query
// because the Map columns survive the union without being projected at
// the outer level (each branch already projects them).
func peelUnionPrefix(query string) string {
	query = strings.TrimSpace(query)
	for strings.HasPrefix(query, "(") {
		// Find the matching `)` at depth 0.
		depth := 0
		end := -1
		for i := 0; i < len(query); i++ {
			switch query[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					end = i
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			return query
		}
		// `(<inner>) <maybe UNION...>` — descend into <inner> if it
		// starts with SELECT (or another paren) at the head.
		inner := strings.TrimSpace(query[1:end])
		innerUpper := strings.ToUpper(inner)
		if strings.HasPrefix(innerUpper, "SELECT ") || strings.HasPrefix(inner, "(") {
			query = inner
			continue
		}
		break
	}
	return query
}

// splitProjections splits a projection list on depth-0 commas.
// Quoted strings (single-quotes, backticks) shield commas. The
// returned slices have leading/trailing whitespace trimmed.
func splitProjections(s string) []string {
	var (
		out   []string
		buf   strings.Builder
		depth int
		inStr byte
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr != 0:
			if c == inStr {
				inStr = 0
			}
			buf.WriteByte(c)
		case c == '\'' || c == '`':
			inStr = c
			buf.WriteByte(c)
		case c == '(':
			depth++
			buf.WriteByte(c)
		case c == ')':
			depth--
			buf.WriteByte(c)
		case c == ',' && depth == 0:
			out = append(out, strings.TrimSpace(buf.String()))
			buf.Reset()
		default:
			buf.WriteByte(c)
		}
	}
	if buf.Len() > 0 {
		out = append(out, strings.TrimSpace(buf.String()))
	}
	return out
}

// splitAlias separates `<expr> AS \`alias\“ into (expr, alias). When
// no AS clause is present returns (s, "").
func splitAlias(s string) (expr, alias string) {
	// Find the last depth-0 " AS " (case-insensitive). Backtick-
	// quoted "AS" is shielded.
	depth := 0
	inStr := byte(0)
	lower := strings.ToLower(s)
	for i := 0; i+4 <= len(s); i++ {
		c := s[i]
		switch {
		case inStr != 0:
			if c == inStr {
				inStr = 0
			}
		case c == '\'' || c == '`':
			inStr = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		}
		if depth == 0 && inStr == 0 && lower[i:i+4] == " as " {
			alias = strings.TrimSpace(s[i+4:])
			alias = unquoteBackticks(alias)
			return strings.TrimSpace(s[:i]), alias
		}
	}
	return s, ""
}

func unquoteBackticks(s string) string {
	if len(s) >= 2 && s[0] == '`' && s[len(s)-1] == '`' {
		return s[1 : len(s)-1]
	}
	return s
}

// ProjectionCount counts top-level SELECT projections by
// re-splitting the outer SELECT's projection list on depth-0 commas.
// Used to size the scan-target slice without calling
// rows.ColumnTypes() (which panics on Map columns per the chDB probe).
//
// Returns 0 when the outer projection list contains a `*` wildcard
// (bare `*`, `R.*`, etc.) — the caller falls back to `rows.Columns()`
// to size the destination slice once the query has executed. Wildcard
// projections appear in structural-join lowerings (`SELECT R.* FROM
// ...`) where the fixture seed schema determines the actual column
// count.
//
// For top-level UNION queries (`(SELECT ...) UNION DISTINCT (SELECT ...)`),
// the function peels the outer paren / UNION wrappers down to the first
// branch's SELECT — every UNION branch shares the same projection shape
// by construction so any branch's count is authoritative.
func ProjectionCount(query string) int {
	// Peel a leading `WITH <cte> AS (...)` head so the column count is
	// read off the real outer SELECT, not the (absent) WITH-prefixed
	// one. Without this the WITH-shaped vector-set-op CSE SQL falls to
	// the wildcard (count 0 → rows.Columns()) path.
	if _, body := stripWithHead(query); body != "" {
		query = body
	}
	head, _ := splitOuterSelect(peelUnionPrefix(query))
	if head == "" {
		return 0
	}
	projs := splitProjections(head)
	for _, p := range projs {
		if isWildcardProjection(p) {
			return 0
		}
	}
	return len(projs)
}

// isWildcardProjection reports whether p is a `*`, `<qualifier>.*`, or
// `<qualifier>.* EXCEPT (...)` projection. The qualifier may be a
// bare identifier or a backtick-quoted alias. The `EXCEPT` variant
// surfaces in the structural-join emitter's projection list (which
// pairs explicit join-key aliases with `R.* EXCEPT (TraceId, ...)`
// to keep all non-key columns flowing through without duplicating
// the keys); the runner can't know the post-EXCEPT column count at
// parse time, so the caller falls back to `rows.Columns()` for sizing.
func isWildcardProjection(p string) bool {
	p = strings.TrimSpace(p)
	if p == "*" {
		return true
	}
	// `<qualifier>.* EXCEPT (...)` — wildcard with an exclusion list.
	// We strip a trailing parenthesised `EXCEPT (...)` clause (case-
	// insensitive) before checking the bare-wildcard suffix.
	upper := strings.ToUpper(p)
	if idx := strings.LastIndex(upper, " EXCEPT "); idx >= 0 {
		p = strings.TrimSpace(p[:idx])
	}
	if i := strings.LastIndex(p, "."); i >= 0 {
		return strings.TrimSpace(p[i+1:]) == "*"
	}
	return false
}

// starOverSubquery matches `SELECT * FROM (<subquery>)` — the wrapper the
// chsql emitter renders around a plan whose gates reference a binding
// declared on the outermost statement. Returns the subquery body and
// ok=true only when the projection is exactly `*` AND the parenthesised
// FROM operand runs to the end of the statement, so a star over a join, a
// star followed by ORDER BY / LIMIT, or a star over a bare table all fall
// through untouched rather than being rewritten on a guess.
func starOverSubquery(query string) (inner string, ok bool) {
	head, tail := splitOuterSelect(query)
	if strings.TrimSpace(head) != "*" {
		return "", false
	}
	return stripOuterParens(tail[len(" FROM "):])
}

// splitTopLevelUnionAll splits a `<arm> UNION ALL <arm> …` statement on
// its depth-0 ` UNION ALL ` separators, returning the arms verbatim (each
// typically a parenthesised `(SELECT …)`). Returns ok=false when no
// depth-0 ` UNION ALL ` is present, so a plain single SELECT falls through
// to the single-SELECT rewrite. Single-quoted strings and backtick
// identifiers shield any ` UNION ALL ` substring inside literals.
func splitTopLevelUnionAll(query string) (arms []string, ok bool) {
	const sep = " UNION ALL "
	var (
		out   []string
		start int
		depth int
		inStr byte
	)
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case inStr != 0:
			if c == inStr {
				inStr = 0
			}
		case c == '\'' || c == '`':
			inStr = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		}
		if depth == 0 && inStr == 0 && i+len(sep) <= len(query) &&
			strings.EqualFold(query[i:i+len(sep)], sep) {
			out = append(out, strings.TrimSpace(query[start:i]))
			i += len(sep) - 1
			start = i + 1
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	out = append(out, strings.TrimSpace(query[start:]))
	return out, true
}

// stripOuterParens returns the contents of a fully-parenthesised
// expression — `(<inner>)` → `<inner>` — when the leading `(` matches the
// trailing `)` at depth 0 (i.e. the whole string is one parenthesised
// group). Returns ok=false otherwise, so a query that merely contains
// parens (but isn't wholly wrapped) falls through untouched. Quote-aware
// so a literal `)` inside a string doesn't close the group early.
func stripOuterParens(s string) (inner string, ok bool) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '(' || s[len(s)-1] != ')' {
		return "", false
	}
	depth := 0
	inStr := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr != 0:
			if c == inStr {
				inStr = 0
			}
		case c == '\'' || c == '`':
			inStr = c
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 && i != len(s)-1 {
				// The opening paren closed before the end — the string is
				// not a single wrapped group (e.g. `(a) UNION ALL (b)`).
				return "", false
			}
		}
	}
	if depth != 0 {
		return "", false
	}
	return strings.TrimSpace(s[1 : len(s)-1]), true
}
