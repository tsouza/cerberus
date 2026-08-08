package promql

import (
	"fmt"
	"math"
)

// verbatimErrorf constructs an error whose message reproduces a
// reference backend's own wording byte for byte — never prefixed with
// "promql: " the way every other lowering error is, because the prefix
// would be cerberus's own annotation on a sentence reference itself
// wrote.
//
// It exists as a named wrapper, rather than a bare fmt.Errorf call,
// so the rejection-parity scanner (isErrorConstructor in
// test/rejection-parity/catalogue.go) can recognise these five call
// sites by name and admit them into the catalogue despite the missing
// prefix its normal filter requires. That recognition is scoped to
// calls written exactly this way — the five domain-violation sites in
// this file are the only ones in the package that should ever call it;
// see the doc comment on this file for why their wording cannot be
// cerberus's own.
func verbatimErrorf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

// Reference Prometheus rejects a handful of scalar *parameter* values at
// evaluation time rather than at parse time: a topk K that is NaN or too
// large to become an int64, a limit_ratio ratio that is NaN, a
// double_exponential_smoothing factor outside (0, 1). The parser types
// those arguments as scalars and stops there, so the check has to run
// where a concrete value exists.
//
// Cerberus reaches that point twice, on two structurally different paths:
//
//   - the literal path, where TryFoldScalar reduces the argument at
//     lowering time and the value is known before any SQL exists; and
//   - the computed path (`scalar(<vector>)` and arithmetic over it),
//     where the value only exists once ClickHouse has run the parameter's
//     own subtree.
//
// This file is the single owner of those rules so the two paths cannot
// drift. Each domain is a plain function over float64 values — the
// literal path calls it with the one value it folded, the computed path
// calls it (via a [ScalarGuard]) with the per-step value series the
// parameter's own evaluation produced. Adding a rule here changes both
// paths at once; there is nowhere to add it to only one.

// Reference Prometheus compares a topk/bottomk/limitk K against these
// bounds before converting it to an int64
// (prometheus/promql/engine.go's minInt64 / maxInt64).
//
// aggParamMaxInt64 is deliberately NOT math.MaxInt64: float64 has 53 bits
// of mantissa, so 2^63-1 is not representable and the nearest
// representable value below it is 2^63-1024. Comparing against that value
// is what makes the subsequent int64 conversion total.
const (
	aggParamMaxInt64 = 9223372036854774784
	aggParamMinInt64 = -9223372036854775808
)

// aggregationParamMinK is the smallest K that selects anything: reference
// returns an empty result — not an error — for every K below it, and
// row_number() being 1-based makes the same value the rank floor.
const aggregationParamMinK = 1

// aggregationParamDomain applies reference Prometheus's K-domain rules for
// `topk` / `bottomk` / `limitk` to the parameter's whole per-step value
// series and reports what reference would do with it.
//
// Reference (prometheus/promql/engine.go, rangeEvalAgg) inspects the
// series as a whole, in this order:
//
//  1. params.Max() < 1         → empty result, no error
//  2. params.HasAnyNaN()       → "Parameter value is NaN"
//  3. params.Min() <= minInt64 → "Scalar value %v underflows int64"
//  4. params.Max() >= maxInt64 → "Scalar value %v overflows int64"
//
// Both the order and the aggregate shape are load-bearing, which is why
// this is not a per-value loop. A series carrying a NaN at one step and an
// overflowing value at another reports the NaN, because HasAnyNaN is
// checked first; and Go's math.Max propagates NaN, so a series with any
// NaN never takes the early-empty branch even when every other step is
// below 1.
//
// An empty series means the parameter produced no values at all, which is
// reference's `len(mat) == 0` zero-value fParams: max is 0, below 1, so
// the query answers empty rather than erroring.
//
// The reported value in the underflow / overflow messages is the extreme
// one reference names (Min for underflow, Max for overflow), not the
// first offending step.
func aggregationParamDomain(values []float64) (empty bool, err error) {
	mn, mx := paramExtrema(values)
	switch {
	case mx < aggregationParamMinK:
		return true, nil
	case hasAnyNaN(values):
		return false, verbatimErrorf("Parameter value is NaN")
	case mn <= aggParamMinInt64:
		return false, verbatimErrorf("Scalar value %v underflows int64", mn)
	case mx >= aggParamMaxInt64:
		return false, verbatimErrorf("Scalar value %v overflows int64", mx)
	}
	return false, nil
}

// limitRatioParamDomain applies reference Prometheus's ratio-domain rules
// for `limit_ratio` to the parameter's whole per-step value series.
//
// Reference (prometheus/promql/engine.go, rangeEvalAgg's LIMIT_RATIO arm)
// inspects the series as a whole, in this order:
//
//  1. params.Max() == 0 && params.Min() == 0 → empty result, no error
//  2. params.HasAnyNaN()                     → "Ratio value is NaN"
//
// A ratio outside [-1, 1] is a WARNING there, not an error, so it is not
// a domain violation and does not appear here.
func limitRatioParamDomain(values []float64) (empty bool, err error) {
	mn, mx := paramExtrema(values)
	switch {
	case mx == 0 && mn == 0:
		return true, nil
	case hasAnyNaN(values):
		return false, verbatimErrorf("Ratio value is NaN")
	}
	return false, nil
}

// holtWintersFactorKind names which of double_exponential_smoothing's two
// factors a domain violation refers to. The two rules are identical apart
// from the word reference puts in the message, and the message is the
// user-visible contract, so the kind travels with the check rather than
// being reconstructed by the caller.
type holtWintersFactorKind struct {
	// noun is how reference's message names the factor.
	noun string
	// symbol is the abbreviation reference's message uses in the
	// expected-range clause.
	symbol string
}

var (
	holtWintersSmoothingFactor = holtWintersFactorKind{noun: "smoothing", symbol: "sf"}
	holtWintersTrendFactor     = holtWintersFactorKind{noun: "trend", symbol: "tf"}
)

// holtWintersFactorDomain applies reference Prometheus's
// double_exponential_smoothing factor rules to the factor's whole
// per-step value series.
//
// Reference (prometheus/promql/functions.go,
// funcDoubleExponentialSmoothing) rejects the query outright when a
// factor falls outside the open interval (0, 1):
//
//	if sf <= 0 || sf >= 1 {
//	    panic(fmt.Errorf("invalid smoothing factor. Expected: 0 < sf < 1, got: %f", sf))
//	}
//
// NaN fails BOTH comparisons in Go, so reference does not reject a NaN
// factor — it smooths with it and produces NaN samples. Adding an
// IsNaN arm here would be an invented rejection, answering 4xx where
// reference answers 200, so the condition is reference's verbatim.
//
// Reference evaluates the factor per step and panics at the first step
// whose value is out of domain, so the reported value is the first
// offender in timestamp order rather than an extreme.
func holtWintersFactorDomain(kind holtWintersFactorKind, values []float64) error {
	for _, v := range values {
		if v <= 0 || v >= 1 {
			return verbatimErrorf(
				"invalid %s factor. Expected: 0 < %s < 1, got: %f", kind.noun, kind.symbol, v,
			)
		}
	}
	return nil
}

// paramExtrema reproduces newFParams' min/max fold, NaN propagation
// included: reference seeds max with -MaxFloat64 and min with +MaxFloat64
// and folds with math.Max / math.Min, both of which return NaN as soon as
// either operand is NaN.
//
// The empty case is NOT the seed values. Reference returns the zero-value
// `&fParams{}` when the parameter evaluates to an empty matrix
// (promql/value.go, newFParams), whose minValue and maxValue are both 0 —
// it never reaches the seeding. Returning the seeds here instead would
// agree with reference for the K domain by accident (-MaxFloat64 < 1 is
// the same empty verdict) and disagree for the limit_ratio domain, whose
// empty rule is max == 0 && min == 0.
func paramExtrema(values []float64) (mn, mx float64) {
	if len(values) == 0 {
		return 0, 0
	}
	mn, mx = math.MaxFloat64, -math.MaxFloat64
	for _, v := range values {
		mx = math.Max(mx, v)
		mn = math.Min(mn, v)
	}
	return mn, mx
}

// hasAnyNaN reproduces fParams.HasAnyNaN.
func hasAnyNaN(values []float64) bool {
	for _, v := range values {
		if math.IsNaN(v) {
			return true
		}
	}
	return false
}
