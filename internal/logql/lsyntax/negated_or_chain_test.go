package lsyntax

import "testing"

// TestNegatedOrChain_DeMorgansEveryAlternate pins the rewrite a negated
// line-filter `or` chain must get.
//
// `!= "a" or "b" or "c"` means "exclude every line containing a, b or c".
// De Morgan turns that into `!a AND !b AND !c` — it distributes over the
// WHOLE chain, not just its head. The alternates reach newOrLineFilterExpr
// as an Or chain because parseOrFilter builds them before the head operator
// is known (they default to LineMatchEqual and take the positive branch),
// so folding only the first alternate left the tail hanging off Or and
// produced `!a AND (!b OR !c)`. That predicate admits a line containing "b"
// but not "c" — a row the reference engine drops.
//
// The two-term case was the only one covered, and it is exactly the one
// that happened to work: with a single alternate there is no tail to lose.
// Every case below therefore carries three terms or more.
func TestNegatedOrChain_DeMorgansEveryAlternate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		query string
		want  string
	}{
		{`{app="x"} != "a" or "b" or "c"`, `{app="x"} != "a" != "b" != "c"`},
		{`{app="x"} != "a" or "b" or "c" or "d"`, `{app="x"} != "a" != "b" != "c" != "d"`},
		{`{app="x"} !~ "a" or "b" or "c"`, `{app="x"} !~ "a" !~ "b" !~ "c"`},
		{`{app="x"} !> "a" or "b" or "c"`, `{app="x"} !> "a" !> "b" !> "c"`},

		// A single alternate is unchanged: the two-term case already
		// folded correctly and must keep doing so.
		{`{app="x"} != "a" or "b"`, `{app="x"} != "a" != "b"`},

		// Positive operators keep their Or chain — De Morgan does not
		// apply, and flattening these would turn a union into an
		// intersection.
		{`{app="x"} |= "a" or "b" or "c"`, `{app="x"} |= "a" or "b" or "c"`},
		{`{app="x"} |~ "a" or "b" or "c"`, `{app="x"} |~ "a" or "b" or "c"`},

		// A negated chain followed by a positive one: each stage keeps
		// its own rule.
		{
			`{app="x"} != "a" or "b" or "c" |= "d" or "e"`,
			`{app="x"} != "a" != "b" != "c" |= "d" or "e"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()

			e, err := ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			if got := e.String(); got != tc.want {
				t.Errorf("ParseExpr(%q).String()\n got: %s\nwant: %s", tc.query, got, tc.want)
			}
		})
	}
}

// TestNegatedOrChain_CarriesNoOrAlternates is the structural half of the
// guarantee above: after the fold, no node in a negated chain may still
// hold an Or alternate. String() alone could hide a surviving alternate if
// the renderer ever changed, so this walks the AST directly.
func TestNegatedOrChain_CarriesNoOrAlternates(t *testing.T) {
	t.Parallel()

	for _, query := range []string{
		`{app="x"} != "a" or "b" or "c"`,
		`{app="x"} !~ "a" or "b" or "c" or "d"`,
		`{app="x"} !> "a" or "b" or "c"`,
	} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			e, err := ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}

			pipe, ok := e.(*PipelineExpr)
			if !ok {
				t.Fatalf("ParseExpr(%q) = %T, want *PipelineExpr", query, e)
			}
			var lf *LineFilterExpr
			for _, st := range pipe.MultiStages {
				if candidate, ok := st.(*LineFilterExpr); ok {
					lf = candidate
				}
			}
			if lf == nil {
				t.Fatalf("ParseExpr(%q): no LineFilterExpr in the pipeline", query)
			}

			terms := 0
			for node := lf; node != nil; node = node.Left {
				terms++
				if node.Or != nil {
					t.Errorf(
						"negated chain kept an Or alternate on the %q term: "+
							"the predicate is an OR of negations, not an AND",
						node.Match,
					)
				}
				if node.IsOrChild {
					t.Errorf("negated chain term %q is still flagged IsOrChild", node.Match)
				}
			}
			if want := 3; terms < want {
				t.Errorf("chain has %d terms, want at least %d — alternates were lost, not folded", terms, want)
			}
		})
	}
}
