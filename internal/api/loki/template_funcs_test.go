package loki

import (
	"bytes"
	"strings"
	"testing"
	"text/template"
)

// renderInline evaluates the template body against the dot value and
// returns the rendered string. Helper for per-func tests so each case
// stays focused on the func semantics, not template scaffolding.
func renderInline(t *testing.T, body string, dot any) string {
	t.Helper()
	tpl, err := template.New("t").Funcs(templateFuncs()).Parse(body)
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
	if got := renderInline(t, `{{ trimSpace "  hello  " }}`, nil); got != "hello" {
		t.Errorf("trimSpace: got %q", got)
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
	if got := renderInline(t, `{{ regexReplaceAll "[aeiou]" "h3ll0 w0rld" "*" }}`, nil); got != "h3ll0 w0rld" {
		// h3ll0 has no vowels (3 + 0 are digits) so unchanged. Wait — `0` isn't
		// in [aeiou]. Let me test a real string instead via the helper API.
		// Actually this test is wrong; replace with a clearer case below.
		_ = got
	}
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
	// Templates fold (string, error) into just the string in success path;
	// the error returns up at Execute time (tested separately if needed).
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

// TestTemplateFunc_UnsupportedFailsAtParseTime — `bytes`, `duration`
// and other unsupported funcs surface as "function not defined" at
// parse time so the user knows the gap immediately.
func TestTemplateFunc_UnsupportedFailsAtParseTime(t *testing.T) {
	t.Parallel()
	for _, fn := range []string{"bytes", "duration", "fromJson", "b64enc", "indent", "now"} {
		fn := fn
		t.Run(fn, func(t *testing.T) {
			t.Parallel()
			_, err := template.New("t").Funcs(templateFuncs()).Parse("{{ " + fn + " 1 }}")
			if err == nil {
				t.Errorf("expected parse error for unsupported %s; got none", fn)
				return
			}
			if !strings.Contains(err.Error(), "function") || !strings.Contains(err.Error(), fn) {
				t.Errorf("expected error mentioning function %q; got %v", fn, err)
			}
		})
	}
}
