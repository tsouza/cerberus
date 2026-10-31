package engine

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// stringToFloatFilter builds the LogQL numeric label filter's shape:
// Filter(toFloat64OrZero(LogAttributes['v']) > 2) over Scan(otel_logs).
func stringToFloatFilter() chplan.Node {
	return &chplan.Filter{
		Input:     &chplan.Scan{Table: "otel_logs"},
		Predicate: gtTwo(parseFloat(chplan.FnToFloat64OrZero, logAttr("v"))),
	}
}

// traceIDNegatedConjunctionFilter builds `{ trace:id = X && !(trace:id = X &&
// span.foo = "nope") }`'s shape: a TraceId equality AND a negated conjunction
// whose second conjunct reads a span attribute.
func traceIDNegatedConjunctionFilter() chplan.Node {
	return traceFilter(and(traceIDEq(), not(and(traceIDEq(), spanAttrEq("foo")))))
}

func logAttr(key string) chplan.Expr {
	return &chplan.MapAccess{Map: &chplan.ColumnRef{Name: "LogAttributes"}, Key: &chplan.LitString{V: key}}
}

func spanAttrEq(key string) chplan.Expr {
	return &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.MapAccess{Map: &chplan.ColumnRef{Name: "SpanAttributes"}, Key: &chplan.LitString{V: key}},
		Right: &chplan.LitString{V: "nope"},
	}
}

func traceIDEq() chplan.Expr {
	return &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "TraceId"},
		Right: &chplan.LitString{V: "0123456789abcdef0123456789abcdef"},
	}
}

func parseFloat(fn chplan.Fn, arg chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: fn, Args: []chplan.Expr{arg}}
}

func gtTwo(e chplan.Expr) chplan.Expr {
	return &chplan.Binary{Op: chplan.OpGt, Left: e, Right: &chplan.LitFloat{V: 2}}
}

func and(l, r chplan.Expr) chplan.Expr { return &chplan.Binary{Op: chplan.OpAnd, Left: l, Right: r} }
func or(l, r chplan.Expr) chplan.Expr  { return &chplan.Binary{Op: chplan.OpOr, Left: l, Right: r} }
func not(e chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnNot, Args: []chplan.Expr{e}}
}

func traceFilter(pred chplan.Expr) chplan.Node {
	return &chplan.Filter{Input: &chplan.Scan{Table: "otel_traces"}, Predicate: pred}
}

// shapeDefectRules returns SettingsRules with the condition cache on, every
// shape defect but the 25.11 read loss gated, and the default schema wired.
func shapeDefectRules() SettingsRules {
	return SettingsRules{
		ConditionCache:                     true,
		TraceIDBitmapFilter:                true,
		ConversionConditionCacheDefect:     true,
		NegatedConjunctionProjectionDefect: true,
		Metrics:                            schema.DefaultOTelMetrics(),
		Traces:                             schema.DefaultOTelTraces(),
		Logs:                               schema.DefaultOTelLogs(),
	}
}

// TestFilterParsesStringToFloat pins which Filter predicates count as a
// String-to-float parse: the two parse functions anywhere in a Filter, and
// nothing else — not the numeric toFloat64 cast, and not a parse that sits in
// a projection rather than the WHERE.
func TestFilterParsesStringToFloat(t *testing.T) {
	tests := []struct {
		name string
		plan chplan.Node
		want bool
	}{
		{"toFloat64OrZero in a filter", stringToFloatFilter(), true},
		{
			"toFloat64OrNull nested under a multiIf in a filter",
			&chplan.Filter{
				Input: &chplan.Scan{Table: "otel_logs"},
				Predicate: &chplan.FuncCall{Fn: chplan.FnMultiIf, Args: []chplan.Expr{
					&chplan.LitBool{V: true}, gtTwo(parseFloat(chplan.FnToFloat64OrNull, logAttr("v"))), &chplan.LitBool{V: false},
				}},
			},
			true,
		},
		{
			"parse in a filter of an embedded subquery",
			&chplan.Filter{
				Input: &chplan.Scan{Table: "otel_logs"},
				Predicate: &chplan.InSubquery{
					Left:     &chplan.ColumnRef{Name: "TraceId"},
					Subquery: stringToFloatFilter(),
				},
			},
			true,
		},
		{
			"numeric toFloat64 cast in a filter",
			&chplan.Filter{
				Input:     &chplan.Scan{Table: "otel_metrics_sum"},
				Predicate: gtTwo(parseFloat(chplan.FnToFloat64, &chplan.ColumnRef{Name: "Value"})),
			},
			false,
		},
		{
			"parse in a projection over a filter without one",
			&chplan.Project{
				Input: filterScan("otel_logs"),
				Projections: []chplan.Projection{{
					Expr: parseFloat(chplan.FnToFloat64OrZero, logAttr("v")), Alias: "Value",
				}},
			},
			false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := inspectShapeDefectFacts(tc.plan, "TraceId").filterStringToFloat; got != tc.want {
				t.Errorf("filterStringToFloat = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestNegatesMixedConjunction pins the ClickHouse#120286 trigger against the
// shapes the per-build survey ran: a negated AND reading a non-TraceId column
// is the trigger whether or not TraceId sits inside it; a negated OR, a
// negated TraceId-only AND, and an un-negated AND are not.
func TestNegatesMixedConjunction(t *testing.T) {
	tests := []struct {
		name string
		pred chplan.Expr
		want bool
	}{
		{"!(trace:id && span.foo)", and(traceIDEq(), not(and(traceIDEq(), spanAttrEq("foo")))), true},
		{"!(span.foo && span.bar)", and(traceIDEq(), not(and(spanAttrEq("foo"), spanAttrEq("bar")))), true},
		{"!(span.foo || (span.bar && span.baz))", and(traceIDEq(), not(or(spanAttrEq("foo"), and(spanAttrEq("bar"), spanAttrEq("baz"))))), true},
		{
			"!(duration > 1h && trace:id)",
			and(traceIDEq(), not(and(&chplan.Binary{Op: chplan.OpGt, Left: &chplan.ColumnRef{Name: "Duration"}, Right: &chplan.LitInt{V: 3_600_000_000_000}}, traceIDEq()))),
			true,
		},
		{"!(trace:id || span.foo)", and(traceIDEq(), not(or(traceIDEq(), spanAttrEq("foo")))), false},
		{"!(span.foo || span.bar)", and(traceIDEq(), not(or(spanAttrEq("foo"), spanAttrEq("bar")))), false},
		{"!(trace:id && trace:id)", and(traceIDEq(), not(and(traceIDEq(), traceIDEq()))), false},
		{"trace:id && span.foo, no negation", and(traceIDEq(), spanAttrEq("foo")), false},
		{"!(span.foo)", and(traceIDEq(), not(spanAttrEq("foo"))), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := inspectShapeDefectFacts(traceFilter(tc.pred), "TraceId").negatedMixedConjunction; got != tc.want {
				t.Errorf("negatedMixedConjunction = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestApplyShapeDefects_ConditionCache pins the per-shape condition-cache
// gate: with ClickHouse#119168 on the fleet a String-to-float filter runs
// with the cache off, whether or not the condition_cache feature resolved
// (the server's own default is on from 25.9), and every other filtered read
// keeps use_query_condition_cache=1. Without the defect the conversion plan
// keeps the cache too.
func TestApplyShapeDefects_ConditionCache(t *testing.T) {
	featureOff := shapeDefectRules()
	featureOff.ConditionCache = false
	noDefect := shapeDefectRules()
	noDefect.ConversionConditionCacheDefect = false
	tests := []struct {
		name  string
		rules SettingsRules
		plan  chplan.Node
		want  any
	}{
		{"defect, conversion filter", shapeDefectRules(), stringToFloatFilter(), 0},
		{"defect, feature off, conversion filter", featureOff, stringToFloatFilter(), 0},
		{"defect, other filter", shapeDefectRules(), filterScan("otel_logs"), 1},
		{"defect, feature off, other filter", featureOff, filterScan("otel_logs"), nil},
		{"no defect, conversion filter", noDefect, stringToFloatFilter(), 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tc.rules.apply(context.Background(), tc.plan)
			if got := settingValue(ctx, settingUseQueryConditionCache); got != tc.want {
				t.Errorf("%s = %v; want %v", settingUseQueryConditionCache, got, tc.want)
			}
		})
	}
}

// TestApplyShapeDefects_ProjectionFiltering pins the per-shape projection
// gate: ClickHouse#120286 switches the projection index off on a TraceId-keyed
// plan that negates a mixed conjunction and nowhere else; the 25.11 read loss
// switches it off on every TraceId-keyed plan and nowhere else; with neither
// defect nothing is stamped.
func TestApplyShapeDefects_ProjectionFiltering(t *testing.T) {
	negatedOnly := shapeDefectRules()
	readLoss := shapeDefectRules()
	readLoss.NegatedConjunctionProjectionDefect = false
	readLoss.TraceIDProjectionReadDefect = true
	noDefect := shapeDefectRules()
	noDefect.NegatedConjunctionProjectionDefect = false
	noTraceID := negatedOnly
	noTraceID.Traces.TraceIDColumn = ""
	negatedWithoutTraceKey := &chplan.Filter{
		Input:     &chplan.Scan{Table: "otel_traces"},
		Predicate: not(and(spanAttrEq("foo"), spanAttrEq("bar"))),
	}
	tests := []struct {
		name  string
		rules SettingsRules
		plan  chplan.Node
		want  any
	}{
		{"#120286, negated mixed conjunction", negatedOnly, traceIDNegatedConjunctionFilter(), 0},
		{"#120286, plain trace-id lookup", negatedOnly, traceIDEqualityFilter(), nil},
		{"#120286, negation without a trace-id key", negatedOnly, negatedWithoutTraceKey, nil},
		{"#120286, no trace-id column in the schema", noTraceID, traceIDNegatedConjunctionFilter(), nil},
		{"25.11 read loss, plain trace-id lookup", readLoss, traceIDEqualityFilter(), 0},
		{"25.11 read loss, negated mixed conjunction", readLoss, traceIDNegatedConjunctionFilter(), 0},
		{"25.11 read loss, not trace-id keyed", readLoss, negatedWithoutTraceKey, nil},
		{"no defect, negated mixed conjunction", noDefect, traceIDNegatedConjunctionFilter(), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tc.rules.apply(context.Background(), tc.plan)
			if got := settingValue(ctx, settingOptimizeUseProjectionFiltering); got != tc.want {
				t.Errorf("%s = %v; want %v", settingOptimizeUseProjectionFiltering, got, tc.want)
			}
		})
	}
}
