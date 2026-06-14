package loki

import (
	"bytes"
	"testing"
	"text/template"
	"time"

	loglib "github.com/grafana/loki/v3/pkg/logql/log"
)

// renderInline evaluates the template body against the dot value and
// returns the rendered string. Helper for per-func tests so each case
// stays focused on the func semantics, not template scaffolding.
//
// The funcmap is the full cerberus surface (sprig allow-list +
// Loki-native + __line__ / __timestamp__) with empty capture closures —
// per-func tests that don't exercise __line__ / __timestamp__ don't
// need real captures.
func renderInline(t *testing.T, body string, dot any) string {
	t.Helper()
	funcs := templateFuncs(func() string { return "" }, func() int64 { return 0 })
	tpl, err := template.New("t").Funcs(funcs).Parse(body)
	if err != nil {
		t.Fatalf("parse %q: %v", body, err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, dot); err != nil {
		t.Fatalf("exec %q: %v", body, err)
	}
	return buf.String()
}

func TestTemplateFunc_LowerUpper(t *testing.T) {
	t.Parallel()
	if got := renderInline(t, `{{ lower "FOO" }}`, nil); got != "foo" {
		t.Errorf("lower: got %q", got)
	}
	if got := renderInline(t, `{{ upper "foo" }}`, nil); got != "FOO" {
		t.Errorf("upper: got %q", got)
	}
	if got := renderInline(t, `{{ ToLower "FOO" }}`, nil); got != "foo" {
		t.Errorf("ToLower alias: got %q", got)
	}
}

func TestTemplateFunc_TitleAndSubstr(t *testing.T) {
	t.Parallel()
	// title — Loki uses strings.Title which capitalizes per-word.
	if got := renderInline(t, `{{ title "hello world" }}`, nil); got != "Hello World" {
		t.Errorf("title: got %q", got)
	}
	// substr — sprig-compatible (start, end, src).
	if got := renderInline(t, `{{ substr 0 5 "hello world" }}`, nil); got != "hello" {
		t.Errorf("substr basic: got %q", got)
	}
	if got := renderInline(t, `{{ substr 6 11 "hello world" }}`, nil); got != "world" {
		t.Errorf("substr offset: got %q", got)
	}
	// trunc — positive
	if got := renderInline(t, `{{ trunc 4 "hello world" }}`, nil); got != "hell" {
		t.Errorf("trunc positive: got %q", got)
	}
	// trunc — negative takes last N
	if got := renderInline(t, `{{ trunc -5 "hello world" }}`, nil); got != "world" {
		t.Errorf("trunc negative: got %q", got)
	}
	// trunc overflow
	if got := renderInline(t, `{{ trunc 100 "short" }}`, nil); got != "short" {
		t.Errorf("trunc overflow: got %q", got)
	}
}

func TestTemplateFunc_TrimFamily(t *testing.T) {
	t.Parallel()
	if got := renderInline(t, `{{ trim "  hello  " }}`, nil); got != "hello" {
		t.Errorf("trim: got %q", got)
	}
	// trimPrefix(prefix, s) — sprig order, NOT stdlib (s, prefix).
	if got := renderInline(t, `{{ trimPrefix "foo-" "foo-bar" }}`, nil); got != "bar" {
		t.Errorf("trimPrefix: got %q", got)
	}
	if got := renderInline(t, `{{ trimSuffix "-bar" "foo-bar" }}`, nil); got != "foo" {
		t.Errorf("trimSuffix: got %q", got)
	}
	// trimAll(cutset, s) — sprig order.
	if got := renderInline(t, `{{ trimAll "x" "xxhelloxx" }}`, nil); got != "hello" {
		t.Errorf("trimAll: got %q", got)
	}
	// Capitalised stdlib aliases keep stdlib arg order.
	if got := renderInline(t, `{{ TrimPrefix "foo-bar" "foo-" }}`, nil); got != "bar" {
		t.Errorf("TrimPrefix: got %q", got)
	}
	if got := renderInline(t, `{{ TrimSpace "  hello  " }}`, nil); got != "hello" {
		t.Errorf("TrimSpace: got %q", got)
	}
}

func TestTemplateFunc_Replace(t *testing.T) {
	t.Parallel()
	// sprig: replace(old, new, src) — all-replace
	if got := renderInline(t, `{{ replace "a" "X" "banana" }}`, nil); got != "bXnXnX" {
		t.Errorf("replace: got %q", got)
	}
	// stdlib: Replace(s, old, new, n)
	if got := renderInline(t, `{{ Replace "banana" "a" "X" 2 }}`, nil); got != "bXnXna" {
		t.Errorf("Replace: got %q", got)
	}
}

func TestTemplateFunc_RegexReplaceAll(t *testing.T) {
	t.Parallel()
	if got := renderInline(t, `{{ regexReplaceAll "[aeiou]" "hello world" "*" }}`, nil); got != "h*ll* w*rld" {
		t.Errorf("regexReplaceAll: got %q", got)
	}
	// Literal version doesn't interpret `$` as backrefs.
	if got := renderInline(t, `{{ regexReplaceAllLiteral "world" "hello world" "$1" }}`, nil); got != "hello $1" {
		t.Errorf("regexReplaceAllLiteral: got %q", got)
	}
}

func TestTemplateFunc_Count(t *testing.T) {
	t.Parallel()
	if got := renderInline(t, `{{ count "o" "hello world" }}`, nil); got != "2" {
		t.Errorf("count: got %q", got)
	}
	if got := renderInline(t, `{{ count "\\d+" "a1 b22 c333" }}`, nil); got != "3" {
		t.Errorf("count digit-groups: got %q", got)
	}
}

func TestTemplateFunc_URL(t *testing.T) {
	t.Parallel()
	if got := renderInline(t, `{{ urlencode "a b&c" }}`, nil); got != "a+b%26c" {
		t.Errorf("urlencode: got %q", got)
	}
	if got := renderInline(t, `{{ urldecode "a+b%26c" }}`, nil); got != "a b&c" {
		t.Errorf("urldecode: got %q", got)
	}
}

func TestTemplateFunc_Predicates(t *testing.T) {
	t.Parallel()
	if got := renderInline(t, `{{ contains "ll" "hello" }}`, nil); got != "true" {
		t.Errorf("contains true: got %q", got)
	}
	if got := renderInline(t, `{{ contains "xx" "hello" }}`, nil); got != "false" {
		t.Errorf("contains false: got %q", got)
	}
	if got := renderInline(t, `{{ hasPrefix "he" "hello" }}`, nil); got != "true" {
		t.Errorf("hasPrefix: got %q", got)
	}
	if got := renderInline(t, `{{ hasSuffix "lo" "hello" }}`, nil); got != "true" {
		t.Errorf("hasSuffix: got %q", got)
	}
}

func TestTemplateFunc_Default(t *testing.T) {
	t.Parallel()
	if got := renderInline(t, `{{ default "fallback" .x }}`, map[string]string{"x": ""}); got != "fallback" {
		t.Errorf("default on empty: got %q", got)
	}
	if got := renderInline(t, `{{ default "fallback" .x }}`, map[string]string{"x": "set"}); got != "set" {
		t.Errorf("default on non-empty: got %q", got)
	}
}

func TestTemplateFunc_Repeat(t *testing.T) {
	t.Parallel()
	if got := renderInline(t, `{{ repeat 3 "ab" }}`, nil); got != "ababab" {
		t.Errorf("repeat: got %q", got)
	}
	if got := renderInline(t, `{{ repeat 0 "ab" }}`, nil); got != "" {
		t.Errorf("repeat 0: got %q", got)
	}
}

// TestTemplateFunc_FullSurfaceParse — the funcmap is now Loki's FULL
// surface (consumed verbatim via loglib.AddLineAndTimestampFunctions),
// so every name that previously failed at parse time with "function not
// defined" now parses and executes. This is the deliberate-subset
// reversal: these used to be wrong-rejected with a 400.
//
// Each case asserts cerberus renders exactly what reference Loki's
// funcmap renders (we share the same func implementations, so equality
// is by construction; the test pins it so a future regression that
// strips a func is caught).
func TestTemplateFunc_FullSurfaceParse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		dot  any
		want string
	}{
		// sprig math pipeline — the canonical wrong-rejection example
		// from the task: `int | add 1`.
		{"int_add", `{{ .x | int | add 1 }}`, map[string]string{"x": "41"}, "42"},
		{"sub", `{{ sub 10 3 }}`, nil, "7"},
		{"mul", `{{ mul 6 7 }}`, nil, "42"},
		{"div", `{{ div 22 7 }}`, nil, "3"},
		{"mod", `{{ mod 10 3 }}`, nil, "1"},
		{"addf", `{{ addf 1.5 2.25 }}`, nil, "3.75"},
		{"max", `{{ max 3 9 2 }}`, nil, "9"},
		{"min", `{{ min 3 9 2 }}`, nil, "2"},
		{"ceil", `{{ ceil 4.1 }}`, nil, "5"},
		{"floor", `{{ floor 4.9 }}`, nil, "4"},
		{"round", `{{ round 4.567 2 }}`, nil, "4.57"},
		// encoding
		{"b64enc", `{{ b64enc "hello" }}`, nil, "aGVsbG8="},
		{"b64dec", `{{ b64dec "aGVsbG8=" }}`, nil, "hello"},
		// indent / nindent
		{"indent", `{{ indent 2 "x" }}`, nil, "  x"},
		// fromJson
		{"fromJson", `{{ (fromJson "{\"a\":7}").a }}`, nil, "7"},
		// Loki-native: bytes / duration / duration_seconds
		{"bytes", `{{ bytes "1KB" }}`, nil, "1000"},
		{"duration", `{{ duration "1m30s" }}`, nil, "90"},
		{"duration_seconds", `{{ duration_seconds "2m" }}`, nil, "120"},
		// Loki-native: alignLeft / alignRight
		{"alignRight", `{{ alignRight 6 "hi" }}`, nil, "    hi"},
		{"alignLeft", `{{ alignLeft 4 "hi" }}`, nil, "hi  "},
		// Loki-native: unixEpochMillis over a __timestamp__ time.Time is
		// exercised in the __timestamp__ test; here just unixToTime.
		{"unixToTime", `{{ (unixToTime "1673798889").Year }}`, nil, "2023"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := renderInline(t, tc.body, tc.dot); got != tc.want {
				t.Errorf("%s: body %q got %q want %q", tc.name, tc.body, got, tc.want)
			}
		})
	}
}

// TestTemplateFunc_LineAndTimestampParity asserts cerberus binds
// __line__ and __timestamp__ to the SAME closures upstream Loki binds
// (loglib.AddLineAndTimestampFunctions): __line__ returns the current
// line and __timestamp__ returns time.Unix(0, ns) so a `| date` filter
// formats it. Nothing previously asserted the __timestamp__ VALUE — it
// was hardwired to the empty string.
func TestTemplateFunc_LineAndTimestampParity(t *testing.T) {
	t.Parallel()

	const ns = int64(1673798889_000000000) // 2023-01-15T16:08:09Z
	line := "the-current-line"

	funcs := templateFuncs(
		func() string { return line },
		func() int64 { return ns },
	)

	// __line__ renders the captured line verbatim.
	tplLine, err := template.New("l").Funcs(funcs).Parse(`[{{ __line__ }}]`)
	if err != nil {
		t.Fatalf("parse __line__: %v", err)
	}
	var bl bytes.Buffer
	if err := tplLine.Execute(&bl, nil); err != nil {
		t.Fatalf("exec __line__: %v", err)
	}
	if got, want := bl.String(), "["+line+"]"; got != want {
		t.Errorf("__line__: got %q want %q", got, want)
	}

	// __timestamp__ is a time.Time; piping through `date` formats it.
	tplTs, err := template.New("t").Funcs(funcs).Parse(`{{ __timestamp__ | date "2006-01-02T15:04:05Z07:00" }}`)
	if err != nil {
		t.Fatalf("parse __timestamp__: %v", err)
	}
	var bt bytes.Buffer
	if err := tplTs.Execute(&bt, nil); err != nil {
		t.Fatalf("exec __timestamp__: %v", err)
	}
	wantTs := time.Unix(0, ns).UTC().Format("2006-01-02T15:04:05Z07:00")
	if got := bt.String(); got != wantTs {
		t.Errorf("__timestamp__|date: got %q want %q", got, wantTs)
	}

	// unixEpochMillis over __timestamp__ — Loki-native func consuming the
	// time.Time directly.
	tplMs, err := template.New("ms").Funcs(funcs).Parse(`{{ __timestamp__ | unixEpochMillis }}`)
	if err != nil {
		t.Fatalf("parse unixEpochMillis: %v", err)
	}
	var bms bytes.Buffer
	if err := tplMs.Execute(&bms, nil); err != nil {
		t.Fatalf("exec unixEpochMillis: %v", err)
	}
	if got, want := bms.String(), "1673798889000"; got != want {
		t.Errorf("__timestamp__|unixEpochMillis: got %q want %q", got, want)
	}
}

// TestTemplateFunc_ParityWithUpstreamFuncmap pins that cerberus's
// funcmap is exactly upstream Loki's set — same keys — so a future
// upstream addition surfaces here rather than silently diverging.
func TestTemplateFunc_ParityWithUpstreamFuncmap(t *testing.T) {
	t.Parallel()
	cer := templateFuncs(func() string { return "" }, func() int64 { return 0 })
	up := loglib.AddLineAndTimestampFunctions(func() string { return "" }, func() int64 { return 0 })
	if len(cer) != len(up) {
		t.Fatalf("funcmap size mismatch: cerberus=%d upstream=%d", len(cer), len(up))
	}
	for k := range up {
		if _, ok := cer[k]; !ok {
			t.Errorf("cerberus funcmap missing upstream func %q", k)
		}
	}
}
