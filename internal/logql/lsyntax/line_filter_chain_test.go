package lsyntax

import "testing"

// lineFilterTerm is one node of a line-filter stage as the lowering reads
// it: the operator, the operand, whether it is an `or` alternate, and how
// many alternates hang off it.
type lineFilterTerm struct {
	ty        LineMatchType
	op, match string
	orChild   bool
	alts      []lineFilterTerm
}

// lineFilterChain flattens the stage's Left chain into source order (the
// oldest filter first), recording each node's Or alternates.
func lineFilterChain(t *testing.T, query string) []lineFilterTerm {
	t.Helper()
	e, err := ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	pipe, ok := e.(*PipelineExpr)
	if !ok {
		t.Fatalf("ParseExpr(%q) = %T, want *PipelineExpr", query, e)
	}
	var stage *LineFilterExpr
	for _, st := range pipe.MultiStages {
		if lf, ok := st.(*LineFilterExpr); ok {
			if stage != nil {
				t.Fatalf("ParseExpr(%q): more than one line-filter stage", query)
			}
			stage = lf
		}
	}
	if stage == nil {
		t.Fatalf("ParseExpr(%q): no line-filter stage", query)
	}
	var chain []lineFilterTerm
	for node := stage; node != nil; node = node.Left {
		term := lineFilterTerm{ty: node.Ty, op: node.Op, match: node.Match, orChild: node.IsOrChild}
		for alt := node.Or; alt != nil; alt = alt.Or {
			if alt.Left != nil {
				t.Errorf("ParseExpr(%q): alternate %q carries a Left chain", query, alt.Match)
			}
			term.alts = append(term.alts, lineFilterTerm{ty: alt.Ty, op: alt.Op, match: alt.Match, orChild: alt.IsOrChild})
		}
		chain = append([]lineFilterTerm{term}, chain...)
	}
	return chain
}

func equalTerms(a, b []lineFilterTerm) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ty != b[i].ty || a[i].op != b[i].op || a[i].match != b[i].match ||
			a[i].orChild != b[i].orChild || !equalTerms(a[i].alts, b[i].alts) {
			return false
		}
	}
	return true
}

// TestLineFilterChain_Shape pins the AST a line-filter stage parses to.
//
// A positive clause (`|=`, `|~`, `|>`) with `or` alternates is ONE node
// whose alternates hang off Or, each carrying the clause's operator and
// flagged IsOrChild. A negated clause (`!=`, `!~`, `!>`) excludes every
// alternate, so each alternate is its own AND-ed node in the Left chain,
// with the clause's operator and no IsOrChild flag. Clauses chain in
// source order on Left, and a clause never loses the filters before it.
func TestLineFilterChain_Shape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		query string
		want  []lineFilterTerm
	}{
		{
			`{app="x"} |= "a"`,
			[]lineFilterTerm{{ty: LineMatchEqual, match: "a"}},
		},
		{
			`{app="x"} |= "a" or "b" or ip("1.2.3.4")`,
			[]lineFilterTerm{{ty: LineMatchEqual, match: "a", alts: []lineFilterTerm{
				{ty: LineMatchEqual, match: "b", orChild: true},
				{ty: LineMatchEqual, op: OpFilterIP, match: "1.2.3.4", orChild: true},
			}}},
		},
		// The reference keeps, of a positive clause's alternates, only
		// those after the last `ip(...)` alternate that another
		// alternate follows: the earlier ones are not part of the
		// predicate it evaluates. Cerberus answers the same query.
		{
			`{app="x"} |= "a" or ip("1.2.3.4") or "c"`,
			[]lineFilterTerm{{ty: LineMatchEqual, match: "a", alts: []lineFilterTerm{
				{ty: LineMatchEqual, match: "c", orChild: true},
			}}},
		},
		{
			`{app="x"} |= "a" or ip("1.2.3.4") or "b" or "c"`,
			[]lineFilterTerm{{ty: LineMatchEqual, match: "a", alts: []lineFilterTerm{
				{ty: LineMatchEqual, match: "b", orChild: true},
				{ty: LineMatchEqual, match: "c", orChild: true},
			}}},
		},
		{
			`{app="x"} |~ "a" or ip("1.2.3.4") or "b" or ip("1.2.3.5") or "c"`,
			[]lineFilterTerm{{ty: LineMatchRegexp, match: "a", alts: []lineFilterTerm{
				{ty: LineMatchRegexp, match: "c", orChild: true},
			}}},
		},
		{
			`{app="x"} |= ip("1.2.3.4") or ip("1.2.3.5") or ip("1.2.3.6")`,
			[]lineFilterTerm{{ty: LineMatchEqual, op: OpFilterIP, match: "1.2.3.4", alts: []lineFilterTerm{
				{ty: LineMatchEqual, op: OpFilterIP, match: "1.2.3.6", orChild: true},
			}}},
		},
		// A negated clause keeps every alternate, `ip(...)` or not.
		{
			`{app="x"} != "a" or ip("1.2.3.4") or "b"`,
			[]lineFilterTerm{
				{ty: LineMatchNotEqual, match: "a"},
				{ty: LineMatchNotEqual, op: OpFilterIP, match: "1.2.3.4"},
				{ty: LineMatchNotEqual, match: "b"},
			},
		},
		{
			`{app="x"} |~ "a" or "b"`,
			[]lineFilterTerm{{ty: LineMatchRegexp, match: "a", alts: []lineFilterTerm{
				{ty: LineMatchRegexp, match: "b", orChild: true},
			}}},
		},
		{
			`{app="x"} |> "<_>a" or "<_>b"`,
			[]lineFilterTerm{{ty: LineMatchPattern, match: "<_>a", alts: []lineFilterTerm{
				{ty: LineMatchPattern, match: "<_>b", orChild: true},
			}}},
		},
		{
			`{app="x"} != "a" or "b" or "c"`,
			[]lineFilterTerm{
				{ty: LineMatchNotEqual, match: "a"},
				{ty: LineMatchNotEqual, match: "b"},
				{ty: LineMatchNotEqual, match: "c"},
			},
		},
		{
			`{app="x"} != ip("1.1.1.1") or ip("2.2.2.2") or "c"`,
			[]lineFilterTerm{
				{ty: LineMatchNotEqual, op: OpFilterIP, match: "1.1.1.1"},
				{ty: LineMatchNotEqual, op: OpFilterIP, match: "2.2.2.2"},
				{ty: LineMatchNotEqual, match: "c"},
			},
		},
		{
			// A negated clause after an earlier filter keeps every one of
			// its alternates: none of `!= "a"`, `!= "b"` may be dropped.
			`{app="x"} |= "p" != "a" or "b"`,
			[]lineFilterTerm{
				{ty: LineMatchEqual, match: "p"},
				{ty: LineMatchNotEqual, match: "a"},
				{ty: LineMatchNotEqual, match: "b"},
			},
		},
		{
			`{app="x"} |= "p" !~ "a" or "b" or "c" |= "q" or "r"`,
			[]lineFilterTerm{
				{ty: LineMatchEqual, match: "p"},
				{ty: LineMatchNotRegexp, match: "a"},
				{ty: LineMatchNotRegexp, match: "b"},
				{ty: LineMatchNotRegexp, match: "c"},
				{ty: LineMatchEqual, match: "q", alts: []lineFilterTerm{
					{ty: LineMatchEqual, match: "r", orChild: true},
				}},
			},
		},
		{
			`{app="x"} |= "p" |= "a" or "b"`,
			[]lineFilterTerm{
				{ty: LineMatchEqual, match: "p"},
				{ty: LineMatchEqual, match: "a", alts: []lineFilterTerm{
					{ty: LineMatchEqual, match: "b", orChild: true},
				}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			if got := lineFilterChain(t, tc.query); !equalTerms(got, tc.want) {
				t.Errorf("ParseExpr(%q) line-filter chain\n got: %+v\nwant: %+v", tc.query, got, tc.want)
			}
		})
	}
}

// TestLineFilterChain_NegatedClauseAfterAFilterRenders pins the rendering
// of a negated `or` clause that follows another filter: every alternate
// survives as its own negated filter.
func TestLineFilterChain_NegatedClauseAfterAFilterRenders(t *testing.T) {
	t.Parallel()

	cases := []struct{ query, want string }{
		{`{app="x"} |= "p" != "a" or "b"`, `{app="x"} |= "p" != "a" != "b"`},
		{`{app="x"} |= "p" != "a" or "b" or "c" |= "q"`, `{app="x"} |= "p" != "a" != "b" != "c" |= "q"`},
		{`{app="x"} != "p" !> "<_>a" or "<_>b"`, `{app="x"} != "p" !> "<_>a" !> "<_>b"`},
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

// TestLineFilterChain_OperandErrors pins the parse errors an `or`
// alternate raises when its operand is missing or malformed.
func TestLineFilterChain_OperandErrors(t *testing.T) {
	t.Parallel()

	for _, q := range []string{
		`{app="x"} |= "a" or`,
		`{app="x"} |= "a" or 5`,
		`{app="x"} |= "a" or ip("1.1.1.1"`,
		`{app="x"} != ip "1.1.1.1"`,
		`{app="x"} |=`,
	} {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseExpr(q); err == nil {
				t.Errorf("ParseExpr(%q) accepted a malformed line filter", q)
			}
		})
	}
}
