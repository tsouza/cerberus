package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// ScalarGuard is one scalar-parameter domain check whose verdict needs a
// value only ClickHouse can produce.
//
// A literal parameter is folded at lowering time, so its domain
// ([aggregationParamDomain], [holtWintersFactorDomain]) can be applied
// on the spot. A computed parameter — `scalar(<vector>)` and arithmetic
// over it — has no value until the query runs, and neither half of
// reference Prometheus's behaviour survives being pushed into the main
// statement:
//
//   - the message quotes the offending value, and ClickHouse's `throwIf`
//     requires a *constant* message, so the value cannot be interpolated
//     into it; and
//   - folding the offending value to a sentinel instead would answer a
//     query reference rejects, which is a wrong answer, not a lenient one.
//
// So the parameter's own evaluation rides beside the plan as its own
// query. Plan projects the parameter's value series in the canonical
// Sample shape — one row per evaluation step, exactly the series
// reference's `newFParams` builds by evaluating the parameter expression
// before the aggregation — and Check is the shared domain applied to
// those values. The executor runs Plan first and fails the request with
// Check's error, so the main statement is never reached, mirroring
// reference aborting before it aggregates.
type ScalarGuard struct {
	// Name identifies the guarded parameter in traces and in the wrapped
	// error, e.g. "topk K" or "double_exponential_smoothing sf".
	Name string
	// Plan projects the parameter's per-step value series as canonical
	// Samples.
	Plan chplan.Node
	// Check applies the parameter's domain to the values Plan produced,
	// in ascending timestamp order.
	Check func(values []float64) error
}

// registerScalarGuard lowers a computed scalar parameter as its own
// standalone query and records it, with its domain, on the lowering's
// guard sink.
//
// It is a no-op when the caller did not supply a sink. Every guarded
// shape reaches lowering through more entry points than the HTTP handler
// ([Lower], [LowerAt], the spec harness), and those callers ask only
// "what plan does this become". The sink is wired by the query path that
// can actually execute a second statement — see [LowerOpts.Guards].
func registerScalarGuard(ctx lowerCtx, name string, e parser.Expr, s schema.Metrics, check func([]float64) error) error {
	if ctx.guards == nil {
		return nil
	}
	plan, err := scalarGuardPlan(e, s, ctx)
	if err != nil {
		return err
	}
	*ctx.guards = append(*ctx.guards, ScalarGuard{Name: name, Plan: plan, Check: check})
	return nil
}

// scalarGuardPlan lowers a scalar-typed parameter expression as the
// standalone query reference Prometheus evaluates before it reaches the
// aggregation: one value per evaluation step in range mode, one value in
// instant mode.
//
// The value expression binds per step ([scalarStepValue]) so the guard
// sees the same series reference does — a parameter that is in domain at
// the eval anchor but out of domain three steps later must still fail the
// query.
//
// Three ctx adjustments make that true regardless of where the guarded
// parameter sits in the main plan:
//
//   - the anchor moves to the synthetic step grid's own `anchor_ts`,
//     because that is what names the step in the plan built below;
//   - per-step binding is re-enabled, because the position that pinned it
//     is a constraint of the MAIN statement's embedding site and this is a
//     different statement; and
//   - the guard sink is cleared, so lowering the parameter cannot register
//     further guards of its own and check the same value twice.
func scalarGuardPlan(e parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	guardCtx := ctx.withStepGridAnchor()
	guardCtx.pinScalarsOnce = false
	guardCtx.guards = nil
	value, err := lowerScalarArg(e, s, guardCtx)
	if err != nil {
		return nil, err
	}
	return syntheticScalarVector(value, nil, s, ctx), nil
}
