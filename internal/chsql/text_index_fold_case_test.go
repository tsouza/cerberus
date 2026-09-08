package chsql

import (
	"regexp"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// likeMatch is an independent implementation of ClickHouse's LIKE operator
// over the subset of patterns textIndexPrefilterArgs emits: a literal body
// wrapped in `%`, with `\` as the escape character and `%` / `_` as the
// any-sequence / any-single-character wildcards. Written from the LIKE
// semantics rather than from escapeLikeLiteral's inverse on purpose — a
// test that un-escaped the needle with the emitter's own rules could not
// catch an escaping bug, and this predicate is what the superset proof
// below actually needs to be true on a real server.
func likeMatch(s, pattern string) bool {
	pr := []rune(pattern)
	sr := []rune(s)
	// star / starS record the most recent `%` position to backtrack to.
	var i, j, star, starS int
	star = -1
	for i < len(sr) {
		switch {
		case j < len(pr) && pr[j] == '\\' && j+1 < len(pr):
			// Escaped literal: matches the escaped rune exactly.
			if sr[i] == pr[j+1] {
				i++
				j += 2
				continue
			}
		case j < len(pr) && pr[j] == '%':
			star, starS = j, i
			j++
			continue
		case j < len(pr) && (pr[j] == '_' || pr[j] == sr[i]):
			i++
			j++
			continue
		}
		if star < 0 {
			return false
		}
		// Backtrack: let the last `%` swallow one more rune.
		starS++
		i, j = starS, star+1
	}
	for j < len(pr) && pr[j] == '%' {
		j++
	}
	return j == len(pr)
}

// TestTextIndexPrefilter_IsAStrictSupersetOfTheRowPredicate is the
// executable form of the guarantee exprLineContent's rewrite rests on: an
// emitted `lower(<Source>) LIKE '%tok%'` conjunct may only ever ELIMINATE
// rows the unrewritten row predicate would also have rejected. Any (body,
// pattern) pair where the row predicate matches but a conjunct does not is
// a row the deployed query silently loses.
//
// Go's regexp is the oracle for the regex arm deliberately: it is the same
// RE2 engine ClickHouse's match() runs, the equivalence textIndexRegexLiteral's
// own doc invokes. asciiLower models `lower()`'s ASCII-only behaviour (see
// asciiLower's doc, which pins it against a live server).
//
// The case-insensitive rows are the regression pin for the defect this test
// was written for: regexp/syntax stores a fold-case OpLiteral as the MINIMUM
// rune of each fold orbit, so `(?i)café` reaches the emitter as "CAFÉ" and
// an ASCII-lowered needle of "cafÉ" — which `lower(Body)` never contains for
// a body holding "café", though match() matches it.
func TestTextIndexPrefilter_IsAStrictSupersetOfTheRowPredicate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		pattern string
		isRegex bool
		bodies  []string
	}{
		{
			name:    "fold_case_non_ascii_literal",
			pattern: "(?i)café",
			isRegex: true,
			bodies:  []string{"le café est ouvert", "LE CAFÉ EST OUVERT", "Le Café", "no match here"},
		},
		{
			name:    "fold_case_ascii_orbit_escapes_ascii_kelvin",
			pattern: "(?i)kelvin",
			isRegex: true,
			// U+212A KELVIN SIGN folds with 'k'/'K' under RE2 but is
			// untouched by ClickHouse's ASCII-only lower().
			bodies: []string{"kelvin reading", "KELVIN reading", "Kelvin reading"},
		},
		{
			name:    "fold_case_ascii_orbit_escapes_ascii_long_s",
			pattern: "(?i)session",
			isRegex: true,
			// U+017F LATIN SMALL LETTER LONG S folds with 's'/'S'.
			bodies: []string{"session opened", "SESSION opened", "ſession opened"},
		},
		{
			name:    "fold_case_all_ascii_safe",
			pattern: "(?i)connection timeout",
			isRegex: true,
			bodies:  []string{"connection timeout", "CONNECTION TIMEOUT", "Connection Timeout", "unrelated line"},
		},
		{
			name:    "fold_case_mixed_safe_and_unsafe_words",
			pattern: "(?i)café connection",
			isRegex: true,
			bodies:  []string{"café connection", "CAFÉ CONNECTION", "Café Connection"},
		},
		{
			name:    "case_sensitive_non_ascii_literal",
			pattern: "CAFÉ",
			bodies:  []string{"le CAFÉ est ouvert", "le café est ouvert"},
		},
		{
			name:    "case_sensitive_regex_literal",
			pattern: "connection reset",
			isRegex: true,
			bodies:  []string{"connection reset by peer", "CONNECTION RESET BY PEER"},
		},
		{
			name:    "plain_literal_with_like_metacharacters",
			pattern: "storage 87%full user_id=50",
			bodies:  []string{"storage 87%full user_id=50", "storage 87XfullXuser_idX50"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			lc := &chplan.LineContent{
				Source:             &chplan.ColumnRef{Name: "Body"},
				Pattern:            tc.pattern,
				IsRegex:            tc.isRegex,
				TextIndexPrefilter: true,
			}
			args := textIndexPrefilterArgs(lc)
			if len(args) == 0 {
				// No prefilter emitted: the rewrite is inert and the
				// superset property is trivially held. Nothing to check.
				return
			}

			var rowMatches func(string) bool
			if tc.isRegex {
				re, err := regexp.Compile(tc.pattern)
				if err != nil {
					t.Fatalf("compile oracle regexp %q: %v", tc.pattern, err)
				}
				rowMatches = re.MatchString
			} else {
				rowMatches = func(body string) bool { return strings.Contains(body, tc.pattern) }
			}

			for _, body := range tc.bodies {
				if !rowMatches(body) {
					continue
				}
				lowered := asciiLower(body)
				for _, arg := range args {
					if !likeMatch(lowered, arg) {
						t.Errorf(
							"prefilter drops a matching row: pattern %q matches body %q, "+
								"but conjunct lower(Body) LIKE %q is false over lower(Body)=%q",
							tc.pattern, body, arg, lowered,
						)
					}
				}
			}
		})
	}
}

// TestAsciiFoldSafe_RejectsOrbitsThatLeaveASCII pins the two rune classes
// that make an ASCII-lowered needle unsound for a case-insensitive match,
// so a future simplification to "is every rune ASCII?" fails here.
func TestAsciiFoldSafe_RejectsOrbitsThatLeaveASCII(t *testing.T) {
	t.Parallel()

	safe := []string{"connection", "timeout", "peer", "error", "412", ""}
	for _, s := range safe {
		if !asciiFoldSafe(s) {
			t.Errorf("asciiFoldSafe(%q) = false, want true", s)
		}
	}

	unsafe := []string{
		"café",    // non-ASCII rune in the literal
		"kelvin",  // 'k' folds with U+212A KELVIN SIGN
		"session", // 's' folds with U+017F LATIN SMALL LETTER LONG S
		"MASK",    // upper-case forms of the same two orbits
	}
	for _, s := range unsafe {
		if asciiFoldSafe(s) {
			t.Errorf("asciiFoldSafe(%q) = true, want false", s)
		}
	}
}

// TestTextIndexRegexLiteral_ReportsFoldCase pins that the fold-case flag
// reaches the caller at all: the defect this guards against was a literal
// extracted correctly while its FoldCase flag was silently discarded.
func TestTextIndexRegexLiteral_ReportsFoldCase(t *testing.T) {
	t.Parallel()

	cases := []struct {
		pattern  string
		wantLit  string
		wantFold bool
		wantOK   bool
	}{
		{pattern: "connection reset", wantLit: "connection reset", wantFold: false, wantOK: true},
		{pattern: "(?i)connection reset", wantLit: "CONNECTION RESET", wantFold: true, wantOK: true},
		{pattern: "(?i)café", wantLit: "CAFÉ", wantFold: true, wantOK: true},
		{pattern: `failed: \w+`, wantOK: false},
		{pattern: "^anchored", wantOK: false},
	}
	for _, tc := range cases {
		lit, fold, ok := textIndexRegexLiteral(tc.pattern)
		if ok != tc.wantOK {
			t.Errorf("textIndexRegexLiteral(%q) ok = %v, want %v", tc.pattern, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if lit != tc.wantLit || fold != tc.wantFold {
			t.Errorf(
				"textIndexRegexLiteral(%q) = (%q, %v), want (%q, %v)",
				tc.pattern, lit, fold, tc.wantLit, tc.wantFold,
			)
		}
	}
}
