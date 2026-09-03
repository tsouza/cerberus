package chclient

import (
	"sort"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// TestFormatArg_SupportedTypes pins the closed set of Go types the columnar
// binder renders into ClickHouse SQL literals (cerberus issue #2991).
//
// formatArg exists because the columnar path binds arguments ITSELF rather
// than handing them to clickhouse-go's binder, so its rendering has to agree
// with clickhouse-go's format() for every type the matrix path emits. A
// disagreement is not a crash: it is a query that runs and returns the wrong
// rows, on the optimised path only, while the row path stays right — the
// hardest possible shape to notice. Each supported type is therefore pinned
// to its exact literal.
func TestFormatArg_SupportedTypes(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string", "abc", `'abc'`},
		{"empty string", "", `''`},
		{"bool true renders as 1", true, "1"},
		{"bool false renders as 0", false, "0"},
		{"int", int(-7), "-7"},
		{"int32", int32(-7), "-7"},
		{"int64", int64(-9007199254740993), "-9007199254740993"},
		{"uint64", uint64(18446744073709551615), "18446744073709551615"},
		{"float32", float32(1.5), "1.5"},
		{"float64", float64(-0.25), "-0.25"},
		{"nil renders as NULL", nil, "NULL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := formatArg(tc.in)
			if !ok {
				t.Fatalf("formatArg(%#v) declined; want the literal %s", tc.in, tc.want)
			}
			if got != tc.want {
				t.Errorf("formatArg(%#v) = %s; want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestFormatArg_EscapesStringLiterals pins the escaping that keeps a string
// argument INSIDE its literal. A single quote that reaches the SQL unescaped
// closes the literal early and the remainder of the value is parsed as SQL —
// the columnar path builds its statement by concatenation, so this is the
// one place where an unescaped byte turns a value into syntax.
//
// Both characters clickhouse-go's stringQuoteReplacer handles are checked,
// including the case that a naive single-pass escaper gets wrong: a
// backslash immediately before a quote, where escaping the quote without
// also escaping the backslash leaves `\\'` and re-opens the hole.
func TestFormatArg_EscapesStringLiterals(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"single quote", `a'b`, `'a\'b'`},
		{"backslash", `a\b`, `'a\\b'`},
		{"backslash before quote", `a\'b`, `'a\\\'b'`},
		{"quote terminator attempt", `x' OR 1=1 --`, `'x\' OR 1=1 --'`},
		{"trailing backslash", `x\`, `'x\\'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := formatArg(tc.in)
			if !ok {
				t.Fatalf("formatArg(%q) declined; want %s", tc.in, tc.want)
			}
			if got != tc.want {
				t.Errorf("formatArg(%q) = %s; want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestFormatArg_DeclinesUnhandledTypes pins the fail-closed half of the
// contract formatArg's doc states: any type outside the closed set returns
// false so queryCursorColumnar falls back to the row path. The failure mode
// this prevents is a default arm that stringified an unknown value into
// something that happens to parse — the columnar decode is an optimisation
// and must never be a correctness gamble, so declining is the only allowed
// answer.
func TestFormatArg_DeclinesUnhandledTypes(t *testing.T) {
	type custom struct{ A int }
	unhandled := []any{
		uint(1), uint8(1), uint16(1), uint32(1), int8(1), int16(1),
		[]string{"a"},
		[]byte("a"),
		map[string]string{"a": "b"},
		custom{A: 1},
		&custom{A: 1},
		[]any{1, 2},
	}
	for _, v := range unhandled {
		got, ok := formatArg(v)
		if ok {
			t.Errorf("formatArg(%#v) = %s, true; want it declined so the caller falls back to the row path", v, got)
		}
		if got != "" {
			t.Errorf("formatArg(%#v) returned literal %q alongside false; want the empty string", v, got)
		}
	}
}

// TestBindArgs_Positional pins the substitution itself: each `?` takes the
// NEXT argument, in order, and nothing else in the statement moves. Argument
// values are deliberately distinguishable from one another so a binder that
// bound them out of order, or bound one argument twice, lands a visibly
// wrong statement rather than a plausible one.
func TestBindArgs_Positional(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		args []any
		want string
	}{
		{
			name: "no placeholders and no args passes through",
			sql:  "SELECT 1",
			args: nil,
			want: "SELECT 1",
		},
		{
			name: "single placeholder",
			sql:  "SELECT * FROM t WHERE a = ?",
			args: []any{int64(1)},
			want: "SELECT * FROM t WHERE a = 1",
		},
		{
			name: "arguments bind in order",
			sql:  "SELECT * FROM t WHERE a = ? AND b = ? AND c = ?",
			args: []any{int64(11), int64(22), int64(33)},
			want: "SELECT * FROM t WHERE a = 11 AND b = 22 AND c = 33",
		},
		{
			name: "mixed types",
			sql:  "SELECT ?, ?, ?, ?",
			args: []any{"s", true, float64(2.5), nil},
			want: "SELECT 's', 1, 2.5, NULL",
		},
		{
			name: "escaped question mark is a literal, not a placeholder",
			sql:  `SELECT 'a\?b', ?`,
			args: []any{int64(5)},
			want: `SELECT 'a?b', 5`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := bindArgs(tc.sql, tc.args)
			if !ok {
				t.Fatalf("bindArgs(%q, %#v) declined; want %q", tc.sql, tc.args, tc.want)
			}
			if got != tc.want {
				t.Errorf("bindArgs(%q, %#v) = %q; want %q", tc.sql, tc.args, got, tc.want)
			}
		})
	}
}

// TestBindArgs_DeclinesOnMismatch pins every way bindArgs refuses. A binder
// that guessed instead of declining would emit a statement whose placeholder
// count and argument count disagree — either a stray literal `?` reaching
// ClickHouse, or a silently dropped predicate that WIDENS the result set.
// The second is the dangerous one: it returns rows, just the wrong ones, so
// it is asserted rather than left to the server to reject.
func TestBindArgs_DeclinesOnMismatch(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{"more placeholders than args", "SELECT ?, ?", []any{int64(1)}},
		{"more args than placeholders", "SELECT ?", []any{int64(1), int64(2)}},
		{"placeholder with no args at all", "SELECT ?", nil},
		{"unhandled arg type", "SELECT ?", []any{uint8(1)}},
		{"unhandled arg among handled ones", "SELECT ?, ?", []any{int64(1), []string{"x"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := bindArgs(tc.sql, tc.args)
			if ok {
				t.Errorf("bindArgs(%q, %#v) = %q, true; want it declined so queryCursorColumnar falls back to the row path", tc.sql, tc.args, got)
			}
			if got != "" {
				t.Errorf("bindArgs(%q, %#v) returned %q alongside false; want the empty string", tc.sql, tc.args, got)
			}
		})
	}
}

// TestBindArgs_EscapedArgumentCannotOpenAPlaceholder is the composition of
// the two halves above, and the reason they belong in one file: a string
// argument is escaped as it is written, and the writer does not re-scan what
// it has already emitted. So a value containing `?` cannot consume the NEXT
// argument, and a value containing `'` cannot swallow the rest of the
// statement. Both are checked against a following placeholder, because that
// is the only arrangement in which either failure is observable.
func TestBindArgs_EscapedArgumentCannotOpenAPlaceholder(t *testing.T) {
	got, ok := bindArgs("SELECT ?, ?", []any{`why? ' OR 1=1 --`, int64(42)})
	if !ok {
		t.Fatalf("bindArgs declined a pair of handled args")
	}
	want := `SELECT 'why? \' OR 1=1 --', 42`
	if got != want {
		t.Errorf("bindArgs = %q; want %q — the value's own `?` must not consume the next argument and its quote must stay escaped", got, want)
	}
}

// TestCHSettings pins the translation of the per-query clickhouse-go
// Settings map onto ch-go's []ch.Setting, which is what makes the columnar
// dial run under the SAME server settings as the row path. Two properties
// carry that guarantee: every key survives with its value stringified (a
// dropped key means the columnar path silently runs without a memory or
// execution-time bound the row path enforces), and every setting is marked
// Important — ch-go's flag for "the server must reject a setting it does not
// know" rather than ignore it, which is the difference between a failed
// query and a query that quietly ran unbounded.
func TestCHSettings(t *testing.T) {
	in := clickhouse.Settings{
		"max_memory_usage":       int64(1 << 30),
		"max_execution_time":     30,
		"timeout_overflow_mode":  "throw",
		"optimize_read_in_order": true,
	}
	got := chSettings(in)
	if len(got) != len(in) {
		t.Fatalf("chSettings returned %d settings; want %d (%+v)", len(got), len(in), got)
	}

	byKey := map[string]string{}
	for _, s := range got {
		if !s.Important {
			t.Errorf("chSettings key %q: Important = false; want true so the server rejects a setting it does not recognise instead of ignoring it", s.Key)
		}
		if _, dup := byKey[s.Key]; dup {
			t.Errorf("chSettings emitted key %q twice", s.Key)
		}
		byKey[s.Key] = s.Value
	}

	want := map[string]string{
		"max_memory_usage":       "1073741824",
		"max_execution_time":     "30",
		"timeout_overflow_mode":  "throw",
		"optimize_read_in_order": "true",
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if byKey[k] != want[k] {
			t.Errorf("chSettings[%q] = %q; want %q", k, byKey[k], want[k])
		}
	}
}

// TestCHSettings_EmptyYieldsNil pins the nil return an empty map produces.
// ch-go treats a nil settings slice as "send none"; an empty non-nil slice
// would be equivalent here, but the distinction is asserted because
// chSettings' doc promises nil and a caller comparing against nil to decide
// whether to take a settings-free dial path would silently change behaviour.
func TestCHSettings_EmptyYieldsNil(t *testing.T) {
	if got := chSettings(nil); got != nil {
		t.Errorf("chSettings(nil) = %+v; want nil", got)
	}
	if got := chSettings(clickhouse.Settings{}); got != nil {
		t.Errorf("chSettings(empty) = %+v; want nil", got)
	}
}
