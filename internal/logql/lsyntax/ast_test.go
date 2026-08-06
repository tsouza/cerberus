package lsyntax

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
)

// ast.go's constructors are the parser's validation layer: they stash a
// ParseError on the node (or panic with one) instead of returning it, and
// the accessors are what cerberus's lowering reads the AST through. Both
// were only reachable through the `agpl_oracle`-gated oracle test, so the
// rejection paths and the accessor error propagation had no default-tag
// coverage.

func TestFoldScalarBinOp(t *testing.T) {
	cases := []struct {
		op          string
		left, right float64
		want        float64
	}{
		{op: OpTypeAdd, left: 2, right: 3, want: 5},
		{op: OpTypeSub, left: 2, right: 3, want: -1},
		{op: OpTypeMul, left: 2, right: 3, want: 6},
		{op: OpTypeDiv, left: 6, right: 3, want: 2},
		{op: OpTypeMod, left: 7, right: 3, want: 1},
		{op: OpTypePow, left: 2, right: 3, want: 8},
		{op: OpTypeCmpEQ, left: 2, right: 2, want: 1},
		{op: OpTypeCmpEQ, left: 2, right: 3, want: 0},
		{op: OpTypeNEQ, left: 2, right: 3, want: 1},
		{op: OpTypeNEQ, left: 2, right: 2, want: 0},
		{op: OpTypeGT, left: 3, right: 2, want: 1},
		{op: OpTypeGT, left: 2, right: 2, want: 0},
		{op: OpTypeGTE, left: 2, right: 2, want: 1},
		{op: OpTypeGTE, left: 1, right: 2, want: 0},
		{op: OpTypeLT, left: 1, right: 2, want: 1},
		{op: OpTypeLT, left: 2, right: 2, want: 0},
		{op: OpTypeLTE, left: 2, right: 2, want: 1},
		{op: OpTypeLTE, left: 3, right: 2, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			got, err := foldScalarBinOp(tc.op, tc.left, tc.right)
			if err != nil {
				t.Fatalf("foldScalarBinOp(%q, %v, %v): unexpected error: %v", tc.op, tc.left, tc.right, err)
			}
			if got != tc.want {
				t.Errorf("foldScalarBinOp(%q, %v, %v) = %v, want %v", tc.op, tc.left, tc.right, got, tc.want)
			}
		})
	}
}

func TestFoldScalarBinOp_ByZeroIsNaN(t *testing.T) {
	// Upstream's scalar merge yields NaN (not +Inf, and not an error) for
	// a zero divisor / modulus, so folding must too.
	for _, op := range []string{OpTypeDiv, OpTypeMod} {
		t.Run(op, func(t *testing.T) {
			got, err := foldScalarBinOp(op, 1, 0)
			if err != nil {
				t.Fatalf("foldScalarBinOp(%q, 1, 0): unexpected error: %v", op, err)
			}
			if !math.IsNaN(got) {
				t.Errorf("foldScalarBinOp(%q, 1, 0) = %v, want NaN", op, got)
			}
		})
	}
}

func TestReduceBinOp_UnexpectedOperationBecomesALiteralError(t *testing.T) {
	// The parser never routes a non-scalar op here, so this is the
	// defensive arm: the error must ride on the literal rather than be
	// dropped, because Value() is what the lowering reads.
	lit := reduceBinOp(OpTypeOr, 1, 2)
	_, err := lit.Value()
	if err == nil {
		t.Fatal("Value() on a folded literal with an unexpected op: got nil error, want one")
	}
	if !strings.Contains(err.Error(), OpTypeOr) {
		t.Errorf("Value() error = %q, want it to name the operation %q", err, OpTypeOr)
	}
}

func TestIsLogicalBinOp(t *testing.T) {
	for _, op := range []string{OpTypeOr, OpTypeAnd, OpTypeUnless} {
		if !IsLogicalBinOp(op) {
			t.Errorf("IsLogicalBinOp(%q) = false, want true", op)
		}
	}
	for _, op := range []string{OpTypeAdd, OpTypeDiv, OpTypeCmpEQ} {
		if IsLogicalBinOp(op) {
			t.Errorf("IsLogicalBinOp(%q) = true, want false", op)
		}
	}
}

func TestParseExpr_Rejections(t *testing.T) {
	// Every case here is a constructor-level rejection: the node stashes
	// or panics with a ParseError that ParseExpr must surface.
	cases := []struct {
		name    string
		query   string
		wantMsg string
	}{
		{
			name:    "regexp parser with an uncompilable pattern",
			query:   `{app="x"} | regexp "("`,
			wantMsg: "invalid regexp parser",
		},
		{
			name:    "regexp parser without any capture group",
			query:   `{app="x"} | regexp "foo"`,
			wantMsg: "at least one named capture must be supplied",
		},
		{
			name:    "regexp parser with only unnamed capture groups",
			query:   `{app="x"} | regexp "(foo)"`,
			wantMsg: "at least one named capture must be supplied",
		},
		{
			name:    "regexp parser with duplicate capture names",
			query:   `{app="x"} | regexp "(?P<a>x)(?P<a>y)"`,
			wantMsg: "duplicate extracted label name 'a'",
		},
		{
			name:    "pattern parser without a named capture",
			query:   `{app="x"} | pattern "foo"`,
			wantMsg: "invalid pattern parser",
		},
		{
			name:    "label_replace with an uncompilable regex",
			query:   `label_replace(rate({app="x"}[5m]), "dst", "$1", "src", "(")`,
			wantMsg: "invalid regex in label_replace",
		},
		{
			name:    "matcher with an uncompilable regex",
			query:   `{app=~"("}`,
			wantMsg: "error parsing regexp",
		},
		{
			name:    "quantile_over_time without its parameter",
			query:   `quantile_over_time({app="x"} | unwrap v [5m])`,
			wantMsg: "parameter required for operation quantile_over_time",
		},
		{
			name:    "range aggregation parameter on an operation that takes none",
			query:   `rate(0.99, {app="x"}[5m])`,
			wantMsg: "not supported for operation rate",
		},
		{
			name:    "topk without its parameter",
			query:   `topk(rate({app="x"}[5m]))`,
			wantMsg: "parameter required for operation topk",
		},
		{
			name:    "topk with a non-positive parameter",
			query:   `topk(0, rate({app="x"}[5m]))`,
			wantMsg: "must be greater than 0",
		},
		{
			name:    "approx_topk with a grouping",
			query:   `approx_topk(5, rate({app="x"}[5m])) by (host)`,
			wantMsg: "grouping not allowed for approx_topk aggregation",
		},
		{
			name:    "parameter on a vector aggregation that takes none",
			query:   `sum(5, rate({app="x"}[5m]))`,
			wantMsg: "unsupported parameter for operation sum",
		},
		{
			name:    "grouping on a range aggregation that takes none",
			query:   `rate({app="x"}[5m]) by (host)`,
			wantMsg: "grouping not allowed for rate aggregation",
		},
		{
			name:    "value aggregation without an unwrap",
			query:   `avg_over_time({app="x"}[5m])`,
			wantMsg: "invalid aggregation avg_over_time without unwrap",
		},
		{
			name:    "line-counting aggregation with an unwrap",
			query:   `count_over_time({app="x"} | unwrap v [5m])`,
			wantMsg: "invalid aggregation count_over_time with unwrap",
		},
		{
			name:    "literal leg on a logical operator",
			query:   `1 or rate({app="x"}[5m])`,
			wantMsg: "unexpected literal for left leg of logical/set binary operation",
		},
		{
			name:    "literal right leg on a logical operator",
			query:   `rate({app="x"}[5m]) unless 1`,
			wantMsg: "unexpected literal for right leg of logical/set binary operation",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := ParseExpr(tc.query)
			if err == nil {
				t.Fatalf("ParseExpr(%q) = %v, want an error", tc.query, e)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("ParseExpr(%q) error = %q, want it to contain %q", tc.query, err, tc.wantMsg)
			}
		})
	}
}

func TestSelector_PropagatesTheStashedError(t *testing.T) {
	// A constructor that fails stashes the error on the node instead of
	// returning it, so Selector() is the only place the lowering learns
	// about it. ParseExpr checks it too — these assert the accessor
	// itself, for the nodes cerberus builds outside the parser.
	wantErr := NewParseError("boom", 0, 0)
	cases := []struct {
		name string
		expr SampleExpr
	}{
		{name: "range aggregation", expr: &RangeAggregationExpr{err: wantErr}},
		{name: "vector aggregation", expr: &VectorAggregationExpr{err: wantErr}},
		{name: "binary operation", expr: &BinOpExpr{err: wantErr}},
		{name: "label_replace", expr: &LabelReplaceExpr{err: wantErr}},
		{name: "literal", expr: &LiteralExpr{err: wantErr}},
		{name: "vector()", expr: &VectorExpr{err: wantErr}},
		{name: "variants", expr: &MultiVariantExpr{err: wantErr}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel, err := tc.expr.Selector()
			if !errors.Is(err, wantErr) {
				t.Fatalf("Selector() error = %v, want %v", err, wantErr)
			}
			if tc.name == "literal" || tc.name == "vector()" {
				// These two are their own log selector, so they hand
				// themselves back alongside the error.
				return
			}
			if sel != nil {
				t.Errorf("Selector() = %v, want nil alongside the error", sel)
			}
		})
	}
}

func TestMultiVariantExpr_Accessors(t *testing.T) {
	e, err := ParseExpr(`variants(count_over_time({app="x"}[5m])) of ({app="x", env="prod"}[5m])`)
	if err != nil {
		t.Fatalf("ParseExpr: unexpected error: %v", err)
	}
	mv, ok := e.(*MultiVariantExpr)
	if !ok {
		t.Fatalf("ParseExpr returned %T, want *MultiVariantExpr", e)
	}
	if got := len(mv.Variants()); got != 1 {
		t.Errorf("Variants() has %d entries, want 1", got)
	}
	lr := mv.LogRange()
	if lr == nil {
		t.Fatal("LogRange() = nil, want the `of (...)` range")
	}
	if lr.Interval != 5*time.Minute {
		t.Errorf("LogRange().Interval = %v, want 5m", lr.Interval)
	}
	if got := len(mv.Matchers()); got != 2 {
		t.Errorf("Matchers() has %d entries, want 2", got)
	}
	sel, err := mv.Selector()
	if err != nil {
		t.Fatalf("Selector(): unexpected error: %v", err)
	}
	if sel != lr.Left {
		t.Error("Selector() did not return the log range's selector")
	}
}

func TestMultiVariantExpr_WithoutALogRange(t *testing.T) {
	// The parser always attaches a range, so this guards the accessors
	// against a hand-built node rather than a parse result.
	mv := &MultiVariantExpr{}
	if got := mv.Matchers(); got != nil {
		t.Errorf("Matchers() = %v, want nil", got)
	}
	if _, err := mv.Selector(); err == nil {
		t.Fatal("Selector() = nil error, want one")
	}
}

func TestDropAndKeepLabels_Accessors(t *testing.T) {
	e, err := ParseExpr(`{app="x"} | drop a, b="v" | keep c, d="w"`)
	if err != nil {
		t.Fatalf("ParseExpr: unexpected error: %v", err)
	}
	pipeline, ok := e.(*PipelineExpr)
	if !ok {
		t.Fatalf("ParseExpr returned %T, want *PipelineExpr", e)
	}
	var (
		drop *DropLabelsExpr
		keep *KeepLabelsExpr
	)
	for _, st := range pipeline.MultiStages {
		switch s := st.(type) {
		case *DropLabelsExpr:
			drop = s
		case *KeepLabelsExpr:
			keep = s
		}
	}
	if drop == nil || keep == nil {
		t.Fatalf("pipeline is missing a drop (%v) or keep (%v) stage", drop, keep)
	}

	if got := len(drop.Matchers()); got != 2 {
		t.Errorf("drop Matchers() has %d entries, want 2", got)
	}
	if !drop.HasNamedMatchers() {
		t.Error(`drop HasNamedMatchers() = false, want true when an entry is a value matcher`)
	}
	wantNames := []string{"a", "b"}
	gotNames := drop.Names()
	if len(gotNames) != len(wantNames) {
		t.Fatalf("drop Names() = %v, want %v", gotNames, wantNames)
	}
	for i := range wantNames {
		if gotNames[i] != wantNames[i] {
			t.Errorf("drop Names()[%d] = %q, want %q", i, gotNames[i], wantNames[i])
		}
	}
	if got := len(keep.Matchers()); got != 2 {
		t.Errorf("keep Matchers() has %d entries, want 2", got)
	}
}

func TestDropLabelsExpr_BareNamesHaveNoNamedMatchers(t *testing.T) {
	e, err := ParseExpr(`{app="x"} | drop a, b`)
	if err != nil {
		t.Fatalf("ParseExpr: unexpected error: %v", err)
	}
	pipeline, ok := e.(*PipelineExpr)
	if !ok {
		t.Fatalf("ParseExpr returned %T, want *PipelineExpr", e)
	}
	drop, ok := pipeline.MultiStages[0].(*DropLabelsExpr)
	if !ok {
		t.Fatalf("first stage is %T, want *DropLabelsExpr", pipeline.MultiStages[0])
	}
	if drop.HasNamedMatchers() {
		t.Error("HasNamedMatchers() = true, want false when every entry is a bare name")
	}
}

func TestLiteralAndVectorExpr_Accessors(t *testing.T) {
	// Both are simultaneously sample and log-selector expressions, so
	// they answer Matchers()/Selector() as themselves.
	lit := mustNewLiteralExpr("2.5", true)
	v, err := lit.Value()
	if err != nil {
		t.Fatalf("LiteralExpr.Value(): unexpected error: %v", err)
	}
	if v != -2.5 {
		t.Errorf("LiteralExpr.Value() = %v, want -2.5 (inverted)", v)
	}
	if got := lit.Matchers(); got != nil {
		t.Errorf("LiteralExpr.Matchers() = %v, want nil", got)
	}
	sel, err := lit.Selector()
	if err != nil {
		t.Fatalf("LiteralExpr.Selector(): unexpected error: %v", err)
	}
	if sel != lit {
		t.Error("LiteralExpr.Selector() did not return the literal itself")
	}

	vec := NewVectorExpr("3")
	if err := vec.Err(); err != nil {
		t.Fatalf("VectorExpr.Err(): unexpected error: %v", err)
	}
	vv, err := vec.Value()
	if err != nil {
		t.Fatalf("VectorExpr.Value(): unexpected error: %v", err)
	}
	if vv != 3 {
		t.Errorf("VectorExpr.Value() = %v, want 3", vv)
	}
	if got := vec.Matchers(); got != nil {
		t.Errorf("VectorExpr.Matchers() = %v, want nil", got)
	}
	vsel, err := vec.Selector()
	if err != nil {
		t.Fatalf("VectorExpr.Selector(): unexpected error: %v", err)
	}
	if vsel != vec {
		t.Error("VectorExpr.Selector() did not return the vector itself")
	}
}

func TestNewVectorExpr_UnparseableScalar(t *testing.T) {
	vec := NewVectorExpr("not-a-number")
	if vec.Err() == nil {
		t.Fatal("Err() = nil, want a parse error")
	}
	if _, err := vec.Value(); err == nil {
		t.Error("Value() = nil error, want the stashed parse error")
	}
	if _, err := vec.Selector(); err == nil {
		t.Error("Selector() = nil error, want the stashed parse error")
	}
}

func TestMustNewLiteralExpr_UnparseableLiteral(t *testing.T) {
	lit := mustNewLiteralExpr("not-a-number", false)
	if _, err := lit.Value(); err == nil {
		t.Fatal("Value() = nil error, want a parse error")
	}
}

func TestMustNewBinOpExpr_NonSampleLeg(t *testing.T) {
	// The parser only ever hands sample expressions to the constructor;
	// this covers the type guards for a programmatically built node.
	logSel := &MatchersExpr{Mts: []*labels.Matcher{mustNewMatcher(labels.MatchEqual, "app", "x")}}
	metric, err := ParseExpr(`rate({app="x"}[5m])`)
	if err != nil {
		t.Fatalf("ParseExpr: unexpected error: %v", err)
	}
	sample, ok := metric.(SampleExpr)
	if !ok {
		t.Fatalf("ParseExpr returned %T, want a SampleExpr", metric)
	}

	if _, err := mustNewBinOpExpr(OpTypeAdd, &BinOpOptions{}, logSel, sample).Selector(); err == nil {
		t.Error("Selector() = nil error for a non-sample left leg, want one")
	}
	if _, err := mustNewBinOpExpr(OpTypeAdd, &BinOpOptions{}, sample, logSel).Selector(); err == nil {
		t.Error("Selector() = nil error for a non-sample right leg, want one")
	}
}

func TestMustNewBinOpExpr_ErroredLiteralLegPropagates(t *testing.T) {
	bad := mustNewLiteralExpr("not-a-number", false)
	good := mustNewLiteralExpr("1", false)
	if _, err := mustNewBinOpExpr(OpTypeAdd, nil, bad, good).Selector(); err == nil {
		t.Error("Selector() = nil error for an errored left literal, want one")
	}
	if _, err := mustNewBinOpExpr(OpTypeAdd, nil, good, bad).Selector(); err == nil {
		t.Error("Selector() = nil error for an errored right literal, want one")
	}
}

func TestMustNewMatcher_PanicsWithAParseError(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("mustNewMatcher with an uncompilable regex did not panic")
		}
		if _, ok := r.(ParseError); !ok {
			t.Errorf("recovered %T, want a ParseError", r)
		}
	}()
	mustNewMatcher(labels.MatchRegexp, "app", "(")
}

func TestLogRangeOffsetAndUnwrap(t *testing.T) {
	e, err := ParseExpr(`sum_over_time({app="x"} | unwrap duration(dur) | dur > 1s [5m] offset 1h)`)
	if err != nil {
		t.Fatalf("ParseExpr: unexpected error: %v", err)
	}
	ra, ok := e.(*RangeAggregationExpr)
	if !ok {
		t.Fatalf("ParseExpr returned %T, want *RangeAggregationExpr", e)
	}
	if ra.Left.Interval != 5*time.Minute {
		t.Errorf("Interval = %v, want 5m", ra.Left.Interval)
	}
	if ra.Left.Offset != time.Hour {
		t.Errorf("Offset = %v, want 1h", ra.Left.Offset)
	}
	if ra.Left.Unwrap == nil {
		t.Fatal("Unwrap = nil, want the unwrap clause")
	}
	if ra.Left.Unwrap.Identifier != "dur" {
		t.Errorf("Unwrap.Identifier = %q, want %q", ra.Left.Unwrap.Identifier, "dur")
	}
	if ra.Left.Unwrap.Operation != OpConvDuration {
		t.Errorf("Unwrap.Operation = %q, want %q", ra.Left.Unwrap.Operation, OpConvDuration)
	}
	if got := len(ra.Left.Unwrap.PostFilters); got != 1 {
		t.Errorf("Unwrap.PostFilters has %d entries, want 1", got)
	}
}

func TestQuantileOverTime_ParameterIsParsed(t *testing.T) {
	e, err := ParseExpr(`quantile_over_time(0.99, {app="x"} | unwrap v [5m])`)
	if err != nil {
		t.Fatalf("ParseExpr: unexpected error: %v", err)
	}
	ra, ok := e.(*RangeAggregationExpr)
	if !ok {
		t.Fatalf("ParseExpr returned %T, want *RangeAggregationExpr", e)
	}
	if ra.Params == nil {
		t.Fatal("Params = nil, want the quantile")
	}
	if *ra.Params != 0.99 {
		t.Errorf("Params = %v, want 0.99", *ra.Params)
	}
}

func TestNewRangeAggregationExpr_UnparseableQuantile(t *testing.T) {
	// The lexer keeps the parser from reaching this arm, so it is only
	// observable through the constructor.
	param := "not-a-number"
	ra := newRangeAggregationExpr(nil, OpRangeTypeQuantile, nil, &param)
	if _, err := ra.Selector(); err == nil {
		t.Fatal("Selector() = nil error, want an invalid-parameter error")
	}
}

func TestMustNewVectorAggregationExpr_UnparseableTopKParameter(t *testing.T) {
	param := "not-a-number"
	va := mustNewVectorAggregationExpr(nil, OpTypeTopK, nil, &param)
	if _, err := va.Selector(); err == nil {
		t.Fatal("Selector() = nil error, want an invalid-parameter error")
	}
}
