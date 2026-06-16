//go:build chdb

// Package spec — chDB-backed round-trip executor.
//
// This file is compiled only when the `chdb` build tag is set, which
// also implies the chdb-go driver and libchdb.so are present (see
// `just chdb-install`). The default `just test` lane stays
// CGO_ENABLED=0 and never sees this code.
//
// The executor opens an ephemeral in-process chDB session, runs the
// fixture's `seed:` DDL+INSERT statements, executes the emitted `sql:`
// (with `args:` bound), and asserts the resulting rows match the
// `expected_rows:` JSON. Map columns are wrapped server-side in
// `toJSONString(...)` to dodge the native parquet Map scan panic
// documented by the chDB driver probe.
package spec

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
)

// chdbEOFSentinel is the spurious end-of-iteration error chdb-go's
// parquet driver returns instead of io.EOF (see chdb-go v1.11.0's
// `parquet.go`: `return fmt.Errorf("empty row")`). It surfaces on
// rows.Err() and must be ignored — any other error is real.
const chdbEOFSentinel = "empty row"

// defaultNowAnchor is the deterministic eval instant every fixed-anchor
// round-trip fixture is seeded against. It mirrors the instant-eval
// anchor `internal/promql/lower_test.go` feeds into `LowerAt`
// (`time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)`), so each round-trip
// fixture sees the same wall-clock the lowering pass used to compute
// filter bounds. [nowAnchorLiteral] is `chNow64Literal(defaultNowAnchor)`
// by construction (asserted in TestNowAnchorLiteralMatchesDefault), so
// the fixed-anchor and per-eval substitution paths share one source of
// truth for the default instant.
var defaultNowAnchor = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// nowAnchorLiteral is the deterministic CH literal we splice in place
// of every `now64(...)` reference in the emitted SQL when no explicit
// per-evaluation anchor is supplied (the established fixed-anchor
// round-trip path). It is held as a package const — not computed from
// [defaultNowAnchor] at init — so its byte shape is pinned to exactly the
// value the goldens were generated against; the equality to
// `chNow64Literal(defaultNowAnchor)` is guarded by a test instead.
//
// The third arg (`'UTC'`) is mandatory: chDB's parser treats the
// timezone slot positionally and rejects `toDateTime64(str, 9)` when
// the literal has fractional seconds.
const nowAnchorLiteral = "toDateTime64('2026-01-01 00:00:01.000000000', 9, 'UTC')"

// chNow64Literal renders a `time.Time` as the CH DateTime64(9) literal
// shape `substituteNow64` splices in — `toDateTime64('YYYY-MM-DD
// HH:MM:SS.fffffffff', 9, 'UTC')`. The nanosecond field is always nine
// digits so the parser sees a fractional-second literal (matching
// [nowAnchorLiteral]'s shape). Used by the eval-instant sweep to anchor
// the residual outer-projection `now64(?)` to the swept eval time T.
func chNow64Literal(at time.Time) string {
	u := at.UTC()
	return fmt.Sprintf(
		"toDateTime64('%04d-%02d-%02d %02d:%02d:%02d.%09d', 9, 'UTC')",
		u.Year(), int(u.Month()), u.Day(),
		u.Hour(), u.Minute(), u.Second(), u.Nanosecond(),
	)
}

// tolerantRowsErr matches the helper used by the chDB probe in
// internal/chclient/chdb_probe_test.go.
func tolerantRowsErr(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), chdbEOFSentinel) {
		return nil
	}
	return err
}

// mapColumnNames is the conservative list of OTel-CH Map column names
// the runner will rewrite to `toJSONString(<name>)` in the emitted
// SQL before execution. We don't have type information at this
// layer; the rewrite is a textual transform keyed off this allow-
// list. Authors with custom Map columns can extend the list as
// fixtures grow.
var mapColumnNames = []string{
	"Attributes",
	"ResourceAttributes",
	"ResourceAttrs",
	"ScopeAttributes",
	"SpanAttributes",
	"LogAttributes",
}

// isMapColumn reports whether name (a backtick-quoted alias) is one
// of the known OTel Map column names.
func isMapColumn(name string) bool {
	for _, c := range mapColumnNames {
		if name == c {
			return true
		}
	}
	return false
}

// nestMapOrderBy guards the Map-wrap output against an outer
// `ORDER BY <MapColumn>[<key>]` clause — the shape the `sort_by_label`
// / `sort_by_label_desc` lowering emits (`SELECT * FROM (<sub>) ORDER
// BY ` + "`Attributes`" + `['<label>']`). After [expandStarProjection]
// + [rewriteMapProjections] rewrite the OUTER projection so the Map
// column is emitted as `toJSONString(Attributes) AS Attributes`, the
// ORDER BY's `Attributes[...]` subscript binds to that String-typed
// SELECT alias (ClickHouse resolves ORDER BY identifiers to SELECT
// aliases ahead of the source column), so the map subscript fails with
// `arrayElement … got 'String'`. Production never hits this — the live
// query path has no toJSONString wrap, so `SELECT * … ORDER BY
// Attributes[k]` keeps `Attributes` a Map. The collision is purely an
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
// whose ORDER BY references a known Map column via `[`-subscript, and
// the FROM clause is exactly one parenthesised subquery. Every other
// shape passes through untouched.
func nestMapOrderBy(query string) string {
	q := strings.TrimSpace(query)
	head, tail := splitOuterSelect(q)
	if head == "" {
		return query
	}
	upperTail := strings.ToUpper(tail)
	obIdx := strings.Index(upperTail, " ORDER BY ")
	if obIdx < 0 {
		return query
	}
	orderBy := tail[obIdx+len(" ORDER BY "):]
	if !orderByReferencesMapSubscript(orderBy) {
		return query
	}
	// `tail` is ` FROM (<sub>) ORDER BY <orderBy>`. Carve the
	// parenthesised subquery out of the FROM so we can re-wrap it with
	// the ORDER BY pushed inside.
	fromBody := strings.TrimSpace(strings.TrimPrefix(tail[:obIdx], " FROM "))
	if !strings.HasPrefix(fromBody, "(") || !strings.HasSuffix(fromBody, ")") {
		return query
	}
	return "SELECT " + head + " FROM (SELECT * FROM " + fromBody + " ORDER BY " + orderBy + ")"
}

// orderByReferencesMapSubscript reports whether an ORDER BY clause body
// sorts on a known Map column via `[`-subscript (e.g.
// "`Attributes`['handler'] DESC"). Used by [nestMapOrderBy] to detect
// the sort_by_label collision shape.
func orderByReferencesMapSubscript(orderBy string) bool {
	for _, name := range mapColumnNames {
		if strings.Contains(orderBy, "`"+name+"`[") {
			return true
		}
	}
	return false
}

// expandStarProjection rewrites a top-level `SELECT * FROM (SELECT
// <projs> FROM ...) ...` into `SELECT <alias-list> FROM (SELECT
// <projs> FROM ...) ...` so the subsequent [rewriteMapProjections]
// pass can wrap Map-typed columns in `toJSONString(...)`. cerberus's
// emitter sometimes hoists a star projection over a fully-aliased
// inner SELECT (e.g. the `Filter ... Project ...` lowering shape of
// `<scalar> < metric`); without expansion, the outer `*` carries the
// inner Map column through unwrapped and chdb-go's parquet driver
// panics with `could not cast to type: MAP`.
//
// The transform is conservative: it fires only when the outer
// projection is exactly `*` and the inner subquery starts with
// `SELECT ` (case-insensitive). Anything else passes through. The
// inner subquery's projections are re-rendered as their aliases
// (preferring explicit `AS <alias>` over the implicit form), which
// lets the outer SELECT name the columns and the Map-wrap pass do
// its work without touching the inner shape.
func expandStarProjection(query string) string {
	// A `WITH <cte> AS (...) SELECT …` head (the vector-set-op CSE CTE,
	// or the structural-join WITH RECURSIVE closure) precedes the outer
	// SELECT — peel it so the projection split sees the real outer
	// SELECT, then re-prepend it on the rewritten result. The CTE
	// bodies keep their raw Map columns (consumed server-side); only
	// the outer projection needs the toJSONString wrap.
	withHead, body := stripWithHead(query)
	if withHead != "" {
		return withHead + expandStarProjection(body)
	}
	head, tail := splitOuterSelect(query)
	if head == "" || strings.TrimSpace(head) != "*" {
		return query
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
	innerHead, _ := splitOuterSelect(inner)
	if innerHead == "" {
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
		aliases = append(aliases, "`"+alias+"`")
	}
	return "SELECT " + strings.Join(aliases, ", ") + tail
}

// rewriteMapProjections wraps any top-level SELECT projection whose
// alias is a known Map column in toJSONString(...). The transform
// fires on the OUTER SELECT only — subqueries keep their Map columns
// as raw maps because CH consumes them server-side.
//
// Two shapes are handled:
//
//	`Attributes`                       → toJSONString(`Attributes`) AS `Attributes`
//	<expr> AS `Attributes`             → toJSONString(<expr>) AS `Attributes`
//	`Attributes` AS `Attributes`       → toJSONString(`Attributes`) AS `Attributes`
//
// Anything else passes through; chdb-go will raise a Parquet panic
// at scan time if a Map column slips through unwrapped, which makes
// the failure mode loud and easy to debug.
func rewriteMapProjections(query string) string {
	// Peel a leading `WITH <cte> AS (...)` head so the outer-SELECT
	// projection split sees the real outer SELECT; re-prepend it after
	// rewriting. The CTE bodies are subqueries — CH consumes their Map
	// columns server-side, so they stay raw (the same rule the
	// subquery branches already follow).
	if withHead, body := stripWithHead(query); withHead != "" {
		return withHead + rewriteMapProjections(body)
	}
	head, tail := splitOuterSelect(query)
	if head == "" {
		// Top-level UNION (`(SELECT ...) UNION DISTINCT (SELECT ...)`):
		// rewrite each branch independently so a Map column projected
		// at the union level still reaches chdb-go as JSON. Without
		// this, chdb-go's parquet driver panics with `index out of
		// range` when a Map cell flows through the unioned result.
		// Surfaced by the structural-union TXTAR fixtures after
		// PR #523 added ResourceAttributes to the wrap projection.
		if rewritten, ok := rewriteUnionMapProjections(query); ok {
			return rewritten
		}
		return query
	}
	projs := splitProjections(head)
	for i, p := range projs {
		expr, alias := splitAlias(p)
		// Implicit alias: bare `Col` or `Qual.\`Col\`` projection.
		if alias == "" {
			alias = mapColAlias(strings.TrimSpace(expr))
		}
		if !isMapColumn(alias) {
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
			rewrittenInner := rewriteMapProjections(strings.TrimSpace(inner))
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

// stripWithHead peels a leading `WITH <cte> AS (...)[, <cte> AS (...)]`
// CTE chain off query, returning (head, body) where head is the verbatim
// `WITH … ` prefix (including the single trailing space before SELECT)
// and body is the outer `SELECT …` that follows. The `RECURSIVE` keyword
// is optional. When query does not begin with `WITH ` (case-insensitive)
// it returns ("", "") so callers fall through to the bare-SELECT path.
//
// The outer SELECT is the first `SELECT` keyword reached at paren depth 0
// after the CTE chain — CTE bodies are parenthesised, so their nested
// SELECTs sit at depth > 0 and are skipped. This lets the Map-rewrite
// passes operate on the real outer projection of the vector-set-op CSE
// CTE (`WITH _setop_lhs_<n> AS (...) SELECT …`) and the structural-join
// `WITH RECURSIVE` closure alike.
func stripWithHead(query string) (head, body string) {
	upper := strings.ToUpper(query)
	if !strings.HasPrefix(upper, "WITH ") {
		return "", ""
	}
	depth := 0
	for i := 0; i < len(query); i++ {
		switch query[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		// The outer SELECT is the first depth-0 `SELECT ` token after the
		// `WITH ` keyword itself (i > 0 guards against matching at the
		// very start, which can't happen here anyway since we begin with
		// WITH).
		if depth == 0 && i+len("SELECT ") <= len(query) &&
			strings.EqualFold(query[i:i+len("SELECT ")], "SELECT ") {
			// Only treat it as the outer SELECT when it's a standalone
			// keyword (preceded by whitespace), not a substring of an
			// identifier.
			if i > 0 && (query[i-1] == ' ' || query[i-1] == ')' || query[i-1] == '\n' || query[i-1] == '\t') {
				return query[:i], query[i:]
			}
		}
	}
	return "", ""
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
// extractProjectionCount so we can count the leading branch's columns;
// the rewriteMapProjections pass still operates on the unmodified query
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

// extractProjectionCount counts top-level SELECT projections by
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
func extractProjectionCount(query string) int {
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

// substituteNow64 rewrites every `now64(...)` and `now()` reference in
// the emitted SQL to the deterministic [nowAnchorLiteral] so instant
// queries that project the wall-clock as `TimeUnix` (PromQL
// aggregations, histogram quantiles, subqueries, predict_linear,
// holt_winters) or read the wall-clock as a DateTime through `now()`
// (PromQL zero-arg date functions like `day_of_month()` / `hour()` /
// `month()` lower to `toDayOfMonth(now())` etc.) produce a repeatable
// cell in `expected_rows:`. Without this, the outer projection would
// scan as the wall-clock at test execution time and never match a
// written-in-stone fixture row.
//
// Three shapes appear in the corpus:
//
//   - `now64(?)` — parameterized; the trailing `?` is bound to an
//     int64 precision arg in `args:`. We strip the corresponding
//     positional slot from args alongside the SQL rewrite so the
//     remaining `?` placeholders re-index correctly.
//
//   - `now64(9)` / `now64(<int>)` — emitted as a literal in subquery,
//     predict_linear, and holt_winters lowerings. No args slot to
//     consume.
//
//   - `now()` — emitted by the zero-arg PromQL date-function lowerings
//     (`day_of_month()` / `day_of_week()` / `days_in_month()` /
//     `hour()` / `minute()` / `month()` / `year()`). CH's date
//     accessors accept DateTime64 the same way they accept DateTime,
//     so substituting the full [nowAnchorLiteral] (which is a
//     DateTime64(9)) preserves type compatibility with `toYear`,
//     `toMonth`, `toDayOfMonth`, `toDayOfWeek`, `toLastDayOfMonth`,
//     `toHour`, `toMinute`. No args slot to consume.
//
// The function tracks `?` placeholder offsets while scanning so the
// args list is mutated in lock-step. This is a test-infra workaround
// for the inherent non-determinism of wall-clock projections in
// instant queries — production code path is untouched. See PR #288's
// audit note ("seed/metric mismatch + non-deterministic now64") and
// the follow-up that lands seed alignment + this substitution
// together.
func substituteNow64(query string, args []any) (string, []any) {
	return substituteNow64At(query, args, nowAnchorLiteral)
}

// substituteNow64At is the anchor-parameterised core of
// [substituteNow64]: it splices `anchorLiteral` (a pre-rendered CH
// DateTime64 literal, e.g. from [chNow64Literal]) in place of every
// `now64(...)` / `now()` reference, instead of the package-fixed
// [nowAnchorLiteral]. [substituteNow64] passes [nowAnchorLiteral] so the
// established fixed-anchor round-trip path is byte-identical; the
// eval-instant sweep passes a per-T literal so the residual outer
// `now64(?)` result-timestamp projection pins to the swept eval instant
// rather than the fixed default. The window bound itself is NOT a
// now64 in the post-fix SQL — it renders as an inline eval-literal — so
// this anchor only governs the wall-clock projection, never the row
// count or the window semantics under test.
func substituteNow64At(query string, args []any, anchorLiteral string) (string, []any) {
	// Fast-path: nothing to do when neither shape is present.
	if !strings.Contains(query, "now64(") && !strings.Contains(query, "now()") {
		return query, args
	}

	var (
		out     strings.Builder
		newArgs = make([]any, 0, len(args))
		argIdx  int
		inStr   byte
	)
	out.Grow(len(query))

	for i := 0; i < len(query); i++ {
		c := query[i]
		// Track string literals so a stray `?` or `now64(` inside
		// quotes is left alone. CH SQL uses single-quotes; backticks
		// quote identifiers, not strings, so they don't interfere
		// with placeholder counting.
		if inStr != 0 {
			out.WriteByte(c)
			if c == inStr {
				inStr = 0
			}
			continue
		}
		if c == '\'' {
			inStr = c
			out.WriteByte(c)
			continue
		}

		// Match `now64(?)` — substitute literal and consume one arg.
		if c == 'n' && strings.HasPrefix(query[i:], "now64(?)") {
			out.WriteString(anchorLiteral)
			// Skip the consumed arg slot. argIdx is the next-to-bind
			// index; it points at the `?` inside `now64(?)` which we
			// are about to drop. Advance past it without copying.
			argIdx++
			i += len("now64(?)") - 1
			continue
		}

		// Match `now64(<int>)` — substitute literal, no args change.
		if c == 'n' && strings.HasPrefix(query[i:], "now64(") {
			// Find the matching `)` at depth 0 starting after the `(`.
			rest := query[i+len("now64("):]
			depth := 1
			j := 0
			for ; j < len(rest); j++ {
				if rest[j] == '(' {
					depth++
				} else if rest[j] == ')' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
			if j < len(rest) {
				inner := strings.TrimSpace(rest[:j])
				// Only substitute when the body is a bare numeric
				// literal (the precision arg). Anything else (no
				// known cases today) passes through to surface a
				// real failure rather than silently mis-rewrite.
				if isIntLiteral(inner) {
					out.WriteString(anchorLiteral)
					i += len("now64(") + j // jump past the closing `)`
					continue
				}
			}
		}

		// Match `now()` — the zero-arg DateTime form emitted by PromQL
		// zero-arg date functions. Substitute with the deterministic
		// DateTime64 anchor literal; CH's `toYear`/`toMonth`/
		// `toDayOfMonth`/`toDayOfWeek`/`toLastDayOfMonth`/`toHour`/
		// `toMinute` accept DateTime64 the same as DateTime, so the
		// type widening is invisible at the call site. No args slot
		// to consume.
		if c == 'n' && strings.HasPrefix(query[i:], "now()") {
			out.WriteString(anchorLiteral)
			i += len("now()") - 1
			continue
		}

		// Generic `?` placeholder: copy the arg through.
		if c == '?' {
			out.WriteByte(c)
			if argIdx < len(args) {
				newArgs = append(newArgs, args[argIdx])
			}
			argIdx++
			continue
		}

		out.WriteByte(c)
	}

	return out.String(), newArgs
}

// isIntLiteral reports whether s is a non-empty run of ASCII digits
// (optionally prefixed by `-`). Used by substituteNow64 to recognise
// the precision literal in `now64(9)` without pulling in strconv.
func isIntLiteral(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '-' {
		i = 1
		if len(s) == 1 {
			return false
		}
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// openChDB returns a fresh ephemeral chDB session. The empty DSN
// triggers a temp-dir-backed session that's torn down with the
// connection — there is no `:memory:` literal in chdb-go.
func openChDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}
	// Enable the experimental timeSeries*ToGrid aggregate family at the
	// session level so the native-rate fixtures (RangeWindowNative →
	// timeSeriesRateToGrid) run. The setting is harmless for every other
	// fixture — it gates only those aggregates, which no other fixture
	// emits — and chDB does not enforce the gate, so this is belt-and-
	// braces for forward-compatibility if a future chDB starts to. The
	// production chclient sends the same setting per-query (only on the
	// native path); see internal/chclient.WithTSGridSetting. The spelling
	// is the CANONICAL `allow_experimental_time_series_aggregate_functions`
	// (ClickHouse PR #80590 renamed the gate before the v25.6 release; the
	// old `..._ts_to_grid_aggregate_function` survives only as an alias —
	// see chclient.SettingExperimentalTSGridAggregate). A chDB build older
	// than the family's introduction would reject the SET — current
	// substrate is 25.8 (probed), well past the v25.6 floor.
	if _, err := db.Exec("SET allow_experimental_time_series_aggregate_functions = 1"); err != nil {
		t.Fatalf("enable experimental ts-grid aggregate: %v", err)
	}
	return db
}

// applySeed splits a multi-statement script on top-level semicolons
// and exec's each piece. Statements wrapped in single-quoted strings
// keep their semicolons literal (handled by a tiny state machine).
//
// Cross-fixture isolation: chdb-go shares one engine across a process,
// so bare `CREATE TABLE foo` from a prior fixture survives to clash
// with the next. The applier promotes bare `CREATE TABLE` to
// `CREATE OR REPLACE TABLE` so re-running a fixture in the same
// process is idempotent. Fixture authors who want strict CH semantics
// can opt out by writing `CREATE OR REPLACE TABLE` /
// `CREATE TABLE IF NOT EXISTS` themselves.
func applySeed(t *testing.T, db *sql.DB, seed string) {
	t.Helper()
	for _, stmt := range backfillResourceAttributes(splitStatements(seed)) {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		stmt = promoteCreateTable(stmt)
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed exec failed:\n--- stmt ---\n%s\n--- err ---\n%v", stmt, err)
		}
	}
}

// metricsTablePrefix is the storage prefix every OTel-CH metric table
// (gauge / sum / histogram / exp-histogram / summary) shares; the
// resource-attribute backfill scopes itself to these so a fixture's own
// helper tables stay untouched.
const metricsTablePrefix = "otel_metrics_"

// resourceAttributesColumnDDL is the column definition the backfill
// injects. DEFAULT map() lets the existing positional INSERTs keep their
// value count: the backfilled INSERTs carry an explicit column list that
// omits this column, so chDB fills it with the empty map — matching
// production, where every metric table carries a (possibly empty)
// ResourceAttributes map.
const resourceAttributesColumnDDL = "ResourceAttributes Map(String, String) DEFAULT map()"

// backfillResourceAttributes mirrors the production OTel-CH invariant —
// every metric table (`otel_metrics_*`) carries a `ResourceAttributes`
// Map column — onto the spec fixtures' simplified seed DDL. The rc.5
// read-path always projects
// `mapUpdate(sanitize(ResourceAttributes), Attributes)`, so a seed table
// that omits the column would fail the chDB round-trip with
// UNKNOWN_IDENTIFIER. Rather than hand-editing ~300 fixtures (and every
// future one), the harness backfills the column centrally:
//
//   - A `CREATE TABLE otel_metrics_*` whose body declares an `Attributes`
//     Map column but no `ResourceAttributes` gets the column injected
//     (DEFAULT map()) and its ordered column names recorded.
//   - Every subsequent `INSERT INTO <that table> VALUES …` with no
//     explicit column list is rewritten to carry the recorded column
//     list (sans ResourceAttributes), so the existing positional VALUES
//     tuples keep working and the DEFAULT fills the new column.
//
// Seeds that already declare ResourceAttributes, or that already use an
// explicit INSERT column list, pass through untouched — so a fixture that
// deliberately populates resource attributes (the rc.5 contract fixtures)
// is honoured verbatim.
func backfillResourceAttributes(stmts []string) []string {
	// table name -> ordered column names (pre-backfill) for tables we
	// injected the column into. Only these tables' INSERTs are rewritten.
	cols := map[string][]string{}
	out := make([]string, 0, len(stmts))
	for _, stmt := range stmts {
		if table, colNames, body, ok := parseMetricsCreate(stmt); ok {
			cols[table] = colNames
			out = append(out, body)
			continue
		}
		if rewritten, ok := rewriteMetricsInsert(stmt, cols); ok {
			out = append(out, rewritten)
			continue
		}
		out = append(out, stmt)
	}
	return out
}

// parseMetricsCreate reports whether stmt is a `CREATE TABLE
// otel_metrics_*` that declares an `Attributes` column but no
// `ResourceAttributes`. On a match it returns the table name, the ordered
// pre-backfill column names, and the rewritten statement with the
// ResourceAttributes column injected right after the Attributes column.
func parseMetricsCreate(stmt string) (table string, colNames []string, rewritten string, ok bool) {
	trimmed := stripLeadingNoise(stmt)
	upper := strings.ToUpper(trimmed)
	idx := strings.Index(upper, "CREATE TABLE ")
	if idx != 0 {
		return "", nil, "", false
	}
	rest := trimmed[len("CREATE TABLE "):]
	name := strings.ToLower(strings.TrimSpace(firstToken(rest)))
	if !strings.HasPrefix(name, metricsTablePrefix) {
		return "", nil, "", false
	}
	open := strings.IndexByte(stmt, '(')
	if open < 0 {
		return "", nil, "", false
	}
	// Match the column-list close paren by balancing from the first `(`
	// — NOT strings.LastIndexByte, which would grab the ENGINE/ORDER BY
	// `(MetricName, …)` paren and drag the engine clause into the column
	// body.
	closeParen := matchParen(stmt, open)
	if closeParen < 0 {
		return "", nil, "", false
	}
	bodyCols := stmt[open+1 : closeParen]
	defs := splitTopLevelCommas(bodyCols)
	hasAttributes, hasResource := false, false
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		cn := firstToken(strings.TrimSpace(d))
		switch cn {
		case "Attributes":
			hasAttributes = true
		case "ResourceAttributes":
			hasResource = true
		}
		if cn != "" {
			names = append(names, cn)
		}
	}
	if !hasAttributes || hasResource {
		return "", nil, "", false
	}
	// Inject the column directly after the Attributes definition so the
	// rewritten DDL reads naturally; column order is otherwise irrelevant
	// because the INSERTs become explicit-column.
	newDefs := make([]string, 0, len(defs)+1)
	for _, d := range defs {
		newDefs = append(newDefs, d)
		if firstToken(strings.TrimSpace(d)) == "Attributes" {
			newDefs = append(newDefs, " "+resourceAttributesColumnDDL)
		}
	}
	rewritten = stmt[:open+1] + strings.Join(newDefs, ",") + stmt[closeParen:]
	return name, names, rewritten, true
}

// rewriteMetricsInsert rewrites a positional `INSERT INTO <table> VALUES`
// into one carrying the recorded column list (sans ResourceAttributes) so
// the DEFAULT-filled column does not break the value count. Inserts into
// untracked tables, or that already carry an explicit column list, pass
// through unchanged.
func rewriteMetricsInsert(stmt string, cols map[string][]string) (string, bool) {
	trimmed := stripLeadingNoise(stmt)
	prefix := stmt[:len(stmt)-len(trimmed)]
	upper := strings.ToUpper(trimmed)
	const needle = "INSERT INTO "
	if !strings.HasPrefix(upper, needle) {
		return "", false
	}
	rest := trimmed[len(needle):]
	name := strings.ToLower(strings.TrimSpace(firstToken(rest)))
	colNames, tracked := cols[name]
	if !tracked {
		return "", false
	}
	// Find the table-name token end; if the next non-space char is '(',
	// the INSERT already carries an explicit column list — leave it.
	afterName := strings.TrimLeft(rest[len(firstToken(rest)):], " \t\n\r")
	if strings.HasPrefix(afterName, "(") {
		return "", false
	}
	colList := "(" + strings.Join(colNames, ", ") + ") "
	return prefix + needle + firstToken(rest) + " " + colList + afterName, true
}

// matchParen returns the index of the `)` that balances the `(` at
// position open in s, honouring single-quoted strings, or -1 when
// unbalanced. open must index a `(`.
func matchParen(s string, open int) int {
	depth := 0
	inStr := false
	esc := false
	for i := open; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '\'':
			inStr = !inStr
		case inStr:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// firstToken returns the leading identifier of s (up to the first space,
// tab, newline, '(' or ',').
func firstToken(s string) string {
	s = strings.TrimLeft(s, " \t\n\r")
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r', '(', ',':
			return s[:i]
		}
	}
	return s
}

// splitTopLevelCommas splits s on commas that sit at paren-depth 0,
// shielding commas inside nested parens (e.g. `Map(String, String)`) and
// single-quoted strings. Used to walk a CREATE TABLE column-definition
// list.
func splitTopLevelCommas(s string) []string {
	var (
		out   []string
		buf   strings.Builder
		depth int
		inStr bool
		esc   bool
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
			buf.WriteByte(c)
		case c == '\\' && inStr:
			esc = true
			buf.WriteByte(c)
		case c == '\'':
			inStr = !inStr
			buf.WriteByte(c)
		case inStr:
			buf.WriteByte(c)
		case c == '(':
			depth++
			buf.WriteByte(c)
		case c == ')':
			depth--
			buf.WriteByte(c)
		case c == ',' && depth == 0:
			out = append(out, buf.String())
			buf.Reset()
		default:
			buf.WriteByte(c)
		}
	}
	if strings.TrimSpace(buf.String()) != "" {
		out = append(out, buf.String())
	}
	return out
}

// promoteCreateTable rewrites a bare `CREATE TABLE …` statement to
// `CREATE OR REPLACE TABLE …` so re-running a seed against a chDB
// session that already holds the table is idempotent. Other variants
// (`CREATE OR REPLACE TABLE`, `CREATE TABLE IF NOT EXISTS`,
// `CREATE TEMPORARY TABLE`) are left untouched.
//
// Leading whitespace and SQL line comments (`-- …`) are skipped when
// locating the `CREATE TABLE` keyword: fixture authors routinely
// document the seed shape with a comment block above the DDL, and
// without comment-skipping those fixtures would bypass the
// OR-REPLACE rewrite and trip TABLE_ALREADY_EXISTS on the second
// run inside a shared chDB session.
func promoteCreateTable(stmt string) string {
	trimmed := stripLeadingNoise(stmt)
	prefix := stmt[:len(stmt)-len(trimmed)]
	upper := strings.ToUpper(trimmed)
	const needle = "CREATE TABLE "
	if !strings.HasPrefix(upper, needle) {
		return stmt
	}
	rest := trimmed[len(needle):]
	return prefix + "CREATE OR REPLACE TABLE " + rest
}

// stripLeadingNoise consumes leading whitespace and `-- …\n` line
// comments from s, returning the remaining suffix. Block comments
// (`/* … */`) are not stripped — no current fixture uses them — but
// the loop is structured so a future extension stays a one-case add.
func stripLeadingNoise(s string) string {
	for {
		t := strings.TrimLeft(s, " \t\n\r")
		if strings.HasPrefix(t, "--") {
			if nl := strings.IndexByte(t, '\n'); nl >= 0 {
				s = t[nl+1:]
				continue
			}
			return ""
		}
		return t
	}
}

func splitStatements(s string) []string {
	var (
		out   []string
		buf   strings.Builder
		inStr bool
		esc   bool
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
			buf.WriteByte(c)
		case c == '\\' && inStr:
			esc = true
			buf.WriteByte(c)
		case c == '\'':
			inStr = !inStr
			buf.WriteByte(c)
		case c == ';' && !inStr:
			out = append(out, buf.String())
			buf.Reset()
		default:
			buf.WriteByte(c)
		}
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}

// RunRoundTrip executes a fixture's seed + rewritten SQL against an
// ephemeral chDB session and asserts the resulting rows match
// `expected_rows:`. Caller passes the loaded fixture; if it's not a
// round-trip fixture, the call is a no-op.
//
// Determinism contract: cerberus's emitted instant-query SQL does not
// carry a top-level ORDER BY (PromQL's instant-query result is a set
// of (labels, value) pairs — no order is promised by the wire shape),
// so RunRoundTrip canonicalises both sides through [sortRows] before
// `reflect.DeepEqual`. Fixtures that rely on a stable order must
// emit one explicitly in the seed/SQL — none do today.
// Map column comparison uses reflect.DeepEqual on map[string]any so
// JSON key ordering is irrelevant.
func RunRoundTrip(t *testing.T, c *Case) {
	t.Helper()
	rt, err := LoadRoundTrip(c)
	if err != nil {
		t.Fatalf("LoadRoundTrip: %v", err)
	}
	if !rt.IsRoundTrip() {
		return
	}
	if strings.TrimSpace(rt.SQL) == "" {
		t.Fatalf("fixture %s has seed/expected_rows but missing sql section", c.Name)
	}

	db := openChDB(t)
	applySeed(t, db, rt.Seed)

	// substituteNow64 must run BEFORE rewriteMapProjections so the
	// `now64(?)`-consumed args are dropped before the Map rewrite
	// inspects the SQL. The two passes are independent textually but
	// the args side is global, and ordering them this way keeps the
	// argIdx accounting in substituteNow64 simple.
	query, queryArgs := substituteNow64(rt.SQL, rt.Args)
	query = expandStarProjection(query)
	query = rewriteMapProjections(query)
	query = nestMapOrderBy(query)
	colCount := extractProjectionCount(query)

	rows, err := db.Query(query, queryArgs...)
	if err != nil {
		t.Fatalf("query failed:\n--- query ---\n%s\n--- args ---\n%#v\n--- err ---\n%v",
			query, queryArgs, err)
	}
	defer func() { _ = rows.Close() }()

	if colCount == 0 {
		// Wildcard outer projection (`SELECT R.* FROM ...`): the
		// fixture seed determines the actual column count. `rows.
		// Columns()` returns names without instantiating the
		// driver's column-type table, so it sidesteps the Map
		// `rows.ColumnTypes()` panic.
		cols, cerr := rows.Columns()
		if cerr != nil {
			t.Fatalf("rows.Columns: %v", cerr)
		}
		colCount = len(cols)
		if colCount == 0 {
			t.Fatalf("fixture %s: cannot determine SELECT projection count from sql", c.Name)
		}
	}

	got := make([][]any, 0, len(rt.ExpectedRows))
	for rows.Next() {
		// Scan into *interface{} so we receive the chdb-go driver's
		// native Go value (string, int64, float64, time.Time, []byte)
		// per chdb/driver/parquet.go's switch table. This sidesteps
		// rows.ColumnTypes() (which panics on Map columns per the
		// chDB driver probe).
		cells := make([]any, colCount)
		ptrs := make([]any, colCount)
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		row := make([]any, colCount)
		for i, v := range cells {
			row[i] = decodeCell(v, rt.RawStrings)
		}
		got = append(got, row)
	}
	if err := tolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	// Coerce expected rows in-place: JSON numbers always decode as
	// float64; the runner normalizes the actual scan output through
	// the same lens so DeepEqual is symmetric. Same for Map columns
	// (already decoded as map[string]any on the got side).
	want := normalizeExpected(rt.ExpectedRows)
	gotNorm := normalizeExpected(got)

	// PromQL instant-query results are sets — the chDB engine is
	// free to return groups in any order when the emitted SQL lacks
	// a top-level ORDER BY (which it does). Sort both sides through
	// the same canonical form so DeepEqual reflects set-equality.
	sortRows(want)
	sortRows(gotNorm)

	if !reflect.DeepEqual(gotNorm, want) {
		// GOLDEN_UPDATE=1: rewrite `expected_rows` in-place rather
		// than failing — same flow as the text-equality goldens in
		// internal/promql/lower_test.go. Lets dev/CI regenerate the
		// round-trip cells after a semantically-correct query
		// change (e.g., PromQL `__name__`-drop fix in #355) without
		// hand-editing 70+ fixtures.
		if os.Getenv(envGoldenUpdate) == "1" {
			Match(t, c, map[string]string{
				"expected_rows": formatExpectedRows(gotNorm),
			})
			return
		}
		t.Fatalf("round-trip mismatch (fixture %s)\n got = %s\nwant = %s",
			c.Name, mustJSON(gotNorm), mustJSON(want))
	}
}

// formatExpectedRows renders a row set in the canonical TXTAR
// `expected_rows:` shape: outer `[`/`]` on their own lines, each row
// on its own line indented with two spaces, cells rendered compact-JSON
// with `, ` separators (matching the byte shape every hand-authored
// fixture in test/spec/{promql,logql,traceql}/ pins).
//
// We don't lean on `json.Marshal` for the row itself because the
// stdlib produces no-space separators (`","`), so manual fixtures and
// regenerated ones would drift by whitespace alone. Each cell flows
// through json.Marshal individually, then we join with `, `.
func formatExpectedRows(rows [][]any) string {
	if len(rows) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.WriteString("[\n")
	for i, row := range rows {
		b.WriteString("  [")
		for j, cell := range row {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteString(formatExpectedCell(cell))
		}
		b.WriteByte(']')
		if i < len(rows)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("]")
	return b.String()
}

// formatExpectedCell renders one cell of an `expected_rows:` row.
// Maps render with spaces between key/value pairs (`{"k": "v", ...}`)
// to match the hand-authored fixture shape; everything else delegates
// to json.Marshal of the infSafe-wrapped value.
func formatExpectedCell(v any) string {
	if m, ok := v.(map[string]any); ok {
		return formatExpectedMap(m)
	}
	raw, err := json.Marshal(infSafe(v))
	if err != nil {
		return fmt.Sprintf(`"<json err: %v>"`, err)
	}
	return string(raw)
}

// formatExpectedMap renders a JSON object with deterministic key order
// (`sort.Strings`) and `, ` separators between pairs and `: ` between
// keys/values, matching the hand-authored fixture style. Used only by
// the GOLDEN_UPDATE regeneration path; the read path goes through
// `json.Unmarshal` which is whitespace-insensitive.
func formatExpectedMap(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		raw, err := json.Marshal(k)
		if err != nil {
			raw = []byte(fmt.Sprintf(`"<json err: %v>"`, err))
		}
		b.Write(raw)
		b.WriteString(": ")
		b.WriteString(formatExpectedCell(m[k]))
	}
	b.WriteByte('}')
	return b.String()
}

// sortRows canonicalises a result set by sorting rows in-place on the
// JSON encoding of their cells. The encoding is deterministic for the
// types the runner emits (string, float64, bool, nil, []any,
// map[string]any), so any two row sets that compare set-equal end up
// with identical post-sort orderings.
func sortRows(rows [][]any) {
	sort.Slice(rows, func(i, j int) bool {
		return mustJSON(rows[i]) < mustJSON(rows[j])
	})
}

// decodeCell turns a chdb-go driver-native value into the Go value
// used for comparison. The driver hands back time.Time, int64,
// float64, bool, string, []byte — see chdb/driver/parquet.go's
// switch table.
//
// For Map columns we wrapped server-side in toJSONString(...), the
// driver returns a string; we try JSON-decode and fall back to the
// raw string. time.Time values are normalized to RFC3339Nano so
// fixture authors can write them as quoted strings.
//
// When rawStrings is true the JSON-decode pass on String/[]byte cells
// is skipped — the runner returns the raw string. Fixtures opt in
// via the `raw_strings:` section when they need to assert literal
// brace-prefixed payloads against the SQL output.
func decodeCell(v any, rawStrings bool) any {
	switch x := v.(type) {
	case nil:
		return nil
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case []byte:
		if rawStrings {
			return string(x)
		}
		return decodeBytes(x)
	case string:
		if rawStrings {
			return x
		}
		return decodeString(x)
	default:
		return v
	}
}

func decodeBytes(b []byte) any {
	return decodeString(string(b))
}

func decodeString(s string) any {
	trim := strings.TrimSpace(s)
	if len(trim) > 0 && (trim[0] == '{' || trim[0] == '[') {
		var v any
		if err := json.Unmarshal([]byte(trim), &v); err == nil {
			return v
		}
	}
	return s
}

// normalizeExpected walks a [][]any and coerces numeric cells to
// float64 so JSON-decoded numbers compare equal to scanned values.
func normalizeExpected(rows [][]any) [][]any {
	out := make([][]any, len(rows))
	for i, row := range rows {
		nr := make([]any, len(row))
		for j, v := range row {
			nr[j] = normalizeValue(v)
		}
		out[i] = nr
	}
	return out
}

func normalizeValue(v any) any {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int8:
		return float64(x)
	case int16:
		return float64(x)
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case uint8:
		return float64(x)
	case uint16:
		return float64(x)
	case uint32:
		return float64(x)
	case uint64:
		return float64(x)
	case float32:
		return float64(x)
	case float64:
		// Non-finite floats normalize to the same string sentinels the
		// expected side uses (below). Comparing through strings — not
		// through math.NaN() — is load-bearing for NaN cells:
		// reflect.DeepEqual(math.NaN(), math.NaN()) is FALSE (IEEE NaN
		// inequality), so a NaN-valued fixture cell could never match
		// if both sides normalized to the float.
		switch {
		case math.IsNaN(x):
			return "NaN"
		case math.IsInf(x, +1):
			return "+Inf"
		case math.IsInf(x, -1):
			return "-Inf"
		}
		return x
	case string:
		// JSON cannot represent ±Inf / NaN natively (json.Unmarshal
		// would reject the bare tokens). Fixture authors encode them
		// as string sentinels in `expected_rows:`; canonicalise the
		// spelling so "Inf" and "+Inf" compare equal and the actual
		// side's float64 specials (normalized above) line up.
		switch x {
		case "Inf", "+Inf":
			return "+Inf"
		case "-Inf":
			return "-Inf"
		case "NaN":
			return "NaN"
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = normalizeValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = normalizeValue(vv)
		}
		return out
	default:
		return v
	}
}

func mustJSON(v any) string {
	// json.Marshal rejects non-finite float64 values; route them
	// through the same sentinel strings normalizeValue accepts on
	// the read path so sortRows can keep delegating to JSON
	// without per-call error handling for ±Inf / NaN cells.
	b, err := json.MarshalIndent(infSafe(v), "", "  ")
	if err != nil {
		return fmt.Sprintf("<json err: %v>", err)
	}
	return string(b)
}

// infSafe walks v and substitutes non-finite float64 values with the
// JSON-friendly string sentinels normalizeValue understands ("+Inf",
// "-Inf", "NaN"). Other types pass through unchanged. Used only by
// the mustJSON sort key — the cell values themselves are still
// compared via reflect.DeepEqual at full float64 precision.
func infSafe(v any) any {
	switch x := v.(type) {
	case float64:
		switch {
		case math.IsNaN(x):
			return "NaN"
		case math.IsInf(x, +1):
			return "+Inf"
		case math.IsInf(x, -1):
			return "-Inf"
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = infSafe(vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = infSafe(vv)
		}
		return out
	}
	return v
}
