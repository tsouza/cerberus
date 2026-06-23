// Package spec — round-trip seed/SQL prep pipeline (build-tag-free).
//
// This file holds the seed-script + emitted-SQL rewrite helpers that are
// independent of any execution substrate: the `now64(...)` substitution,
// the seed-statement splitter + idempotency promotion, and the
// ResourceAttributes backfill. They were originally defined in the
// `//go:build chdb` runner (runner_chdb.go), but two consumers now need
// them — the chDB round-trip assertion AND the `integration`-tagged
// strict-scan differential (strictscan_integration_test.go), which runs the
// same emitted SQL against a REAL ClickHouse via clickhouse-go/v2 to catch
// the prod-vs-chDB scan-type divergence the chDB lane is structurally blind
// to (chDB leniently coerces e.g. UInt8 -> *float64; prod clickhouse-go
// strict-scans and 502s).
//
// Keeping these in one untagged file means there is exactly one rewrite
// pipeline, consumed by both runners — the chDB runner and the strict-scan
// differential prep the byte-identical seed + SQL they would otherwise have
// to duplicate.
//
// Note: the chDB-specific rewrites that MASK the prod divergence — the
// `toJSONString(...)` Map-column wrap (rewriteMapProjections), star-projection
// expansion, ORDER-BY-over-Map nesting — deliberately stay in runner_chdb.go.
// The strict-scan differential must execute the RAW emitted SQL (native Map /
// DateTime64 / Float64 columns) so it observes exactly what production does.
package spec

import (
	"strings"
	"time"
)

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

// SubstituteNow64 splices the deterministic fixed anchor literal in place of
// every `now64(...)` / `now()` reference in query, consuming the `now64(?)`
// arg slot. It is the exported, build-tag-free seam onto [substituteNow64] so
// the `integration`-tagged strict-scan differential rewrites a fixture's
// emitted SQL identically to the chDB round-trip runner before executing it
// against a real ClickHouse.
func SubstituteNow64(query string, args []any) (string, []any) {
	return substituteNow64(query, args)
}

// substituteNow64 splices the package-fixed [nowAnchorLiteral] in place of
// every `now64(...)` / `now()` reference, consuming the `now64(?)` arg slot.
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

// metricsTablePrefix is the storage prefix every OTel-CH metric table
// (gauge / sum / histogram / exp-histogram / summary) shares; the
// resource-attribute backfill scopes itself to these so a fixture's own
// helper tables stay untouched.
const metricsTablePrefix = "otel_metrics_"

// resourceAttributesColumnDDL is the column definition the backfill
// injects. DEFAULT map() lets the existing positional INSERTs keep their
// value count: the backfilled INSERTs carry an explicit column list that
// omits this column, so the empty map is filled — matching production,
// where every metric table carries a (possibly empty) ResourceAttributes
// map.
const resourceAttributesColumnDDL = "ResourceAttributes Map(String, String) DEFAULT map()"

// backfillResourceAttributes mirrors the production OTel-CH invariant —
// every metric table (`otel_metrics_*`) carries a `ResourceAttributes`
// Map column — onto the spec fixtures' simplified seed DDL. The rc.5
// read-path always projects
// `mapUpdate(sanitize(ResourceAttributes), Attributes)`, so a seed table
// that omits the column would fail with UNKNOWN_IDENTIFIER. Rather than
// hand-editing ~300 fixtures (and every future one), the harness backfills
// the column centrally:
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
// BackfillResourceAttributes is the exported, build-tag-free seam onto
// [backfillResourceAttributes] for the strict-scan differential's seed loop.
func BackfillResourceAttributes(stmts []string) []string {
	return backfillResourceAttributes(stmts)
}

// SplitSeedStatements splits a seed script on top-level semicolons, shielding
// semicolons inside single-quoted strings — the same split the chDB runner's
// applySeed and the strict-scan differential use before exec'ing each
// statement (both drivers are single-statement). Exported, build-tag-free.
func SplitSeedStatements(seed string) []string {
	return splitStatements(seed)
}

// PromoteCreateTable rewrites a bare `CREATE TABLE …` to
// `CREATE OR REPLACE TABLE …` for cross-fixture idempotency inside a shared
// session/database, leaving `CREATE OR REPLACE` / `IF NOT EXISTS` /
// `TEMPORARY` variants untouched. Exported, build-tag-free.
func PromoteCreateTable(stmt string) string {
	return promoteCreateTable(stmt)
}

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
// `CREATE OR REPLACE TABLE …` so re-running a seed against a session that
// already holds the table is idempotent. Other variants
// (`CREATE OR REPLACE TABLE`, `CREATE TABLE IF NOT EXISTS`,
// `CREATE TEMPORARY TABLE`) are left untouched.
//
// Leading whitespace and SQL line comments (`-- …`) are skipped when
// locating the `CREATE TABLE` keyword: fixture authors routinely
// document the seed shape with a comment block above the DDL, and
// without comment-skipping those fixtures would bypass the
// OR-REPLACE rewrite and trip TABLE_ALREADY_EXISTS on the second
// run inside a shared session.
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

// splitStatements splits a seed script on top-level semicolons, shielding
// semicolons inside single-quoted strings AND inside `-- …` line comments.
//
// The comment-awareness is load-bearing: a fixture's `-- seed --` block
// routinely opens with a prose comment that contains an apostrophe (e.g.
// "the runner's `SELECT *`"). Without skipping comment runs, that stray
// apostrophe flips the in-string state machine and the very next real `;`
// — the one terminating `CREATE TABLE … ENGINE = Memory` — is swallowed as
// "inside a string", gluing the CREATE and the following INSERT into one
// statement. chdb-go's database/sql driver tolerates the resulting
// multi-statement; clickhouse-go's native Exec rejects it (code 62,
// "Multi-statements are not allowed"). Skipping comment bytes (and the `;`
// they may contain) keeps both drivers fed one statement at a time.
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
		// A `--` line comment outside a string runs to end-of-line; copy it
		// through verbatim (so the emitted statement still documents itself)
		// but do NOT let its bytes touch the quote / semicolon state machine.
		case c == '-' && !inStr && i+1 < len(s) && s[i+1] == '-':
			for i < len(s) && s[i] != '\n' {
				buf.WriteByte(s[i])
				i++
			}
			if i < len(s) {
				buf.WriteByte(s[i]) // the '\n'
			}
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
