package engine

import (
	"context"
	"fmt"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

// ShapeDefectGate is how the engine handles the queries of one fleet shape
// defect's shape (the ConversionConditionCacheDefect field group of
// SettingsRules). It mirrors chopt.DefectGate, which internal/choptwire maps
// onto it.
type ShapeDefectGate uint8

const (
	// ShapeDefectAbsent: no reachable node carries the defect.
	ShapeDefectAbsent ShapeDefectGate = iota
	// ShapeDefectGated: the query carries the setting that switches the
	// defective mechanism off.
	ShapeDefectGated
	// ShapeDefectRefused: a reachable node carries the defect and another
	// rejects the setting, so the query is refused before dispatch
	// (chclient.ErrUnsafeMixedFleet).
	ShapeDefectRefused
)

// inForce reports whether the defect is gated or refused.
func (g ShapeDefectGate) inForce() bool { return g != ShapeDefectAbsent }

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
	return r.ConversionConditionCacheDefect.inForce() ||
		r.NegatedConjunctionProjectionDefect.inForce() ||
		r.TraceIDProjectionReadDefect.inForce()
}

// shapeDefectFactsOf returns the shape-defect facts of f's plan, or none when
// no defect is in force.
func (r SettingsRules) shapeDefectFactsOf(f planShapeFacts) shapeDefectFacts {
	if !r.anyShapeDefect() {
		return shapeDefectFacts{}
	}
	return inspectShapeDefectFacts(f.plan, r.Traces.TraceIDColumn)
}

// conversionConditionCacheGate reports how f's plan is handled against
// ClickHouse#119168: the defect's gate when a Filter of the plan parses a
// String to a float, absent otherwise. Anything but absent also withholds the
// condition-cache rule's 1.
func (r SettingsRules) conversionConditionCacheGate(d shapeDefectFacts) ShapeDefectGate {
	if !d.filterStringToFloat {
		return ShapeDefectAbsent
	}
	return r.ConversionConditionCacheDefect
}

// projectionFilteringGate reports how f's plan is handled against the
// projection-index defects. The plan must be TraceId-keyed (the only reads
// cerberus sends through proj_trace_id); then the 25.11 line's read loss
// applies to every such read and ClickHouse#120286 to one whose Filter
// negates a conjunction reading another column. When both apply, refused
// wins over gated.
func (r SettingsRules) projectionFilteringGate(f planShapeFacts, d shapeDefectFacts) ShapeDefectGate {
	if !r.eligibleForTraceIDBitmapFilter(f) {
		return ShapeDefectAbsent
	}
	gate := r.TraceIDProjectionReadDefect
	if d.negatedMixedConjunction {
		gate = max(gate, r.NegatedConjunctionProjectionDefect)
	}
	return gate
}

// applyShapeDefects switches the defective mechanism off on a query of a
// shape a fleet shape defect returns wrong rows for — see the
// ConversionConditionCacheDefect field group — or, where the fleet cannot
// accept the setting that does it, refuses the query
// (chclient.WithDispatchRefusal). conversion is
// conversionConditionCacheGate's verdict, computed once by the caller, which
// also withholds the condition-cache rule's 1 on the same plan.
func (r SettingsRules) applyShapeDefects(ctx context.Context, f planShapeFacts, d shapeDefectFacts, conversion ShapeDefectGate) context.Context {
	ctx = applyShapeDefectGate(ctx, conversion, "conversion_condition_cache", settingUseQueryConditionCache)
	return applyShapeDefectGate(ctx, r.projectionFilteringGate(f, d), "projection_filtering", settingOptimizeUseProjectionFiltering)
}

// applyShapeDefectGate stamps setting=0 under a gated verdict and marks the
// query refused under a refused one, naming the defect and the setting.
func applyShapeDefectGate(ctx context.Context, gate ShapeDefectGate, defect, setting string) context.Context {
	switch gate {
	case ShapeDefectGated:
		return chclient.WithQuerySetting(ctx, setting, 0)
	case ShapeDefectRefused:
		return chclient.WithDispatchRefusal(ctx, fmt.Sprintf(
			"shape defect %s: a reachable node returns wrong rows for this query unless %s=0, and another rejects that setting; upgrade every node to a build that accepts it",
			defect, setting,
		))
	default:
		return ctx
	}
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
