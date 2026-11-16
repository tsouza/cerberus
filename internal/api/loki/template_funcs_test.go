package loki

import (
	"bytes"
	"strings"
	"testing"
	"text/template"
	"time"
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
// surface (the in-house sprig allow-list + Loki-native funcs in
// templateFuncs, parity-checked by the agpl_oracle test against
// upstream), so every name that previously failed at parse time with "function not
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
	// sprig's `date` formats in the process-local zone (as reference Loki
	// does), so the expected string is built in time.Local too.
	wantTs := time.Unix(0, ns).Local().Format("2006-01-02T15:04:05Z07:00")
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

// TestTemplateFunc_SizeAndDurationConversions pins the `bytes`,
// `duration` and `duration_seconds` template functions: a humanised size
// becomes its byte count and a Go duration its seconds, and an unparsable
// argument fails the template execution with the parser's error.
func TestTemplateFunc_SizeAndDurationConversions(t *testing.T) {
	t.Parallel()

	ok := []struct{ body, want string }{
		{`{{ bytes "0" }}`, "0"},
		{`{{ bytes "42" }}`, "42"},
		{`{{ bytes "1.5KB" }}`, "1500"},
		{`{{ bytes "1KiB" }}`, "1024"},
		{`{{ bytes "2 MiB" }}`, "2.097152e+06"},
		{`{{ bytes "1gb" }}`, "1e+09"},
		{`{{ duration "1.5s" }}`, "1.5"},
		{`{{ duration "-2m" }}`, "-120"},
		{`{{ duration "250ms" }}`, "0.25"},
		{`{{ duration_seconds "1h" }}`, "3600"},
	}
	for _, tc := range ok {
		if got := renderInline(t, tc.body, nil); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.body, got, tc.want)
		}
	}

	funcs := templateFuncs(func() string { return "" }, func() int64 { return 0 })
	for _, tc := range []struct{ body, wantErr string }{
		{`{{ bytes "lots" }}`, `strconv.ParseFloat: parsing "": invalid syntax`},
		{`{{ bytes "12 parsecs" }}`, "unhandled size name: parsecs"},
		{`{{ duration "soon" }}`, `time: invalid duration "soon"`},
		{`{{ duration_seconds "5" }}`, `time: missing unit in duration "5"`},
	} {
		tpl, err := template.New("t").Funcs(funcs).Parse(tc.body)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.body, err)
		}
		var buf bytes.Buffer
		err = tpl.Execute(&buf, nil)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error = %v, want it to contain %q", tc.body, err, tc.wantErr)
		}
	}
}

// TestTemplateFunc_AlignWidths pins alignLeft / alignRight over every
// width case: negative and exact widths leave the text as it is (bytes
// included), a wider width pads with spaces on the far side, and a
// narrower one keeps the first (alignLeft) or last (alignRight) runes.
func TestTemplateFunc_AlignWidths(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		width       int
		src         string
		left, right string
	}{
		{-1, "abc", "abc", "abc"},
		{0, "abc", "", ""},
		{3, "abc", "abc", "abc"},
		{5, "abc", "abc  ", "  abc"},
		{2, "abc", "ab", "bc"},
		{0, "", "", ""},
		{2, "", "  ", "  "},
		{2, "héllo", "hé", "lo"},
		{4, "日本", "日本  ", "  日本"},
		{1, "日本", "日", "本"},
		{2, "a\xffb", "a�", "�b"},
		{3, "a\xffb", "a\xffb", "a\xffb"},
		{4, "a\xffb", "a\xffb ", " a\xffb"},
	} {
		if got := alignLeft(c.width, c.src); got != c.left {
			t.Errorf("alignLeft(%d, %q) = %q, want %q", c.width, c.src, got, c.left)
		}
		if got := alignRight(c.width, c.src); got != c.right {
			t.Errorf("alignRight(%d, %q) = %q, want %q", c.width, c.src, got, c.right)
		}
	}
}

// TestToDateInZone pins toDateInZone, the body of Loki's `toDateInZone`
// template function: the string is parsed in the named zone, an unknown
// zone falls back to UTC, and an unparsable string yields the zero time.
func TestToDateInZone(t *testing.T) {
	t.Parallel()

	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	cases := []struct {
		name, format, zone, in string
		want                   time.Time
		wantZone               string
	}{
		{"named zone", "2006-01-02 15:04", "America/New_York", "2024-03-10 12:30", time.Date(2024, 3, 10, 12, 30, 0, 0, newYork), "America/New_York"},
		{"unknown zone falls back to UTC", "2006-01-02", "Not/AZone", "2024-03-10", time.Date(2024, 3, 10, 0, 0, 0, 0, time.UTC), "UTC"},
		{"empty zone is UTC", "2006-01-02", "", "2024-03-10", time.Date(2024, 3, 10, 0, 0, 0, 0, time.UTC), "UTC"},
		{"unparsable string is the zero time", "2006-01-02", "America/New_York", "not a date", time.Time{}, "UTC"},
		{"unparsable string in an unknown zone is the zero time", "2006-01-02", "Not/AZone", "10/03/2024", time.Time{}, "UTC"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := toDateInZone(c.format, c.zone, c.in)
			if !got.Equal(c.want) || got.IsZero() != c.want.IsZero() {
				t.Fatalf("toDateInZone(%q, %q, %q) = %v, want %v", c.format, c.zone, c.in, got, c.want)
			}
			if zone := got.Location().String(); zone != c.wantZone {
				t.Fatalf("toDateInZone(%q, %q, %q) is in %s, want %s", c.format, c.zone, c.in, zone, c.wantZone)
			}
		})
	}
}
