package logql

import (
	"reflect"
	"strings"
	"testing"
)

// This file pins the mutants gremlins reports LIVED in jsonpath.go. The
// clean-room JSON-path parser (jsonpath.go) replaced grafana/loki's AGPL
// jsonexpr package; its only prior coverage was jsonpath_agpl_test.go,
// which is `//go:build agpl_oracle`-tagged and therefore NOT compiled by
// the plain `go test ./internal/logql` invocation gremlins drives — so
// every predicate / boundary / control-flow mutant in the file survived.
//
// jsonpath.go is matched by NO phase's exclude_files regex in
// mutation.yml, so it is mutated by ALL FOUR phase4-logql-* phases
// (aggregation, lower, other-a, other-b). The same 18 LIVED mutants
// therefore deflated every one of those phases below the 95% efficacy
// bar; killing them here lifts all four back over the bar at once.
//
// Each case below depends on the exact value or branch a cited mutation
// alters, so the mutation breaks the assertion. Mutants pinned:
//   - isJSONStartIdentifier (line 266): boundary 'a'/'z'/'A'/'Z', the '_'
//     equality, and the two `||` joins — distinguished by single-char
//     fields "a","z","A","Z","_".
//   - isJSONIdentifier (line 270): continuation-digit boundary '0'/'9' and
//     the `||` join — distinguished by "a0","a9","ab".
//   - next() digit detection (line 170): boundaries '0'/'9' — "[0]","[9]".
//   - isJSONWhitespace (line 263): the `||` joins — space/tab/newline must
//     be skipped inside brackets.
//   - jpDot error gate (line 64): a scan error after '.' must surface the
//     scan error, not the generic "expected field" message.
//   - unread() guard (line 156): `pos > 0` must stay a no-op at pos 0.
func TestJSONPathParse_Mutation(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []any
	}{
		// isJSONStartIdentifier boundary + negation + logical joins (line 266):
		// each boundary char must be accepted as a field start.
		{"start_lower_a", "a", []any{"a"}},
		{"start_lower_z", "z", []any{"z"}},
		{"start_upper_A", "A", []any{"A"}},
		{"start_upper_Z", "Z", []any{"Z"}},
		{"start_underscore", "_", []any{"_"}},

		// isJSONIdentifier boundary + logical join (line 270): a digit must
		// be accepted as a field *continuation* char (else the field splits
		// and the bare index trips the "unexpected token" path).
		{"cont_digit_zero", "a0", []any{"a0"}},
		{"cont_digit_nine", "a9", []any{"a9"}},
		{"cont_multichar", "ab", []any{"ab"}},
		{"cont_digit_mid", "k8s", []any{"k8s"}},

		// next() digit-start detection boundaries (line 170): '0' and '9'
		// must both be recognised as the start of an integer index.
		{"index_zero", "[0]", []any{0}},
		{"index_nine", "[9]", []any{9}},
		{"index_multidigit", "[10]", []any{10}},

		// isJSONWhitespace logical joins (line 263): space, tab, and newline
		// must each be skipped — both before the index (next() skip path)
		// and after it (scanInt terminator path).
		{"ws_space_before_index", "[ 0]", []any{0}},
		{"ws_space_after_index", "[0 ]", []any{0}},
		{"ws_tab_before_index", "[\t0]", []any{0}},
		{"ws_newline_before_index", "[\n0]", []any{0}},

		// Chaining + quoted keys exercise the bracketSegment / dot paths.
		{"quoted_key", `["a"]`, []any{"a"}},
		{"chain_field_index_field", "a[0].b", []any{"a", 0, "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := jsonPathParse(tt.in)
			if err != nil {
				t.Fatalf("jsonPathParse(%q): unexpected error: %v", tt.in, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("jsonPathParse(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

// TestJSONPathParse_DotFollowedByScanErrorSurfacesScanError pins the
// `if err != nil` gate in the jpDot branch (line 64). After a '.', the
// next token is scanned; a float array index makes scanInt error. The
// original surfaces THAT error ("cannot use float array index"). Negating
// the gate (`err == nil`) instead falls through to the generic
// "expected field after '.'" message — so asserting the specific scan
// error kills the mutant.
func TestJSONPathParse_DotFollowedByScanErrorSurfacesScanError(t *testing.T) {
	_, err := jsonPathParse("a.5.0")
	if err == nil {
		t.Fatalf("jsonPathParse(%q): expected an error", "a.5.0")
	}
	if !strings.Contains(err.Error(), "float array index") {
		t.Fatalf("jsonPathParse(%q): expected the scan error to surface, got: %v", "a.5.0", err)
	}
}

// TestJSONPathParse_NonDigitInsideIndexRejected pins the `r < '0' || r > '9'`
// digit guard in scanInt (line 252). A non-digit, non-terminator char
// inside an index must be rejected with the "non-integer value" error.
// Inverting the `||` to `&&` (no char is ever both `< '0'` and `> '9'`)
// lets the stray char fall through to strconv.Atoi, which fails with a
// different ("invalid syntax") message — so asserting the specific
// "non-integer value" error kills the mutant.
func TestJSONPathParse_NonDigitInsideIndexRejected(t *testing.T) {
	_, err := jsonPathParse("[1a]")
	if err == nil {
		t.Fatalf("jsonPathParse(%q): expected an error", "[1a]")
	}
	if !strings.Contains(err.Error(), "non-integer value") {
		t.Fatalf("jsonPathParse(%q): want a non-integer-value scan error, got: %v", "[1a]", err)
	}
}

// Two scanInt/scanStr mutants are GENUINELY EQUIVALENT w.r.t. the parser's
// reachable input domain and are documented (not pinned) here:
//
//   - jsonpath.go:245:30 CONDITIONALS_BOUNDARY `len(digits) > 0` → `>= 0`
//     in scanInt's float-index guard. scanInt is only ever entered from
//     next() AFTER a digit is detected and unread, so the first rune it
//     reads is always a digit and len(digits) >= 1 by the time a '.' can
//     appear. The `> 0` qualifier therefore never evaluates at len == 0 in
//     any reachable path; `>= 0` yields identical behaviour. (A leading '.'
//     is tokenised as jpDot by next(), never routed to scanInt.)
//   - jsonpath.go:218:9 INVERT_LOGICAL `!ok || r != '"'` → `&&` in scanStr's
//     opening-quote guard. scanStr is only ever entered from next() after a
//     '"' is detected and unread, so its first read is always a successful
//     '"' (ok == true, r == '"'); both operands are false and the `||`/`&&`
//     collapse to the same false, taking the same branch.
//
// Both are defensive guards on inputs the tokeniser cannot deliver to these
// helpers; no black-box test can distinguish original from mutant.

// TestJSONPathScanner_UnreadAtStartIsNoOp pins the `sc.pos > 0` guard in
// unread() (line 156). At pos 0 the guard must keep unread a no-op;
// loosening the boundary to `>= 0` drives pos negative, and the next
// read mis-indexes (panics), failing this test.
func TestJSONPathScanner_UnreadAtStartIsNoOp(t *testing.T) {
	sc := &jsonPathScanner{src: []rune("xy")}
	sc.unread() // pos is already 0; must remain 0.
	r, ok := sc.read()
	if !ok || r != 'x' {
		t.Fatalf("read() after no-op unread = (%q, %v), want ('x', true)", r, ok)
	}
}
