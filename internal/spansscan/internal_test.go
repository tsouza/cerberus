package spansscan

import "testing"

// White-box unit tests for the unexported matcher helpers. They exist so the
// mutation lane (phase6-spansscan) can constrain the boundary/range conditions
// inside recursiveBodySpans / wordAt / isIdentByte / withinAnySpan that the
// external integration fixtures cannot reach through the public API: a flipped
// `>=`/`<=`, a negated guard, or a dropped self-assignment in these leaf
// predicates produces no observable difference at the UnwindowedSpansScans
// surface (the offending input never arises from real emitted SQL), yet is a
// genuine latent break in the helper's contract. Asserting the contract
// directly kills those survivors without weakening the matcher.

// TestIsIdentByte pins the four identifier-byte ranges at their exact
// boundaries plus the bytes immediately outside each, killing the
// CONDITIONALS_BOUNDARY (`>=`→`>`, `<=`→`<`), CONDITIONALS_NEGATION, and
// INVERT_LOGICAL (`||`/`&&`) mutants on isIdentByte.
func TestIsIdentByte(t *testing.T) {
	t.Parallel()
	cases := []struct {
		c    byte
		want bool
	}{
		// In-range boundary bytes — each must be an identifier byte. A `>`/`<`
		// boundary flip drops exactly the edge byte, so testing the edges is
		// what kills those mutants.
		{'_', true},
		{'a', true},
		{'z', true},
		{'A', true},
		{'Z', true},
		{'0', true},
		{'9', true},
		// A couple of interior bytes for good measure.
		{'m', true},
		{'M', true},
		{'5', true},
		// Just-outside bytes — one below and one above each range. These kill
		// the boundary widenings and, via the half-true `{`/`[`/`:` cases, the
		// `&&`→`||` inversions inside each parenthesised range.
		{'^', false}, // 0x5E, just below '_' (0x5F)
		{'`', false}, // 0x60, just below 'a' (0x61)
		{'{', false}, // 0x7B, just above 'z' (0x7A)
		{'@', false}, // 0x40, just below 'A' (0x41)
		{'[', false}, // 0x5B, just above 'Z' (0x5A)
		{'/', false}, // 0x2F, just below '0' (0x30)
		{':', false}, // 0x3A, just above '9' (0x39)
		{' ', false},
		{'-', false},
		{'.', false},
	}
	for _, tc := range cases {
		if got := isIdentByte(tc.c); got != tc.want {
			t.Errorf("isIdentByte(%q) = %v, want %v", tc.c, got, tc.want)
		}
	}
}

// TestWithinAnySpan pins the inclusive `i >= s[0] && i <= s[1]` membership at
// both edges and just outside, killing the two CONDITIONALS_BOUNDARY mutants
// (`>=`→`>`, `<=`→`<`) and the INVERT_LOGICAL (`&&`→`||`) mutant on line 297.
func TestWithinAnySpan(t *testing.T) {
	t.Parallel()
	const lo, hi = 10, 20
	spans := [][2]int{{lo, hi}}
	cases := []struct {
		i    int
		want bool
	}{
		{lo, true},      // lower edge inclusive — kills `>=`→`>`
		{hi, true},      // upper edge inclusive — kills `<=`→`<`
		{15, true},      // interior
		{lo - 1, false}, // below: i<lo, i<=hi — kills `&&`→`||` (would read true)
		{hi + 1, false}, // above: i>=lo, i>hi — kills `&&`→`||` (would read true)
		{0, false},
	}
	for _, tc := range cases {
		if got := withinAnySpan(tc.i, spans); got != tc.want {
			t.Errorf("withinAnySpan(%d, [%d,%d]) = %v, want %v", tc.i, lo, hi, got, tc.want)
		}
	}
	// Empty span set never matches (kills a stray loop/return inversion).
	if withinAnySpan(5, nil) {
		t.Errorf("withinAnySpan over no spans must be false")
	}
}

// TestWordAt pins the standalone-keyword recogniser's three guards:
//   - the length precondition `j+len(kw) > len(sql)` (line 271),
//   - the preceding-identifier check guarded by `j > 0` (line 277),
//   - the following-identifier check guarded by `end < len(sql)` (line 280).
//
// Each case is chosen so a single boundary flip or negation on those guards
// changes the verdict (or dereferences out of range, which fails the test the
// same way a wrong verdict would).
func TestWordAt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sql  string
		j    int
		kw   string
		want bool
	}{
		// Keyword fills the string to its exact end: j+len(kw) == len(sql).
		// `>`→`>=` would reject this as out of range; the leading space keeps
		// the preceding-byte check happy so the only distinguishing guard is
		// line 271.
		{"exact_end_standalone", " UNION", 1, "UNION", true},
		// Keyword genuinely past the end: j+len(kw) > len(sql) → false.
		{"past_end", "UNIO", 0, "UNION", false},
		// Keyword at offset 0: the `j > 0` guard is false, so the preceding
		// byte (sql[-1]) is never read. A `>`→`>=` boundary or a negation makes
		// the guard true at j==0 and dereferences sql[-1] (out of range) —
		// which fails this test, killing both line-277 mutants. A trailing
		// space keeps the following-byte check from rejecting it.
		{"start_standalone", "UNION x", 0, "UNION", true},
		// Preceded by an identifier byte → not standalone. Confirms the
		// preceding-byte check actually rejects.
		{"preceded_by_ident", "xUNION ", 1, "UNION", false},
		// Keyword ends exactly at the string end: end == len(sql). `<`→`<=`
		// would read sql[end] (out of range); the negation `end < len` →
		// `end >= len` would skip the (here irrelevant) following-byte check.
		// Standalone, so want true.
		{"ends_at_string_end", "x UNION", 2, "UNION", true},
		// Followed by an identifier byte → not standalone. Kills the
		// CONDITIONALS_NEGATION on line 280 (`end < len` → `end >= len` would
		// skip the check and wrongly accept).
		{"followed_by_ident", "UNIONX", 0, "UNION", false},
		// Followed by a non-identifier byte → standalone.
		{"followed_by_space", "UNION ", 0, "UNION", true},
		// Case-insensitive match is honoured.
		{"case_insensitive", " union ", 1, "UNION", true},
	}
	for _, tc := range cases {
		if got := wordAt(tc.sql, tc.j, tc.kw); got != tc.want {
			t.Errorf("%s: wordAt(%q, %d, %q) = %v, want %v", tc.name, tc.sql, tc.j, tc.kw, got, tc.want)
		}
	}
}

// TestRecursiveBodySpans pins the parenthesis-carving helper that defines a
// recursive arm's byte range. It kills:
//   - INVERT_NEGATIVES / ARITHMETIC_BASE on the `-1` FindAll limit (line 196):
//     a two-CTE input must yield two spans, not one;
//   - CONDITIONALS_BOUNDARY on `open < 0` (line 198): a `(` immediately after
//     `WITH RECURSIVE` (relative index 0) is a real open paren, not "absent";
//   - REMOVE_SELF_ASSIGNMENTS on `open += loc[1]` (line 201): the recorded span
//     must use absolute offsets, so sql[span[0]] is the `(`;
//   - CONDITIONALS_BOUNDARY on the depth-walk loop guard `j < len(sql)`
//     (line 203): an unclosed `(` must terminate cleanly (no span, no panic),
//     not run the index one past the end.
func TestRecursiveBodySpans(t *testing.T) {
	t.Parallel()

	t.Run("two_ctes_yield_two_spans", func(t *testing.T) {
		t.Parallel()
		sql := "WITH RECURSIVE a AS (SELECT 1) SELECT * FROM " +
			"(WITH RECURSIVE b AS (SELECT 2) SELECT 3)"
		spans := recursiveBodySpans(sql)
		if len(spans) != 2 {
			t.Fatalf("two WITH RECURSIVE CTEs: got %d span(s), want 2 (FindAll limit must be -1)", len(spans))
		}
		for n, s := range spans {
			if sql[s[0]] != '(' || sql[s[1]] != ')' {
				t.Errorf("span %d = [%d,%d] must bracket a (...) body, got %q…%q", n, s[0], s[1], sql[s[0]], sql[s[1]])
			}
		}
	})

	t.Run("absolute_offsets_when_not_at_start", func(t *testing.T) {
		t.Parallel()
		// WITH RECURSIVE deliberately NOT at offset 0, so a dropped
		// `open += loc[1]` leaves `open` as a small relative index that does
		// not point at the '('.
		sql := "zzzz WITH RECURSIVE r AS (SELECT 1)"
		spans := recursiveBodySpans(sql)
		if len(spans) != 1 {
			t.Fatalf("got %d span(s), want 1", len(spans))
		}
		if sql[spans[0][0]] != '(' {
			t.Errorf("span open offset %d must point at '(', got %q (absolute offset lost?)", spans[0][0], sql[spans[0][0]])
		}
		if sql[spans[0][1]] != ')' {
			t.Errorf("span close offset %d must point at ')', got %q", spans[0][1], sql[spans[0][1]])
		}
	})

	t.Run("paren_immediately_after_recursive", func(t *testing.T) {
		t.Parallel()
		// '(' at relative index 0 after the WITH RECURSIVE match: a valid open
		// paren. `open < 0` widened to `open <= 0` would discard it.
		sql := "WITH RECURSIVE(SELECT 1)"
		spans := recursiveBodySpans(sql)
		if len(spans) != 1 {
			t.Fatalf("'(' at relative index 0: got %d span(s), want 1 (open==0 is found, not absent)", len(spans))
		}
		if sql[spans[0][0]] != '(' || sql[spans[0][1]] != ')' {
			t.Errorf("span [%d,%d] must bracket the body", spans[0][0], spans[0][1])
		}
	})

	t.Run("unclosed_paren_terminates_cleanly", func(t *testing.T) {
		t.Parallel()
		// No matching ')': the depth walk must run to the end and record no
		// completed span. A `j < len(sql)` → `j <= len(sql)` guard would index
		// sql[len(sql)] and panic.
		sql := "WITH RECURSIVE r AS (SELECT 1 FROM x"
		spans := recursiveBodySpans(sql)
		if len(spans) != 0 {
			t.Fatalf("unclosed recursive body: got %d span(s), want 0", len(spans))
		}
	})
}
