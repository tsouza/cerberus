package promql

import (
	"fmt"
	"math"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/test/property"
)

// Evaluator carries the per-query evaluation context: the model, the
// effective lookback delta, and the @start()/@end() reference
// timestamps. It's constructed once per query in [Evaluate].
type Evaluator struct {
	model    *Model
	lookback time.Duration
	// startMs / endMs are the reference timestamps for @start() and
	// @end(). For instant queries (PR 2's only shape), both equal the
	// eval ts.
	startMs, endMs int64
}

// Options tunes the evaluator. Today the only knob is the lookback
// delta; defaults track Prom (5min).
type Options struct {
	LookbackDelta time.Duration
}

// Evaluate is the top-level entry point. It parses the query via
// Prometheus's parser (so the AST shape matches what cerberus's
// pipeline sees), then walks the AST under in-tree evaluation rules.
//
// Returns an [property.Outcome] in the same shape the framework's
// comparator consumes — the labels-stripped, eval-ts-stamped vector
// representation.
//
// On parse error or any AST node the oracle doesn't support, the
// returned Outcome carries the error and an empty row set. The
// framework's CompareOutcomes treats both-erroring queries as
// agreement, so an unsupported shape doesn't fail the property; it
// just means the test doesn't exercise that shape.
func Evaluate(d property.Dataset, q property.Query, opts Options) property.Outcome {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(q.String)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("oracle: parse %q: %w", q.String, err)}
	}
	lookback := opts.LookbackDelta
	if lookback == 0 {
		lookback = DefaultLookbackDelta
	}
	evalMs := time.Unix(q.EvalTs, 0).UTC().UnixMilli()
	e := &Evaluator{
		model:    FromDataset(d),
		lookback: lookback,
		startMs:  evalMs,
		endMs:    evalMs,
	}
	val, err := e.evalAny(expr, evalMs)
	if err != nil {
		return property.Outcome{Err: err}
	}
	return outcomeFromValue(val)
}

// value carries any of the three things an AST node can evaluate to:
// an instant vector, a scalar, or a range vector (only as an
// intermediate inside Call). The fields aren't exclusive — Vec is
// nil-when-Scalar and vice versa.
type value struct {
	Kind   valueKind
	Vec    []VectorRow
	Scalar float64
	Range  []RangePoints
}

type valueKind int

const (
	kindVec valueKind = iota
	kindScalar
	kindRange
)

// evalAny is the AST dispatch. It returns a `value` so callers can
// branch on the result type.
func (e *Evaluator) evalAny(expr parser.Expr, evalTsMs int64) (value, error) {
	switch v := expr.(type) {
	case *parser.NumberLiteral:
		return value{Kind: kindScalar, Scalar: v.Val}, nil
	case *parser.ParenExpr:
		return e.evalAny(v.Expr, evalTsMs)
	case *parser.UnaryExpr:
		inner, err := e.evalAny(v.Expr, evalTsMs)
		if err != nil {
			return value{}, err
		}
		return applyUnary(v.Op, inner)
	case *parser.VectorSelector:
		return value{Kind: kindVec, Vec: e.evalVectorSelector(v, evalTsMs)}, nil
	case *parser.MatrixSelector:
		return value{Kind: kindRange, Range: e.evalMatrixSelector(v, evalTsMs)}, nil
	case *parser.Call:
		return e.evalCall(v, evalTsMs)
	case *parser.AggregateExpr:
		inner, err := e.evalAny(v.Expr, evalTsMs)
		if err != nil {
			return value{}, err
		}
		if inner.Kind != kindVec {
			return value{}, fmt.Errorf("oracle: aggregation requires vector input, got kind=%d", inner.Kind)
		}
		out, err := e.evalAggregation(v, inner.Vec, evalTsMs)
		if err != nil {
			return value{}, err
		}
		return value{Kind: kindVec, Vec: out}, nil
	case *parser.BinaryExpr:
		lhs, err := e.evalAny(v.LHS, evalTsMs)
		if err != nil {
			return value{}, err
		}
		rhs, err := e.evalAny(v.RHS, evalTsMs)
		if err != nil {
			return value{}, err
		}
		return e.applyBinaryAST(v, lhs, rhs, evalTsMs)
	case *parser.StringLiteral:
		return value{}, fmt.Errorf("oracle: bare string literal not supported at top level")
	}
	return value{}, fmt.Errorf("oracle: unsupported AST node %T", expr)
}

// applyUnary handles `+x` (no-op) and `-x` (negate). PromQL's grammar
// only allows unary on scalars + vectors; we mirror that.
func applyUnary(op parser.ItemType, in value) (value, error) {
	switch op {
	case parser.ADD:
		return in, nil
	case parser.SUB:
		switch in.Kind {
		case kindScalar:
			return value{Kind: kindScalar, Scalar: -in.Scalar}, nil
		case kindVec:
			out := make([]VectorRow, len(in.Vec))
			for i, r := range in.Vec {
				out[i] = VectorRow{
					Labels: DropLabel(r.Labels, MetricNameLabel),
					T:      r.T,
					V:      -r.V,
				}
			}
			return value{Kind: kindVec, Vec: out}, nil
		}
	}
	return value{}, fmt.Errorf("oracle: unsupported unary op %s on kind=%d", op, in.Kind)
}

// evalCall dispatches a function call. The MVP set:
//
//   - rate/increase/delta + *_over_time over range vectors.
//   - histogram_quantile(phi, vector) — its second arg is an instant
//     vector (typically a `sum by(le)(rate(...[range]))`) so we
//     evaluate it the same way as any other instant vector.
//   - scalar(vec)         — pick the single-element vector's value.
//   - vector(scalar)      — wrap a scalar into a one-row, no-label
//     vector.
func (e *Evaluator) evalCall(c *parser.Call, evalTsMs int64) (value, error) {
	name := c.Func.Name
	if isRangeFunctionName(name) {
		out, err := e.evalRangeFunction(c, evalTsMs)
		if err != nil {
			return value{}, err
		}
		return value{Kind: kindVec, Vec: out}, nil
	}
	switch name {
	case "histogram_quantile":
		if len(c.Args) != 2 {
			return value{}, fmt.Errorf("oracle: histogram_quantile expects 2 args, got %d", len(c.Args))
		}
		phiVal, err := e.evalAny(c.Args[0], evalTsMs)
		if err != nil {
			return value{}, err
		}
		if phiVal.Kind != kindScalar {
			return value{}, fmt.Errorf("oracle: histogram_quantile phi must be scalar")
		}
		bucketsVal, err := e.evalAny(c.Args[1], evalTsMs)
		if err != nil {
			return value{}, err
		}
		if bucketsVal.Kind != kindVec {
			return value{}, fmt.Errorf("oracle: histogram_quantile bucket arg must be vector")
		}
		out, err := histogramQuantile(phiVal.Scalar, bucketsVal.Vec, evalTsMs)
		if err != nil {
			return value{}, err
		}
		return value{Kind: kindVec, Vec: out}, nil
	case "scalar":
		if len(c.Args) != 1 {
			return value{}, fmt.Errorf("oracle: scalar() expects 1 arg")
		}
		inner, err := e.evalAny(c.Args[0], evalTsMs)
		if err != nil {
			return value{}, err
		}
		if inner.Kind != kindVec {
			return value{}, fmt.Errorf("oracle: scalar() argument must be vector")
		}
		if len(inner.Vec) != 1 {
			// Prom: if the vector has 0 or >1 elements, scalar
			// returns NaN.
			return value{Kind: kindScalar, Scalar: nan()}, nil
		}
		return value{Kind: kindScalar, Scalar: inner.Vec[0].V}, nil
	case "vector":
		if len(c.Args) != 1 {
			return value{}, fmt.Errorf("oracle: vector() expects 1 arg")
		}
		inner, err := e.evalAny(c.Args[0], evalTsMs)
		if err != nil {
			return value{}, err
		}
		if inner.Kind != kindScalar {
			return value{}, fmt.Errorf("oracle: vector() argument must be scalar")
		}
		return value{Kind: kindVec, Vec: []VectorRow{
			{Labels: map[string]string{}, T: evalTsMs, V: inner.Scalar},
		}}, nil
	}
	return value{}, fmt.Errorf("oracle: unsupported function %q", name)
}

// applyBinaryAST adapts the AST-level binary expr into the
// scalarOrVector shape evalBinary works on.
func (e *Evaluator) applyBinaryAST(b *parser.BinaryExpr, lhs, rhs value, evalTsMs int64) (value, error) {
	lhsSV, err := toScalarOrVector(lhs)
	if err != nil {
		return value{}, err
	}
	rhsSV, err := toScalarOrVector(rhs)
	if err != nil {
		return value{}, err
	}
	rows, scalar, ok, err := e.evalBinary(b, lhsSV, rhsSV, evalTsMs)
	if err != nil {
		return value{}, err
	}
	if rows != nil {
		return value{Kind: kindVec, Vec: rows}, nil
	}
	if !ok {
		return value{}, fmt.Errorf("oracle: binary op %s scalar/scalar produced undefined result", b.Op)
	}
	return value{Kind: kindScalar, Scalar: scalar}, nil
}

func toScalarOrVector(v value) (scalarOrVector, error) {
	switch v.Kind {
	case kindScalar:
		return scalarOrVector{isScalar: true, scalar: v.Scalar}, nil
	case kindVec:
		return scalarOrVector{isScalar: false, rows: v.Vec}, nil
	}
	return scalarOrVector{}, fmt.Errorf("oracle: binary op operand has invalid kind=%d", v.Kind)
}

// outcomeFromValue reshapes the evaluator's `value` into the
// framework's property.Outcome. The framework's comparator strips
// __name__ in its own labelKey, so we strip here as well — keep the
// two paths symmetric for the row matcher.
func outcomeFromValue(v value) property.Outcome {
	switch v.Kind {
	case kindVec:
		out := property.Outcome{Rows: make([]property.OutcomeRow, 0, len(v.Vec))}
		for _, r := range v.Vec {
			out.Rows = append(out.Rows, property.OutcomeRow{
				Labels:      DropLabel(r.Labels, MetricNameLabel),
				TimestampMs: r.T,
				Value:       r.V,
			})
		}
		return out
	case kindScalar:
		// A scalar at the top level is reported as a vector with one
		// label-less row at the eval ts. Same convention as Prom's
		// HTTP layer (which surfaces a scalar via the "scalar"
		// resultType, but the framework's instant-only comparator
		// only deals in vectors).
		return property.Outcome{Rows: []property.OutcomeRow{
			{Labels: map[string]string{}, TimestampMs: 0, Value: v.Scalar},
		}}
	}
	return property.Outcome{}
}

func nan() float64 {
	// math.NaN() in a tiny wrapper so callers don't need the import
	// just for this one literal.
	return math.NaN()
}
