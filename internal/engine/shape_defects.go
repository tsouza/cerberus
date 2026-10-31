package engine

import (
	"context"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

// shapeDefectFacts are the plan facts the shape-defect gates read, all taken
// off Filter predicates: the WHERE / PREWHERE text ClickHouse's condition
// cache keys on and its projection index analyses. Expressions outside a
// Filter (a projection, an aggregate's argument) are never a granule filter,
// so they carry neither.
type shapeDefectFacts struct {
	// filterStringToFloat: a Filter parses a String to a float
	// (filterParsesStringToFloat).
	filterStringToFloat bool
	// negatedMixedConjunction: a Filter negates a conjunction reading a
	// column other than the trace-id column (negatesMixedConjunction).
	negatedMixedConjunction bool
}

// inspectShapeDefectFacts walks every Filter of plan, embedded subquery plans
// included, for the shape-defect facts.
func inspectShapeDefectFacts(plan chplan.Node, traceIDColumn string) shapeDefectFacts {
	var d shapeDefectFacts
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		flt, ok := n.(*chplan.Filter)
		if !ok {
			return true
		}
		if !d.filterStringToFloat && filterParsesStringToFloat(flt.Predicate) {
			d.filterStringToFloat = true
		}
		if traceIDColumn != "" && !d.negatedMixedConjunction && negatesMixedConjunction(flt.Predicate, traceIDColumn) {
			d.negatedMixedConjunction = true
		}
		return true
	})
	return d
}

// anyShapeDefect reports whether any shape defect is in force, so a fleet
// without one never pays for the walk.
func (r SettingsRules) anyShapeDefect() bool {
	return r.ConversionConditionCacheDefect || r.NegatedConjunctionProjectionDefect || r.TraceIDProjectionReadDefect
}

// shapeDefectFactsOf returns the shape-defect facts of f's plan, or none when
// no defect is in force.
func (r SettingsRules) shapeDefectFactsOf(f planShapeFacts) shapeDefectFacts {
	if !r.anyShapeDefect() {
		return shapeDefectFacts{}
	}
	return inspectShapeDefectFacts(f.plan, r.Traces.TraceIDColumn)
}

// conversionConditionCacheUnsafe reports whether the condition cache would
// share a wrong granule verdict for the plan d was read from: the fleet
// carries ClickHouse#119168 and a Filter of the plan parses a String to a
// float.
func (r SettingsRules) conversionConditionCacheUnsafe(d shapeDefectFacts) bool {
	return r.ConversionConditionCacheDefect && d.filterStringToFloat
}

// projectionFilteringUnsafe reports whether the projection index would drop
// rows f's plan keeps: the plan is TraceId-keyed (the only reads cerberus
// sends through proj_trace_id) and either the fleet carries the 25.11 line's
// projection-index read loss, which every such read triggers, or it carries
// ClickHouse#120286 and a Filter negates a conjunction reading another
// column.
func (r SettingsRules) projectionFilteringUnsafe(f planShapeFacts, d shapeDefectFacts) bool {
	if !r.eligibleForTraceIDBitmapFilter(f) {
		return false
	}
	return r.TraceIDProjectionReadDefect || (r.NegatedConjunctionProjectionDefect && d.negatedMixedConjunction)
}

// applyShapeDefects switches the defective mechanism off on a query of a
// shape a fleet shape defect returns wrong rows for — see the
// ConversionConditionCacheDefect field group. conversionUnsafe is
// conversionConditionCacheUnsafe(d), computed once by the caller, which also
// withholds the condition-cache rule's 1 on the same plan.
//
// The setting is stamped whatever the oldest node: a node too old to accept
// it predates the defect, rejects the query with UNKNOWN_SETTING (which
// chclient reports as a GateSettingUnsupportedError naming the upgrade), and
// never answers it wrong.
func (r SettingsRules) applyShapeDefects(ctx context.Context, f planShapeFacts, d shapeDefectFacts, conversionUnsafe bool) context.Context {
	if conversionUnsafe {
		ctx = chclient.WithQuerySetting(ctx, settingUseQueryConditionCache, 0)
	}
	if r.projectionFilteringUnsafe(f, d) {
		ctx = chclient.WithQuerySetting(ctx, settingOptimizeUseProjectionFiltering, 0)
	}
	return ctx
}

// filterParsesStringToFloat reports whether pred calls toFloat64OrZero or
// toFloat64OrNull, the String-to-float parses cerberus emits: the LogQL
// numeric and bytes label filters (internal/logql/numbytes.go), the TraceQL
// numeric attribute coercion (internal/traceql/lower.go), and the PromQL
// classic-bucket `le` read (internal/promql/histogram_quantile_le.go). Their
// result depends on the session's precise_float_parsing, which is the
// conversion setting ClickHouse#119168 left out of the condition-cache key.
//
// Plain toFloat64 is not counted: every lowering that emits it passes a
// numeric operand (a sample value, a count, a tuple element of a numeric
// tuple, a duration), and a number-to-float cast reads no conversion setting.
func filterParsesStringToFloat(pred chplan.Expr) bool {
	found := false
	chplan.InspectExpr(pred, func(e chplan.Expr) bool {
		if fc, ok := e.(*chplan.FuncCall); ok && (fc.Fn == chplan.FnToFloat64OrZero || fc.Fn == chplan.FnToFloat64OrNull) {
			found = true
		}
		return !found
	})
	return found
}

// negatesMixedConjunction reports whether pred carries a not(...) over a
// subtree holding an AND that reads any column other than the bare trace-id
// column: the shape ClickHouse#120286's projection-index analysis weakens to
// a wrong prefilter. Its weakening replaces a conjunct the projection cannot
// compute by true; under the negation that turns `NOT (a AND b)` into a
// stricter `NOT a`, so a conjunction the projection CAN compute in full (one
// reading TraceId alone) and a negated disjunction are left untouched — the
// survey in docs/clickhouse-optimizations.background.md has both come back
// correct.
func negatesMixedConjunction(pred chplan.Expr, traceIDColumn string) bool {
	found := false
	chplan.InspectExpr(pred, func(e chplan.Expr) bool {
		if found {
			return false
		}
		not, ok := e.(*chplan.FuncCall)
		if !ok || not.Fn != chplan.FnNot {
			return true
		}
		for _, arg := range not.Args {
			chplan.InspectExpr(arg, func(sub chplan.Expr) bool {
				if b, ok := sub.(*chplan.Binary); ok && b.Op == chplan.OpAnd && !readsOnlyTraceID(b, traceIDColumn) {
					found = true
				}
				return !found
			})
		}
		return !found
	})
	return found
}

// readsOnlyTraceID reports whether every column e reads is the bare trace-id
// column. Any other column reference, and every expression that reads storage
// on its own (a map or JSON access, a nested-array test, a subquery, the
// LogQL line content, the structure-tab trace scope), counts as reading
// another column; literals and lambda parameters read none.
func readsOnlyTraceID(e chplan.Expr, traceIDColumn string) bool {
	only := true
	chplan.InspectExpr(e, func(sub chplan.Expr) bool {
		switch v := sub.(type) {
		case *chplan.ColumnRef:
			if v.Name != traceIDColumn {
				only = false
			}
		case *chplan.FieldAccess, *chplan.MapAccess, *chplan.NestedArrayExists, *chplan.ScalarSubquery,
			*chplan.InSubquery, *chplan.LineContent, *chplan.BoundedTraceScope:
			only = false
		}
		return only
	})
	return only
}
