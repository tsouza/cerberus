package logql

import (
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
	valid := &chplan.Binary{
		Op:   chplan.OpOr,
		Left: isZero,
		Right: &chplan.FuncCall{
			Fn:   chplan.FnRegexMatch,
			Args: []chplan.Expr{stripped, &chplan.LitString{V: goDurationValidRe}},
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

// goDurationMaxFractionDigits is how many fraction digits the lowering
// reads per component: the longest digit run that always fits Go's
// leadingFraction accumulator (a uint64 that stops growing past
// (1<<63-1)/10) and whose power-of-ten scale intExp10 returns exactly.
const goDurationMaxFractionDigits = 18

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
// with scale = 10^len(frac). The total d is then converted by
// Duration.Seconds(): `float64(d / 1e9) + float64(d % 1e9) / 1e9`. The
// expression mirrors both steps operation for operation:
//
//	sec  = intDiv(d, 1e9)
//	secs = toFloat64(sec) + toFloat64(d - sec * 1e9) / 1e9
//	d    = arraySum(arrayMap((i, f, u) -> <component nanos>, groups…))
//
// Every function on this path is total over strings, so the expression
// never aborts a query. Fraction digits past
// goDurationMaxFractionDigits are ignored; Go's own accumulator reads
// at most one more before it saturates.
func goDurationSeconds(stripped chplan.Expr) chplan.Expr {
	groups := &chplan.FuncCall{
		Fn:   chplan.FnRegexExtractAllGroupsHorizontal,
		Args: []chplan.Expr{stripped, &chplan.LitString{V: goDurationComponentRe}},
	}
	group := func(n int64) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnArrayElement, Args: []chplan.Expr{groups, &chplan.LitInt{V: n}}}
	}

	unitNames := make([]chplan.Expr, len(goDurationUnitNames))
	for i, name := range goDurationUnitNames {
		unitNames[i] = &chplan.LitString{V: name}
	}
	unitNanosLits := make([]chplan.Expr, len(goDurationUnitNanos))
	for i, n := range goDurationUnitNanos {
		unitNanosLits[i] = &chplan.LitInt{V: n}
	}
	unitNanos := &chplan.FuncCall{
		Fn: chplan.FnTransform,
		Args: []chplan.Expr{
			&chplan.BareIdent{Name: "u"},
			&chplan.FuncCall{Fn: chplan.FnArray, Args: unitNames},
			&chplan.FuncCall{Fn: chplan.FnArray, Args: unitNanosLits},
			&chplan.LitInt{V: 0},
		},
	}
	whole := &chplan.Binary{
		Op:    chplan.OpMul,
		Left:  &chplan.FuncCall{Fn: chplan.FnToUInt64OrZero, Args: []chplan.Expr{&chplan.BareIdent{Name: "i"}}},
		Right: unitNanos,
	}
	frac := &chplan.FuncCall{
		Fn: chplan.FnSubstring,
		Args: []chplan.Expr{
			&chplan.BareIdent{Name: "f"}, &chplan.LitInt{V: 1}, &chplan.LitInt{V: goDurationMaxFractionDigits},
		},
	}
	scale := &chplan.FuncCall{
		Fn: chplan.FnToFloat64,
		Args: []chplan.Expr{&chplan.FuncCall{
			Fn:   chplan.FnIntExp10,
			Args: []chplan.Expr{&chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{frac}}},
		}},
	}
	fracNanos := &chplan.FuncCall{
		Fn: chplan.FnToUInt64,
		Args: []chplan.Expr{&chplan.Binary{
			Op: chplan.OpMul,
			Left: &chplan.FuncCall{
				Fn:   chplan.FnToFloat64,
				Args: []chplan.Expr{&chplan.FuncCall{Fn: chplan.FnToUInt64OrZero, Args: []chplan.Expr{frac}}},
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
