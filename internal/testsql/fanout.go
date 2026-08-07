package testsql

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// CheckSeedCoversFanOut asserts that a fixture's own seed script is
// self-sufficient for an emitted statement that scans a ClickHouse
// `merge()` table-function fan-out.
//
// # Why a fan-out needs its own gate
//
// `merge(<db>, '<regex>')` resolves against whatever tables happen to
// exist in the session at query time, and its column set is the UNION of
// the matched tables' columns. Both halves are silent: an arm the seed
// never created simply drops out of the union, and a column the seed
// never declared still resolves as long as SOME other table matching the
// regex carries it. A test substrate that shares one session across
// fixtures — every `sql.Open("chdb", "")` resolves to a single in-process
// session, so every chdb-tagged test in a package shares one catalog —
// therefore lets one fixture's seed silently satisfy another's query.
//
// The failure mode is not a flaky test; it is worse. The whole-package
// run passes and the narrowed `go test -run '^TestName$'` run — the
// invocation the repo mandates for reproducing a red check — fails with
// an UNKNOWN_IDENTIFIER that reads exactly like a real SQL-lowering bug.
// This check turns that into a named, deterministic assertion made
// against the fixture's own seed text, with no dependence on which
// sibling ran first.
//
// A single-table scan (`FROM <table>`) needs no such gate: an unseeded
// table fails with UNKNOWN_TABLE, which nobody mistakes for a lowering
// bug. `merge()` is the shape that fails confusingly, so it is the shape
// checked here.
//
// # What it asserts
//
// For every `merge()` fan-out in emittedSQL:
//
//  1. every arm named by the fan-out regex is CREATEd by seed itself;
//  2. every column emittedSQL references off a base relation is declared
//     by at least one of those arms — "at least one" rather than "all"
//     because `merge()` unions columns, and production's metric tables
//     legitimately differ (only the histogram tables carry `Count` /
//     `Sum`, and inventing them on the gauge table would mask a real
//     bug).
//
// A fan-out regex this function cannot decompose is an error, never a
// pass: a check that quietly skips what it does not understand is a gap
// wearing a green tick.
//
// The referenced-column set is derived textually and is deliberately
// UNDER-approximate: an identifier that appears anywhere as an `AS`
// target, as a `FROM`/`JOIN` operand, or as a `WITH <name> AS (…)` CTE
// head is dropped wholesale, because those are alias/relation names
// rather than base columns. In practice that discards exactly the four
// canonical output names the emitter aliases to (`MetricName`,
// `Attributes`, `TimeUnix`, `Value`) — the columns no seed omits — and
// keeps the ones seeds do omit (`ResourceAttributes`, `ServiceName`,
// `Count`, `Sum`). Under-approximating is the safe direction: it can
// only fail to flag, never flag something the query does not need.
func CheckSeedCoversFanOut(seed, emittedSQL string) error {
	fanOuts, err := fanOutArms(emittedSQL)
	if err != nil {
		return err
	}
	if len(fanOuts) == 0 {
		return nil
	}
	seeded := seededTables(seed)
	referenced := referencedColumns(emittedSQL)

	var problems []string
	for _, arms := range fanOuts {
		problems = append(problems, fanOutProblems(arms, seeded, referenced)...)
	}
	if len(problems) == 0 {
		return nil
	}
	slices.Sort(problems)
	return fmt.Errorf("seed is not self-sufficient for the emitted merge() fan-out:\n  %s",
		strings.Join(slices.Compact(problems), "\n  "))
}

// fanOutProblems reports every way `seed` fails to stand alone for one
// fan-out: arms it never created, and referenced columns no created arm
// declares.
func fanOutProblems(arms []string, seeded map[string][]string, referenced []string) []string {
	var problems []string
	covered := map[string]bool{}
	for _, arm := range arms {
		cols, ok := seeded[arm]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"fan-out arm %q is scanned but never created by this seed — it resolves only because a sibling fixture created it in the shared session",
				arm,
			))
			continue
		}
		for _, c := range cols {
			covered[c] = true
		}
	}
	for _, col := range referenced {
		if covered[col] {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"column %q is referenced by the emitted SQL but declared by none of the fan-out arms this seed creates (%s)",
			col, strings.Join(arms, ", "),
		))
	}
	return problems
}

// mergeCall is the ClickHouse table function whose regex argument names
// the fan-out arms.
const mergeCall = "merge"

// fanOutArms returns one entry per `merge(<db>, '<regex>')` call in sql,
// each holding that call's arm table names in regex order.
func fanOutArms(sql string) ([][]string, error) {
	var out [][]string
	for i := 0; i < len(sql); i++ {
		if sql[i] == '\'' {
			// A `merge(` spelled inside a SQL string literal is data, not
			// a call; skip the whole literal.
			i = skipStringLiteral(sql, i)
			continue
		}
		start, ok := mergeCallAt(sql, i)
		if !ok {
			continue
		}
		open := start + len(mergeCall)
		closeParen := matchParen(sql, open)
		if closeParen < 0 {
			return nil, fmt.Errorf("unbalanced merge() call at offset %d in emitted SQL", start)
		}
		args := splitTopLevelCommas(sql[open+1 : closeParen])
		const wantArgs = 2 // merge(<database>, '<table-name regex>')
		if len(args) != wantArgs {
			return nil, fmt.Errorf("merge() at offset %d takes %d arguments, want %d: %q",
				start, len(args), wantArgs, sql[start:closeParen+1])
		}
		arms, err := armsFromPattern(strings.TrimSpace(args[1]))
		if err != nil {
			return nil, err
		}
		out = append(out, arms)
		i = closeParen
	}
	return out, nil
}

// mergeCallAt reports whether a `merge(` call token starts at index i —
// i.e. the keyword is not the tail of a longer identifier such as
// `arrayMerge(`.
func mergeCallAt(sql string, i int) (int, bool) {
	if !strings.HasPrefix(sql[i:], mergeCall+"(") {
		return 0, false
	}
	if i > 0 && isIdentByte(sql[i-1]) {
		return 0, false
	}
	return i, true
}

// armsFromPattern decomposes the single-quoted RE2 literal chsql's
// mergeTableFrag emits — `'^(a|b|c)$'`, each arm regex-escaped — back
// into its arm names. Anything it cannot decompose is an error rather
// than an empty result, so an emitter change to the fan-out shape
// breaks this check loudly instead of silently disarming it.
func armsFromPattern(lit string) ([]string, error) {
	if len(lit) < 2 || lit[0] != '\'' || lit[len(lit)-1] != '\'' {
		return nil, fmt.Errorf("merge() table pattern %q is not a single-quoted literal", lit)
	}
	pattern := strings.ReplaceAll(lit[1:len(lit)-1], "''", "'")
	body, ok := strings.CutPrefix(pattern, "^")
	if !ok {
		return nil, fmt.Errorf("merge() table pattern %q is not anchored at ^", pattern)
	}
	body, ok = strings.CutSuffix(body, "$")
	if !ok {
		return nil, fmt.Errorf("merge() table pattern %q is not anchored at $", pattern)
	}
	if inner, ok := strings.CutPrefix(body, "("); ok {
		body, ok = strings.CutSuffix(inner, ")")
		if !ok {
			return nil, fmt.Errorf("merge() table pattern %q has an unbalanced alternation group", pattern)
		}
	}
	var arms []string
	for _, raw := range strings.Split(body, "|") {
		arm := strings.ReplaceAll(raw, `\`, "")
		if arm == "" || strings.ContainsFunc(arm, func(r rune) bool { return r > unicode.MaxASCII || !isIdentByte(byte(r)) }) {
			return nil, fmt.Errorf("merge() table pattern %q holds a non-literal arm %q that this check cannot resolve to a table name", pattern, raw)
		}
		arms = append(arms, arm)
	}
	return arms, nil
}

// seededTables maps each table a seed script CREATEs to its declared
// column names, lower-cased on the table name to match the fan-out arms
// (ClickHouse table names are case-sensitive, but the seeds and the
// schema both spell them lower-case; normalising avoids a spurious miss
// on a differently-cased seed).
func seededTables(seed string) map[string][]string {
	out := map[string][]string{}
	for _, stmt := range SplitStatements(seed) {
		trimmed := stripLeadingNoise(stmt)
		rest, ok := createTableTail(trimmed)
		if !ok {
			continue
		}
		name := strings.ToLower(unquoteIdent(strings.TrimSpace(firstToken(rest))))
		open := strings.IndexByte(stmt, '(')
		if open < 0 {
			continue
		}
		closeParen := matchParen(stmt, open)
		if closeParen < 0 {
			continue
		}
		var cols []string
		for _, def := range splitTopLevelCommas(stmt[open+1 : closeParen]) {
			if cn := unquoteIdent(firstToken(strings.TrimSpace(def))); cn != "" {
				cols = append(cols, cn)
			}
		}
		out[name] = cols
	}
	return out
}

// referencedColumns returns the sorted set of backtick-quoted
// identifiers in sql that name a base-relation column: every quoted
// identifier, minus those that appear anywhere as an `AS` target, as a
// `FROM`/`JOIN` operand, or as a `WITH <name> AS (…)` CTE head. See
// [CheckSeedCoversFanOut] for why the subtraction is applied globally
// (and is therefore under-approximate).
func referencedColumns(sql string) []string {
	seen := map[string]bool{}
	notColumn := map[string]bool{}
	for i := 0; i < len(sql); i++ {
		switch sql[i] {
		case '\'':
			i = skipStringLiteral(sql, i)
		case '`':
			end := strings.IndexByte(sql[i+1:], '`')
			if end < 0 {
				return sortedKeys(seen, notColumn)
			}
			name := sql[i+1 : i+1+end]
			seen[name] = true
			if precededByKeyword(sql[:i]) || isCTEHead(sql[i+1+end+1:]) {
				notColumn[name] = true
			}
			i += end + 1
		}
	}
	return sortedKeys(seen, notColumn)
}

// relationKeywords introduce a name that is an alias or a relation
// rather than a base column.
var relationKeywords = []string{"AS", "FROM", "JOIN"}

// precededByKeyword reports whether the text immediately before a quoted
// identifier ends with one of [relationKeywords].
func precededByKeyword(before string) bool {
	trimmed := strings.TrimRight(before, " \t\n\r")
	if len(trimmed) == len(before) {
		return false // the identifier is glued to the previous token
	}
	upper := strings.ToUpper(trimmed)
	for _, kw := range relationKeywords {
		if !strings.HasSuffix(upper, kw) {
			continue
		}
		head := upper[:len(upper)-len(kw)]
		if head == "" || !isIdentByte(head[len(head)-1]) {
			return true
		}
	}
	return false
}

// isCTEHead reports whether the text immediately after a quoted
// identifier opens a `AS (` CTE body — ClickHouse's `WITH <name> AS
// (<body>)` form, where the name precedes the AS.
func isCTEHead(after string) bool {
	rest := strings.TrimLeft(after, " \t\n\r")
	rest, ok := strings.CutPrefix(strings.ToUpper(rest), "AS")
	if !ok {
		return false
	}
	trimmed := strings.TrimLeft(rest, " \t\n\r")
	return len(trimmed) != len(rest) && strings.HasPrefix(trimmed, "(")
}

// sortedKeys returns the members of seen absent from excluded, sorted.
func sortedKeys(seen, excluded map[string]bool) []string {
	out := make([]string, 0, len(seen))
	for name := range seen {
		if !excluded[name] {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// skipStringLiteral returns the index of the quote closing the SQL string
// literal that opens at start (which must index a `'`), or len(s)-1 when
// the literal is unterminated. Both escape conventions chsql emits are
// honoured: SQL-standard `”` doubling (escapeSingleQuotes, used for the
// merge() regex) and backslash escaping (InlineLit).
func skipStringLiteral(s string, start int) int {
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '\'':
			if i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			return i
		}
	}
	return len(s) - 1
}

// unquoteIdent strips the backtick quoting a seed may wrap an identifier
// in.
func unquoteIdent(s string) string {
	return strings.Trim(strings.TrimSpace(s), "`")
}

// isIdentByte reports whether c may appear inside an unquoted SQL
// identifier.
func isIdentByte(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}
