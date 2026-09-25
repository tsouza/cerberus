package logql

import (
	"math"
	"time"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Go-duration parsing on the ClickHouse side, faithful to reference
// Loki's semantics. Reference Loki parses duration-valued labels with
// Go's `time.ParseDuration` at two sites:
//
//   - duration label filters (`| dur > 5s`) —
//     pkg/logql/log/label_filter.go::(*DurationLabelFilter).Process
//   - `| unwrap duration(x)` / `duration_seconds(x)` —
//     pkg/logql/log/metrics_extraction.go::convertDuration
//
// and NEVER aborts a query on an unparseable value: the row gets the
// `__error__` / `__error_details__` labels and flows on (label filters
// keep the row; unwrap keeps the sample with value 0).
//
// ClickHouse's `parseTimeDelta` is not used: it throws (code 36) on the
// first value it can't parse — one malformed row would abort the whole
// query (e.g. the Logs Drilldown fields tab) — rejects Go-valid shapes
// (`µs` on 24.8, a leading sign, the bare `0`, `.5s`, `1.s`), and scales
// units in floating point, landing one ulp away from Go's
// `Duration.Seconds()` for `us` / `ns` values. Instead:
//
//   - validity is decided by a Go-shaped regex (Go's exact unit set: no
//     `d` / `w` — reference Loki calls `time.ParseDuration` directly at
//     both sites, NOT the extended `model.ParseDuration` used for query
//     range literals);
//   - the value is recomputed from the (number, unit) components with
//     Go's own integer-nanosecond arithmetic ([goDurationSeconds]),
//     using only functions that are total over strings, and gated on
//     validity so an invalid row yields 0.
//
// Durations past Go's int64 nanosecond range (~2562047h) are accepted
// by the regex gate though Go rejects them as overflowing (cerberus
// issue #3686).

// goDurationNumberRe is one Go duration "number": digits with an
// optional fraction, or a bare fraction — `time.ParseDuration` accepts
// a component when digits appear before OR after the dot (`pre || post`).
const goDurationNumberRe = `[0-9]+(\.[0-9]*)?|\.[0-9]+`

// goDurationUnitRe is Go's exact `unitMap` key set: `us` has the two
// non-ASCII aliases `µs` (U+00B5 micro sign — what Go's own
// Duration.String() emits) and `μs` (U+03BC Greek small letter mu).
const goDurationUnitRe = `ns|us|µs|μs|ms|s|m|h`

// goDurationValidRe accepts exactly the strings Go's time.ParseDuration
// accepts after the optional leading sign is stripped — one or more
// (number, unit) components. The bare-zero special case ("0") is
// handled separately by the caller (Go: `if s == "0" { return 0 }`).
// TestGoDurationRegexParity pins this against time.ParseDuration itself.
const goDurationValidRe = `^((` + goDurationNumberRe + `)(` + goDurationUnitRe + `))+$`

// goDurationMissingUnitRe classifies Go's `time: missing unit in
// duration %q` error: zero or more valid components, then a complete
// number followed by end-of-string or a second dot (Go's unit scan
// breaks on '.' and digits, so `5.5.5s` errors with "missing unit"
// after consuming the maximal number `5.5`).
const goDurationMissingUnitRe = `^((` + goDurationNumberRe + `)(` + goDurationUnitRe + `))*` +
	`((` + goDurationNumberRe + `)$|[0-9]+\.[0-9]*\.|\.[0-9]+\.)`

// goDurationUnknownUnitRe classifies Go's `time: unknown unit %q in
// duration %q` error: zero or more valid components, then a number,
// then at least one character that is neither a digit nor a dot (Go's
// unit scan consumes ALL such characters as the unit token — `1m x`
// errors with unknown unit "m x" because the regex backtracks the
// `1m` component once no number follows the space).
const goDurationUnknownUnitRe = `^((` + goDurationNumberRe + `)(` + goDurationUnitRe + `))*` +
	`(` + goDurationNumberRe + `)[^0-9.]`

// goDurationUnknownUnitExtractRe is the capture variant for the unknown
// unit token itself. Non-capturing groups everywhere except the unit
// run so CH's `extract()` returns the token Go would quote.
const goDurationUnknownUnitExtractRe = `^(?:(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:ns|us|µs|μs|ms|s|m|h))*` +
	`(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)([^0-9.]+)`

// Reference Loki's error kinds for the `__error__` label, from
// pkg/logql/log/error.go (unexported upstream constants `errLabelFilter`
// / `errSampleExtraction` — the values are part of the wire contract,
// surfaced verbatim as label values in query responses).
const (
	errLabelFilterKind      = "LabelFilterErr"
	errSampleExtractionKind = "SampleExtractionErr"
)

// durationParse bundles the CH expressions that evaluate one
// Go-duration-shaped label value. All fields reference `raw` (the label
// access expression) — CH's common-subexpression handling collapses the
// repeats at execution time.
type durationParse struct {
	// raw is the label access expression the parse reads.
	raw chplan.Expr
	// valid is a UInt8 predicate: Go's time.ParseDuration would accept
	// raw.
	valid chplan.Expr
	// seconds is the Float64 duration in seconds (reference:
	// time.Duration.Seconds()). 0 when invalid — mirroring
	// convertDuration's `return 0, err`.
	seconds chplan.Expr
	// details is the Go-shaped parse-error message (`time: invalid
	// duration "x"` / `time: missing unit in duration "5"` / `time:
	// unknown unit "x" in duration "5x"`). Only meaningful on rows
	// where valid is false. Byte-faithful to Go for ASCII inputs; Go
	// additionally hex-escapes non-ASCII bytes inside the quotes
	// (time.quote's `\xc2\xb5` form) which this expression does not
	// replicate.
	details chplan.Expr
}

// newDurationParse builds the parse expressions for one label access.
func newDurationParse(raw chplan.Expr) durationParse {
	// stripped = raw minus one leading sign character — Go consumes a
	// single optional [+-] before anything else.
	stripped := &chplan.FuncCall{
		Fn:   chplan.FnRegexReplaceAll,
		Args: []chplan.Expr{raw, &chplan.LitString{V: `^[+-]`}, &chplan.LitString{V: ``}},
	}
	isZero := &chplan.Binary{Op: chplan.OpEq, Left: stripped, Right: &chplan.LitString{V: "0"}}
	// A regex-shaped value is only valid when it also fits Go's int64
	// nanosecond range — the regex alone accepts "123456789h", which Go
	// rejects at overflow (cerberus issue #3686). The bare-zero special
	// case never overflows, so isZero skips the overflow check.
	valid := &chplan.Binary{
		Op:   chplan.OpOr,
		Left: isZero,
		Right: &chplan.Binary{
			Op: chplan.OpAnd,
			Left: &chplan.FuncCall{
				Fn:   chplan.FnRegexMatch,
				Args: []chplan.Expr{stripped, &chplan.LitString{V: goDurationValidRe}},
			},
			Right: notExpr(goDurationOverflows(raw, stripped)),
		},
	}

	// seconds: a zero magnitude sits OUTSIDE the sign multiplier so "-0",
	// "-0s" and "-0.1ns" yield +0.0 like Go, which negates an integer
	// nanosecond count (a bare `sign * 0.` would emit IEEE -0, which
	// JSON-marshals differently from reference Loki's 0). The bare "0"
	// has no (number, unit) component, so its magnitude is 0 too.
	sign := &chplan.FuncCall{
		Fn: chplan.FnIf,
		Args: []chplan.Expr{
			&chplan.FuncCall{Fn: chplan.FnStartsWith, Args: []chplan.Expr{raw, &chplan.LitString{V: "-"}}},
			&chplan.LitFloat{V: -1},
			&chplan.LitFloat{V: 1},
		},
	}
	seconds := &chplan.FuncCall{
		Fn: chplan.FnIf,
		Args: []chplan.Expr{
			&chplan.Binary{
				Op:    chplan.OpAnd,
				Left:  valid,
				Right: &chplan.Binary{Op: chplan.OpNe, Left: goDurationSeconds(stripped), Right: &chplan.LitFloat{V: 0}},
			},
			&chplan.Binary{Op: chplan.OpMul, Left: sign, Right: goDurationSeconds(stripped)},
			&chplan.LitFloat{V: 0},
		},
	}

	// details: classify in Go's scan order. Note both classification
	// regexes run against `stripped` (Go scans after the sign) while
	// the quoted duration is the RAW original — `time.ParseDuration("-5x")`
	// quotes "-5x", not "5x".
	quote := &chplan.LitString{V: `"`}
	details := &chplan.FuncCall{
		Fn: chplan.FnMultiIf,
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Fn:   chplan.FnRegexMatch,
				Args: []chplan.Expr{stripped, &chplan.LitString{V: goDurationMissingUnitRe}},
			},
			&chplan.FuncCall{
				Fn: chplan.FnConcat,
				Args: []chplan.Expr{
					&chplan.LitString{V: `time: missing unit in duration "`}, raw, quote,
				},
			},
			&chplan.FuncCall{
				Fn:   chplan.FnRegexMatch,
				Args: []chplan.Expr{stripped, &chplan.LitString{V: goDurationUnknownUnitRe}},
			},
			&chplan.FuncCall{
				Fn: chplan.FnConcat,
				Args: []chplan.Expr{
					&chplan.LitString{V: `time: unknown unit "`},
					&chplan.FuncCall{
						Fn:   chplan.FnRegexExtractFirst,
						Args: []chplan.Expr{stripped, &chplan.LitString{V: goDurationUnknownUnitExtractRe}},
					},
					&chplan.LitString{V: `" in duration "`}, raw, quote,
				},
			},
			&chplan.FuncCall{
				Fn: chplan.FnConcat,
				Args: []chplan.Expr{
					&chplan.LitString{V: `time: invalid duration "`}, raw, quote,
				},
			},
		},
	}

	return durationParse{raw: raw, valid: valid, seconds: seconds, details: details}
}

// labelFilterMark is one potential `__error__` stamping event produced
// by lowering a label-filter tree. `cond` is fully gated: it already
// folds in the reference engine's short-circuit reachability (an OR's
// right side only marks when the left side neither erred nor matched —
// see labelFiltererLower).
type labelFilterMark struct {
	cond    chplan.Expr
	kind    string
	details chplan.Expr
}

// gateMark returns a copy of m whose condition is AND-ed with gate.
func gateMark(m labelFilterMark, gate chplan.Expr) labelFilterMark {
	return labelFilterMark{
		cond:    &chplan.Binary{Op: chplan.OpAnd, Left: gate, Right: m.cond},
		kind:    m.kind,
		details: m.details,
	}
}

// notExpr wraps e in CH's `not(...)`.
func notExpr(e chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnNot, Args: []chplan.Expr{e}}
}

// wrapLabelsWithMarks folds a mark list onto labelsExpr:
//
//	mapConcat(<labels>, multiIf(
//	    not(mapContains(<labels>, '__error__')) AND <cond_1>, map('__error__', kind_1, '__error_details__', details_1),
//	    …,
//	    map()))
//
// Reference semantics being mirrored (pkg/logql/log/label_filter.go):
//
//   - "Don't overwrite what might be a more useful error" — every
//     branch is gated on the PREVIOUS stage's labels not already
//     carrying `__error__`. CH's map lookup also takes the FIRST
//     occurrence on duplicate keys, but the gate keeps the map
//     duplicate-free so the Go-side map conversion (last-wins) can't
//     disagree with the CH-side lookup (first-wins).
//   - mark order inside one filter tree follows the reference engine's
//     left-to-right Process order — multiIf picks the first matching
//     branch.
//
// Returns labelsExpr unchanged when marks is empty.
func wrapLabelsWithMarks(labelsExpr chplan.Expr, marks []labelFilterMark) chplan.Expr {
	if len(marks) == 0 {
		return labelsExpr
	}
	noPriorError := notExpr(&chplan.FuncCall{
		Fn:   chplan.FnMapContainsKey,
		Args: []chplan.Expr{labelsExpr, &chplan.LitString{V: syntax.ErrorLabel}},
	})
	args := make([]chplan.Expr, 0, len(marks)*2+1)
	for _, m := range marks {
		args = append(
			args,
			&chplan.Binary{Op: chplan.OpAnd, Left: noPriorError, Right: m.cond},
			&chplan.FuncCall{Fn: chplan.FnMap, Args: []chplan.Expr{
				&chplan.LitString{V: syntax.ErrorLabel},
				&chplan.LitString{V: m.kind},
				&chplan.LitString{V: syntax.ErrorDetailsLabel},
				m.details,
			}},
		)
	}
	args = append(args, &chplan.FuncCall{Fn: chplan.FnMap})
	branch := chplan.Expr(&chplan.FuncCall{Fn: chplan.FnMultiIf, Args: args})
	if len(marks) == 1 {
		// multiIf demands ≥2 condition/branch pairs only in spirit; CH
		// accepts the 3-arg form but `if` is the canonical spelling.
		branch = &chplan.FuncCall{Fn: chplan.FnIf, Args: args}
	}
	return &chplan.FuncCall{
		Fn:   chplan.FnMapMerge,
		Args: []chplan.Expr{labelsExpr, branch},
	}
}

// goDurationComponentRe captures one (integer, fraction, unit) component
// of an already-validated Go duration. Either number half may be empty
// (`.5s`, `1.s`); an empty half contributes zero, as it does in Go.
const goDurationComponentRe = `([0-9]*)(?:\.([0-9]*))?(` + goDurationUnitRe + `)`

// goFractionSaturation is (1<<63-1)/10, the accumulator bound past
// which Go's leadingFraction stops reading fraction digits.
const goFractionSaturation = (1<<63 - 1) / 10

// goFractionSaturationLastDigit is the largest digit leadingFraction
// still accepts onto an accumulator equal to goFractionSaturation:
// 10*goFractionSaturation + 8 == 1<<63, and one more is past it.
const goFractionSaturationLastDigit = 8

// decimalRadix is the base leadingFraction shifts its accumulator and
// scale by per digit.
const decimalRadix = 10

// maxSafeIntegerDigits is the digit-length of 1<<63
// (9223372036854775808, 19 digits) — see [goDurationOverflows].
const maxSafeIntegerDigits = 19

// Lambda parameter names for the fraction fold. They are emitted
// verbatim into the SQL, so they are named to be unmistakable in a
// golden and impossible to confuse with a column.
const (
	durationFracAccParam   = "__dur_frac_acc"
	durationFracDigitParam = "__dur_frac_digit"
)

// nanosPerSecond is time.Second in nanoseconds — the divisor
// time.Duration.Seconds() splits a duration by.
const nanosPerSecond = int64(time.Second)

// goDurationUnitNames / goDurationUnitNanos are Go's time.unitMap: each
// unit spelling and its length in nanoseconds.
var (
	goDurationUnitNames = []string{"ns", "us", "µs", "μs", "ms", "s", "m", "h"}
	goDurationUnitNanos = []int64{
		int64(time.Nanosecond), int64(time.Microsecond), int64(time.Microsecond), int64(time.Microsecond),
		int64(time.Millisecond), int64(time.Second), int64(time.Minute), int64(time.Hour),
	}
)

// goDurationSeconds is time.ParseDuration(stripped).Seconds() for a
// stripped value the validity regex accepts, computed the way Go
// computes it rather than through ClickHouse's parseTimeDelta, whose
// floating-point unit scaling lands one ulp away from Go for `us` and
// `ns` values (`5us` → 4.9999999999999996e-06, Go 5e-06).
//
// Per component, Go's ParseDuration accumulates an integer nanosecond
// count: `whole * unit + uint64(float64(frac) * (float64(unit) / scale))`
// with (frac, scale) as leadingFraction returns them. The total d is then converted by
// Duration.Seconds(): `float64(d / 1e9) + float64(d % 1e9) / 1e9`. The
// expression mirrors both steps operation for operation:
//
//	sec  = intDiv(d, 1e9)
//	secs = toFloat64(sec) + toFloat64(d - sec * 1e9) / 1e9
//	d    = arraySum(arrayMap((i, f, u) -> <component nanos>, groups…))
//
// The fraction's (frac, scale) pair is [goLeadingFraction], a fold that
// replays Go's leadingFraction digit by digit, so a fraction of any
// length reads exactly the digits Go reads and scales by the same
// float64 product of tens.
//
// Every function on this path is total over strings, so the expression
// never aborts a query.
//
// unitNanosExpr maps a captured unit token (bound to the given ident
// expression) to its length in nanoseconds via Go's time.unitMap
// ([goDurationUnitNames] / [goDurationUnitNanos]). Shared by
// [goDurationSeconds] and [goDurationOverflows] so both read the same
// unit table.
func unitNanosExpr(u chplan.Expr) chplan.Expr {
	unitNames := make([]chplan.Expr, len(goDurationUnitNames))
	for i, name := range goDurationUnitNames {
		unitNames[i] = &chplan.LitString{V: name}
	}
	unitNanosLits := make([]chplan.Expr, len(goDurationUnitNanos))
	for i, n := range goDurationUnitNanos {
		unitNanosLits[i] = &chplan.LitInt{V: n}
	}
	return &chplan.FuncCall{
		Fn: chplan.FnTransform,
		Args: []chplan.Expr{
			u,
			&chplan.FuncCall{Fn: chplan.FnArray, Args: unitNames},
			&chplan.FuncCall{Fn: chplan.FnArray, Args: unitNanosLits},
			&chplan.LitInt{V: 0},
		},
	}
}

// pow2_63 is `1<<63` as a UInt64 expression — one past
// [math.MaxInt64], the magnitude bound Go's ParseDuration checks
// against. It cannot be a [chplan.LitInt] (a signed field, so the
// literal itself would overflow); built at runtime via bitShiftLeft
// instead. A fresh instance every call — the emitter does not require
// Expr sharing, and CH's own common-subexpression handling collapses
// the repeats.
func pow2_63() chplan.Expr {
	return &chplan.FuncCall{
		Fn: chplan.FnBitShiftLeft,
		Args: []chplan.Expr{
			&chplan.FuncCall{Fn: chplan.FnToUInt64, Args: []chplan.Expr{&chplan.LitInt{V: 1}}},
			&chplan.LitInt{V: 63},
		},
	}
}

// goDurationOverflows is a UInt8 predicate: Go's time.ParseDuration
// would reject stripped with the overflow error (cerberus issue
// #3686), computed by replaying its uint64 accumulator loop
// (time/format.go's ParseDuration) component by component:
//
//   - a component's integer part times its unit is past 1<<63
//     (`v > 1<<63/unit`, checked via integer division before the
//     multiply, exactly as Go checks it);
//   - adding the fractional nanoseconds pushes that component past
//     1<<63 (`v > 1<<63` after the fraction add — Go reaches this
//     line only when the component has a fraction, but the prior
//     bound already keeps a fraction-less v at or under 1<<63, so
//     checking it unconditionally agrees with Go either way);
//   - the running sum over all components so far goes past 1<<63
//     (`d > 1<<63` inside Go's loop).
//
// After the fold, Go applies one more check outside the loop: a
// non-negative total must not exceed 1<<63-1 (MaxInt64) — a negative
// total may legitimately reach -1<<63 (MinInt64), the one case where
// the running sum hits 1<<63 exactly without erroring.
//
// CH's UInt64 arithmetic wraps mod 2^64 exactly as Go's uint64 does,
// so mirroring Go's explicit bound checks — each evaluated before the
// value it guards could wrap — reproduces its overflow behaviour
// bit-for-bit rather than merely approximating it.
func goDurationOverflows(raw, stripped chplan.Expr) chplan.Expr {
	groups := &chplan.FuncCall{
		Fn:   chplan.FnRegexExtractAllGroupsHorizontal,
		Args: []chplan.Expr{stripped, &chplan.LitString{V: goDurationComponentRe}},
	}
	group := func(n int64) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnArrayElement, Args: []chplan.Expr{groups, &chplan.LitInt{V: n}}}
	}

	const (
		accParam    = "__dur_ovf_acc"
		digitIParam = "__dur_ovf_i"
		digitFParam = "__dur_ovf_f"
		digitUParam = "__dur_ovf_u"
	)
	acc := &chplan.BareIdent{Name: accParam}
	accD := &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{acc, &chplan.LitInt{V: 1}}}
	accOvf := &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{acc, &chplan.LitInt{V: 2}}}

	whole := &chplan.FuncCall{Fn: chplan.FnToUInt64OrZero, Args: []chplan.Expr{&chplan.BareIdent{Name: digitIParam}}}
	unit := unitNanosExpr(&chplan.BareIdent{Name: digitUParam})
	// Go's own leadingInt (time/format.go) overflows — and errors —
	// before ParseDuration ever reaches the unit multiply, once the
	// integer-part digit run itself exceeds 1<<63. `toUInt64OrZero`
	// has no such check: past UInt64's own range (20+ digits) it
	// silently returns 0, which would hide the overflow from bound1
	// below (0 times anything is never past the bound). 1<<63 is a
	// 19-digit number, so any 20-plus-digit run is unconditionally
	// past it regardless of unit — maxSafeIntegerDigits catches that
	// case directly instead of trusting the (possibly zeroed) parse.
	tooManyDigits := &chplan.Binary{
		Op:    chplan.OpGt,
		Left:  &chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{&chplan.BareIdent{Name: digitIParam}}},
		Right: &chplan.LitInt{V: maxSafeIntegerDigits},
	}
	bound1 := &chplan.Binary{
		Op:   chplan.OpOr,
		Left: tooManyDigits,
		Right: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  whole,
			Right: &chplan.FuncCall{Fn: chplan.FnIntDiv, Args: []chplan.Expr{pow2_63(), unit}},
		},
	}
	v := &chplan.Binary{Op: chplan.OpMul, Left: whole, Right: unit}

	fraction := goLeadingFraction(&chplan.BareIdent{Name: digitFParam})
	frac := &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{fraction, &chplan.LitInt{V: 1}}}
	scale := &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{fraction, &chplan.LitInt{V: 2}}}
	fracNanos := &chplan.FuncCall{
		Fn: chplan.FnToUInt64,
		Args: []chplan.Expr{&chplan.Binary{
			Op:   chplan.OpMul,
			Left: &chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{frac}},
			Right: &chplan.Binary{
				Op:    chplan.OpDiv,
				Left:  &chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{unit}},
				Right: scale,
			},
		}},
	}
	v2 := &chplan.Binary{Op: chplan.OpAdd, Left: v, Right: fracNanos}
	bound2 := &chplan.Binary{Op: chplan.OpGt, Left: v2, Right: pow2_63()}

	newD := &chplan.Binary{Op: chplan.OpAdd, Left: accD, Right: v2}
	bound3 := &chplan.Binary{Op: chplan.OpGt, Left: newD, Right: pow2_63()}

	newOvf := &chplan.Binary{
		Op:   chplan.OpOr,
		Left: accOvf,
		Right: &chplan.Binary{
			Op: chplan.OpOr, Left: bound1,
			Right: &chplan.Binary{Op: chplan.OpOr, Left: bound2, Right: bound3},
		},
	}
	step := &chplan.FuncCall{Fn: chplan.FnTuple, Args: []chplan.Expr{
		&chplan.FuncCall{Fn: chplan.FnToUInt64, Args: []chplan.Expr{newD}},
		&chplan.FuncCall{Fn: chplan.FnToUInt64, Args: []chplan.Expr{newOvf}},
	}}

	folded := &chplan.FuncCall{
		Fn: chplan.FnArrayFold,
		Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{accParam, digitIParam, digitFParam, digitUParam}, Body: step},
			group(1), group(2), group(3),
			&chplan.FuncCall{Fn: chplan.FnTuple, Args: []chplan.Expr{
				&chplan.FuncCall{Fn: chplan.FnToUInt64, Args: []chplan.Expr{&chplan.LitInt{V: 0}}},
				&chplan.FuncCall{Fn: chplan.FnToUInt64, Args: []chplan.Expr{&chplan.LitInt{V: 0}}},
			}},
		},
	}
	foldedD := &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{folded, &chplan.LitInt{V: 1}}}
	foldedOvf := &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{folded, &chplan.LitInt{V: 2}}}

	isNeg := &chplan.FuncCall{Fn: chplan.FnStartsWith, Args: []chplan.Expr{raw, &chplan.LitString{V: "-"}}}
	finalBound := &chplan.Binary{
		Op:   chplan.OpAnd,
		Left: notExpr(isNeg),
		Right: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  foldedD,
			Right: &chplan.FuncCall{Fn: chplan.FnToUInt64, Args: []chplan.Expr{&chplan.LitInt{V: math.MaxInt64}}},
		},
	}
	return &chplan.Binary{
		Op:    chplan.OpOr,
		Left:  &chplan.Binary{Op: chplan.OpEq, Left: foldedOvf, Right: &chplan.LitInt{V: 1}},
		Right: finalBound,
	}
}

func goDurationSeconds(stripped chplan.Expr) chplan.Expr {
	groups := &chplan.FuncCall{
		Fn:   chplan.FnRegexExtractAllGroupsHorizontal,
		Args: []chplan.Expr{stripped, &chplan.LitString{V: goDurationComponentRe}},
	}
	group := func(n int64) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnArrayElement, Args: []chplan.Expr{groups, &chplan.LitInt{V: n}}}
	}

	unitNanos := unitNanosExpr(&chplan.BareIdent{Name: "u"})
	whole := &chplan.Binary{
		Op:    chplan.OpMul,
		Left:  &chplan.FuncCall{Fn: chplan.FnToUInt64OrZero, Args: []chplan.Expr{&chplan.BareIdent{Name: "i"}}},
		Right: unitNanos,
	}
	fraction := goLeadingFraction(&chplan.BareIdent{Name: "f"})
	frac := &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{fraction, &chplan.LitInt{V: 1}}}
	scale := &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{fraction, &chplan.LitInt{V: 2}}}
	fracNanos := &chplan.FuncCall{
		Fn: chplan.FnToUInt64,
		Args: []chplan.Expr{&chplan.Binary{
			Op: chplan.OpMul,
			Left: &chplan.FuncCall{
				Fn:   chplan.FnToFloat64,
				Args: []chplan.Expr{frac},
			},
			Right: &chplan.Binary{
				Op:    chplan.OpDiv,
				Left:  &chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{unitNanos}},
				Right: scale,
			},
		}},
	}
	nanos := &chplan.FuncCall{
		Fn: chplan.FnArraySum,
		Args: []chplan.Expr{&chplan.FuncCall{
			Fn: chplan.FnArrayMap,
			Args: []chplan.Expr{
				&chplan.Lambda{
					Params: []string{"i", "f", "u"},
					Body:   &chplan.Binary{Op: chplan.OpAdd, Left: whole, Right: fracNanos},
				},
				group(1), group(2), group(3),
			},
		}},
	}

	sec := &chplan.FuncCall{Fn: chplan.FnIntDiv, Args: []chplan.Expr{nanos, &chplan.LitInt{V: nanosPerSecond}}}
	nsec := &chplan.Binary{
		Op:    chplan.OpSub,
		Left:  nanos,
		Right: &chplan.Binary{Op: chplan.OpMul, Left: sec, Right: &chplan.LitInt{V: nanosPerSecond}},
	}
	return &chplan.Binary{
		Op:   chplan.OpAdd,
		Left: &chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{sec}},
		Right: &chplan.Binary{
			Op:    chplan.OpDiv,
			Left:  &chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{nsec}},
			Right: &chplan.LitFloat{V: float64(nanosPerSecond)},
		},
	}
}

// goLeadingFraction replays Go's time.leadingFraction over the fraction
// digits of one duration component, returning the Tuple(UInt64, Float64,
// UInt64) of its (x, scale, overflow) state:
//
//	for each digit c:
//	    if overflow                         { continue }
//	    if x > (1<<63-1)/10                 { overflow = true; continue }
//	    y := x*10 + c
//	    if y > 1<<63                        { overflow = true; continue }
//	    x = y; scale *= 10
//
// With x <= (1<<63-1)/10, y exceeds 1<<63 only when x equals that bound
// and c exceeds goFractionSaturationLastDigit, so the fold tests that
// instead of forming a constant past the int64 range. scale is the same
// running float64 product Go keeps, not a correctly rounded 10^n; the two
// part ways from 10^25 on.
func goLeadingFraction(digits chplan.Expr) chplan.Expr {
	acc := &chplan.BareIdent{Name: durationFracAccParam}
	field := func(n int64) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{acc, &chplan.LitInt{V: n}}}
	}
	x, scale, overflow := field(1), field(2), field(3)
	digit := &chplan.FuncCall{Fn: chplan.FnToUInt64OrZero, Args: []chplan.Expr{&chplan.BareIdent{Name: durationFracDigitParam}}}
	state := func(x, scale chplan.Expr, overflow int64) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnTuple, Args: []chplan.Expr{
			&chplan.FuncCall{Fn: chplan.FnToUInt64, Args: []chplan.Expr{x}},
			&chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{scale}},
			&chplan.FuncCall{Fn: chplan.FnToUInt64, Args: []chplan.Expr{&chplan.LitInt{V: overflow}}},
		}}
	}
	saturates := &chplan.Binary{
		Op:   chplan.OpOr,
		Left: &chplan.Binary{Op: chplan.OpGt, Left: x, Right: &chplan.LitInt{V: goFractionSaturation}},
		Right: &chplan.Binary{
			Op:   chplan.OpAnd,
			Left: &chplan.Binary{Op: chplan.OpEq, Left: x, Right: &chplan.LitInt{V: goFractionSaturation}},
			Right: &chplan.Binary{
				Op: chplan.OpGt, Left: digit, Right: &chplan.LitInt{V: goFractionSaturationLastDigit},
			},
		},
	}
	step := &chplan.FuncCall{
		Fn: chplan.FnMultiIf,
		Args: []chplan.Expr{
			&chplan.Binary{Op: chplan.OpNe, Left: overflow, Right: &chplan.LitInt{V: 0}},
			state(x, scale, 1),
			saturates,
			state(x, scale, 1),
			state(
				&chplan.Binary{
					Op:    chplan.OpAdd,
					Left:  &chplan.Binary{Op: chplan.OpMul, Left: x, Right: &chplan.LitInt{V: decimalRadix}},
					Right: digit,
				},
				&chplan.Binary{Op: chplan.OpMul, Left: scale, Right: &chplan.LitFloat{V: decimalRadix}},
				0,
			),
		},
	}
	return &chplan.FuncCall{
		Fn: chplan.FnArrayFold,
		Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{durationFracAccParam, durationFracDigitParam}, Body: step},
			&chplan.FuncCall{Fn: chplan.FnRegexExtractAll, Args: []chplan.Expr{digits, &chplan.LitString{V: "[0-9]"}}},
			state(&chplan.LitInt{V: 0}, &chplan.LitInt{V: 1}, 0),
		},
	}
}
