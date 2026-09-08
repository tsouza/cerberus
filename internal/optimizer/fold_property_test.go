// Property tests for the literal-fold helpers behind ConstantFoldSemantic.
//
// The fold helpers (`foldIntInt`, `foldFloatFloat`) are the analyzer rule's
// hot path: every pure-literal Binary subtree in a Filter predicate,
// Project expression, or Aggregate group-by / arg expression flows through
// them. A divergence here would silently flip the row set of any plan that
// contained a literal arithmetic subtree (`metric{} - 1` after a
// `LitInt - LitInt` fold, etc.).
//
// The INT oracle is arbitrary-precision, not the Go operator. This file used to
// assert `foldIntInt(OpAdd, l, r) == l + r`, restating the implementation with
// a second copy of the same Go operator (#3188). That form does catch a change
// to foldIntInt itself, but it can only ever conclude "the fold agrees with
// Go" — and agreeing with Go is not the requirement. Folding `LitInt op LitInt`
// REPLACES arithmetic ClickHouse would otherwise have performed, so the fold is
// correct only if it matches CLICKHOUSE. Over the overflow region that is a
// substantive claim, and an oracle spelled `l + r` asserts it vacuously: it
// reads as if wraparound had been verified when nothing about ClickHouse was
// consulted.
//
// wrappedInt64 computes the exact result in math/big and reduces it into
// int64's range, so it states the owed contract outright — ClickHouse Int64
// arithmetic wraps two's-complement, it neither saturates nor throws — rather
// than deferring to whatever Go happens to do.
//
// That the contract HOLDS against a real engine is settled where it can only be
// settled, against ClickHouse itself:
// test/property/int_fold_ch_agreement_test.go (build tag chdb) drives these
// same operand pairs through the shipped ConstantFoldSemantic rule and through
// chDB, and requires them to agree.
//
// The FLOAT oracle is still Go's own operator, and that is not the same
// mistake: IEEE-754 double semantics are the contract, ClickHouse Float64
// evaluates them, and Go evaluates the same hardware ones. There is no
// higher-precision oracle for NaN / signed-zero / Inf propagation, and
// math/big has no NaN or Inf at all.
//
// The audit at PR #375 Round 4 flagged `foldFloatFloat` at 0% line
// coverage and `foldIntInt` at 21%. These property tests close that gap
// at Layer 4: for every supported operator, draw a random pair of
// operands and assert the helper returns the same value the native Go
// `int64` / `float64` operator computes, with division-by-zero handled
// exactly as the helper specifies (folder declines the rewrite — returns
// `(nil, false)` — so downstream emit deals with division by zero at
// execution time).
//
// Operator scope mirrors the helpers' switch statements (see
// constant_fold.go): the 4 arithmetic ops (`+`, `-`, `*`, `/`) and the 6
// comparison ops (`=`, `!=`, `<`, `<=`, `>`, `>=`). `OpMod` and `OpPow`
// are intentionally absent from the helpers (ClickHouse and Go disagree
// on `%` sign rules + `^` is XOR in Go) — those binaries fall through to
// the no-fold branch, which is exercised by the negative-shape tests.
//
// Float coverage extends to IEEE-754 corner cases — NaN, ±Inf,
// signed-zero — drawn from a deterministic pool the property combines
// with rapid's `Float64` generator. The helper applies the IEEE rules
// for every op (NaN != NaN, Inf - Inf = NaN, …) — the property pins
// that contract by mirroring it through Go's native arithmetic.
//
// No `chdb` build tag — the helpers are pure scalar logic; no
// round-trip needed.

package optimizer

import (
	"math"
	"math/big"
	"testing"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/internal/chplan"
)

// arithIntOps enumerates the integer arithmetic ops the fold helper
// supports. Each must produce a *chplan.LitInt holding the value
// wrappedInt64 derives independently. `OpDiv` is handled separately so the
// property can guard against division-by-zero (the helper declines the
// rewrite — returns `(nil, false)`).
var arithIntOps = []chplan.BinaryOp{chplan.OpAdd, chplan.OpSub, chplan.OpMul}

// int64Bits is the width wrappedInt64 reduces into, and int64SignBit is the
// bit whose value decides whether that reduction is a negative number in
// two's-complement.
const (
	int64Bits    = 64
	int64SignBit = int64Bits - 1
)

// wrappedInt64 is the independent oracle for foldIntInt's arithmetic: it
// computes a op b EXACTLY in arbitrary precision and only then reduces the
// result into int64, so it shares no arithmetic with the implementation.
//
// The reduction is modulo 2^64 with the high half reinterpreted as negative —
// two's-complement wraparound, which is what ClickHouse's Int64 `+`/`-`/`*`
// do (they neither saturate nor raise). Stating it this way means a fold that
// saturated at MaxInt64, promoted to a wider type, or declined on overflow
// would each be caught, where comparing against Go's own operator could catch
// none of them.
func wrappedInt64(op chplan.BinaryOp, a, b int64) int64 {
	x := new(big.Int).SetInt64(a)
	y := new(big.Int).SetInt64(b)
	z := new(big.Int)
	switch op {
	case chplan.OpAdd:
		z.Add(x, y)
	case chplan.OpSub:
		z.Sub(x, y)
	case chplan.OpMul:
		z.Mul(x, y)
	default:
		panic("wrappedInt64: " + string(op) + " is not an arithmetic op this oracle models")
	}

	modulus := new(big.Int).Lsh(big.NewInt(1), int64Bits)
	// big.Int.Mod is Euclidean, so the result lands in [0, 2^64) whatever the
	// sign of z — which is exactly the unsigned bit pattern to reinterpret.
	z.Mod(z, modulus)
	if z.Cmp(new(big.Int).Lsh(big.NewInt(1), int64SignBit)) >= 0 {
		z.Sub(z, modulus)
	}
	return z.Int64()
}

// arithFloatOps mirrors arithIntOps for float64. `OpDiv` is again
// handled separately so the property can pin division-by-zero against
// IEEE-754 (the helper declines the rewrite for `r == 0` even when
// `l != 0` — same as foldIntInt).
var arithFloatOps = []struct {
	op chplan.BinaryOp
	fn func(a, b float64) float64
}{
	{chplan.OpAdd, func(a, b float64) float64 { return a + b }},
	{chplan.OpSub, func(a, b float64) float64 { return a - b }},
	{chplan.OpMul, func(a, b float64) float64 { return a * b }},
}

// cmpIntOps enumerates the integer comparison ops. Each must produce a
// *chplan.LitBool matching the native Go operator.
var cmpIntOps = []struct {
	op chplan.BinaryOp
	fn func(a, b int64) bool
}{
	{chplan.OpEq, func(a, b int64) bool { return a == b }},
	{chplan.OpNe, func(a, b int64) bool { return a != b }},
	{chplan.OpLt, func(a, b int64) bool { return a < b }},
	{chplan.OpLe, func(a, b int64) bool { return a <= b }},
	{chplan.OpGt, func(a, b int64) bool { return a > b }},
	{chplan.OpGe, func(a, b int64) bool { return a >= b }},
}

// cmpFloatOps mirrors cmpIntOps for float64. IEEE-754 ordering applies:
// any comparison involving NaN is false except `!=`, which is true.
var cmpFloatOps = []struct {
	op chplan.BinaryOp
	fn func(a, b float64) bool
}{
	{chplan.OpEq, func(a, b float64) bool { return a == b }},
	{chplan.OpNe, func(a, b float64) bool { return a != b }},
	{chplan.OpLt, func(a, b float64) bool { return a < b }},
	{chplan.OpLe, func(a, b float64) bool { return a <= b }},
	{chplan.OpGt, func(a, b float64) bool { return a > b }},
	{chplan.OpGe, func(a, b float64) bool { return a >= b }},
}

// floatSpecials is the deterministic pool of IEEE-754 corner-case
// float64 values the float properties mix into rapid's default draws.
// The helper must apply the IEEE rules for every value here (NaN
// propagation, ±Inf comparison, signed-zero equality).
var floatSpecials = []float64{
	0,
	math.Copysign(0, -1), // -0
	1,
	-1,
	math.SmallestNonzeroFloat64,
	-math.SmallestNonzeroFloat64,
	math.MaxFloat64,
	-math.MaxFloat64,
	math.Inf(1),
	math.Inf(-1),
	math.NaN(),
}

// drawInt64 draws an int64 operand. Coverage targets:
//
//   - the full int64 range via rapid's default generator,
//   - the boundary values (MinInt64 / MaxInt64) so the property
//     exercises 2's-complement wraparound for OpSub / OpMul.
func drawInt64(t *rapid.T, label string) int64 {
	if rapid.Bool().Draw(t, label+":pickBoundary") {
		return rapid.SampledFrom([]int64{
			0,
			1,
			-1,
			math.MaxInt64,
			math.MinInt64,
			math.MaxInt32,
			math.MinInt32,
			math.MaxInt32 + 1,
			math.MinInt32 - 1,
		}).Draw(t, label+":boundary")
	}
	return rapid.Int64().Draw(t, label+":int64")
}

// drawFloat64 draws a float64 operand. Coverage targets:
//
//   - the full float64 range via rapid's default generator,
//   - the IEEE-754 corner cases via floatSpecials so NaN / ±Inf /
//     signed-zero flow through the helper on every property run.
func drawFloat64(t *rapid.T, label string) float64 {
	if rapid.Bool().Draw(t, label+":pickSpecial") {
		return rapid.SampledFrom(floatSpecials).Draw(t, label+":special")
	}
	return rapid.Float64().Draw(t, label+":float64")
}

// TestFoldIntInt_Arithmetic asserts foldIntInt produces a LitInt with
// the same value as the native Go int64 operator for every supported
// arithmetic op, except `/` which is split out into the dedicated
// division test (so the property can guard against r==0 explicitly).
func TestFoldIntInt_Arithmetic(t *testing.T) {
	t.Parallel()

	for _, op := range arithIntOps {
		t.Run(string(op), func(t *testing.T) {
			t.Parallel()
			rapid.Check(t, func(t *rapid.T) {
				l := drawInt64(t, "l")
				r := drawInt64(t, "r")
				got, ok := foldIntInt(op, l, r)
				if !ok {
					t.Fatalf("foldIntInt(%v, %d, %d) declined the fold; expected a LitInt", op, l, r)
				}
				lit, isInt := got.(*chplan.LitInt)
				if !isInt {
					t.Fatalf("foldIntInt(%v, %d, %d) = %T; want *chplan.LitInt", op, l, r, got)
				}
				want := wrappedInt64(op, l, r)
				if lit.V != want {
					t.Fatalf("foldIntInt(%v, %d, %d) = %d; want %d (exact big.Int result reduced "+
						"into int64 two's-complement)", op, l, r, lit.V, want)
				}
			})
		})
	}
}

// TestFoldIntInt_Comparison asserts foldIntInt produces a LitBool with
// the same value as the native Go int64 comparison operator for every
// supported op.
func TestFoldIntInt_Comparison(t *testing.T) {
	t.Parallel()

	for _, tc := range cmpIntOps {
		tc := tc
		t.Run(string(tc.op), func(t *testing.T) {
			t.Parallel()
			rapid.Check(t, func(t *rapid.T) {
				l := drawInt64(t, "l")
				r := drawInt64(t, "r")
				got, ok := foldIntInt(tc.op, l, r)
				if !ok {
					t.Fatalf("foldIntInt(%v, %d, %d) declined the fold; expected a LitBool", tc.op, l, r)
				}
				lit, isBool := got.(*chplan.LitBool)
				if !isBool {
					t.Fatalf("foldIntInt(%v, %d, %d) = %T; want *chplan.LitBool", tc.op, l, r, got)
				}
				want := tc.fn(l, r)
				if lit.V != want {
					t.Fatalf("foldIntInt(%v, %d, %d) = %v; want %v", tc.op, l, r, lit.V, want)
				}
			})
		})
	}
}

// TestFoldIntInt_Division pins integer division semantics:
//
//   - r == 0 → fold declines (returns (nil, false)); the emitter must
//     handle integer-division-by-zero at execution time.
//   - r != 0 → fold returns a LitInt with the truncated Go quotient.
//
// Note Go's `/` is truncated-toward-zero, matching ClickHouse's
// `intDiv` semantics for the fold output. ClickHouse's bare `/` for
// integer arguments returns a Float64, but ConstantFoldSemantic only
// folds Binaries whose *both* operands are LitInt, so the result type
// is unambiguous: a single LitInt.
func TestFoldIntInt_Division(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		l := drawInt64(t, "l")
		r := drawInt64(t, "r")
		got, ok := foldIntInt(chplan.OpDiv, l, r)
		if r == 0 {
			if ok {
				t.Fatalf("foldIntInt(/, %d, 0) folded to %#v; expected (nil, false)", l, got)
			}
			if got != nil {
				t.Fatalf("foldIntInt(/, %d, 0) returned %#v; expected nil expr", l, got)
			}
			return
		}
		if !ok {
			t.Fatalf("foldIntInt(/, %d, %d) declined the fold; expected a LitInt", l, r)
		}
		lit, isInt := got.(*chplan.LitInt)
		if !isInt {
			t.Fatalf("foldIntInt(/, %d, %d) = %T; want *chplan.LitInt", l, r, got)
		}
		want := l / r
		if lit.V != want {
			t.Fatalf("foldIntInt(/, %d, %d) = %d; want %d", l, r, lit.V, want)
		}
	})
}

// TestFoldIntInt_UnsupportedOps pins the no-fold branch for ops the
// helper does NOT support (OpMod / OpPow / boolean / regex / etc.).
// The fold helper must return (nil, false) and the emitter falls back
// to standard binary emission.
func TestFoldIntInt_UnsupportedOps(t *testing.T) {
	t.Parallel()

	unsupported := []chplan.BinaryOp{
		chplan.OpMod,
		chplan.OpPow,
		chplan.OpAnd,
		chplan.OpOr,
		chplan.OpMatch,
		chplan.OpNotMatch,
	}
	rapid.Check(t, func(t *rapid.T) {
		op := rapid.SampledFrom(unsupported).Draw(t, "op")
		l := drawInt64(t, "l")
		r := drawInt64(t, "r")
		got, ok := foldIntInt(op, l, r)
		if ok {
			t.Fatalf("foldIntInt(%v, %d, %d) folded to %#v; expected (nil, false) for unsupported op", op, l, r, got)
		}
		if got != nil {
			t.Fatalf("foldIntInt(%v, %d, %d) returned %#v; expected nil expr", op, l, r, got)
		}
	})
}

// TestFoldFloatFloat_Arithmetic mirrors TestFoldIntInt_Arithmetic for
// float64 operands. IEEE-754 rules apply: NaN propagates through every
// arithmetic op, ±Inf - ±Inf produces NaN, etc. The helper must match
// Go's native float64 arithmetic on every draw.
func TestFoldFloatFloat_Arithmetic(t *testing.T) {
	t.Parallel()

	for _, tc := range arithFloatOps {
		tc := tc
		t.Run(string(tc.op), func(t *testing.T) {
			t.Parallel()
			rapid.Check(t, func(t *rapid.T) {
				l := drawFloat64(t, "l")
				r := drawFloat64(t, "r")
				got, ok := foldFloatFloat(tc.op, l, r)
				if !ok {
					t.Fatalf("foldFloatFloat(%v, %g, %g) declined the fold; expected a LitFloat", tc.op, l, r)
				}
				lit, isFloat := got.(*chplan.LitFloat)
				if !isFloat {
					t.Fatalf("foldFloatFloat(%v, %g, %g) = %T; want *chplan.LitFloat", tc.op, l, r, got)
				}
				want := tc.fn(l, r)
				if !floatEqualIEEE(lit.V, want) {
					t.Fatalf("foldFloatFloat(%v, %g, %g) = %g (bits %x); want %g (bits %x)",
						tc.op, l, r, lit.V, math.Float64bits(lit.V), want, math.Float64bits(want))
				}
			})
		})
	}
}

// TestFoldFloatFloat_Comparison mirrors TestFoldIntInt_Comparison for
// float64 operands. IEEE-754 ordering: any comparison involving NaN is
// false except `!=`, which is true.
func TestFoldFloatFloat_Comparison(t *testing.T) {
	t.Parallel()

	for _, tc := range cmpFloatOps {
		tc := tc
		t.Run(string(tc.op), func(t *testing.T) {
			t.Parallel()
			rapid.Check(t, func(t *rapid.T) {
				l := drawFloat64(t, "l")
				r := drawFloat64(t, "r")
				got, ok := foldFloatFloat(tc.op, l, r)
				if !ok {
					t.Fatalf("foldFloatFloat(%v, %g, %g) declined the fold; expected a LitBool", tc.op, l, r)
				}
				lit, isBool := got.(*chplan.LitBool)
				if !isBool {
					t.Fatalf("foldFloatFloat(%v, %g, %g) = %T; want *chplan.LitBool", tc.op, l, r, got)
				}
				want := tc.fn(l, r)
				if lit.V != want {
					t.Fatalf("foldFloatFloat(%v, %g, %g) = %v; want %v", tc.op, l, r, lit.V, want)
				}
			})
		})
	}
}

// TestFoldFloatFloat_Division pins float64 division semantics. Unlike
// the int helper, the float helper still declines the rewrite when the
// divisor is exactly zero — see constant_fold.go:`r == 0`. That
// keeps the IEEE-754 ±Inf / NaN result out of the plan IR: the
// emitter handles division-by-zero at execution time so the runtime
// vector engine and the plan-time fold stay in sync.
//
// Note `-0.0 == 0` is true in IEEE-754, so a divisor of `-0` also
// declines the rewrite.
//
// Property:
//
//   - r == 0 (including -0) → fold declines: (nil, false).
//   - r != 0 → fold returns LitFloat matching Go's native `/`.
func TestFoldFloatFloat_Division(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		l := drawFloat64(t, "l")
		r := drawFloat64(t, "r")
		got, ok := foldFloatFloat(chplan.OpDiv, l, r)
		if r == 0 {
			if ok {
				t.Fatalf("foldFloatFloat(/, %g, %g) folded to %#v; expected (nil, false) because r == 0", l, r, got)
			}
			if got != nil {
				t.Fatalf("foldFloatFloat(/, %g, %g) returned %#v; expected nil expr", l, r, got)
			}
			return
		}
		if !ok {
			t.Fatalf("foldFloatFloat(/, %g, %g) declined the fold; expected a LitFloat", l, r)
		}
		lit, isFloat := got.(*chplan.LitFloat)
		if !isFloat {
			t.Fatalf("foldFloatFloat(/, %g, %g) = %T; want *chplan.LitFloat", l, r, got)
		}
		want := l / r
		if !floatEqualIEEE(lit.V, want) {
			t.Fatalf("foldFloatFloat(/, %g, %g) = %g (bits %x); want %g (bits %x)",
				l, r, lit.V, math.Float64bits(lit.V), want, math.Float64bits(want))
		}
	})
}

// TestFoldFloatFloat_UnsupportedOps pins the no-fold branch for ops
// the helper does NOT support. Same shape as the int test — OpMod and
// OpPow flow through unchanged so the emitter can render them.
func TestFoldFloatFloat_UnsupportedOps(t *testing.T) {
	t.Parallel()

	unsupported := []chplan.BinaryOp{
		chplan.OpMod,
		chplan.OpPow,
		chplan.OpAnd,
		chplan.OpOr,
		chplan.OpMatch,
		chplan.OpNotMatch,
	}
	rapid.Check(t, func(t *rapid.T) {
		op := rapid.SampledFrom(unsupported).Draw(t, "op")
		l := drawFloat64(t, "l")
		r := drawFloat64(t, "r")
		got, ok := foldFloatFloat(op, l, r)
		if ok {
			t.Fatalf("foldFloatFloat(%v, %g, %g) folded to %#v; expected (nil, false) for unsupported op", op, l, r, got)
		}
		if got != nil {
			t.Fatalf("foldFloatFloat(%v, %g, %g) returned %#v; expected nil expr", op, l, r, got)
		}
	})
}

// TestFoldFloatFloat_NaNComparison pins the IEEE-754 NaN contract at a
// finer grain than the general comparison property: every comparison
// op that involves NaN must produce `false`, except `!=` which must
// produce `true`. The fold helper is the plan-time mirror of CH's
// runtime semantics — diverging here would let `metric{} = NaN` flip
// to `true` after a literal fold.
func TestFoldFloatFloat_NaNComparison(t *testing.T) {
	t.Parallel()

	nan := math.NaN()
	rapid.Check(t, func(t *rapid.T) {
		other := drawFloat64(t, "other")
		side := rapid.SampledFrom([]string{"left", "right", "both"}).Draw(t, "nanSide")
		op := rapid.SampledFrom([]chplan.BinaryOp{
			chplan.OpEq,
			chplan.OpNe,
			chplan.OpLt,
			chplan.OpLe,
			chplan.OpGt,
			chplan.OpGe,
		}).Draw(t, "op")

		var l, r float64
		switch side {
		case "left":
			l, r = nan, other
		case "right":
			l, r = other, nan
		case "both":
			l, r = nan, nan
		}
		got, ok := foldFloatFloat(op, l, r)
		if !ok {
			t.Fatalf("foldFloatFloat(%v, NaN-involved %g %g) declined the fold; expected a LitBool", op, l, r)
		}
		lit, isBool := got.(*chplan.LitBool)
		if !isBool {
			t.Fatalf("foldFloatFloat(%v, %g, %g) = %T; want *chplan.LitBool", op, l, r, got)
		}
		// IEEE-754: any NaN comparison is false except !=.
		want := op == chplan.OpNe
		if lit.V != want {
			t.Fatalf("foldFloatFloat(%v, %g, %g) = %v; want %v (IEEE-754 NaN rule)", op, l, r, lit.V, want)
		}
	})
}

// TestFoldFloatFloat_InfArithmetic pins ±Inf arithmetic at a finer
// grain than the general property. The fold helper must match Go's
// native float64 arithmetic on every Inf combination: `Inf + Inf =
// Inf`, `Inf - Inf = NaN`, `0 * Inf = NaN`, etc.
func TestFoldFloatFloat_InfArithmetic(t *testing.T) {
	t.Parallel()

	infs := []float64{math.Inf(1), math.Inf(-1)}
	finites := []float64{0, math.Copysign(0, -1), 1, -1, math.MaxFloat64, -math.MaxFloat64}

	for _, tc := range arithFloatOps {
		tc := tc
		t.Run(string(tc.op), func(t *testing.T) {
			t.Parallel()
			rapid.Check(t, func(t *rapid.T) {
				l := rapid.SampledFrom(append(append([]float64{}, infs...), finites...)).Draw(t, "l")
				r := rapid.SampledFrom(append(append([]float64{}, infs...), finites...)).Draw(t, "r")
				got, ok := foldFloatFloat(tc.op, l, r)
				if !ok {
					t.Fatalf("foldFloatFloat(%v, %g, %g) declined the fold; expected a LitFloat", tc.op, l, r)
				}
				lit, isFloat := got.(*chplan.LitFloat)
				if !isFloat {
					t.Fatalf("foldFloatFloat(%v, %g, %g) = %T; want *chplan.LitFloat", tc.op, l, r, got)
				}
				want := tc.fn(l, r)
				if !floatEqualIEEE(lit.V, want) {
					t.Fatalf("foldFloatFloat(%v, %g, %g) = %g (bits %x); want %g (bits %x)",
						tc.op, l, r, lit.V, math.Float64bits(lit.V), want, math.Float64bits(want))
				}
			})
		})
	}
}

// floatEqualIEEE compares two float64 values with NaN treated as
// equal-to-NaN (mirroring chplan.LitFloat.Equal). Reflect.DeepEqual on
// bits would also work, but signed-zero would then trip equality
// (`+0 != -0` on raw bits despite IEEE `+0 == -0`). The property tests
// want IEEE-comparison plus NaN==NaN, so this helper is the explicit
// contract.
func floatEqualIEEE(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	return a == b
}

// saturatingInt64 is a DELIBERATELY WRONG fold: it clamps at the int64
// boundaries instead of wrapping. It exists only for the meta-test below.
//
// It is the most plausible way foldIntInt could actually be got wrong: a
// well-meaning overflow guard.
func saturatingInt64(op chplan.BinaryOp, a, b int64) int64 {
	x := new(big.Int).SetInt64(a)
	y := new(big.Int).SetInt64(b)
	z := new(big.Int)
	switch op {
	case chplan.OpAdd:
		z.Add(x, y)
	case chplan.OpSub:
		z.Sub(x, y)
	case chplan.OpMul:
		z.Mul(x, y)
	default:
		panic("saturatingInt64: unsupported op " + string(op))
	}
	if z.Cmp(big.NewInt(math.MaxInt64)) > 0 {
		return math.MaxInt64
	}
	if z.Cmp(big.NewInt(math.MinInt64)) < 0 {
		return math.MinInt64
	}
	return z.Int64()
}

// TestWrappedInt64OracleIsIndependent proves the oracle
// TestFoldIntInt_Arithmetic compares against can actually reject a wrong fold.
//
// Two claims, and both are needed:
//
//   - On operands that do NOT overflow, the oracle agrees with the Go operator.
//     Without this the oracle could be arbitrary and the property would fail on
//     correct code.
//   - On operands that DO overflow, the oracle disagrees with a saturating
//     fold — the most plausible way this could actually be got wrong, a
//     well-meaning overflow guard. An oracle that cannot separate wrapping from
//     saturating is not an oracle for the thing this file claims to pin.
func TestWrappedInt64OracleIsIndependent(t *testing.T) {
	t.Parallel()

	t.Run("agrees with the Go operator below overflow", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(t *rapid.T) {
			// Half-width operands: no int64 product or sum can overflow.
			l := int64(rapid.Int32().Draw(t, "l"))
			r := int64(rapid.Int32().Draw(t, "r"))
			for _, op := range arithIntOps {
				var want int64
				switch op {
				case chplan.OpAdd:
					want = l + r
				case chplan.OpSub:
					want = l - r
				case chplan.OpMul:
					want = l * r
				}
				if got := wrappedInt64(op, l, r); got != want {
					t.Fatalf("wrappedInt64(%v, %d, %d) = %d; want %d — the oracle disagrees with "+
						"the Go operator on operands that cannot overflow", op, l, r, got, want)
				}
			}
		})
	})

	t.Run("rejects a saturating fold on overflow", func(t *testing.T) {
		t.Parallel()
		overflowing := []struct {
			op   chplan.BinaryOp
			l, r int64
		}{
			{chplan.OpAdd, math.MaxInt64, 1},
			{chplan.OpSub, math.MinInt64, 1},
			{chplan.OpMul, math.MaxInt64, 2},
			{chplan.OpMul, math.MinInt64, -1},
		}
		for _, c := range overflowing {
			wrapped := wrappedInt64(c.op, c.l, c.r)
			saturated := saturatingInt64(c.op, c.l, c.r)
			if wrapped == saturated {
				t.Errorf("wrappedInt64(%v, %d, %d) = saturatingInt64(...) = %d — the oracle cannot "+
					"tell a wrapping fold from a saturating one here, so the property it backs "+
					"could not catch that regression", c.op, c.l, c.r, wrapped)
			}
			// And the real implementation is the wrapping one.
			got, ok := foldIntInt(c.op, c.l, c.r)
			if !ok {
				t.Fatalf("foldIntInt(%v, %d, %d) declined an overflowing fold; ClickHouse Int64 "+
					"arithmetic wraps rather than refusing", c.op, c.l, c.r)
			}
			lit, isInt := got.(*chplan.LitInt)
			if !isInt || lit.V != wrapped {
				t.Errorf("foldIntInt(%v, %d, %d) = %v; want the wrapped value %d",
					c.op, c.l, c.r, got, wrapped)
			}
		}
	})
}
