package logql

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestHasTimeWindowAsymmetric pins the asymmetric guards in
// [lowerCtx.hasTimeWindow]: a non-degenerate window requires BOTH bounds
// to be non-zero. The helper reads `!Start.IsZero() && !End.IsZero()`;
// flipping the connective to `||` would treat a half-zero pair as a
// valid window and emit a spurious BETWEEN predicate. Test each of the
// four corners explicitly.
func TestHasTimeWindowAsymmetric(t *testing.T) {
	t.Parallel()

	someTS := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		start time.Time
		end   time.Time
		want  bool
	}{
		{name: "both zero -> no window", start: time.Time{}, end: time.Time{}, want: false},
		{name: "only start set -> no window", start: someTS, end: time.Time{}, want: false},
		{name: "only end set -> no window", start: time.Time{}, end: someTS, want: false},
		{name: "both set -> window", start: someTS, end: someTS.Add(time.Hour), want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			lc := lowerCtx{Start: tc.start, End: tc.end}
			if got := lc.hasTimeWindow(); got != tc.want {
				t.Fatalf("hasTimeWindow() = %v, want %v (start=%v end=%v)", got, tc.want, tc.start, tc.end)
			}
		})
	}
}

// TestRegexpMergeLabelsSkipsUnnamedSubexps pins the
// `i == 0 || n == ""` skip in [regexpMergeLabels]: index 0 is the
// whole-match group and any positional (unnamed) subexp at i > 0 has no
// name in `re.SubexpNames()`. Both shapes must be dropped before the
// duplicate-detection map (`seen`) ingests them — otherwise multiple
// unnamed subexps in the same pattern would all hash under the same
// empty-string key, tripping the duplicate-capture guard and erroring
// out on patterns LogQL accepts. Flipping the connective to `&&` keeps
// the i==0 skip but lets every positional subexp leak through.
//
// The returned expression is a `mapConcat(prev, map(<key>, <val>, ...))`
// FuncCall — the inner `map(...)` must carry exactly `2*len(named)` args.
// We pin both directions: (a) the inner map has 2 args (one named
// capture, key+value), (b) every other arg of that map is a non-empty
// string literal (so unnamed subexps with `n == ""` did not leak in
// as keys).
func TestRegexpMergeLabelsSkipsUnnamedSubexps(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()

	// Pattern with two unnamed positional groups plus one named group.
	// SubexpNames() returns ["", "", "", "name"]. The original guard
	// processes only index 3; the mutant `&&` form would walk all four,
	// build three namedGroup entries (two with empty names), and trip
	// the "duplicate named capture" error on the second empty name.
	pattern := `(\d+)-(\d+) (?P<name>\w+)`

	expr, err := regexpMergeLabels(nil, s, pattern)
	if err != nil {
		t.Fatalf("regexpMergeLabels(%q) returned error: %v — unnamed subexps leaked into the duplicate-check map", pattern, err)
	}
	if expr == nil {
		t.Fatalf("regexpMergeLabels(%q) returned nil expression", pattern)
	}

	outer, ok := expr.(*chplan.FuncCall)
	if !ok {
		t.Fatalf("regexpMergeLabels(%q) returned %T, want *chplan.FuncCall (mapConcat)", pattern, expr)
	}
	if outer.Name != "mapConcat" {
		t.Fatalf("regexpMergeLabels(%q) outer FuncCall.Name = %q, want %q", pattern, outer.Name, "mapConcat")
	}
	if len(outer.Args) != 2 {
		t.Fatalf("regexpMergeLabels(%q) mapConcat has %d args, want 2", pattern, len(outer.Args))
	}

	inner, ok := outer.Args[1].(*chplan.FuncCall)
	if !ok {
		t.Fatalf("regexpMergeLabels(%q) inner Args[1] is %T, want *chplan.FuncCall (map)", pattern, outer.Args[1])
	}
	if inner.Name != "map" {
		t.Fatalf("regexpMergeLabels(%q) inner FuncCall.Name = %q, want %q", pattern, inner.Name, "map")
	}

	// One named capture -> exactly 2 (key, value) args. With the `&&`
	// mutant, this would be 6 (three captures: two unnamed + the named
	// "name") — assuming the duplicate-check didn't error first.
	if len(inner.Args) != 2 {
		t.Fatalf("regexpMergeLabels(%q) inner map has %d args, want 2 — unnamed subexps leaked through the skip", pattern, len(inner.Args))
	}

	// First arg is the key literal. Confirm it's the named capture and
	// not the empty positional-subexp name.
	key, ok := inner.Args[0].(*chplan.LitString)
	if !ok {
		t.Fatalf("regexpMergeLabels(%q) inner map key is %T, want *chplan.LitString", pattern, inner.Args[0])
	}
	if key.V != "name" {
		t.Fatalf("regexpMergeLabels(%q) inner map key = %q, want %q", pattern, key.V, "name")
	}
}
