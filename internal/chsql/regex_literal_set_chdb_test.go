//go:build chdb

// chDB-backed proof that a regex label matcher rewritten to set membership
// answers what match() over its anchored pattern answers.
//
// A matcher reaches ClickHouse through one of two forms: match() over the
// `^(?:…)$` pattern [anchoredRegexPattern] builds, or — when that pattern's
// language is a finite set of literals ([anchoredRegexLiteralSet]) —
// `subject IN (set)`. The rewrite is sound only if the two forms select
// the same rows. This test runs both forms, for `=~` and `!~`, over one
// seeded table and compares them row by row. The table holds an absent
// label, which reads as the empty string, the empty value itself, invalid
// UTF-8, and the near misses of every set member: the member repeated,
// extended on either side, case-flipped, cut short, and spelled with an
// invalid byte where it holds U+FFFD.
package chsql

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

// literalSetEquivalencePatterns are the matcher patterns compared. A
// pattern with members has a finite language, written out by hand: it must
// render as set membership, and each member and its near misses are
// probed. The members never come from the code under test, so a rewrite
// that drops one is probed on the member it dropped. A pattern without
// members has an infinite or engine-dependent language and renders as
// match(): it is there to catch a rewrite accepting a pattern it must not,
// whose membership form would then miss a probed value.
var literalSetEquivalencePatterns = []struct {
	pattern string
	members []string
}{
	{"", []string{""}},
	{"api", []string{"api"}},
	{"api|web", []string{"api", "web"}},
	{"api|", []string{"api", ""}},
	{"prod|stg", []string{"prod", "stg"}},
	{`a\.b|c`, []string{"a.b", "c"}},
	{"api|api", []string{"api"}},
	{"api|apx", []string{"api", "apx"}},
	{"ab(?:c)?", []string{"abc", "ab"}},
	{"(a|b)(c|)", []string{"ac", "a", "bc", "b"}},
	{"x?y?", []string{"xy", "x", "y", ""}},
	{`\d`, []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9"}},
	{`[ab]-[12]`, []string{"a-1", "a-2", "b-1", "b-2"}},
	{"a{2}", []string{"aa"}},
	{"a{1,2}", []string{"a", "aa"}},
	{`\|`, []string{"|"}},
	{"é|ü", []string{"é", "ü"}},
	{"日本", []string{"日本"}},
	{"a+", nil},
	{"a*", nil},
	{`\d+`, nil},
	{"a{2,}", nil},
	{"(?:ab)+", nil},
	{"a.b", nil},
	{"api.*", nil},
	{"(?i)api", nil},
	{"api|(?i:web)", nil},
	{"^api$", nil},
	{"a^b", nil},
	{"a$b", nil},
	{"a(?m:^)b", nil},
	{"a(?m:$)b", nil},
	{"(?m)^api$", nil},
	{`\bapi`, nil},
	{"a\uFFFD|b", nil},
	{`[\x{FFFD}x]`, nil},
	{`[x\x{D800}]`, nil},
	{`[a-z][a-z][a-z]`, nil},
	{`a)\Q`, nil},
}

// literalSetEquivalenceValues are the label values every pattern is
// compared on, besides the absent label and the near misses of each set
// member.
var literalSetEquivalenceValues = []string{
	"", "a", "aa", "aaa", "b", "ab", "abab", "abc", "c", "x", "y", "xy", "yy",
	"api", "API", "Api", "apx", "apix", "xapi", "api ", "ap", "api\n", "\napi",
	"web", "WEB", "api|web", "|", "prod", "production", "stg", "a.b", "axb",
	"0", "5", "9", "00", "a-1", "b-2", "a-12", "é", "É", "ü", "日本", "日本日本",
	"\uFFFD", "a\uFFFD", "\xff", "a\xff", "\xc3", "aab", "a\nb", "\xed\xa0\x80",
}

func TestRegexLiteralSet_AnswersAsAnchoredMatch_ChDB(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	const labelKey = "k"
	subject := &chplan.MapAccess{Map: &chplan.ColumnRef{Name: "Attributes"}, Key: &chplan.LitString{V: labelKey}}

	// The probes are the hand-written members' near misses and, so that a
	// rewrite adding a member is probed on it too, those of every set the
	// rewrite returns.
	values := slices.Clone(literalSetEquivalenceValues)
	for _, c := range literalSetEquivalencePatterns {
		reference := regexp.MustCompile("^(?s:" + c.pattern + ")$")
		for _, m := range c.members {
			if !reference.MatchString(m) {
				t.Fatalf("pattern %q: listed member %q is not in its language", c.pattern, m)
			}
			values = append(values, literalSetNearMisses(m)...)
		}
		anchored := anchoredRegexPattern(&chplan.LitString{V: c.pattern}).(*chplan.LitString).V
		if set, ok := anchoredRegexLiteralSet(anchored); ok {
			for _, m := range set {
				values = append(values, literalSetNearMisses(m)...)
			}
		}
	}
	values = dedupeStable(values)

	if _, err := db.Exec("CREATE TABLE vals (i UInt32, Attributes Map(String, String)) ENGINE = Memory"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	// Row 0 has no label at all: the map lookup reads it as ''.
	if _, err := db.Exec("INSERT INTO vals VALUES (0, map())"); err != nil {
		t.Fatalf("insert absent label: %v", err)
	}
	for i, v := range values {
		if _, err := db.Exec("INSERT INTO vals VALUES (?, map(?, ?))", i+1, labelKey, v); err != nil {
			t.Fatalf("insert %q: %v", v, err)
		}
	}
	// The comparison is only as good as the seed: read every value back and
	// require it byte for byte, so an invalid UTF-8 value the driver
	// re-encoded on the way in cannot pass for the value it was meant to be.
	seeded, err := db.Query("SELECT i, Attributes[?] FROM vals ORDER BY i", labelKey)
	if err != nil {
		t.Fatalf("read back seed: %v", err)
	}
	readBack := 0
	for seeded.Next() {
		var i uint32
		var v string
		if err := seeded.Scan(&i, &v); err != nil {
			t.Fatalf("scan seed: %v", err)
		}
		want := ""
		if i > 0 {
			want = values[i-1]
		}
		if v != want {
			t.Fatalf("seed row %d holds %q, want %q", i, v, want)
		}
		readBack++
	}
	if err := seeded.Err(); err != nil {
		t.Fatalf("read back seed: %v", err)
	}
	_ = seeded.Close()
	if want := len(values) + 1; readBack != want {
		t.Fatalf("seed holds %d rows, want %d", readBack, want)
	}

	for _, c := range literalSetEquivalencePatterns {
		t.Run(c.pattern, func(t *testing.T) {
			anchored := anchoredRegexPattern(&chplan.LitString{V: c.pattern})
			rewritten := c.members != nil
			if _, ok := anchoredRegexLiteralSet(anchored.(*chplan.LitString).V); rewritten && !ok {
				t.Fatalf("pattern %q is not rewritten to a literal set, so this would compare match() with itself", c.pattern)
			}
			var cols []string
			var args []any
			for _, op := range []chplan.BinaryOp{chplan.OpMatch, chplan.OpNotMatch} {
				b := NewBuilder()
				if err := b.Expr(&chplan.Binary{Op: op, Left: subject, Right: &chplan.LitString{V: c.pattern}}); err != nil {
					t.Fatalf("render %v %q: %v", op, c.pattern, err)
				}
				emitted, emittedArgs := b.String(), b.Args()
				if rewritten && strings.Contains(emitted, "match(") {
					t.Fatalf("pattern %q: rewritten form still calls match(): %s", c.pattern, emitted)
				}
				control, controlArgs := Render(anchoredRegexMatch(op, func(fb *Builder) { _ = fb.Expr(subject) }, anchored))
				cols = append(cols, emitted, control)
				args = append(append(args, emittedArgs...), controlArgs...)
			}
			query := "SELECT i, " + strings.Join(cols, ", ") + " FROM vals ORDER BY i"
			rows, err := db.Query(query, args...)
			if err != nil {
				t.Fatalf("pattern %q: %v\n--- sql ---\n%s", c.pattern, err, query)
			}
			defer rows.Close()
			seen := 0
			for rows.Next() {
				var i uint32
				var match, matchControl, notMatch, notMatchControl uint8
				if err := rows.Scan(&i, &match, &matchControl, &notMatch, &notMatchControl); err != nil {
					t.Fatalf("pattern %q: scan: %v", c.pattern, err)
				}
				seen++
				subjectDesc := "absent label"
				if i > 0 {
					subjectDesc = fmt.Sprintf("value %q", values[i-1])
				}
				if match != matchControl {
					t.Errorf("pattern %q, %s: =~ emitted %d, match() over %q answers %d",
						c.pattern, subjectDesc, match, anchored.(*chplan.LitString).V, matchControl)
				}
				if notMatch != notMatchControl {
					t.Errorf("pattern %q, %s: !~ emitted %d, NOT match() over %q answers %d",
						c.pattern, subjectDesc, notMatch, anchored.(*chplan.LitString).V, notMatchControl)
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("pattern %q: rows: %v", c.pattern, err)
			}
			if want := len(values) + 1; seen != want {
				t.Fatalf("pattern %q: compared %d rows, want %d", c.pattern, seen, want)
			}
		})
	}
}
