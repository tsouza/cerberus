package lsyntax

import (
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
)

// TestDropLabelsTargets pins the two views of a `| drop` stage: whether any
// entry carries a matcher, and the label each entry names — the entry's
// own name, else its matcher's label, and nothing for an entry with
// neither. An empty stage yields an empty, non-nil list.
func TestDropLabelsTargets(t *testing.T) {
	t.Parallel()

	matcher := func(name string) *labels.Matcher {
		return labels.MustNewMatcher(labels.MatchEqual, name, "v")
	}
	cases := []struct {
		name        string
		entries     []NamedLabelMatcher
		wantMatcher bool
		wantNames   []string
	}{
		{"empty", nil, false, []string{}},
		{"names only", []NamedLabelMatcher{{Name: "a"}, {Name: "b"}}, false, []string{"a", "b"}},
		{"a matcher names its label", []NamedLabelMatcher{{Name: "a"}, {Matcher: matcher("b")}}, true, []string{"a", "b"}},
		{"the entry's name wins over its matcher's", []NamedLabelMatcher{{Name: "n", Matcher: matcher("m")}}, true, []string{"n"}},
		{"an entry with neither names nothing", []NamedLabelMatcher{{}, {Name: "a"}}, false, []string{"a"}},
		{"a matcher on an empty label name", []NamedLabelMatcher{{Matcher: matcher("")}}, true, []string{""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newDropLabelsExpr(c.entries)
			if got := e.HasNamedMatchers(); got != c.wantMatcher {
				t.Errorf("HasNamedMatchers() = %v, want %v", got, c.wantMatcher)
			}
			got := e.Names()
			if got == nil {
				t.Fatalf("Names() = nil, want a non-nil list")
			}
			if len(got) != len(c.wantNames) {
				t.Fatalf("Names() = %q, want %q", got, c.wantNames)
			}
			for i := range got {
				if got[i] != c.wantNames[i] {
					t.Fatalf("Names() = %q, want %q", got, c.wantNames)
				}
			}
		})
	}
}

// TestBinOpConstruction pins mustNewBinOpExpr: every leg is checked to be a
// sample expression before any literal leg is evaluated, a literal leg's
// own error comes next, a logical or set operator then rejects a literal
// leg (left before right), two literal legs fold into one literal, and any
// other pair builds the binary node.
func TestBinOpConstruction(t *testing.T) {
	t.Parallel()

	sample := func(q string) SampleExpr {
		t.Helper()
		e, err := ParseExpr(q)
		if err != nil {
			t.Fatalf("parse %q: %v", q, err)
		}
		return e.(SampleExpr)
	}
	logs := newMatcherExpr([]*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "app", "x")})
	rate := sample(`rate({app="x"}[1m])`)
	one := mustNewLiteralExpr("1", false)
	two := mustNewLiteralExpr("2", false)
	bad := mustNewLiteralExpr("nope", false)
	opts := &BinOpOptions{ReturnBool: true}

	errorCases := []struct {
		name     string
		op       string
		lhs, rhs Expr
		want     string
	}{
		{"left not a sample", OpTypeAdd, logs, rate, "parse error : unexpected type for left leg of binary operation (+): *lsyntax.MatchersExpr"},
		{"right not a sample", OpTypeAdd, rate, logs, "parse error : unexpected type for right leg of binary operation (+): *lsyntax.MatchersExpr"},
		{"left reported first", OpTypeAdd, logs, logs, "parse error : unexpected type for left leg of binary operation (+): *lsyntax.MatchersExpr"},
		{"leg types before literal values", OpTypeAdd, bad, logs, "parse error : unexpected type for right leg of binary operation (+): *lsyntax.MatchersExpr"},
		{"left literal error", OpTypeAdd, bad, rate, bad.err.Error()},
		{"right literal error", OpTypeAdd, rate, bad, bad.err.Error()},
		{"literal values before the logical check", OpTypeAnd, one, bad, bad.err.Error()},
		{"logical left literal", OpTypeAnd, one, rate, "parse error : unexpected literal for left leg of logical/set binary operation (and): 1.000000"},
		{"logical right literal", OpTypeOr, rate, two, "parse error : unexpected literal for right leg of logical/set binary operation (or): 2.000000"},
		{"logical left reported first", OpTypeUnless, one, two, "parse error : unexpected literal for left leg of logical/set binary operation (unless): 1.000000"},
	}
	for _, c := range errorCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, ok := mustNewBinOpExpr(c.op, opts, c.lhs, c.rhs).(*BinOpExpr)
			if !ok || got.err == nil {
				t.Fatalf("mustNewBinOpExpr = %#v, want a node carrying %q", got, c.want)
			}
			if got.err.Error() != c.want {
				t.Fatalf("error = %q, want %q", got.err, c.want)
			}
			if got.SampleExpr != nil || got.RHS != nil {
				t.Fatalf("an error node carries legs: %#v", got)
			}
		})
	}

	t.Run("two literals fold", func(t *testing.T) {
		t.Parallel()
		got, ok := mustNewBinOpExpr(OpTypeAdd, opts, one, two).(*LiteralExpr)
		if !ok || got.err != nil || got.Val != 3 {
			t.Fatalf("mustNewBinOpExpr(1 + 2) = %#v, want the literal 3", got)
		}
	})
	for _, legs := range [][2]SampleExpr{{rate, rate}, {one, rate}, {rate, two}} {
		t.Run("builds "+legs[0].String()+" + "+legs[1].String(), func(t *testing.T) {
			t.Parallel()
			got, ok := mustNewBinOpExpr(OpTypeAdd, opts, legs[0], legs[1]).(*BinOpExpr)
			if !ok || got.err != nil || got.Op != OpTypeAdd || got.Opts != opts || got.SampleExpr != legs[0] || got.RHS != legs[1] {
				t.Fatalf("mustNewBinOpExpr = %#v, want the binary node over both legs", got)
			}
		})
	}
}

// TestHelperRenderings pins the text of a bytes label filter, a line filter
// chain and a log range.
func TestHelperRenderings(t *testing.T) {
	t.Parallel()

	sel := newMatcherExpr([]*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "app", "x")})
	filter := func(ty LineMatchType, match, op string) *LineFilterExpr {
		return &LineFilterExpr{LineFilter: LineFilter{Ty: ty, Match: match, Op: op}}
	}
	chained := filter(LineMatchNotRegexp, "b", "")
	chained.Left = filter(LineMatchEqual, "a", "")
	alternate := filter(LineMatchEqual, "c", "")
	alternate.IsOrChild = true
	withOr := filter(LineMatchEqual, "a", "")
	withOr.Or = alternate
	ipAlternate := filter(LineMatchEqual, "10.0.0.0/8", OpFilterIP)
	ipAlternate.IsOrChild = true
	full := filter(LineMatchNotEqual, "x", OpFilterIP)
	full.Left = chained
	full.Or = ipAlternate
	unwrap := newUnwrapExpr("latency", "")

	cases := []struct {
		got, want string
	}{
		{NewBytesLabelFilter(LabelFilterGreaterThanOrEqual, "size", 5000).String(), "size>=5.0kB"},
		{NewBytesLabelFilter(LabelFilterEqual, "size", 0).String(), "size==0B"},
		{NewBytesLabelFilter(LabelFilterLesserThan, "b", 999).String(), "b<999B"},
		{NewBytesLabelFilter(LabelFilterNotEqual, "b", 1_500_000).String(), "b!=1.5MB"},
		{NewBytesLabelFilter(LabelFilterGreaterThan, "b", 12_345_678_901).String(), "b>12GB"},
		{filter(LineMatchEqual, "a", "").String(), `|= "a"`},
		{filter(LineMatchPattern, "<_> x", "").String(), `|> "<_> x"`},
		{filter(LineMatchEqual, "q\"\\\n", "").String(), `|= "q\"\\\n"`},
		{filter(LineMatchEqual, "::1", OpFilterIP).String(), `|= ip("::1")`},
		{chained.String(), `|= "a" !~ "b"`},
		{withOr.String(), `|= "a" or "c"`},
		{alternate.String(), `or "c"`},
		{full.String(), `|= "a" !~ "b" != ip("x") or ip("10.0.0.0/8")`},
		{newLogRange(sel, time.Minute, nil, nil).String(), `{app="x"}[1m]`},
		{newLogRange(sel, 90*time.Second, nil, nil).String(), `{app="x"}[1m30s]`},
		{newLogRange(sel, time.Minute, unwrap, nil).String(), `{app="x"} | unwrap latency[1m]`},
		{newLogRange(sel, 5*time.Minute, nil, newOffsetExpr(time.Hour)).String(), `{app="x"}[5m] offset 1h`},
		{newLogRange(sel, time.Minute, unwrap, newOffsetExpr(-time.Minute)).String(), `{app="x"} | unwrap latency[1m] offset -1m`},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("rendered %s, want %s", c.got, c.want)
		}
	}
}
