package lsyntax

import "testing"

// The String methods back `/loki/api/v1/format_query`, so their output is
// a wire contract: a rendered AST must be re-parseable and must render
// identically the second time round. Before this file the only test that
// exercised them was oracle_agpl_test.go, which is gated behind the
// `agpl_oracle` build tag and never runs in `go test ./...` — so the
// canonical rendering of most stages (parser stages, label filters,
// drop/keep, unwrap, offset, grouping, vector matching, variants) had no
// default-tag coverage at all.

// formatCorpus pairs a query with the canonical LogQL its AST renders to.
// It spans every stage and metric form the grammar accepts, so a
// rendering change anywhere in string.go moves a line in this table.
var formatCorpus = []struct {
	query string
	want  string
}{
	// --- stream selectors ---
	{`{app="x"}`, `{app="x"}`},
	{
		`{app="x", env=~"prod|dev", ns!="kube", host!~"a.*"}`,
		`{app="x", env=~"prod|dev", ns!="kube", host!~"a.*"}`,
	},

	// --- line filters ---
	{
		`{app="x"} |= "err" != "warn" |~ "e.+" !~ "w.+"`,
		`{app="x"} |= "err" != "warn" |~ "e.+" !~ "w.+"`,
	},
	{
		`{app="x"} |> "<_>err<_>" !> "<_>ok<_>"`,
		`{app="x"} |> "<_>err<_>" !> "<_>ok<_>"`,
	},
	// A positive operator keeps the `or` alternates as an Or chain.
	{`{app="x"} |= "a" or "b"`, `{app="x"} |= "a" or "b"`},
	// A negated operator De Morgans the alternates into a nested chain,
	// so the rendering loses the `or` — `!= "a" or "b"` IS `!= "a" != "b"`.
	{`{app="x"} != "a" or "b"`, `{app="x"} != "a" != "b"`},
	{`{app="x"} |= ip("10.0.0.1")`, `{app="x"} |= ip("10.0.0.1")`},
	{`{app="x"} != ip("10.0.0.0/24")`, `{app="x"} != ip("10.0.0.0/24")`},

	// --- parser stages ---
	{`{app="x"} | logfmt`, `{app="x"} | logfmt`},
	{`{app="x"} | logfmt --strict --keep-empty`, `{app="x"} | logfmt --strict --keep-empty`},
	{`{app="x"} | logfmt a="b.c", d`, `{app="x"} | logfmt a="b.c", d`},
	{`{app="x"} | logfmt --strict a="b.c"`, `{app="x"} | logfmt --strict a="b.c"`},
	{`{app="x"} | json`, `{app="x"} | json`},
	{`{app="x"} | json a="b.c", d="e"`, `{app="x"} | json a="b.c", d="e"`},
	{`{app="x"} | unpack`, `{app="x"} | unpack`},
	{`{app="x"} | regexp "(?P<foo>.+)"`, `{app="x"} | regexp "(?P<foo>.+)"`},
	{`{app="x"} | pattern "<foo> bar"`, `{app="x"} | pattern "<foo> bar"`},

	// --- format / projection stages ---
	{`{app="x"} | line_format "{{.foo}}"`, `{app="x"} | line_format "{{.foo}}"`},
	{
		`{app="x"} | label_format dst=src, other="{{.foo}}"`,
		`{app="x"} | label_format dst=src,other="{{.foo}}"`,
	},
	{`{app="x"} | decolorize`, `{app="x"} | decolorize`},
	{`{app="x"} | drop a, b="v"`, `{app="x"} | drop a,b="v"`},
	{`{app="x"} | keep a, b="v"`, `{app="x"} | keep a,b="v"`},

	// --- label filters ---
	{`{app="x"} | foo = "bar"`, `{app="x"} | foo="bar"`},
	{
		`{app="x"} | foo != "bar" | bar =~ "b.+" | baz !~ "z.+"`,
		`{app="x"} | foo!="bar" | bar=~"b.+" | baz!~"z.+"`,
	},
	{`{app="x"} | status >= 400`, `{app="x"} | status>=400`},
	{`{app="x"} | dur > 1s`, `{app="x"} | dur>1s`},
	// The bytes filter renders humanised, with the space stripped.
	{`{app="x"} | size > 5kB`, `{app="x"} | size>5.0kB`},
	// An ip() filter renders `=` (not `==`) for the equal case.
	{`{app="x"} | addr = ip("10.0.0.1")`, `{app="x"} | addr=ip("10.0.0.1")`},
	{`{app="x"} | addr != ip("10.0.0.1")`, `{app="x"} | addr!=ip("10.0.0.1")`},
	// `and` renders as `,` and binds tighter than `or`.
	{
		`{app="x"} | foo == 1 and bar < 2 or baz = "z"`,
		`{app="x"} | ( ( foo==1 , bar<2 ) or baz="z" )`,
	},
	{`{app="x"} | (foo == 1 or bar == 2)`, `{app="x"} | ( foo==1 or bar==2 )`},
	{`{app="x"} | __error__ = ""`, `{app="x"} | __error__=""`},

	// --- range aggregations ---
	{`count_over_time({app="x"}[5m])`, `count_over_time({app="x"}[5m])`},
	{`count_over_time({app="x"}[5m] offset 1h)`, `count_over_time({app="x"}[5m] offset 1h)`},
	{`rate({app="x"} | logfmt [1m])`, `rate({app="x"} | logfmt[1m])`},
	{`bytes_over_time({app="x"}[30s])`, `bytes_over_time({app="x"}[30s])`},
	{`bytes_rate({app="x"}[30s])`, `bytes_rate({app="x"}[30s])`},
	{`absent_over_time({app="x"}[5m])`, `absent_over_time({app="x"}[5m])`},
	{`avg_over_time({app="x"} | unwrap bytes [5m])`, `avg_over_time({app="x"} | unwrap bytes[5m])`},
	{
		`sum_over_time({app="x"} | unwrap duration(dur) [5m])`,
		`sum_over_time({app="x"} | unwrap duration(dur)[5m])`,
	},
	{
		`max_over_time({app="x"} | unwrap duration_seconds(dur) [5m]) by (host)`,
		`max_over_time({app="x"} | unwrap duration_seconds(dur)[5m]) by (host)`,
	},
	{
		`min_over_time({app="x"} | unwrap v [5m]) without (host)`,
		`min_over_time({app="x"} | unwrap v[5m]) without (host)`,
	},
	{
		`quantile_over_time(0.99, {app="x"} | unwrap v [5m]) by (a, b)`,
		`quantile_over_time(0.99,{app="x"} | unwrap v[5m]) by (a,b)`,
	},
	{`stddev_over_time({app="x"} | unwrap v [5m])`, `stddev_over_time({app="x"} | unwrap v[5m])`},
	{`stdvar_over_time({app="x"} | unwrap v [5m])`, `stdvar_over_time({app="x"} | unwrap v[5m])`},
	{`first_over_time({app="x"} | unwrap v [5m])`, `first_over_time({app="x"} | unwrap v[5m])`},
	{`last_over_time({app="x"} | unwrap v [5m])`, `last_over_time({app="x"} | unwrap v[5m])`},
	{`rate_counter({app="x"} | unwrap v [5m])`, `rate_counter({app="x"} | unwrap v[5m])`},
	{
		`avg_over_time({app="x"} | unwrap bytes | foo > 5 [5m])`,
		`avg_over_time({app="x"} | unwrap bytes | foo>5[5m])`,
	},

	// --- vector aggregations ---
	{`sum(rate({app="x"}[5m])) by (host)`, `sum by (host)(rate({app="x"}[5m]))`},
	{`avg without (host) (rate({app="x"}[5m]))`, `avg without (host)(rate({app="x"}[5m]))`},
	{`count(rate({app="x"}[5m]))`, `count(rate({app="x"}[5m]))`},
	{`min(rate({app="x"}[5m]))`, `min(rate({app="x"}[5m]))`},
	{`max(rate({app="x"}[5m]))`, `max(rate({app="x"}[5m]))`},
	{`stddev(rate({app="x"}[5m]))`, `stddev(rate({app="x"}[5m]))`},
	{`stdvar(rate({app="x"}[5m]))`, `stdvar(rate({app="x"}[5m]))`},
	{`topk(5, rate({app="x"}[5m]))`, `topk(5,rate({app="x"}[5m]))`},
	{`bottomk(3, rate({app="x"}[5m]))`, `bottomk(3,rate({app="x"}[5m]))`},
	{`approx_topk(3, rate({app="x"}[5m]))`, `approx_topk(3,rate({app="x"}[5m]))`},
	{`sort(rate({app="x"}[5m]))`, `sort(rate({app="x"}[5m]))`},
	{`sort_desc(rate({app="x"}[5m]))`, `sort_desc(rate({app="x"}[5m]))`},

	// --- literals, vector(), label_replace ---
	{`10`, `10`},
	// Two literal legs constant-fold before rendering.
	{`1 + 2`, `3`},
	{`vector(1)`, `vector(1.000000)`},
	{
		`label_replace(rate({app="x"}[5m]), "dst", "$1", "src", "(.*)")`,
		`label_replace(rate({app="x"}[5m]),"dst","$1","src","(.*)")`,
	},

	// --- binary operations ---
	{`rate({app="x"}[5m]) + rate({app="y"}[5m])`, `rate({app="x"}[5m]) + rate({app="y"}[5m])`},
	{
		`rate({app="x"}[5m]) / ignoring(host) rate({app="y"}[5m])`,
		`rate({app="x"}[5m]) / ignoring(host) rate({app="y"}[5m])`,
	},
	{
		`rate({app="x"}[5m]) > bool on(host) group_left(extra) rate({app="y"}[5m])`,
		`rate({app="x"}[5m]) > bool on(host) group_left(extra) rate({app="y"}[5m])`,
	},
	{
		`rate({app="x"}[5m]) and on(host) group_right(extra) rate({app="y"}[5m])`,
		`rate({app="x"}[5m]) and on(host) group_right(extra) rate({app="y"}[5m])`,
	},
	{`rate({app="x"}[5m]) or rate({app="y"}[5m])`, `rate({app="x"}[5m]) or rate({app="y"}[5m])`},
	{
		`rate({app="x"}[5m]) unless rate({app="y"}[5m])`,
		`rate({app="x"}[5m]) unless rate({app="y"}[5m])`,
	},
	{`2 * rate({app="x"}[5m])`, `2 * rate({app="x"}[5m])`},

	// --- variants ---
	{
		`variants(count_over_time({app="x"}[5m]), rate({app="x"}[5m])) of ({app="x"}[5m])`,
		`variants(count_over_time({app="x"}[5m]), rate({app="x"}[5m])) of ({app="x"}[5m])`,
	},
}

func TestExprString_Canonical(t *testing.T) {
	for _, tc := range formatCorpus {
		t.Run(tc.query, func(t *testing.T) {
			e, err := ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): unexpected error: %v", tc.query, err)
			}
			if got := e.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExprString_ReparsesToItself(t *testing.T) {
	// The rendered form is what format_query hands back to Grafana, so it
	// must parse and must be a fixed point of the render — otherwise a
	// client that formats twice gets two different queries.
	for _, tc := range formatCorpus {
		t.Run(tc.query, func(t *testing.T) {
			e, err := ParseExpr(tc.want)
			if err != nil {
				t.Fatalf("ParseExpr(%q) (the rendered form): unexpected error: %v", tc.want, err)
			}
			if got := e.String(); got != tc.want {
				t.Errorf("re-rendered %q as %q, want a fixed point", tc.want, got)
			}
		})
	}
}

func TestGroupingString(t *testing.T) {
	cases := []struct {
		name string
		g    Grouping
		want string
	}{
		{name: "by with labels", g: Grouping{Groups: []string{"a", "b"}}, want: " by (a,b)"},
		{name: "without with labels", g: Grouping{Groups: []string{"a"}, Without: true}, want: " without (a)"},
		{name: "empty by", g: Grouping{}, want: " by ()"},
		{name: "empty without", g: Grouping{Without: true}, want: " without ()"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.g.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
			// Only a `by ()` with no labels is the "no grouping was
			// written" singleton the vector-aggregation renderer omits.
			wantSingleton := len(tc.g.Groups) == 0 && !tc.g.Without
			if got := tc.g.singleton(); got != wantSingleton {
				t.Errorf("singleton() = %v, want %v", got, wantSingleton)
			}
		})
	}
}

func TestBinOpExprString_IncompleteNodeRendersEmpty(t *testing.T) {
	// An errored BinOpExpr carries nil legs (mustNewBinOpExpr stashes the
	// error instead of the operands); String must not dereference them.
	if got := (&BinOpExpr{Op: OpTypeAdd}).String(); got != "" {
		t.Errorf("String() on a leg-less BinOpExpr = %q, want %q", got, "")
	}
}

func TestVectorMatchCardinalityString(t *testing.T) {
	cases := []struct {
		card VectorMatchCardinality
		want string
	}{
		{CardOneToOne, "one-to-one"},
		{CardManyToOne, "many-to-one"},
		{CardOneToMany, "one-to-many"},
		{VectorMatchCardinality(-1), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.card.String(); got != tc.want {
			t.Errorf("VectorMatchCardinality(%d).String() = %q, want %q", tc.card, got, tc.want)
		}
	}
}

func TestLineMatchTypeString(t *testing.T) {
	cases := []struct {
		ty   LineMatchType
		want string
	}{
		{LineMatchEqual, "|="},
		{LineMatchNotEqual, "!="},
		{LineMatchRegexp, "|~"},
		{LineMatchNotRegexp, "!~"},
		{LineMatchPattern, "|>"},
		{LineMatchNotPattern, "!>"},
		{LineMatchType(-1), ""},
	}
	for _, tc := range cases {
		if got := tc.ty.String(); got != tc.want {
			t.Errorf("LineMatchType(%d).String() = %q, want %q", tc.ty, got, tc.want)
		}
	}
}

func TestLabelFilterTypeString(t *testing.T) {
	cases := []struct {
		ty   LabelFilterType
		want string
	}{
		{LabelFilterEqual, "=="},
		{LabelFilterNotEqual, "!="},
		{LabelFilterGreaterThan, ">"},
		{LabelFilterGreaterThanOrEqual, ">="},
		{LabelFilterLesserThan, "<"},
		{LabelFilterLesserThanOrEqual, "<="},
		{LabelFilterType(-1), ""},
	}
	for _, tc := range cases {
		if got := tc.ty.String(); got != tc.want {
			t.Errorf("LabelFilterType(%d).String() = %q, want %q", tc.ty, got, tc.want)
		}
	}
}

func TestLabelFiltererString(t *testing.T) {
	// The constructors are the surface cerberus's own code builds filters
	// through (the parser aside), so their rendering is pinned directly.
	cases := []struct {
		name string
		f    LabelFilterer
		want string
	}{
		{
			name: "numeric",
			f:    NewNumericLabelFilter(LabelFilterGreaterThan, "status", 400),
			want: "status>400",
		},
		{
			name: "duration",
			f:    NewDurationLabelFilter(LabelFilterLesserThanOrEqual, "dur", 90e9),
			want: "dur<=1m30s",
		},
		{
			name: "bytes",
			f:    NewBytesLabelFilter(LabelFilterGreaterThanOrEqual, "size", 5000),
			want: "size>=5.0kB",
		},
		{
			name: "ip equal renders a single '='",
			f:    NewIPLabelFilter("10.0.0.1", "addr", LabelFilterEqual),
			want: `addr=ip("10.0.0.1")`,
		},
		{
			name: "ip not-equal",
			f:    NewIPLabelFilter("10.0.0.1", "addr", LabelFilterNotEqual),
			want: `addr!=ip("10.0.0.1")`,
		},
		{
			name: "and renders as a comma",
			f: NewAndLabelFilter(
				NewNumericLabelFilter(LabelFilterEqual, "a", 1),
				NewNumericLabelFilter(LabelFilterEqual, "b", 2),
			),
			want: "( a==1 , b==2 )",
		},
		{
			name: "or",
			f: NewOrLabelFilter(
				NewNumericLabelFilter(LabelFilterEqual, "a", 1),
				NewNumericLabelFilter(LabelFilterEqual, "b", 2),
			),
			want: "( a==1 or b==2 )",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.f.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}
