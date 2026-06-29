package ast

import "testing"

// Mutation-coverage tests for enum.go. They pin the exact String() spelling of
// every enum value (the reference-parser-aligned names diagnostics and A/B
// oracles depend on), the numeric-type predicate, and the bare-identifier →
// intrinsic resolution. Each assertion is chosen to break a specific mutation
// of the underlying code.

// TestStaticTypeIsNumeric pins the numeric-type set. Negating or AND-ing the
// `==` clauses in isNumeric flips at least one of these.
func TestStaticTypeIsNumeric(t *testing.T) {
	t.Parallel()
	numeric := []StaticType{TypeInt, TypeFloat, TypeDuration}
	for _, ty := range numeric {
		if !ty.isNumeric() {
			t.Errorf("isNumeric(%v) = false; want true", ty)
		}
	}
	nonNumeric := []StaticType{
		TypeNil, TypeSpanset, TypeAttribute, TypeString, TypeBoolean,
		TypeIntArray, TypeFloatArray, TypeStringArray, TypeBooleanArray,
		TypeStatus, TypeKind,
	}
	for _, ty := range nonNumeric {
		if ty.isNumeric() {
			t.Errorf("isNumeric(%v) = true; want false", ty)
		}
	}
}

// TestStaticTypeString pins the name table plus the bounds guard. A boundary
// or negation mutant on `int(t) >= 0 && int(t) < len(...)` makes one of the
// out-of-range cases mis-render.
func TestStaticTypeString(t *testing.T) {
	t.Parallel()
	cases := map[StaticType]string{
		TypeNil: "TypeNil", TypeSpanset: "TypeSpanset", TypeAttribute: "TypeAttribute",
		TypeInt: "TypeInt", TypeFloat: "TypeFloat", TypeString: "TypeString",
		TypeBoolean: "TypeBoolean", TypeIntArray: "TypeIntArray",
		TypeFloatArray: "TypeFloatArray", TypeStringArray: "TypeStringArray",
		TypeBooleanArray: "TypeBooleanArray", TypeDuration: "TypeDuration",
		TypeStatus: "TypeStatus", TypeKind: "TypeKind",
	}
	for ty, want := range cases {
		if got := ty.String(); got != want {
			t.Errorf("StaticType(%d).String() = %q; want %q", int(ty), got, want)
		}
	}
	// Out of range on both sides of the bounds guard. The value exactly equal
	// to len(staticTypeNames) pins the upper `<` boundary: a `<=` mutant would
	// index out of range and panic instead of returning the fallback.
	if got := StaticType(len(cases)).String(); got != "StaticType(14)" {
		t.Errorf("StaticType(14).String() = %q; want StaticType(14)", got)
	}
	if got := StaticType(99).String(); got != "StaticType(99)" {
		t.Errorf("StaticType(99).String() = %q; want StaticType(99)", got)
	}
	if got := StaticType(-1).String(); got != "StaticType(-1)" {
		t.Errorf("StaticType(-1).String() = %q; want StaticType(-1)", got)
	}
}

// TestOperatorString pins every operator symbol and the unknown fallback.
func TestOperatorString(t *testing.T) {
	t.Parallel()
	cases := map[Operator]string{
		OpAdd: "+", OpSub: "-", OpDiv: "/", OpMod: "%", OpMult: "*",
		OpEqual: "=", OpNotEqual: "!=", OpRegex: "=~", OpNotRegex: "!~",
		OpGreater: ">", OpGreaterEqual: ">=", OpLess: "<", OpLessEqual: "<=",
		OpPower: "^", OpAnd: "&&", OpOr: "||", OpNot: "!",
		OpSpansetChild: ">", OpSpansetParent: "<", OpSpansetDescendant: ">>",
		OpSpansetAncestor: "<<", OpSpansetSibling: "~",
		OpSpansetNotChild: "!>", OpSpansetNotParent: "!<", OpSpansetNotSibling: "!~",
		OpSpansetNotAncestor: "!<<", OpSpansetNotDescendant: "!>>",
		OpSpansetUnionChild: "&>", OpSpansetUnionParent: "&<", OpSpansetUnionSibling: "&~",
		OpSpansetUnionAncestor: "&<<", OpSpansetUnionDescendant: "&>>",
		OpExists: "!= nil", OpNotExists: "= nil", OpIn: "in", OpNotIn: "not in",
	}
	for op, want := range cases {
		if got := op.String(); got != want {
			t.Errorf("Operator(%d).String() = %q; want %q", int(op), got, want)
		}
	}
	if got := Operator(9999).String(); got != "operator(9999)" {
		t.Errorf("unknown Operator.String() = %q; want operator(9999)", got)
	}
}

// TestAttributeScopeString pins each scope spelling and the fallback.
func TestAttributeScopeString(t *testing.T) {
	t.Parallel()
	cases := map[AttributeScope]string{
		AttributeScopeNone: "none", AttributeScopeTrace: "trace",
		AttributeScopeResource: "resource", AttributeScopeSpan: "span",
		AttributeScopeEvent: "event", AttributeScopeLink: "link",
		AttributeScopeInstrumentation: "instrumentation",
	}
	for sc, want := range cases {
		if got := sc.String(); got != want {
			t.Errorf("AttributeScope(%d).String() = %q; want %q", int(sc), got, want)
		}
	}
	if got := AttributeScope(99).String(); got != "att(99)." {
		t.Errorf("unknown AttributeScope.String() = %q; want att(99).", got)
	}
}

// TestStatusKindString pins Status and Kind spellings + fallbacks.
func TestStatusKindString(t *testing.T) {
	t.Parallel()
	st := map[Status]string{StatusError: "error", StatusOk: "ok", StatusUnset: "unset"}
	for s, want := range st {
		if got := s.String(); got != want {
			t.Errorf("Status(%d).String() = %q; want %q", int(s), got, want)
		}
	}
	if got := Status(42).String(); got != "status(42)" {
		t.Errorf("unknown Status.String() = %q; want status(42)", got)
	}
	kd := map[Kind]string{
		KindUnspecified: "unspecified", KindInternal: "internal", KindClient: "client",
		KindServer: "server", KindProducer: "producer", KindConsumer: "consumer",
	}
	for k, want := range kd {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q; want %q", int(k), got, want)
		}
	}
	if got := Kind(42).String(); got != "kind(42)" {
		t.Errorf("unknown Kind.String() = %q; want kind(42)", got)
	}
}

// TestAggregateOpString pins the pipeline aggregate spellings.
func TestAggregateOpString(t *testing.T) {
	t.Parallel()
	cases := map[AggregateOp]string{
		AggregateCount: "count", AggregateMax: "max", AggregateMin: "min",
		AggregateSum: "sum", AggregateAvg: "avg",
	}
	for a, want := range cases {
		if got := a.String(); got != want {
			t.Errorf("AggregateOp(%d).String() = %q; want %q", int(a), got, want)
		}
	}
	if got := AggregateOp(42).String(); got != "aggregate(42)" {
		t.Errorf("unknown AggregateOp.String() = %q; want aggregate(42)", got)
	}
}

// TestMetricsAggregateOpString pins the first-stage metric spellings.
func TestMetricsAggregateOpString(t *testing.T) {
	t.Parallel()
	cases := map[MetricsAggregateOp]string{
		MetricsAggregateRate: "rate", MetricsAggregateCountOverTime: "count_over_time",
		MetricsAggregateMinOverTime: "min_over_time", MetricsAggregateMaxOverTime: "max_over_time",
		MetricsAggregateAvgOverTime: "avg_over_time", MetricsAggregateSumOverTime: "sum_over_time",
		MetricsAggregateQuantileOverTime:  "quantile_over_time",
		MetricsAggregateHistogramOverTime: "histogram_over_time",
	}
	for a, want := range cases {
		if got := a.String(); got != want {
			t.Errorf("MetricsAggregateOp(%d).String() = %q; want %q", int(a), got, want)
		}
	}
	if got := MetricsAggregateOp(42).String(); got != "metricsAggregate(42)" {
		t.Errorf("unknown MetricsAggregateOp.String() = %q; want metricsAggregate(42)", got)
	}
}

// TestSecondStageOpString pins topk/bottomk + fallback.
func TestSecondStageOpString(t *testing.T) {
	t.Parallel()
	if got := OpTopK.String(); got != "topk" {
		t.Errorf("OpTopK.String() = %q; want topk", got)
	}
	if got := OpBottomK.String(); got != "bottomk" {
		t.Errorf("OpBottomK.String() = %q; want bottomk", got)
	}
	if got := SecondStageOp(42).String(); got != "unknown" {
		t.Errorf("unknown SecondStageOp.String() = %q; want unknown", got)
	}
}

// allNamedIntrinsics enumerates every intrinsic that has a surface spelling,
// excluding IntrinsicNone and IntrinsicParent (which intrinsicFromString
// deliberately refuses to resolve from a bare identifier).
var allNamedIntrinsics = []struct {
	in   Intrinsic
	name string
}{
	{IntrinsicDuration, "duration"},
	{IntrinsicName, "name"},
	{IntrinsicStatus, "status"},
	{IntrinsicStatusMessage, "statusMessage"},
	{IntrinsicKind, "kind"},
	{IntrinsicChildCount, "span:childCount"},
	{IntrinsicEventName, "event:name"},
	{IntrinsicEventTimeSinceStart, "event:timeSinceStart"},
	{IntrinsicLinkSpanID, "link:spanID"},
	{IntrinsicLinkTraceID, "link:traceID"},
	{IntrinsicTraceRootService, "rootServiceName"},
	{IntrinsicTraceRootSpan, "rootName"},
	{IntrinsicTraceDuration, "traceDuration"},
	{IntrinsicTraceID, "trace:id"},
	{IntrinsicTraceStartTime, "traceStartTime"},
	{ScopedIntrinsicSpanStatus, "span:status"},
	{ScopedIntrinsicSpanStatusMessage, "span:statusMessage"},
	{ScopedIntrinsicSpanDuration, "span:duration"},
	{ScopedIntrinsicSpanName, "span:name"},
	{ScopedIntrinsicSpanKind, "span:kind"},
	{ScopedIntrinsicTraceRootName, "trace:rootName"},
	{ScopedIntrinsicTraceRootService, "trace:rootService"},
	{ScopedIntrinsicTraceDuration, "trace:duration"},
	{IntrinsicSpanID, "span:id"},
	{IntrinsicParentID, "span:parentID"},
	{IntrinsicInstrumentationName, "instrumentation:name"},
	{IntrinsicInstrumentationVersion, "instrumentation:version"},
	{IntrinsicSpanStartTime, "spanStartTime"},
	{IntrinsicNestedSetLeft, "nestedSetLeft"},
	{IntrinsicNestedSetRight, "nestedSetRight"},
	{IntrinsicNestedSetParent, "nestedSetParent"},
}

// TestIntrinsicStringRoundTrip pins both directions of the intrinsic ↔ name
// mapping for every named intrinsic. Replacing the `continue` in
// intrinsicFromString with a `break` (INVERT_LOOPCTRL) makes the loop bail out
// the first time it encounters IntrinsicNone/IntrinsicParent in map order, so
// at least one of these ~31 lookups resolves to IntrinsicNone instead.
func TestIntrinsicStringRoundTrip(t *testing.T) {
	t.Parallel()
	for _, c := range allNamedIntrinsics {
		if got := c.in.String(); got != c.name {
			t.Errorf("Intrinsic(%d).String() = %q; want %q", int(c.in), got, c.name)
		}
		if got := intrinsicFromString(c.name); got != c.in {
			t.Errorf("intrinsicFromString(%q) = %v; want %v", c.name, got, c.in)
		}
	}
	if got := IntrinsicNone.String(); got != "none" {
		t.Errorf("IntrinsicNone.String() = %q; want none", got)
	}
	if got := IntrinsicParent.String(); got != "parent" {
		t.Errorf("IntrinsicParent.String() = %q; want parent", got)
	}
}

// TestIntrinsicFromStringRefusesParentAndUnknown pins the two guarded skips.
// intrinsicFromString must NOT resolve the bare identifiers "parent" or "none"
// to their intrinsics (they are control values, not user-referenceable), and
// must return IntrinsicNone for an ordinary attribute name. Flipping the
// `in == IntrinsicParent` guard (CONDITIONALS_NEGATION) makes "parent" resolve
// to IntrinsicParent.
func TestIntrinsicFromStringRefusesParentAndUnknown(t *testing.T) {
	t.Parallel()
	if got := intrinsicFromString("parent"); got != IntrinsicNone {
		t.Errorf("intrinsicFromString(parent) = %v; want IntrinsicNone", got)
	}
	if got := intrinsicFromString("http.method"); got != IntrinsicNone {
		t.Errorf("intrinsicFromString(http.method) = %v; want IntrinsicNone", got)
	}
}
