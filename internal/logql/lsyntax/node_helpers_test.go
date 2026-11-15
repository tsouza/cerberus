package lsyntax

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
)

// TestRenderedForms pins the canonical text of the grouping, range
// aggregation, unwrap and label-filter nodes, including the forms the
// format corpus does not reach: empty groupings, parameter decimals and
// several post-unwrap filters.
func TestRenderedForms(t *testing.T) {
	t.Parallel()

	sel := newMatcherExpr([]*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "app", "x")})
	param := func(f float64) *float64 { return &f }
	rangeOf := func(op string, p *float64, g *Grouping) *RangeAggregationExpr {
		return &RangeAggregationExpr{Left: newLogRange(sel, time.Minute, nil, nil), Operation: op, Params: p, Grouping: g}
	}
	eq := NewStringLabelFilter(labels.MustNewMatcher(labels.MatchEqual, "a", "1"))
	re := NewStringLabelFilter(labels.MustNewMatcher(labels.MatchRegexp, "b", "2.*"))
	unwrap := newUnwrapExpr("latency", "duration_seconds")
	unwrap.PostFilters = []LabelFilterer{eq, re}

	cases := []struct {
		got  string
		want string
	}{
		{Grouping{Groups: []string{"a", "b"}}.String(), " by (a,b)"},
		{Grouping{Groups: []string{"a"}, Without: true}.String(), " without (a)"},
		{Grouping{}.String(), " by ()"},
		{Grouping{Without: true}.String(), " without ()"},
		{rangeOf("count_over_time", nil, nil).String(), `count_over_time({app="x"}[1m])`},
		{rangeOf("quantile_over_time", param(0.99), nil).String(), `quantile_over_time(0.99,{app="x"}[1m])`},
		{rangeOf("quantile_over_time", param(1), nil).String(), `quantile_over_time(1,{app="x"}[1m])`},
		{rangeOf("quantile_over_time", param(100), nil).String(), `quantile_over_time(100,{app="x"}[1m])`},
		{rangeOf("quantile_over_time", param(0), nil).String(), `quantile_over_time(0,{app="x"}[1m])`},
		{rangeOf("quantile_over_time", param(-0.5), nil).String(), `quantile_over_time(-0.5,{app="x"}[1m])`},
		{rangeOf("quantile_over_time", param(1e-7), nil).String(), `quantile_over_time(0,{app="x"}[1m])`},
		{rangeOf("quantile_over_time", param(2.5000001), nil).String(), `quantile_over_time(2.5,{app="x"}[1m])`},
		{rangeOf("quantile_over_time", param(0.123456), nil).String(), `quantile_over_time(0.123456,{app="x"}[1m])`},
		{rangeOf("quantile_over_time", param(0.1234567), nil).String(), `quantile_over_time(0.123457,{app="x"}[1m])`},
		{
			rangeOf("max_over_time", nil, &Grouping{Groups: []string{"job"}}).String(),
			`max_over_time({app="x"}[1m]) by (job)`,
		},
		{newUnwrapExpr("bytes", "").String(), " | unwrap bytes"},
		{newUnwrapExpr("latency", "duration").String(), " | unwrap duration(latency)"},
		{unwrap.String(), ` | unwrap duration_seconds(latency) | a="1" | b=~"2.*"`},
		{NewAndLabelFilter(eq, re).String(), `( a="1" , b=~"2.*" )`},
		{NewOrLabelFilter(eq, NewAndLabelFilter(re, eq)).String(), `( a="1" or ( b=~"2.*" , a="1" ) )`},
		{NewIPLabelFilter("10.0.0.0/8", "addr", LabelFilterEqual).String(), `addr=ip("10.0.0.0/8")`},
		{NewIPLabelFilter("10.0.0.1", "addr", LabelFilterNotEqual).String(), `addr!=ip("10.0.0.1")`},
		{NewIPLabelFilter(`a"b`, "addr", LabelFilterEqual).String(), `addr=ip("a\"b")`},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("rendered %q, want %q", c.got, c.want)
		}
	}
}

// TestStashedConstructionErrors pins that a node whose construction failed
// reports the stashed error from Selector and Value instead of reaching its
// operands, and that a well-formed node resolves through them.
func TestStashedConstructionErrors(t *testing.T) {
	t.Parallel()

	stashed := NewParseError("stashed", 0, 0)
	sel := newMatcherExpr([]*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "app", "x")})
	rng := newLogRange(sel, time.Minute, nil, nil)
	inner := &RangeAggregationExpr{Left: rng, Operation: OpRangeTypeCount}

	failed := []SampleExpr{
		&RangeAggregationExpr{err: stashed},
		&VectorAggregationExpr{err: stashed},
		&LabelReplaceExpr{err: stashed},
	}
	for _, e := range failed {
		got, err := e.Selector()
		if got != nil || !errors.Is(err, stashed) {
			t.Errorf("%T.Selector() = (%v, %v), want (nil, the stashed error)", e, got, err)
		}
	}
	for _, e := range []SampleExpr{
		inner,
		&VectorAggregationExpr{Left: inner, Operation: OpTypeSum},
		&LabelReplaceExpr{Left: inner},
	} {
		got, err := e.Selector()
		if err != nil || got != LogSelectorExpr(sel) {
			t.Errorf("%T.Selector() = (%v, %v), want the inner selector", e, got, err)
		}
	}

	for _, c := range []struct {
		name  string
		value func() (float64, error)
		want  float64
		fails bool
	}{
		{"literal", (&LiteralExpr{Val: 2.5}).Value, 2.5, false},
		{"failed literal", (&LiteralExpr{Val: 7, err: stashed}).Value, 0, true},
		{"vector", (&VectorExpr{Val: -1}).Value, -1, false},
		{"failed vector", (&VectorExpr{Val: 7, err: stashed}).Value, 0, true},
	} {
		v, err := c.value()
		if v != c.want || (err != nil) != c.fails {
			t.Errorf("%s Value() = (%v, %v), want (%v, failed=%v)", c.name, v, err, c.want, c.fails)
		}
	}
}

// TestConstructorHelpers pins the label_replace, log-range and logfmt
// constructors: label_replace anchors its regex and, on a bad one, carries
// nothing but the error; a missing offset is zero; the logfmt flags are
// read by name, ignoring anything else.
func TestConstructorHelpers(t *testing.T) {
	t.Parallel()

	lr := mustNewLabelReplaceExpr(nil, "dst", "$1", "src", "a(b)")
	if lr.err != nil || lr.Dst != "dst" || lr.Replacement != "$1" || lr.Src != "src" || lr.Regex != "a(b)" {
		t.Errorf("label_replace fields = %+v", lr)
	}
	if lr.Re == nil || !lr.Re.MatchString("ab") || lr.Re.MatchString("xab") || lr.Re.MatchString("abx") {
		t.Errorf("label_replace regex %v is not anchored at both ends", lr.Re)
	}
	bad := mustNewLabelReplaceExpr(&LiteralExpr{}, "dst", "$1", "src", "(")
	if bad.err == nil || !strings.HasPrefix(bad.err.Error(), "parse error : invalid regex in label_replace: error parsing regexp") {
		t.Errorf("bad label_replace error = %v", bad.err)
	}
	if bad.Left != nil || bad.Dst != "" || bad.Replacement != "" || bad.Src != "" || bad.Regex != "" || bad.Re != nil {
		t.Errorf("bad label_replace carries more than its error: %+v", bad)
	}

	sel := newMatcherExpr(nil)
	unwrap := newUnwrapExpr("x", "")
	if r := newLogRange(sel, time.Minute, unwrap, nil); r.Left != LogSelectorExpr(sel) || r.Interval != time.Minute || r.Unwrap != unwrap || r.Offset != 0 {
		t.Errorf("newLogRange without offset = %+v", r)
	}
	if r := newLogRange(sel, time.Second, nil, newOffsetExpr(time.Hour)); r.Offset != time.Hour || r.Unwrap != nil {
		t.Errorf("newLogRange with offset = %+v", r)
	}

	for _, c := range []struct {
		flags             []string
		strict, keepEmpty bool
	}{
		{nil, false, false},
		{[]string{OpStrict}, true, false},
		{[]string{OpKeepEmpty}, false, true},
		{[]string{OpKeepEmpty, OpStrict, OpStrict}, true, true},
		{[]string{"--other"}, false, false},
	} {
		p := newLogfmtParserExpr(c.flags)
		x := newLogfmtExpressionParser([]LabelExtractionExpr{{Identifier: "a", Expression: "a"}}, c.flags)
		if p.Strict != c.strict || p.KeepEmpty != c.keepEmpty || x.Strict != c.strict || x.KeepEmpty != c.keepEmpty {
			t.Errorf("flags %v: parser (%v, %v), expression parser (%v, %v), want (%v, %v)",
				c.flags, p.Strict, p.KeepEmpty, x.Strict, x.KeepEmpty, c.strict, c.keepEmpty)
		}
		if len(x.Expressions) != 1 {
			t.Errorf("flags %v: expression parser lost its expressions", c.flags)
		}
	}
}

// TestParseErrorText pins the two ParseError renderings: with no position
// and with one, where either coordinate alone counts as a position.
func TestParseErrorText(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		err  ParseError
		want string
	}{
		{NewParseError("boom", 0, 0), "parse error : boom"},
		{NewParseError("boom", 2, 7), "parse error at line 2, col 7: boom"},
		{NewParseError("boom", 1, 0), "parse error at line 1, col 0: boom"},
		{NewParseError("boom", 0, 3), "parse error at line 0, col 3: boom"},
	} {
		if got := c.err.Error(); got != c.want {
			t.Errorf("Error() = %q, want %q", got, c.want)
		}
	}
}
