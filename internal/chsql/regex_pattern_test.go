package chsql

import (
	"regexp"
	"regexp/syntax"
	"testing"
)

// regexPatternInputs are strings the equivalence checks below evaluate every
// pattern against: newlines at each position, carriage returns, multi-byte
// UTF-8, and the empty string.
var regexPatternInputs = []string{
	"", "a", "b", "ab", "a\nb", "\n", "api", "api\n", "\napi", "api-7", "host-1",
	"host-\n2", "x\r", "été", "K", "a)|(b", "aXb", "a.b", "12ms",
}

// program is pattern's parse under flags, simplified and printed — two
// patterns with the same program match the same strings with the same
// submatches.
func program(t *testing.T, pattern string, flags syntax.Flags) string {
	t.Helper()
	re, err := syntax.Parse(pattern, flags)
	if err != nil {
		t.Fatalf("parse %q: %v", pattern, err)
	}
	return programOf(re)
}

func TestAnchorLabelReplaceRegex_EquivalentToReferenceAnchoring(t *testing.T) {
	for _, regex := range []string{
		``, `.*`, `(.*)`, `host-(.*)`, `(\w+)(-(\d+))?`, `(?P<name>\w+)-\d+`,
		`api|web-(\d)`, `(?i)(k)elvin`, `(?-s:a.b)`, `a(?-s).b`, `[^-]+`,
		`(?:x(y))?api`, `\Qa.b\E`, `(a)|(b)`, `^a$`,
	} {
		t.Run(regex, func(t *testing.T) {
			reference := "^(?s:" + regex + ")$"
			got := anchorLabelReplaceRegex(regex)
			want := "(?s)^(?:" + regex + ")$"
			if got != want {
				t.Fatalf("anchorLabelReplaceRegex(%q) = %q; want %q", regex, got, want)
			}
			if a, b := program(t, got, goRegexpFlags), program(t, reference, goRegexpFlags); a != b {
				t.Fatalf("%q parses to %s; the reference %q parses to %s", got, a, reference, b)
			}
			emitted, ref := regexp.MustCompile(got), regexp.MustCompile(reference)
			if emitted.NumSubexp() != ref.NumSubexp() {
				t.Fatalf("%q has %d groups; the reference has %d", got, emitted.NumSubexp(), ref.NumSubexp())
			}
			for _, in := range regexPatternInputs {
				if a, b := emitted.FindStringSubmatchIndex(in), ref.FindStringSubmatchIndex(in); !equalInts(a, b) {
					t.Errorf("on %q: %q submatches %v; the reference %v", in, got, a, b)
				}
			}
		})
	}
}

// A regex whose `)` closes the reference group early keeps the reference
// spelling: a leading `(?s)` would reach the text after that `)`.
func TestAnchorLabelReplaceRegex_UnbalancedKeepsReferenceSpelling(t *testing.T) {
	for _, regex := range []string{`a)|(b`, `a)|(.`, `(`, `a\Qb`} {
		if got, want := anchorLabelReplaceRegex(regex), "^(?s:"+regex+")$"; got != want {
			t.Errorf("anchorLabelReplaceRegex(%q) = %q; want the reference spelling %q", regex, got, want)
		}
	}
}

// lineFilterRegex's output, read with `.` matching a newline (ClickHouse's
// match()), must mean what the input means under Go's default flags (Loki).
func TestLineFilterRegex_MeansUnderDotNLWhatGoDefaultsMean(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		want    string
	}{
		{`api`, `api`},
		{`timeout after \d+ms`, `timeout after \d+ms`},
		{`error|timeout`, `error|timeout`},
		{`[^-]+`, `[^-]+`},
		{`\s`, `\s`},
		{`^api$`, `^api$`},
		{`(?s)a.b`, `(?s)a.b`},
		{`(?s:.)x`, `(?s:.)x`},
		{`a\.b`, `a\.b`},
		{`[.]x`, `[.]x`},
		{`\Qa.b\E`, `\Qa.b\E`},
		{`a.b`, `a[^\n]b`},
		{`.*`, `[^\n]*`},
		{`api.*`, `api[^\n]*`},
		{`(?i)err.*`, `(?i)err[^\n]*`},
		{`host-(.*)`, `host-([^\n]*)`},
		{`(?P<x>.+)\.log`, `(?P<x>[^\n]+)\.log`},
		{`[].]x.`, `[].]x[^\n]`},
		{`[^]]x.`, `[^]]x[^\n]`},
		{`[[:alpha:].]+.`, `[[:alpha:].]+[^\n]`},
		{`\Qa.b\E.`, `\Qa.b\E[^\n]`},
		{`\Qa.b`, `\Qa.b`},
		{`status=5..`, `status=5[^\n][^\n]`},
		{`x(?s:.)y.`, `(?-s)x(?s:.)y.`},
		{`(?-s:.)(?s).`, `(?-s)(?-s:.)(?s).`},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			got := lineFilterRegex(tc.pattern)
			if got != tc.want {
				t.Fatalf("lineFilterRegex(%q) = %q; want %q", tc.pattern, got, tc.want)
			}
			if a, b := program(t, got, goRegexpFlags|syntax.DotNL), program(t, tc.pattern, goRegexpFlags); a != b {
				t.Fatalf("%q with `.` matching newline parses to %s; Go reads %q as %s", got, a, tc.pattern, b)
			}
			// The same, by execution: `(?s)` in front reads got as match()
			// does, and every match and submatch must land where Go's do.
			emitted, ref := regexp.MustCompile("(?s)"+got), regexp.MustCompile(tc.pattern)
			for _, in := range regexPatternInputs {
				if a, b := emitted.FindAllStringSubmatchIndex(in, -1), ref.FindAllStringSubmatchIndex(in, -1); !equalMatches(a, b) {
					t.Errorf("on %q: %q matches %v; Go's %q matches %v", in, got, a, tc.pattern, b)
				}
			}
		})
	}
}

func TestLineFilterRegex_UnparseableUnchanged(t *testing.T) {
	for _, p := range []string{`a(`, `[`, `*`} {
		if got := lineFilterRegex(p); got != p {
			t.Errorf("lineFilterRegex(%q) = %q; want it unchanged", p, got)
		}
	}
}

func equalMatches(a, b [][]int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !equalInts(a[i], b[i]) {
			return false
		}
	}
	return true
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// regexTokens are the pieces TestLineFilterRegex_ExhaustiveSmallPatterns
// concatenates: the constructs spellDotsAsNotNewline scans past, rewrites or
// declines. Class edge cases and group names are pinned by the table test.
var regexTokens = []string{
	"a", ".", `\.`, "[.]", "[^a]", "(", ")", "*", "|", `\n`, "(?s)", "(?-s)", "(?s:", `\Q`,
}

// tokenPatternInputs cover what the tokens can tell apart: a newline where
// `.` would or would not match, the literal characters, and the empty string.
var tokenPatternInputs = []string{"", "a\nb", "\n\n", "a.]A", "\\Q.\\E\na"}

// maxRegexTokens bounds the concatenation length; three tokens already
// combine every scanner state with every other once.
const maxRegexTokens = 3

// Every parseable concatenation of up to maxRegexTokens tokens must mean,
// after lineFilterRegex and read with `.` matching a newline, what it means
// under Go's defaults — checked by execution, not by the parser comparison
// lineFilterRegex itself relies on.
func TestLineFilterRegex_ExhaustiveSmallPatterns(t *testing.T) {
	checked := 0
	var walk func(prefix string, depth int)
	walk = func(prefix string, depth int) {
		if prefix != "" {
			if ref, err := regexp.Compile(prefix); err == nil {
				checked++
				got := lineFilterRegex(prefix)
				emitted, err := regexp.Compile("(?s)" + got)
				if err != nil {
					t.Fatalf("lineFilterRegex(%q) = %q does not compile: %v", prefix, got, err)
				}
				for _, in := range tokenPatternInputs {
					if a, b := emitted.FindAllStringSubmatchIndex(in, -1), ref.FindAllStringSubmatchIndex(in, -1); !equalMatches(a, b) {
						t.Fatalf("on %q: lineFilterRegex(%q) = %q matches %v; Go matches %v", in, prefix, got, a, b)
					}
				}
			}
		}
		if depth == maxRegexTokens {
			return
		}
		for _, tok := range regexTokens {
			walk(prefix+tok, depth+1)
		}
	}
	walk("", 0)
	if checked == 0 {
		t.Fatal("no generated pattern parsed; the generator is broken")
	}
}
