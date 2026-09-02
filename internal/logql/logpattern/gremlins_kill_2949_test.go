package logpattern

import (
	"reflect"
	"testing"
)

// Tests in this file kill LIVED gremlins mutants reported on the
// phase4-logql-other-b leg (cerberus issue #2949), whose scope includes
// this package.

// TestIdentifierAlphabetBoundaries kills the CONDITIONALS_BOUNDARY and
// CONDITIONALS_NEGATION mutants on the capture-identifier alphabet,
// pattern.go:isIdentStart:`b >= 'a'` and its three siblings on the same
// line, plus pattern.go:isIdentCont:`b >= '0'` and `b <= '9'`.
//
// A capture is `<` then `[A-Za-z_][A-Za-z0-9_]*` then `>` (see the
// package doc). Any `<` that does not open a valid capture stays an
// ordinary literal, so shrinking the alphabet by one character at either
// end of a range silently demotes a capture to text — the pattern still
// compiles, it just stops extracting the field the user asked for.
//
// Each case below sits on one range endpoint, which is exactly what a
// boundary mutant moves:
//
//   - `z`  — `b <= 'z'` -> `b < 'z'` drops the last lower-case start.
//   - `A`  — `b >= 'A'` -> `b > 'A'` drops the first upper-case start;
//     the same case also kills `b <= 'Z'` -> `b > 'Z'` (NEGATION), which
//     makes the whole upper-case arm `b >= 'A' && b > 'Z'` reject `A`.
//   - `Z`  — `b <= 'Z'` -> `b < 'Z'` drops the last upper-case start.
//   - `a0` — `b >= '0'` -> `b > '0'` drops `0` from the CONTINUATION
//     alphabet, so `<a0>` stops closing on `>`.
//   - `a9` — `b <= '9'` -> `b < '9'` drops `9` the same way.
func TestIdentifierAlphabetBoundaries(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		pattern string
		want    []string
	}{
		{name: "lower-case upper endpoint", pattern: `<z> tail`, want: []string{"z"}},
		{name: "upper-case lower endpoint", pattern: `<A> tail`, want: []string{"A"}},
		{name: "upper-case upper endpoint", pattern: `<Z> tail`, want: []string{"Z"}},
		{name: "digit continuation lower endpoint", pattern: `<a0> tail`, want: []string{"a0"}},
		{name: "digit continuation upper endpoint", pattern: `<a9> tail`, want: []string{"a9"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, err := New(tc.pattern)
			if err != nil {
				t.Fatalf("New(%q): %v — the identifier alphabet no longer accepts this capture name, so the `<…>` run stayed a literal and the pattern has no capture at all", tc.pattern, err)
			}
			if got := m.Names(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("New(%q).Names() = %q, want %q — the identifier alphabet no longer accepts this capture name", tc.pattern, got, tc.want)
			}
		})
	}
}

// TestLineFilterOffsetAdvancesCumulatively kills the
// REMOVE_SELF_ASSIGNMENTS mutant on pattern.go:Test:`off += j + len(lit)`
// (`+=` -> `=`).
//
// `off` is the cursor into the subject line: each literal run is searched
// for in `in[off:]`, so the amount found must be ADDED to the cursor, not
// assigned over it. The mutant is invisible while `off` is still 0, which
// is why the pattern below carries THREE literal runs — the error only
// appears from the second advance onwards, and then compounds.
//
// For pattern `a<_>b<_>c` against `aXbYc` the cursor walks 0 -> 1 -> 3 ->
// 5 and lands exactly on the end of the line, so the trailing literal
// leaves no remainder and Test reports a match. Under the mutant it walks
// 0 -> 1 -> 2 -> 3, stops three bytes short, and the leftover tail makes
// Test report no match.
func TestLineFilterOffsetAdvancesCumulatively(t *testing.T) {
	t.Parallel()

	const pattern = `a<_>b<_>c`
	m, err := ParseLineFilter([]byte(pattern))
	if err != nil {
		t.Fatalf("ParseLineFilter(%q): %v", pattern, err)
	}
	const line = "aXbYc"
	if !m.Test([]byte(line)) {
		t.Fatalf("ParseLineFilter(%q).Test(%q) = false, want true — the literal-run cursor was assigned instead of advanced, so it stopped short of the end of the line and reported a spurious remainder", pattern, line)
	}
}

// NOT KILLABLE — documented, not defended by a test. These are the LIVED
// and TIMED OUT mutants phase4-logql-other-b reports in this package that
// the kills above do not reach.
//
// pattern.go:`i+1 >= len(e)` guards a bare `break` that INVERT_LOOPCTRL
// rewrites to `continue`. The guard is true only on the LAST iteration of
// `for i, n := range e`, and on the last iteration `continue` and `break`
// do the same thing: the range is exhausted, so both leave the loop with
// the same state and fall through to the same `return nil`. The
// CONDITIONALS_BOUNDARY mutant on the same guard is killed, because
// widening it to `i+1 > len(e)` lets `e[i+1]` index out of range.
//
// pattern.go:`len(s) < 2` — CONDITIONALS_BOUNDARY (`<` -> `<=`). The forms
// differ only at len(s) == 2, i.e. a `<` and one identifier byte at the
// very end of the input. The original then runs the scan: j starts at 2,
// the continuation loop's `j < len(s)` is immediately false, and the
// closing-`>` test `j < len(s)` is false too, so it returns
// `("", 0, false)` — the same triple the mutant returns from the widened
// guard. The `<` itself stays a literal either way. (The guard is NOT
// redundant below that boundary: at len(s) < 2 it is what keeps `s[1]`
// from indexing out of range.)
//
// pattern.go:`i += consumed` and pattern.go:`i += size` carry the leg's
// three TIMED OUT mutants (INVERT_ASSIGNMENTS and REMOVE_SELF_ASSIGNMENTS).
// Both statements are the sole advance of `parse`'s cursor, so every
// mutant of either spins forever on the same input byte. They are the
// memory-guard-reaped class recorded on cerberus issue #2949: the guard
// holds after reaping precisely so no exit status can be read as a
// verdict, which destroys the evidence a detection credit would be read
// from. They stay in the denominator, credited to nobody.
