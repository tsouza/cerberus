package logql

import (
	"github.com/tsouza/cerberus/internal/chplan"
)

// Number / bytes label-filter parsing on the ClickHouse side, faithful
// to reference Loki's per-row error semantics. This mirrors the
// duration path in duration.go: reference Loki parses a number- or
// bytes-valued label and NEVER aborts a query on an unparseable value —
// the row gets the `__error__="LabelFilterErr"` / `__error_details__`
// labels and flows on (the predicate evaluates true so the row is
// KEPT). The two sites:
//
//   - numeric label filters (`| status_code >= 400`) —
//     pkg/logql/log/label_filter.go::(*NumericLabelFilter).Process,
//     which calls `strconv.ParseFloat(v, 64)`.
//   - bytes label filters (`| size > 5MB`) —
//     pkg/logql/log/label_filter.go::(*BytesLabelFilter).Process,
//     which calls `humanize.ParseBytes(v)`.
//
// Both Process methods follow the same three-way contract the duration
// filter does:
//
//   - label absent → the row is DROPPED (predicate false), no error.
//   - label present but the conversion rejects the value → the row is
//     KEPT (predicate true) and `__error__="LabelFilterErr"` +
//     `__error_details__` are stamped (only if no prior error — "Don't
//     overwrite what might be a more useful error").
//   - otherwise → compare the parsed value against the literal.
//
// The Go-side error string is reproduced byte-faithfully for the common
// (ASCII) cases so a `| __error__ = ""` post-filter and the compat
// differential harness both agree with reference Loki.

// numericValidRe is the CH-side `match()` regex deciding whether
// `strconv.ParseFloat(v, 64)` would accept the value. Go's ParseFloat
// accepts an optional sign, an integer or fractional decimal mantissa
// (digits before OR after the dot), and an optional `e`/`E` exponent —
// plus the `inf` / `infinity` / `nan` specials (case-insensitive). The
// underscore-digit-separator and hex-float forms Go also accepts are a
// documented narrow divergence (see numericParse).
const numericValidRe = `^[+-]?(([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+)?|[iI][nN][fF]([iI][nN][iI][tT][yY])?|[nN][aA][nN])$`

// numericParse bundles the CH expressions for one Float64-shaped label
// value. valid is the ParseFloat-accepts predicate; details is the
// Go-shaped `*strconv.NumError` message. All reference `raw`.
//
// Narrow divergences from Go's strconv.ParseFloat (documented, not
// fixed — the realistic label corpus never hits them):
//
//   - Go 1.13+ accepts underscore digit separators (`1_000`) and hex
//     floats (`0x1p-2`); this regex rejects both, so cerberus would
//     stamp an error where reference Loki parses the value.
//   - Go reports `value out of range` (not `invalid syntax`) for a
//     magnitude above ~1.8e308; this classifier always emits the
//     invalid-syntax message. CH's parser would round such a literal to
//     `inf` rather than erroring, so the divergence is parse-side too.
type numericParse struct {
	raw     chplan.Expr
	valid   chplan.Expr
	value   chplan.Expr
	details chplan.Expr
}

// newNumericParse builds the parse expressions for one label access.
func newNumericParse(raw chplan.Expr) numericParse {
	valid := &chplan.FuncCall{
		Fn:   chplan.FnRegexMatch,
		Args: []chplan.Expr{raw, &chplan.LitString{V: numericValidRe}},
	}
	// value mirrors strconv.ParseFloat's result on the accepted set:
	// toFloat64OrZero coincides with ParseFloat on every string the
	// regex admits (it is only ever read under `valid`). The bare
	// `inf` / `nan` specials parse identically on both sides.
	value := &chplan.FuncCall{
		Fn:   chplan.FnToFloat64OrZero,
		Args: []chplan.Expr{raw},
	}
	// details: strconv.NumError.Error() — `strconv.ParseFloat: parsing
	// "<v>": invalid syntax`. The quoted token is the RAW value.
	details := &chplan.FuncCall{
		Fn: chplan.FnConcat,
		Args: []chplan.Expr{
			&chplan.LitString{V: `strconv.ParseFloat: parsing "`},
			raw,
			&chplan.LitString{V: `": invalid syntax`},
		},
	}
	return numericParse{raw: raw, valid: valid, value: value, details: details}
}

// bytesUnitRe is the lowercased key set of humanize's bytesSizeTable
// (github.com/dustin/go-humanize) — the units `humanize.ParseBytes`
// accepts after lowercasing + trimming the suffix. The empty unit (bare
// number → bytes) is handled by the trailing `?` on the whole group, so
// `^…?$` accepts an empty remainder. Longest alternatives first so the
// regex engine prefers `kib` over `ki` over `k`.
const bytesUnitRe = `^(kib|mib|gib|tib|pib|eib|ki|mi|gi|ti|pi|ei|kb|mb|gb|tb|pb|eb|b|k|m|g|t|p|e)?$`

// bytesNumberRe is the leading-number run humanize peels off the front
// of the string: a maximal run of digits, dots and commas. ParseBytes
// strips commas before `strconv.ParseFloat`, so validity is decided on
// the comma-stripped form (see newBytesParse).
const bytesNumberRe = `^[0-9.,]*`

// bytesParse bundles the CH expressions for one bytes-shaped label
// value, faithful to humanize.ParseBytes's two-stage parse (peel the
// leading `[0-9.,]*` number run, ParseFloat the comma-stripped number,
// then look the lowercased+trimmed remainder up in the unit table).
type bytesParse struct {
	raw     chplan.Expr
	valid   chplan.Expr
	value   chplan.Expr
	details chplan.Expr
}

// Why the byte count is computed arithmetically rather than handed to
// ClickHouse's own `parseReadableSize`.
//
// `parseReadableSize` is neither a superset nor a subset of
// humanize.ParseBytes, and both directions were wrong answers:
//
//   - It THROWS (`Code: 6 … Unknown readable size unit`) on four whole
//     shapes humanize accepts — a bare number with no unit at all (`5`,
//     an ordinary logfmt `size=1024`), the single-letter units (`5k`),
//     the binary prefixes without the trailing `b` (`5 ki`), and any
//     value carrying humanize's comma separators (`5,000 kb`). A throw
//     is not a per-row error: it aborts the WHOLE query with a 502,
//     where reference Loki keeps the row and stamps `__error__`. The
//     regex gate does not save it — those values are VALID per
//     humanize, so the gate lets them through to the call. Measured
//     against a real engine; see TestBytesParseMatchesHumanize.
//   - Where it does parse, it ROUNDS to nearest (`1.5 B` → 2) where
//     humanize truncates (`uint64(1.5)` → 1), so every fractional
//     value with a non-integral byte count answered off by one.
//   - It throws again (`Code: 36 … Result is too big`) on the overflow
//     humanize reports as the ordinary parse error `too large: <s>`.
//
// The arithmetic below is humanize's own formula — `f *= float64(m)`,
// reject at `f >= math.MaxUint64`, then `uint64(f)` — expressed in
// typed Frags. It cannot throw for any input, and it agrees with
// humanize on every unit spelling and every magnitude.

// The two multiplier bases of humanize's bytesSizeTable: a unit spelled
// with an `i` (`ki`, `kib`, `mi`, `mib`, …) is a power of 1024, every
// other spelling a power of 1000. Both are exactly representable in
// Float64 up to the sixth power (2^60 and 1e18), so the multiplication
// carries no rounding the reference does not also carry.
const (
	bytesDecimalUnitBase = 1000
	bytesBinaryUnitBase  = 1024
)

// bytesUnitPrefixLetters is humanize's SI prefix set in ascending
// magnitude order. A unit's exponent is its 1-based index here; the
// empty unit and a bare `b` carry no prefix and so take exponent 0.
const bytesUnitPrefixLetters = "kmgtpe"

// bytesTooLargeThreshold is humanize's overflow bound: it rejects with
// `too large: <s>` once the scaled value reaches math.MaxUint64.
// Written as the Float64 that Go's `f >= math.MaxUint64` comparison
// actually performs — the untyped constant converts to 2^64 exactly.
const bytesTooLargeThreshold = 18446744073709551616.0

// newBytesParse builds the parse expressions for one label access,
// replicating humanize.ParseBytes in CH function calls.
func newBytesParse(raw chplan.Expr) bytesParse {
	// number = the leading [0-9.,] run; numStripped = that run with
	// commas removed (humanize's `strings.Replace(num, ",", "", -1)`).
	number := &chplan.FuncCall{
		Fn:   chplan.FnRegexExtractFirst,
		Args: []chplan.Expr{raw, &chplan.LitString{V: bytesNumberRe}},
	}
	numStripped := &chplan.FuncCall{
		Fn:   chplan.FnReplaceAll,
		Args: []chplan.Expr{number, &chplan.LitString{V: ","}, &chplan.LitString{V: ""}},
	}
	// rest = the suffix after the number run, lowercased and
	// whitespace-trimmed — humanize's `strings.ToLower(strings.TrimSpace(...))`.
	rest := &chplan.FuncCall{
		Fn: chplan.FnRegexReplaceAll,
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Fn: chplan.FnLower,
				Args: []chplan.Expr{
					&chplan.FuncCall{
						Fn:   chplan.FnRegexReplaceAll,
						Args: []chplan.Expr{raw, &chplan.LitString{V: bytesNumberRe}, &chplan.LitString{V: ""}},
					},
				},
			},
			&chplan.LitString{V: `^\s+|\s+$`},
			&chplan.LitString{V: ""},
		},
	}
	numberValid := &chplan.FuncCall{
		Fn: chplan.FnIsNotNull,
		Args: []chplan.Expr{
			&chplan.FuncCall{Fn: chplan.FnToFloat64OrNull, Args: []chplan.Expr{numStripped}},
		},
	}
	unitValid := &chplan.FuncCall{
		Fn:   chplan.FnRegexMatch,
		Args: []chplan.Expr{rest, &chplan.LitString{V: bytesUnitRe}},
	}

	scaled := bytesScaledExpr(numStripped, rest)
	inRange := &chplan.Binary{
		Op:    chplan.OpLt,
		Left:  scaled,
		Right: &chplan.LitFloat{V: bytesTooLargeThreshold},
	}
	valid := &chplan.Binary{
		Op:   chplan.OpAnd,
		Left: &chplan.Binary{Op: chplan.OpAnd, Left: numberValid, Right: unitValid},
		// The overflow bound is humanize's THIRD rejection, and it is
		// reached only once the number and the unit have both been
		// accepted — mirroring the order of ParseBytes's own returns.
		Right: inRange,
	}
	// value: humanize's `uint64(f)`, i.e. truncation toward zero. The
	// scaled value is never negative (bytesNumberRe admits no sign), so
	// floor and truncate coincide. Only ever read under `valid`.
	value := &chplan.FuncCall{Fn: chplan.FnFloor, Args: []chplan.Expr{scaled}}
	// details: classify in humanize's scan order — number, then unit,
	// then overflow.
	details := &chplan.FuncCall{
		Fn: chplan.FnMultiIf,
		Args: []chplan.Expr{
			notExpr(numberValid),
			&chplan.FuncCall{
				Fn: chplan.FnConcat,
				Args: []chplan.Expr{
					&chplan.LitString{V: `strconv.ParseFloat: parsing "`},
					numStripped,
					&chplan.LitString{V: `": invalid syntax`},
				},
			},
			notExpr(unitValid),
			&chplan.FuncCall{
				Fn:   chplan.FnConcat,
				Args: []chplan.Expr{&chplan.LitString{V: `unhandled size name: `}, rest},
			},
			&chplan.FuncCall{
				Fn:   chplan.FnConcat,
				Args: []chplan.Expr{&chplan.LitString{V: `too large: `}, raw},
			},
		},
	}
	return bytesParse{raw: raw, valid: valid, value: value, details: details}
}

// bytesScaledExpr is humanize's `f *= float64(m)`: the comma-stripped
// number multiplied by the multiplier its unit names.
//
// The multiplier is derived from the unit's SHAPE rather than from a
// 26-entry lookup, because humanize's table is itself generated from
// that shape: the first letter picks the exponent
// ([bytesUnitPrefixLetters]) and a following `i` picks the base
// ([bytesBinaryUnitBase] vs [bytesDecimalUnitBase]). `pow(base, 0)` is
// 1, which covers both the empty unit and a bare `b`.
//
// This expression is only meaningful for a unit [bytesUnitRe] admits;
// for anything else the exponent falls through to 0, and the caller
// gates on `valid` before reading it.
func bytesScaledExpr(numStripped, rest chplan.Expr) chplan.Expr {
	head := &chplan.FuncCall{
		Fn:   chplan.FnSubstring,
		Args: []chplan.Expr{rest, &chplan.LitInt{V: 1}, &chplan.LitInt{V: 1}},
	}
	isBinary := &chplan.Binary{
		Op: chplan.OpEq,
		Left: &chplan.FuncCall{
			Fn:   chplan.FnSubstring,
			Args: []chplan.Expr{rest, &chplan.LitInt{V: 2}, &chplan.LitInt{V: 1}},
		},
		Right: &chplan.LitString{V: "i"},
	}
	expArgs := make([]chplan.Expr, 0, 2*len(bytesUnitPrefixLetters)+1)
	for i, letter := range bytesUnitPrefixLetters {
		expArgs = append(
			expArgs,
			&chplan.Binary{Op: chplan.OpEq, Left: head, Right: &chplan.LitString{V: string(letter)}},
			&chplan.LitInt{V: int64(i + 1)},
		)
	}
	expArgs = append(expArgs, &chplan.LitInt{V: 0})
	exponent := &chplan.FuncCall{Fn: chplan.FnMultiIf, Args: expArgs}

	base := &chplan.FuncCall{
		Fn: chplan.FnIf,
		Args: []chplan.Expr{
			isBinary,
			&chplan.LitInt{V: bytesBinaryUnitBase},
			&chplan.LitInt{V: bytesDecimalUnitBase},
		},
	}
	return &chplan.Binary{
		Op:    chplan.OpMul,
		Left:  &chplan.FuncCall{Fn: chplan.FnToFloat64OrZero, Args: []chplan.Expr{numStripped}},
		Right: &chplan.FuncCall{Fn: chplan.FnPow, Args: []chplan.Expr{base, exponent}},
	}
}
