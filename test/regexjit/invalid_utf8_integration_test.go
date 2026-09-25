//go:build integration

package regexjit

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/qlcommon"
)

// byteCorpus is the input of the byte-exact checks: values that are not
// valid UTF-8 — lone and consecutive invalid bytes, truncated sequences,
// overlong forms, an encoded surrogate — next to U+FFFD itself, private-use
// characters of planes 15 and 16 (where the emitter picks the block it
// restores invalid bytes through: characters of the top two blocks, of
// their bases and of their edges), and valid values, which must keep
// answering as before.
var byteCorpus = []string{
	"", "a", "api-7", "host-1", "a\nb", "café", "�",
	"\xff", "\xff\xfe", "a\xffb", "api\xff", "\x85", "caf\xe9", "\xe2\x82",
	"a\xe2\x82b", "\xed\xa0\x80", "\xc0\xaf", "\xf0\x90\x80", "�\xff",
	"host-\xff\xfe", "host-\n\xff", "é\xff", "\xff\n\xfe", "k\xffelvin",
	"api-\xff-7", "a-\xff\xfe-b", "\xffb\xfe", "x\U0010FF80\xff", "\U0010FFBF",
	"\xf4\x8f\xbf\xbf\xff", "\xe0\x80\x80", "\xf4\x90\x80\x80", "a\xed\xbf\xbfb",
	"\U0010FF00\xff\U0010FF81", "\xff\U0010FE80\U0010FEFF\xfe", "\U0010FE00\xff\uFFFD\U0010FFFD",
}

// byteTable holds byteCorpus: v as a column, as the `v` key of a map, and
// as the `v` key of a Nested element's map.
const byteTable = "rjit_bytes"

// byteMatcherPatterns are the label-matcher and line-filter patterns of the
// byte-exact checks: matcherCases' patterns and ones that count runes or
// name U+FFFD.
var byteMatcherPatterns = func() []string {
	out := []string{`..`, `^..$`, `\x{FFFD}+`, `[^\x{FFFD}]*`, `[\x{D800}-\x{DFFF}a]+`, `\p{Co}.*`, `k.?elvin`, `\bapi`, `api\b.*`, `[^a-z]*`}
	for _, c := range matcherCases {
		out = append(out, c.pattern)
	}
	return out
}()

// byteLiteralFFFDPatterns name U+FFFD as a literal. Prometheus's matcher
// compares a pattern's literal parts byte for byte where it can (the
// labels.FastRegexMatcher string matchers), so there U+FFFD matches only
// itself, while the patterns it hands to Go's regexp read U+FFFD as any
// invalid byte. The emitted matcher reads every pattern as Go's regexp
// does, and is held to that here (docs/compatibility.md, "Invalid UTF-8").
var byteLiteralFFFDPatterns = []string{`\x{FFFD}`, `a\x{FFFD}b`, `\x{FFFD}.*`}

// byteLabelReplaceCase is a label_replace the byte-exact checks run.
type byteLabelReplaceCase struct{ regex, repl string }

var byteLabelReplaceCases = func() []byteLabelReplaceCase {
	out := []byteLabelReplaceCase{
		{regex: `(.)(.)`, repl: `$2$1`},
		{regex: `(.*)-(.*)`, repl: `$2/$1`},
		{regex: `([^-]*)-?(.*)`, repl: `[$1|$2]`},
		{regex: `(.)(.)(.)(.)(.)(.)(.)(.)(.)(.*)`, repl: `$10<$1>`},
		{regex: `(.*)`, repl: `$0!`},
		{regex: `(?P<head>.)(.*)`, repl: `${head}`},
		{regex: `(\x{FFFD}+)(.*)`, repl: `[$1|$2]`},
		{regex: `([^\x{FFFD}]*)(.*)`, repl: `$1/$2`},
		{regex: `(\p{Co}*)(.*)`, repl: `$2$1`},
		{regex: `([^\x{10FF81}]*)(\x{10FF81}?)(.*)`, repl: `$3|$2|$1`},
	}
	for _, c := range labelReplaceCases {
		out = append(out, byteLabelReplaceCase{regex: c.regex, repl: c.repl})
	}
	return out
}()

// byteFnCase is a regex function call the byte-exact checks run over the
// v column.
type byteFnCase struct {
	fn      chplan.Fn
	pattern string
	repl    string
}

var byteFnCases = []byteFnCase{
	{fn: chplan.FnRegexMatch, pattern: `a.b`},
	{fn: chplan.FnRegexMatch, pattern: `^[^-]+$`},
	{fn: chplan.FnRegexExtractFirst, pattern: `a(.)`},
	{fn: chplan.FnRegexExtractFirst, pattern: `[^a-z]+`},
	{fn: chplan.FnRegexExtractFirst, pattern: `\x{FFFD}.`},
	{fn: chplan.FnRegexExtractAll, pattern: `.`},
	{fn: chplan.FnRegexExtractAll, pattern: `[^a-z]+`},
	{fn: chplan.FnRegexExtractAll, pattern: `[\x{FFFD}]`},
	{fn: chplan.FnRegexExtractAllGroupsHorizontal, pattern: `(\w*)(\W)`},
	{fn: chplan.FnRegexExtractAllGroupsHorizontal, pattern: `(.)(.)`},
	{fn: chplan.FnRegexReplaceFirst, pattern: `([^a-z])`, repl: `<$1>`},
	{fn: chplan.FnRegexReplaceFirst, pattern: `-([^-]*)`, repl: `+$1`},
	{fn: chplan.FnRegexReplaceAll, pattern: `[^a-z]`, repl: `_`},
	{fn: chplan.FnRegexReplaceAll, pattern: `([^\n])`, repl: `[$1]`},
	{fn: chplan.FnRegexReplaceAll, pattern: `\x{FFFD}`, repl: `?`},
	{fn: chplan.FnRegexExtractAll, pattern: `[\x{FFFD}\x{10FF81}]|\p{Co}`},
	{fn: chplan.FnRegexExtractAllGroupsHorizontal, pattern: `(\x{FFFD}?)([^\x{FFFD}])`},
	{fn: chplan.FnRegexReplaceFirst, pattern: `(\x{FFFD})(.)`, repl: `$2$1`},
	{fn: chplan.FnRegexReplaceAll, pattern: `\x{FFFD}.`, repl: `?`},
	{fn: chplan.FnRegexReplaceAll, pattern: `(\p{Co})(.?)`, repl: `<$2$1>`},
}

// TestRegexJIT_InvalidUTF8ShapesMatchGo renders every emitted regex
// shape with the production emitter, evaluates it over byteCorpus on every
// semantic build and in every compilation mode, and requires the raw bytes
// of each answer to be what Go's regexp — the reference engines' — computes:
// the rows a matcher or line filter selects, the value label_replace
// writes, and what each regex function returns.
func TestRegexJIT_InvalidUTF8ShapesMatchGo(t *testing.T) {
	for _, image := range semanticBuilds {
		t.Run(image, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			s := startServer(ctx, t, image, otherJITOffProfile())
			seedByteCorpus(ctx, t, s.admin.Conn())
			modes := []jitMode{jitDefault}
			if s.regexpJIT {
				modes = []jitMode{jitOff, jitNow}
			}
			for _, mode := range modes {
				t.Run(string(mode), func(t *testing.T) {
					checkByteShapes(byteModeCtx(ctx, s, mode), t, s.admin.Conn())
				})
			}
		})
	}
}

// byteModeCtx attaches mode's compiler settings, with the query condition
// cache off, as driver settings: the byte-exact checks query the server
// directly rather than through a handler.
func byteModeCtx(ctx context.Context, s *server, mode jitMode) context.Context {
	settings := clickhouse.Settings{}
	switch mode {
	case jitOff:
		settings[settingCompileRegexp] = 0
	case jitNow:
		settings[settingCompileRegexp] = 1
		settings[settingMinCountRegexp] = minCountCompileNow
	}
	if s.conditionCache {
		settings[settingConditionCache] = 0
	}
	return clickhouse.Context(ctx, clickhouse.WithSettings(settings))
}

func seedByteCorpus(ctx context.Context, t *testing.T, conn driver.Conn) {
	t.Helper()
	if err := conn.Exec(ctx, "CREATE TABLE "+byteTable+" (i UInt32, v String, m Map(String, String), Events Nested(Attributes Map(String, String))) ENGINE = MergeTree ORDER BY i"); err != nil {
		t.Fatalf("create %s: %v", byteTable, err)
	}
	batch, err := conn.PrepareBatch(ctx, "INSERT INTO "+byteTable)
	if err != nil {
		t.Fatalf("prepare insert: %v", err)
	}
	for i, v := range byteCorpus {
		m := map[string]string{probeLabel: v}
		if err := batch.Append(uint32(i), v, m, []map[string]string{m}); err != nil {
			t.Fatalf("append %q: %v", v, err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("insert %s: %v", byteTable, err)
	}
}

func checkByteShapes(ctx context.Context, t *testing.T, conn driver.Conn) {
	v := &chplan.ColumnRef{Name: probeLabel}
	for _, p := range byteMatcherPatterns {
		for _, op := range []chplan.BinaryOp{chplan.OpMatch, chplan.OpNotMatch} {
			mt := labels.MatchRegexp
			if op == chplan.OpNotMatch {
				mt = labels.MatchNotRegexp
			}
			m, err := labels.NewMatcher(mt, probeLabel, p)
			if err != nil {
				t.Fatalf("reference matcher %q: %v", p, err)
			}
			t.Run(fmt.Sprintf("matcher/%s%s", op, p), func(t *testing.T) {
				got := queryBools(ctx, t, conn, &chplan.Binary{Op: op, Left: v, Right: &chplan.LitString{V: p}})
				assertPerValue(t, got, func(s string) any { return m.Matches(s) })
			})
			t.Run(fmt.Sprintf("nested-matcher/%s%s", op, p), func(t *testing.T) {
				got := queryBools(ctx, t, conn, &chplan.NestedArrayExists{Column: "Events", SubField: "Attributes", Key: probeLabel, Op: op, Value: &chplan.LitString{V: p}})
				assertPerValue(t, got, func(s string) any { return m.Matches(s) })
			})
		}
		checkLineFilters(ctx, t, conn, p)
	}
	for _, p := range byteLiteralFFFDPatterns {
		re := regexp.MustCompile("^(?s:" + p + ")$")
		for _, op := range []chplan.BinaryOp{chplan.OpMatch, chplan.OpNotMatch} {
			t.Run(fmt.Sprintf("matcher/%s%s", op, p), func(t *testing.T) {
				got := queryBools(ctx, t, conn, &chplan.Binary{Op: op, Left: v, Right: &chplan.LitString{V: p}})
				assertPerValue(t, got, func(s string) any { return re.MatchString(s) == (op == chplan.OpMatch) })
			})
		}
		checkLineFilters(ctx, t, conn, p)
	}
	checkByteLabelReplace(ctx, t, conn)
	checkByteFns(ctx, t, conn)
}

// checkLineFilters runs p as a line filter and a negated one, held to
// Loki's unanchored Go-regexp match.
func checkLineFilters(ctx context.Context, t *testing.T, conn driver.Conn, p string) {
	v := &chplan.ColumnRef{Name: probeLabel}
	re := regexp.MustCompile(p)
	for _, negated := range []bool{false, true} {
		t.Run(fmt.Sprintf("line-filter/negated=%v/%s", negated, p), func(t *testing.T) {
			got := queryBools(ctx, t, conn, &chplan.LineContent{Source: v, Pattern: p, IsRegex: true, Negated: negated})
			assertPerValue(t, got, func(s string) any { return re.MatchString(s) != negated })
		})
	}
}

func checkByteLabelReplace(ctx context.Context, t *testing.T, conn driver.Conn) {
	for _, c := range byteLabelReplaceCases {
		t.Run(fmt.Sprintf("label_replace/%s/%s", c.regex, c.repl), func(t *testing.T) {
			ch, err := qlcommon.ReplacementToCH(c.repl, c.regex)
			if err != nil {
				t.Fatalf("lower replacement: %v", err)
			}
			lr := &chplan.LabelReplace{
				Map:              &chplan.ColumnRef{Name: "m"},
				Dst:              dstLabel,
				Replacement:      ch.Template,
				Segments:         ch.Segments,
				ProbedRegex:      ch.ProbedRegex,
				Src:              probeLabel,
				Regex:            c.regex,
				EmptyReplacement: qlcommon.EmptyCapturesReplacement(c.repl),
			}
			got := queryStrings(ctx, t, conn, &chplan.MapAccess{Map: lr, Key: &chplan.LitString{V: dstLabel}})
			re := regexp.MustCompile("^(?s:" + c.regex + ")$")
			assertPerValue(t, got, func(s string) any {
				return goLabelReplace(re, c.repl, s)
			})
		})
	}
}

// checkByteFns runs byteFnCases, each held to Go's regexp reading the
// pattern as ClickHouse does: match and the extract family read `.` as
// matching a newline, and the replaceRegexp pair as the server at hand
// reads it (the 24.8 floor does not match a newline there, later builds
// do).
func checkByteFns(ctx context.Context, t *testing.T, conn driver.Conn) {
	v := &chplan.ColumnRef{Name: probeLabel}
	replaceFlag := "(?-s)"
	if replaceDotMatchesNewline(ctx, t, conn) {
		replaceFlag = "(?s)"
	}
	for _, c := range byteFnCases {
		t.Run(fmt.Sprintf("fn/%s/%s/%s", c.fn, c.pattern, c.repl), func(t *testing.T) {
			args := []chplan.Expr{v, &chplan.LitString{V: c.pattern}}
			flag := "(?s)"
			if c.fn == chplan.FnRegexReplaceFirst || c.fn == chplan.FnRegexReplaceAll {
				args = append(args, &chplan.LitString{V: chReplacement(c.repl)})
				flag = replaceFlag
			}
			call := &chplan.FuncCall{Fn: c.fn, Args: args}
			re := regexp.MustCompile(flag + c.pattern)
			var got []any
			switch c.fn {
			case chplan.FnRegexMatch:
				got = queryBools(ctx, t, conn, call)
			case chplan.FnRegexExtractAll:
				got = queryAny[[]string](ctx, t, conn, call)
			case chplan.FnRegexExtractAllGroupsHorizontal:
				got = queryAny[[][]string](ctx, t, conn, call)
			default:
				got = queryStrings(ctx, t, conn, call)
			}
			assertPerValue(t, got, func(s string) any { return goRegexFn(re, c.fn, c.repl, s) })
		})
	}
}

// replaceDotMatchesNewline asks the server how its replaceRegexp functions
// read `.`: whether it matches a newline.
func replaceDotMatchesNewline(ctx context.Context, t *testing.T, conn driver.Conn) bool {
	t.Helper()
	var got string
	if err := conn.QueryRow(ctx, "SELECT replaceRegexpOne('\\n', '.', 'x')").Scan(&got); err != nil {
		t.Fatalf("probe replaceRegexp's reading of `.`: %v", err)
	}
	return got == "x"
}

// goLabelReplace is Prometheus's funcLabelReplace for one source value:
// the expanded replacement when re matches, else no label.
func goLabelReplace(re *regexp.Regexp, repl, s string) string {
	idx := re.FindStringSubmatchIndex(s)
	if idx == nil {
		return ""
	}
	return string(re.ExpandString(nil, repl, s, idx))
}

// goRegexFn is what fn returns over s, computed with Go's regexp.
func goRegexFn(re *regexp.Regexp, fn chplan.Fn, repl, s string) any {
	switch fn {
	case chplan.FnRegexMatch:
		return re.MatchString(s)
	case chplan.FnRegexExtractFirst:
		m := re.FindStringSubmatch(s)
		switch {
		case m == nil:
			return ""
		case len(m) > 1:
			return m[1]
		}
		return m[0]
	case chplan.FnRegexExtractAll:
		out := []string{}
		for _, m := range re.FindAllStringSubmatch(s, -1) {
			if len(m) > 1 {
				out = append(out, m[1])
			} else {
				out = append(out, m[0])
			}
		}
		return out
	case chplan.FnRegexExtractAllGroupsHorizontal:
		out := make([][]string, re.NumSubexp())
		for g := range out {
			out[g] = []string{}
		}
		for _, m := range re.FindAllStringSubmatch(s, -1) {
			for g := range out {
				out[g] = append(out[g], m[g+1])
			}
		}
		return out
	case chplan.FnRegexReplaceFirst:
		idx := re.FindStringSubmatchIndex(s)
		if idx == nil {
			return s
		}
		return s[:idx[0]] + string(re.ExpandString(nil, repl, s, idx)) + s[idx[1]:]
	case chplan.FnRegexReplaceAll:
		return re.ReplaceAllString(s, repl)
	}
	panic("unhandled " + string(fn))
}

// chReplacement spells a Go replacement template that references groups
// only as `$<digit>` in ClickHouse's `\<digit>` syntax.
func chReplacement(goRepl string) string {
	return regexp.MustCompile(`\$(\d)`).ReplaceAllString(goRepl, `\$1`)
}

// query renders e with the production emitter and returns its value on
// every byteTable row, in byteCorpus order.
func queryAny[T any](ctx context.Context, t *testing.T, conn driver.Conn, e chplan.Expr) []any {
	t.Helper()
	b := chsql.NewBuilder()
	if err := b.Expr(e); err != nil {
		t.Fatalf("render: %v", err)
	}
	expr, args, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	sql := "SELECT " + expr + " FROM " + byteTable + " ORDER BY i"
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		t.Fatalf("query: %v\n%s", err, sql)
	}
	defer func() { _ = rows.Close() }()
	var out []any
	for rows.Next() {
		var v T
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v\n%s", err, sql)
	}
	if len(out) != len(byteCorpus) {
		t.Fatalf("%d rows; want %d", len(out), len(byteCorpus))
	}
	return out
}

func queryStrings(ctx context.Context, t *testing.T, conn driver.Conn, e chplan.Expr) []any {
	t.Helper()
	return queryAny[string](ctx, t, conn, e)
}

// queryBools reads a UInt8 predicate as booleans.
func queryBools(ctx context.Context, t *testing.T, conn driver.Conn, e chplan.Expr) []any {
	t.Helper()
	got := queryAny[uint8](ctx, t, conn, e)
	for i, v := range got {
		got[i] = v.(uint8) != 0
	}
	return got
}

// assertPerValue requires got, one answer per byteCorpus value, to equal
// want of that value.
func assertPerValue(t *testing.T, got []any, want func(string) any) {
	t.Helper()
	for i, s := range byteCorpus {
		if w := want(s); !reflect.DeepEqual(got[i], w) {
			t.Errorf("on %q: ClickHouse answered %#v; Go's regexp answers %#v", s, got[i], w)
		}
	}
}
