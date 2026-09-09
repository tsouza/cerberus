package promql

import "github.com/tsouza/cerberus/internal/chplan"

// histogram_shortest_g.go holds the ONE runtime decomposition of a float
// into Go's shortest `%g` layout, and the two spellings built on top of
// it.
//
// # Why a decomposition at all
//
// ClickHouse's `toString(Float64)` already produces the same shortest
// round-tripping digits Go's `strconv.FormatFloat(f, 'g', -1, 64)` does —
// the two disagree only on LAYOUT. CH switches to scientific notation at a
// different magnitude threshold and spells the exponent differently
// (`1e-7` / `1e21` where Go writes `1e-07` / `1e+21`). So the expression
// below reads the digits and the decimal exponent back out of CH's
// rendering — exactly, from the string, never via log10, so the `[-4, 6)`
// boundary cannot be misclassified by a log rounding error — and re-lays
// them out under Go's rule.
//
// # Why it is shared (cerberus issue #3228)
//
// Two callers need that layout and differ only in what they wrap around
// it: [nativeHistogramShortestGString] renders a native histogram's own
// float fields, and [openMetricsFloatExpr] renders the OpenMetrics variant
// `histogram_quantiles` stamps on a computed phi (same digits, a trailing
// `.0` on integral values, and its own hardcoded 1 / 0 / -1 / NaN / ±Inf
// spellings). They used to carry two transcriptions of the identical
// decomposition, and that is how they drifted into disagreeing about the
// scientific-notation threshold — one carried Go's 1e6, the other
// ClickHouse's 1e21 — which #3214 had to fix. The threshold constants were
// collapsed there; this file collapses the decomposition itself.

// goShortestGSciLowerBound and goShortestGSciUpperBound bracket the
// magnitudes Go's shortest `%g` lays out in FIXED notation. `%g` uses
// scientific notation exactly when the decimal exponent falls outside
// `[-4, eprec)`, and `strconv/ftoa.go` pins `eprec` to 6 whenever the
// requested precision is "shortest" — so the fixed window is `[1e-4,
// 1e6)`, and `strconv.FormatFloat(1e6, 'g', -1, 64)` really is `1e+06`.
const (
	goShortestGSciLowerBound = 1e-4
	goShortestGSciUpperBound = 1e6
	// Exponents below this get a leading zero: Go writes at least two
	// exponent digits ("1e-05", never "1e-5").
	goSciExpPadBelow = 10
)

// Lambda parameter and hqLet binding names for the decomposition. They
// all nest inside the value/digits lambda [shortestGExpr] opens, so they
// are distinct from each other and from every other hqLet binding in this
// package — see histogram_quantile_window.go's own naming note.
const (
	shortestGValueParam      = "hgv"
	shortestGDigitsParam     = "hgu"
	shortestGPosParam        = "hge"
	shortestGMantRawParam    = "hgm"
	shortestGDigitsAllParam  = "hgda"
	shortestGDigitsLeadParam = "hgdl"
	shortestGDigitsEndParam  = "hgd"
	shortestGPointPosParam   = "hgp"
	shortestGIntLenParam     = "hgi"
	shortestGExpValParam     = "hgx"
)

// shortestGParts is what [shortestGExpr] hands a caller: the bound pieces
// of ClickHouse's own rendering, plus the finished scientific layout and
// the predicate that selects it.
//
// Everything here is a bare identifier or a small expression over bare
// identifiers, so a caller may mention any field as many times as its own
// layout needs without re-expanding the derivation behind it.
type shortestGParts struct {
	// value is the signed value under formatting.
	value chplan.Expr
	// rendered is ClickHouse's own `toString(abs(value))`, which is the
	// whole of the FIXED layout for a caller that wants no suffix.
	rendered chplan.Expr
	// pointPos is the 1-based position of `.` in rendered's mantissa, or 0
	// when there is none — how a caller decides whether a fixed rendering
	// already carries a fractional part.
	pointPos chplan.Expr
	// sign is "-" for a negative value and "" otherwise. rendered carries
	// the magnitude only, so every layout reattaches this itself.
	sign chplan.Expr
	// sci is the complete scientific layout, sign included.
	sci chplan.Expr
	// useSci reports whether Go's shortest `%g` would choose scientific
	// notation for this value.
	useSci chplan.Expr
}

// shortestGExpr binds `value` and ClickHouse's rendering of its magnitude
// as the two parameters of a one-element `arrayMap`, walks the
// decomposition once under [hqLet], and hands `body` the pieces to lay
// out. The result is `body`'s expression evaluated with all of that in
// scope.
//
// # Why every intermediate is bound
//
// hqLet renders `arrayMap(<param> -> <body>, array(<val>))[1]` —
// ClickHouse's spelling of a let-binding — so `mantRaw`, say, appears
// ONCE in the emitted SQL no matter how many of the derivations below read
// it. Left unbound, the repetition compounds multiplicatively across the
// ~6 nested layers between `value` and the top-level layout: enough to
// blow ClickHouse's `max_query_size` on a single histogram bucket cell.
// A caller that mentions this function's own result several times —
// nativeHistogramStringExpr calls it eleven times over across Count, Sum
// and every bucket's lower/upper/count — still renders eleven independent
// copies of the WHOLE chain; that duplication is inherent (each call
// receives a different `value`) and is not what this binding fixes. What
// it fixes is each of those eleven copies no longer being its own
// multiplicative blowup on top of that.
//
// `value` and `valueDigits` must be two independent expressions spelling
// the SAME quantity: chplan Expr trees are trees, so a caller whose value
// is a scalar subquery mints it twice rather than sharing one node.
func shortestGExpr(value, valueDigits chplan.Expr, body func(shortestGParts) chplan.Expr) chplan.Expr {
	call := func(fn chplan.Fn, args ...chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Fn: fn, Args: args}
	}
	// Shape constants go inline rather than through `?` placeholders:
	// they feed `concat`, where a bound parameter leaves the operand type
	// indeterminate and CH mis-dispatches to `arrayConcat` (Code 43). Same
	// reasoning as chplan.InlineString's doc comment.
	str := func(v string) chplan.Expr { return &chplan.InlineString{V: v} }
	i := func(v int64) chplan.Expr { return &chplan.LitInt{V: v} }
	f := func(v float64) chplan.Expr { return &chplan.LitFloat{V: v} }
	bin := func(op chplan.BinaryOp, l, r chplan.Expr) chplan.Expr {
		return &chplan.Binary{Op: op, Left: l, Right: r}
	}
	// `position` and `length` return UInt64, while subtracting from one
	// yields Int64. An `if` with one arm of each has no common supertype,
	// and ClickHouse resolves that pair to `Variant(Int64, UInt64)`;
	// arithmetic or `toString` over a Variant then comes back Nullable.
	// That nullability rides the whole label expression up into
	// `mapConcat`, so Attributes arrives as `Map(String, Nullable(String))`
	// and the production cursor refuses to scan it into `map[string]string`.
	// chDB coerces it away, so only the strict-scan differential sees it.
	// Landing every count on Int64 at the source keeps each `if`
	// single-typed.
	countOf := func(fn chplan.Fn, args ...chplan.Expr) chplan.Expr {
		return call(chplan.FnToInt64, call(fn, args...))
	}

	v := &chplan.BareIdent{Name: shortestGValueParam}
	u := &chplan.BareIdent{Name: shortestGDigitsParam}

	// Position of the exponent marker in CH's rendering; 0 when CH chose
	// fixed notation.
	epos := countOf(chplan.FnStringPosition, u, str("e"))
	lambdaBody := hqLet(shortestGPosParam, epos, func(pos chplan.Expr) chplan.Expr {
		// The mantissa CH rendered — the whole string in fixed notation.
		mantRaw := call(chplan.FnIf, bin(chplan.OpGt, pos, i(0)),
			call(chplan.FnSubstring, u, i(1), bin(chplan.OpSub, pos, i(1))),
			u)
		return hqLet(shortestGMantRawParam, mantRaw, func(mr chplan.Expr) chplan.Expr {
			// Mantissa digits with the decimal point removed, then with
			// leading and trailing zeros stripped: the significant digits,
			// most significant first.
			digitsAll := call(chplan.FnReplaceAll, mr, str("."), str(""))
			return hqLet(shortestGDigitsAllParam, digitsAll, func(da chplan.Expr) chplan.Expr {
				digitsLead := call(chplan.FnRegexReplaceFirst, da, str("^0+"), str(""))
				return hqLet(shortestGDigitsLeadParam, digitsLead, func(dl chplan.Expr) chplan.Expr {
					digits := call(chplan.FnRegexReplaceFirst, dl, str("0+$"), str(""))
					return hqLet(shortestGDigitsEndParam, digits, func(d chplan.Expr) chplan.Expr {
						pointPos := countOf(chplan.FnStringPosition, mr, str("."))
						return hqLet(shortestGPointPosParam, pointPos, func(pp chplan.Expr) chplan.Expr {
							// Digit count left of the decimal point (the
							// whole mantissa when there is no point).
							intLen := call(chplan.FnIf, bin(chplan.OpGt, pp, i(0)),
								bin(chplan.OpSub, pp, i(1)),
								countOf(chplan.FnLength, mr))
							return hqLet(shortestGIntLenParam, intLen, func(il chplan.Expr) chplan.Expr {
								// The decimal exponent: read straight off
								// CH's exponent when it used scientific
								// notation, else derived from where the
								// first significant digit sits relative to
								// the decimal point. FnToInt64OrZero, not
								// the throwing FnToInt64: ClickHouse's
								// vectorized `if` does not reliably skip
								// evaluating the untaken branch inside this
								// deep an arrayMap/hqLet nesting, so a
								// fixed-notation row (pos = 0, this branch
								// never SELECTED) can still have its
								// substring — the whole digit string, not
								// an exponent suffix — pushed through
								// toInt64 and abort the query on a string
								// like "2.565". The OrZero result is
								// discarded exactly when that happens
								// (pos > 0 is false), so the 0 fallback
								// never reaches the output.
								leadingZeros := bin(chplan.OpSub, countOf(chplan.FnLength, da), countOf(chplan.FnLength, dl))
								expVal := call(chplan.FnIf, bin(chplan.OpGt, pos, i(0)),
									call(chplan.FnToInt64OrZero, call(chplan.FnSubstring, u, bin(chplan.OpAdd, pos, i(1)))),
									bin(chplan.OpSub, bin(chplan.OpSub, il, leadingZeros), i(1)))
								return hqLet(shortestGExpValParam, expVal, func(ev chplan.Expr) chplan.Expr {
									// `d` or `d.ddd` — Go's normalised
									// scientific mantissa.
									mantissa := call(chplan.FnIf, bin(chplan.OpLe, countOf(chplan.FnLength, d), i(1)),
										d,
										call(chplan.FnConcat, call(chplan.FnSubstring, d, i(1), i(1)), str("."), call(chplan.FnSubstring, d, i(2))))
									expDigits := call(chplan.FnToString, call(chplan.FnAbs, ev))
									expSuffix := call(chplan.FnConcat,
										call(chplan.FnIf, bin(chplan.OpLt, ev, i(0)), str("-"), str("+")),
										call(chplan.FnIf, bin(chplan.OpLt, call(chplan.FnAbs, ev), i(goSciExpPadBelow)),
											call(chplan.FnConcat, str("0"), expDigits),
											expDigits))
									sign := call(chplan.FnIf, bin(chplan.OpLt, v, f(0)), str("-"), str(""))
									return body(shortestGParts{
										value:    v,
										rendered: u,
										pointPos: pp,
										sign:     sign,
										sci:      call(chplan.FnConcat, sign, mantissa, str("e"), expSuffix),
										useSci: bin(chplan.OpOr,
											bin(chplan.OpLt, call(chplan.FnAbs, v), f(goShortestGSciLowerBound)),
											bin(chplan.OpGe, call(chplan.FnAbs, v), f(goShortestGSciUpperBound))),
									})
								})
							})
						})
					})
				})
			})
		})
	})

	return &chplan.Subscript{
		Container: call(chplan.FnArrayMap,
			&chplan.Lambda{Params: []string{shortestGValueParam, shortestGDigitsParam}, Body: lambdaBody},
			call(chplan.FnArray, value),
			call(chplan.FnArray, call(chplan.FnToString, call(chplan.FnAbs, valueDigits)))),
		Key: i(1),
	}
}
