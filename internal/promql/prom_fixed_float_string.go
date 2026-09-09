package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
)

// promFixedFloatStringExpr renders Go's
// `strconv.FormatFloat(f, 'f', -1, 64)` for a RUNTIME float expression.
//
// That verb — shortest round-tripping digits, laid out in POSITIONAL
// notation, never scientific — is the one Prometheus uses everywhere it
// turns a float into a label VALUE, and a label value is part of the
// series identity on the wire:
//
//   - `count_values("l", expr)` stamps the observed value as the label
//     `l`: `promql/engine.go`'s aggregationCountValues does
//     `enh.lb.Set(valueLabel, strconv.FormatFloat(s.F, 'f', -1, 64))`
//     for a float sample. (A histogram sample goes through
//     `FloatHistogram.String()` instead, which is `%g` — see
//     [nativeHistogramFloatString] for that variant.)
//   - the classic-bucket `le` label: Prometheus's own OTLP receiver, the
//     code that explodes an OTel explicit-bucket histogram into classic
//     `<name>_bucket{le="…"}` series, does
//     `boundStr := strconv.FormatFloat(bound, 'f', -1, 64)` in
//     `storage/remote/otlptranslator/prometheusremotewrite/helper.go`.
//     Cerberus serves the SAME OTel histograms through the Prometheus
//     API, so that is the spelling a drop-in replacement has to produce.
//
// ClickHouse's `toString(Float64)` produces the same shortest
// round-tripping DIGITS Go's shortest mode does; the two disagree only on
// LAYOUT, and on two axes:
//
//   - Special values. CH writes `nan` / `inf` / `-inf` where Go writes
//     `NaN` / `+Inf` / `-Inf`.
//   - Scientific notation. CH switches to it once the magnitude leaves
//     roughly `[1e-7, 1e21)` (`1e-7`, `1e21`, `1.2345e22`); the `'f'`
//     verb NEVER does, spelling those `0.0000001`,
//     `1000000000000000000000` and `12345000000000000000000`.
//
// So the expression takes CH's rendering apart — from the STRING, never
// via a floating-point log, so no boundary can be misclassified by a
// rounding error — and re-lays the digits out positionally whenever CH
// chose scientific notation. When CH already chose fixed notation its
// output is byte-identical to Go's and is returned unchanged, sign and
// all, which is also what keeps negative zero spelled `-0`.
//
// Every intermediate quantity is bound with [hqLet] exactly once. A
// chplan.Expr tree is rendered as a tree, so an unbound sub-expression
// read by several of the steps below would be re-expanded at every
// mention and the copies would multiply with depth; see hqLet's own doc.
func promFixedFloatStringExpr(value chplan.Expr) chplan.Expr {
	const (
		// Lambda binding names. Distinct from every other hqLet binding
		// in this package (see histogram_quantile_window.go's naming
		// note) because these nest inside one another.
		valueParam      = "pffv"
		renderParam     = "pffu"
		magnitudeParam  = "pffs"
		expPosParam     = "pffe"
		mantissaParam   = "pffm"
		pointPosParam   = "pffp"
		digitsParam     = "pffd"
		pointShiftParam = "pffn"

		// `substring` is 1-based, so the magnitude of a negative
		// rendering starts one byte past the leading minus sign.
		afterSignOffset = 2
	)

	call := func(fn chplan.Fn, args ...chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Fn: fn, Args: args}
	}
	// Shape constants go inline rather than through `?` placeholders:
	// they feed `concat`, where a bound parameter leaves the operand type
	// indeterminate and ClickHouse mis-dispatches to `arrayConcat`
	// (Code 43). Same reasoning as chplan.InlineString's doc comment.
	str := func(v string) chplan.Expr { return &chplan.InlineString{V: v} }
	i := func(v int64) chplan.Expr { return &chplan.LitInt{V: v} }
	f := func(v float64) chplan.Expr { return &chplan.LitFloat{V: v} }
	bin := func(op chplan.BinaryOp, l, r chplan.Expr) chplan.Expr {
		return &chplan.Binary{Op: op, Left: l, Right: r}
	}
	// `position` and `length` return UInt64, while subtracting from one
	// yields Int64. An `if` with one arm of each has no common supertype
	// and ClickHouse resolves the pair to `Variant(Int64, UInt64)`, whose
	// arithmetic comes back Nullable — nullability that would ride the
	// whole label expression up into `mapConcat` and leave Attributes as
	// `Map(String, Nullable(String))`, which the production cursor
	// refuses to scan into `map[string]string`. Landing every count on
	// Int64 at the source keeps each branch single-typed.
	countOf := func(fn chplan.Fn, args ...chplan.Expr) chplan.Expr {
		return call(chplan.FnToInt64, call(fn, args...))
	}
	// The fn registry carries no `repeat`; `leftPad` of the empty string
	// with a single-character pad is the same thing.
	zeros := func(n chplan.Expr) chplan.Expr {
		return call(chplan.FnLeftPad, str(""), n, str("0"))
	}

	// expand re-lays CH's scientific rendering `s` of a finite POSITIVE
	// magnitude out positionally, given `ep`, the 1-based position of its
	// `e`. `s` is `<mantissa>e<exponent>` with the mantissa `d` or
	// `d.ddd`; concatenating the mantissa's digits and shifting the
	// decimal point right by the exponent gives the positional spelling.
	// The three branches are the three places the shifted point can land:
	// past the last digit (pad with zeros), inside the digits (split
	// them), or at or before the first (a leading `0.` plus enough zeros
	// to push the digits down).
	expand := func(s, ep chplan.Expr) chplan.Expr {
		// Both `substring(x, from, len)` and `substring(x, from)` are
		// 1-based, so the mantissa runs up to the byte before `e` and the
		// exponent starts one byte after it.
		mantissa := call(chplan.FnSubstring, s, i(1), bin(chplan.OpSub, ep, i(1)))
		exponent := call(chplan.FnToInt64, call(chplan.FnSubstring, s, bin(chplan.OpAdd, ep, i(1))))

		return hqLet(mantissaParam, mantissa, func(m chplan.Expr) chplan.Expr {
			return hqLet(pointPosParam, countOf(chplan.FnStringPosition, m, str(".")), func(p chplan.Expr) chplan.Expr {
				// The mantissa's digits with its decimal point removed,
				// and the count of digits that stood left of that point.
				digits := call(chplan.FnIf, bin(chplan.OpGt, p, i(0)),
					call(chplan.FnConcat,
						call(chplan.FnSubstring, m, i(1), bin(chplan.OpSub, p, i(1))),
						call(chplan.FnSubstring, m, bin(chplan.OpAdd, p, i(1)))),
					m)
				intLen := call(chplan.FnIf, bin(chplan.OpGt, p, i(0)),
					bin(chplan.OpSub, p, i(1)),
					countOf(chplan.FnLength, m))

				return hqLet(digitsParam, digits, func(d chplan.Expr) chplan.Expr {
					// Where the decimal point lands once the exponent has
					// shifted it, counted in digits from the left.
					return hqLet(pointShiftParam, bin(chplan.OpAdd, intLen, exponent), func(n chplan.Expr) chplan.Expr {
						digitCount := countOf(chplan.FnLength, d)
						return call(chplan.FnMultiIf,
							bin(chplan.OpGe, n, digitCount),
							call(chplan.FnConcat, d, zeros(bin(chplan.OpSub, n, digitCount))),
							bin(chplan.OpGt, n, i(0)),
							call(chplan.FnConcat,
								call(chplan.FnSubstring, d, i(1), n),
								str("."),
								call(chplan.FnSubstring, d, bin(chplan.OpAdd, n, i(1)))),
							call(chplan.FnConcat, str("0."), zeros(bin(chplan.OpSub, i(0), n)), d))
					})
				})
			})
		})
	}

	return hqLet(valueParam, value, func(v chplan.Expr) chplan.Expr {
		return hqLet(renderParam, call(chplan.FnToString, v), func(u chplan.Expr) chplan.Expr {
			negative := func() chplan.Expr { return call(chplan.FnStartsWith, u, str("-")) }

			// CH's rendering of the magnitude: the whole thing, minus a
			// leading minus sign. Splitting the sign off first keeps the
			// exponent scan below from tripping over it.
			magnitude := call(chplan.FnIf, negative(),
				call(chplan.FnSubstring, u, i(afterSignOffset)),
				u)

			finite := hqLet(magnitudeParam, magnitude, func(s chplan.Expr) chplan.Expr {
				// Position of the exponent marker in CH's rendering; 0
				// when CH chose fixed notation, which is precisely when
				// its output already IS Go's.
				return hqLet(expPosParam, countOf(chplan.FnStringPosition, s, str("e")), func(ep chplan.Expr) chplan.Expr {
					return call(chplan.FnIf, bin(chplan.OpEq, ep, i(0)),
						u,
						call(chplan.FnConcat,
							call(chplan.FnIf, negative(), str("-"), str("")),
							expand(s, ep)))
				})
			})

			return call(chplan.FnMultiIf,
				call(chplan.FnIsNaN, v), str("NaN"),
				bin(chplan.OpAnd, call(chplan.FnIsInfinite, v), bin(chplan.OpGt, v, f(0))), str("+Inf"),
				call(chplan.FnIsInfinite, v), str("-Inf"),
				finite)
		})
	})
}
