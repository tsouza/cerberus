//go:build agpl_oracle

// A/B oracle: the in-house logpattern package must agree with the
// upstream AGPL grafana/loki pkg/logql/log/pattern on New / ParseLineFilter
// / ParseLiterals (accept/reject + names + literal runs) and on
// Matches / Test output for a line corpus.
//
// This is the ONLY file that imports the AGPL pattern package, gated
// behind the `agpl_oracle` build tag. Run with:
//
//	CGO_ENABLED=1 go test -tags agpl_oracle ./internal/logql/logpattern/
package logpattern

import (
	"bytes"
	"math/rand/v2"
	"reflect"
	"testing"

	lokipattern "github.com/grafana/loki/v3/pkg/logql/log/pattern"
)

var patternCorpus = []string{
	"<ip> <_> <method> <path>",
	"<_> <method> <_>",
	"<foo>",
	"<_>",
	"GET <path> <status>",
	"<a><b>",           // consecutive captures (invalid for New + line filter)
	"<a> <a>",          // duplicate names (invalid for New)
	"no captures here", // no capture
	"",
	"literal <_> tail",
	"<ip>:<port>",
	"prefix<_>suffix",
	"<1bad>", // digit-leading name -> '<' is literal
	"<a-b>",  // dash -> '<' is literal
	"<unterminated",
	"plain text",
	"a<_>b<_>c",
	"<method> <_> <code>",
}

func TestPattern_New_MatchesLoki(t *testing.T) {
	for _, p := range patternCorpus {
		t.Run(p, func(t *testing.T) {
			want, wantErr := lokipattern.New(p)
			got, gotErr := New(p)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("New(%q) accept/reject mismatch: loki err=%v in-house err=%v", p, wantErr, gotErr)
			}
			if wantErr != nil {
				return
			}
			if !reflect.DeepEqual(want.Names(), got.Names()) {
				t.Fatalf("New(%q) names mismatch: loki=%v in-house=%v", p, want.Names(), got.Names())
			}
		})
	}
}

func TestPattern_ParseLineFilter_MatchesLoki(t *testing.T) {
	for _, p := range patternCorpus {
		t.Run(p, func(t *testing.T) {
			_, wantErr := lokipattern.ParseLineFilter([]byte(p))
			_, gotErr := ParseLineFilter([]byte(p))
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("ParseLineFilter(%q) accept/reject mismatch: loki err=%v in-house err=%v", p, wantErr, gotErr)
			}
		})
	}
}

func TestPattern_ParseLiterals_MatchesLoki(t *testing.T) {
	for _, p := range patternCorpus {
		t.Run(p, func(t *testing.T) {
			want, wantErr := lokipattern.ParseLiterals(p)
			got, gotErr := ParseLiterals(p)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("ParseLiterals(%q) accept/reject mismatch: loki=%v in-house=%v", p, wantErr, gotErr)
			}
			if wantErr != nil {
				return
			}
			if len(want) != len(got) {
				t.Fatalf("ParseLiterals(%q) count mismatch: loki=%d in-house=%d", p, len(want), len(got))
			}
			for i := range want {
				if !bytes.Equal(want[i], got[i]) {
					t.Fatalf("ParseLiterals(%q)[%d] mismatch: loki=%q in-house=%q", p, i, want[i], got[i])
				}
			}
		})
	}
}

var lineCorpus = [][]byte{
	[]byte("192.168.0.1 - GET /api/users"),
	[]byte("GET /api/users 200"),
	[]byte("foo bar baz"),
	[]byte(""),
	[]byte("10.0.0.5:8080"),
	[]byte("prefixVALUEsuffix"),
	[]byte("abab"),
	[]byte("no match for this line"),
	[]byte("method GET code 200"),
}

func TestPattern_MatchesAndTest_MatchLoki(t *testing.T) {
	// Patterns valid for New (have captures, no consecutive/dup) — only
	// these build a usable Matcher on both sides.
	// Patterns with at least one NAMED capture (New rejects unnamed-only).
	valid := []string{
		"<ip> <_> <method> <path>",
		"<method> <_> <code>",
		"<foo>",
		"<ip>:<port>",
		"prefix<val>suffix",
		"<a>ab",
	}
	for _, p := range valid {
		lm, err := lokipattern.New(p)
		if err != nil {
			t.Fatalf("loki New(%q) unexpectedly errored: %v", p, err)
		}
		gm, err := New(p)
		if err != nil {
			t.Fatalf("in-house New(%q) unexpectedly errored: %v", p, err)
		}
		for _, line := range lineCorpus {
			wantCaps := lm.Matches(line)
			gotCaps := gm.Matches(line)
			if len(wantCaps) != len(gotCaps) {
				t.Fatalf("Matches(%q, %q) count mismatch: loki=%d in-house=%d", p, line, len(wantCaps), len(gotCaps))
			}
			for i := range wantCaps {
				if !bytes.Equal(wantCaps[i], gotCaps[i]) {
					t.Fatalf("Matches(%q, %q)[%d] mismatch: loki=%q in-house=%q", p, line, i, wantCaps[i], gotCaps[i])
				}
			}
			if lm.Test(line) != gm.Test(line) {
				t.Fatalf("Test(%q, %q) mismatch: loki=%v in-house=%v", p, line, lm.Test(line), gm.Test(line))
			}
		}
	}
}

// randomPatternAlphabet is the text the generated patterns and lines draw
// from: the capture delimiters, both name alphabets, the unnamed capture,
// separators, a multi-byte rune, a valid encoding of U+FFFD, and byte
// sequences that are not valid UTF-8 — a lone invalid byte, a lead byte
// with no continuation, a truncated three-byte sequence and an encoded
// surrogate.
var randomPatternAlphabet = []string{
	"<", "<", ">", ">", "_", "a", "b", "1", " ", "-", ":", "\u00e9", "\uFFFD",
	"\xff", "\xc3", "\xe2\x82", "\xed\xa0\x80",
}

// randomPatternSamples is how many generated patterns the random
// differential compares, each against randomLinesPerPattern lines.
const (
	randomPatternSamples  = 4000
	randomLinesPerPattern = 8
	randomMaxLength       = 12
)

// randomText is up to n random entries of randomPatternAlphabet.
func randomText(r *rand.Rand, n int) []byte {
	var out []byte
	for range r.IntN(n + 1) {
		out = append(out, randomPatternAlphabet[r.IntN(len(randomPatternAlphabet))]...)
	}
	return out
}

func randomBytes(r *rand.Rand) []byte { return randomText(r, randomMaxLength) }

// randomPatternTokens are whole captures, near-captures and literal text,
// so a generated pattern often carries several named captures.
var randomPatternTokens = []string{"<a>", "<b>", "<_>", "<ab1>", "x", " ", "-", "\u00e9", "<", ">", "<1>", "\xff", "\uFFFD"}

// randomPatternTokenRun is the most tokens a token-built pattern joins.
const randomPatternTokenRun = 6

// randomPattern is raw random text for one pattern in two, and a run of
// randomPatternTokens for the other.
func randomPattern(r *rand.Rand) []byte {
	if r.IntN(2) == 0 {
		return randomBytes(r)
	}
	var out []byte
	for range 1 + r.IntN(randomPatternTokenRun) {
		out = append(out, randomPatternTokens[r.IntN(len(randomPatternTokens))]...)
	}
	return out
}

// randomFiller is zero to two random entries of the alphabet, some of
// them invalid UTF-8, the text a generated line puts around a pattern's
// literal runs.
func randomFiller(r *rand.Rand) []byte { return randomText(r, 2) }

func sameCaptures(a, b [][]byte) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// TestPattern_RandomDifferential_MatchesLoki compares the two
// implementations on generated patterns and lines: accept/reject and the
// validation error text of New and ParseLineFilter, the capture names, the
// literal runs, and every Matches (including nil versus empty) and Test
// result.
func TestPattern_RandomDifferential_MatchesLoki(t *testing.T) {
	r := rand.New(rand.NewPCG(4132, 1))
	for range randomPatternSamples {
		p := randomPattern(r)
		lines := make([][]byte, randomLinesPerPattern)
		for i := range lines {
			lines[i] = randomBytes(r)
		}
		// Lines built from the pattern's own literal runs, every third one
		// missing one run, exercise the matching paths that random bytes
		// rarely reach.
		if lits, err := ParseLiterals(string(p)); err == nil {
			for i := range lines {
				var b []byte
				skip := -1
				if i%3 == 0 && len(lits) > 0 {
					skip = r.IntN(len(lits))
				}
				for j, l := range lits {
					b = append(b, randomFiller(r)...)
					if j != skip {
						b = append(b, l...)
					}
				}
				if i%2 == 0 {
					b = append(b, randomFiller(r)...)
				}
				lines[i] = b
			}
		}

		wantLits, wantErr := lokipattern.ParseLiterals(string(p))
		gotLits, gotErr := ParseLiterals(string(p))
		if (wantErr == nil) != (gotErr == nil) || (wantErr == nil && !sameCaptures(wantLits, gotLits)) {
			t.Fatalf("ParseLiterals(%q): loki=(%q, %v) in-house=(%q, %v)", p, wantLits, wantErr, gotLits, gotErr)
		}

		lm, lErr := lokipattern.New(string(p))
		gm, gErr := New(string(p))
		compareErrors(t, "New", p, lErr, gErr)
		if lErr == nil {
			if !reflect.DeepEqual(lm.Names(), gm.Names()) {
				t.Fatalf("New(%q).Names(): loki=%q in-house=%q", p, lm.Names(), gm.Names())
			}
			for _, line := range lines {
				if want, got := lm.Matches(line), gm.Matches(line); !sameCaptures(want, got) {
					t.Fatalf("New(%q).Matches(%q): loki=%q in-house=%q", p, line, want, got)
				}
				if want, got := lm.Test(line), gm.Test(line); want != got {
					t.Fatalf("New(%q).Test(%q): loki=%v in-house=%v", p, line, want, got)
				}
			}
		}

		lf, lErr := lokipattern.ParseLineFilter(p)
		gf, gErr := ParseLineFilter(p)
		compareErrors(t, "ParseLineFilter", p, lErr, gErr)
		if lErr == nil {
			for _, line := range lines {
				if want, got := lf.Matches(line), gf.Matches(line); !sameCaptures(want, got) {
					t.Fatalf("ParseLineFilter(%q).Matches(%q): loki=%q in-house=%q", p, line, want, got)
				}
				if want, got := lf.Test(line), gf.Test(line); want != got {
					t.Fatalf("ParseLineFilter(%q).Test(%q): loki=%v in-house=%v", p, line, want, got)
				}
			}
		}
	}
}

// compareErrors fails unless both sides accept, or both reject with the
// same text.
func compareErrors(t *testing.T, fn string, p []byte, want, got error) {
	t.Helper()
	if (want == nil) != (got == nil) {
		t.Fatalf("%s(%q) accept/reject: loki err=%v in-house err=%v", fn, p, want, got)
	}
	if want != nil && want.Error() != got.Error() {
		t.Fatalf("%s(%q) error text: loki=%q in-house=%q", fn, p, want, got)
	}
}
