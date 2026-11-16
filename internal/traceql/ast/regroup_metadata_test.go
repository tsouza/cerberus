package ast

import "testing"

// TestRegroupLeavesTraceFieldUnset pins, for each of the three trace-level
// search-summary fields, which queries reference Tempo answers with the field
// empty. Every row outside the second-pass group is a query sent to
// reference Tempo's /api/search, with the field's observed state recorded in
// want: true means every returned trace carried rootTraceName "" /
// durationMs 0 / rootServiceName "<root span not yet received>" for the field
// named, false means every returned trace carried the real value. The
// compatibility corpus replays a sample of each kind (the group_by_* /
// coalesce_* search cases). The second-pass rows follow Tempo's source:
// cerberus cannot yet answer a stage after a regrouping one (#4290), so no
// reference run compares them. The false rows matter as much as the true
// ones: an over-reporting walk would blank a field Tempo fills.
func TestRegroupLeavesTraceFieldUnset(t *testing.T) {
	t.Parallel()

	const (
		rootName = IntrinsicTraceRootSpan
		rootSvc  = IntrinsicTraceRootService
		traceDur = IntrinsicTraceDuration
	)
	cases := []struct {
		query string
		field Intrinsic
		want  bool
		why   string
	}{
		// by(...) clears AllConditions, so only a predicate-free read counts.
		{`{ } | by(rootName)`, rootName, true, "a bare by() key reads the intrinsic with no predicate"},
		{`{ } | by(rootName)`, rootSvc, false, "another trace intrinsic is still read in the second pass"},
		{`{ } | by(rootName)`, traceDur, false, "another trace intrinsic is still read in the second pass"},
		{`{ } | by(trace:rootName)`, rootName, true, "the scoped spelling is the same intrinsic"},
		{`{ } | by(rootServiceName)`, rootSvc, true, "rootServiceName behaves like rootName"},
		{`{ } | by(trace:duration)`, traceDur, true, "the scoped duration spelling"},
		{`{ } | by(traceDuration * 2)`, traceDur, true, "arithmetic is not a predicate the fetch layer can build"},
		{`{ } | by(rootName = name)`, rootName, true, "an attribute-vs-attribute comparison reads both with no predicate"},
		{`{ rootName = name } | by(kind)`, rootName, true, "the filter's predicate-free read counts too"},
		{`{ rootName =~ "GET.*" } | by(name)`, rootName, false, "a literal comparison carries a predicate"},
		{`{ rootName != nil } | by(name)`, rootName, false, "!= nil is an existence test, which carries its operator"},
		{`{ traceDuration > 1ms } | by(name)`, traceDur, false, "a literal comparison carries a predicate"},
		{`{ } | by(name) | select(rootName)`, rootName, false, "select() reads in the second pass"},
		{`{ } | select(name) | by(rootName)`, rootName, false, "every stage after select() reads in the second pass"},
		{`{ } | by(rootName) | coalesce()`, rootName, true, "a later coalesce() regroups again"},

		// coalesce() alone keeps AllConditions, so any first-pass read counts.
		{`{ } | coalesce()`, rootName, false, "nothing reads the intrinsic"},
		{`{ rootName =~ "GET.*" } | coalesce()`, rootName, true, "every condition must hold, so the predicate read suffices"},
		{`{ rootName != nil } | coalesce()`, rootName, true, "every condition must hold, so the existence read suffices"},
		{`{ traceDuration > 1ms } | coalesce()`, traceDur, true, "every condition must hold"},
		{`{ rootServiceName = "checkout" } | coalesce()`, rootSvc, true, "every condition must hold"},
		{`{ rootName =~ "GET.*" || name = "x" } | coalesce()`, rootName, false, "|| clears AllConditions and the read carries a predicate"},
		{`{ rootName = name || kind = server } | coalesce()`, rootName, true, "|| clears AllConditions but the read has no predicate"},
		{`{ rootName =~ "GET.*" } | count() > 0 | coalesce()`, rootName, false, "an aggregate clears AllConditions"},
		{`{ rootName =~ "GET.*" } && { } | coalesce()`, rootName, false, "a spanset operation clears AllConditions"},

		// A second-pass read refills the field: select() reads its attributes
		// there, and so does every later stage whose attribute the first pass
		// does not already read with no predicate.
		{`{ } | by(rootName) | select(rootName)`, rootName, false, "select() reads rootName in the second pass"},
		{`{ rootName =~ "GET.*" } | coalesce() | select(rootName)`, rootName, false, "select() reads rootName in the second pass"},
		{`{ rootName =~ "GET.*" } | coalesce() | select(name) | by(rootName)`, rootName, false, "the later by() reads rootName in the second pass"},
		{`{ } | by(rootName) | select(name) | by(rootName)`, rootName, true, "the first pass already reads rootName with no predicate, so the later read stays out of the second pass"},

		// No regrouping stage: the pipeline returns the first pass's spansets,
		// which carry every trace-level field.
		{`{ } | select(rootName)`, rootName, false, "select() does not regroup"},
		{`{ rootName = name }`, rootName, false, "a plain filter does not regroup"},
		{`{ } | avg(traceDuration) > 100ms`, traceDur, false, "an aggregate filter does not regroup"},
	}
	for _, tc := range cases {
		expr, err := Parse(tc.query)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.query, err)
		}
		if got := expr.RegroupLeavesTraceFieldUnset(tc.field); got != tc.want {
			t.Errorf("%s: RegroupLeavesTraceFieldUnset(%s) = %v, want %v (%s)", tc.query, tc.field, got, tc.want, tc.why)
		}
	}
}

// TestRegroupLeavesTraceFieldUnset_OtherIntrinsics pins that an intrinsic
// backing no trace-level summary field is never reported, even where the
// same query blanks a trace-level one.
func TestRegroupLeavesTraceFieldUnset_OtherIntrinsics(t *testing.T) {
	t.Parallel()

	expr, err := Parse(`{ } | by(name)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []Intrinsic{IntrinsicName, IntrinsicDuration, IntrinsicNone} {
		if expr.RegroupLeavesTraceFieldUnset(in) {
			t.Errorf("RegroupLeavesTraceFieldUnset(%s) = true, want false", in)
		}
	}
	var nilExpr *RootExpr
	if nilExpr.RegroupLeavesTraceFieldUnset(IntrinsicTraceRootSpan) {
		t.Error("nil RootExpr reported a field unset")
	}
}
