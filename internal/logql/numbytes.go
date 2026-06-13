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
		Name: "match",
		Args: []chplan.Expr{raw, &chplan.LitString{V: numericValidRe}},
	}
	// value mirrors strconv.ParseFloat's result on the accepted set:
	// toFloat64OrZero coincides with ParseFloat on every string the
	// regex admits (it is only ever read under `valid`). The bare
	// `inf` / `nan` specials parse identically on both sides.
	value := &chplan.FuncCall{
		Name: "toFloat64OrZero",
		Args: []chplan.Expr{raw},
	}
	// details: strconv.NumError.Error() — `strconv.ParseFloat: parsing
	// "<v>": invalid syntax`. The quoted token is the RAW value.
	details := &chplan.FuncCall{
		Name: "concat",
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
//
// Narrow divergences (documented, not fixed):
//
//   - humanize returns `too large: <s>` when the byte count overflows
//     uint64 (a value above ~1.8e19 bytes); this classifier never emits
//     that branch. parseReadableSize would also disagree numerically at
//     that magnitude, so the realistic corpus never reaches it.
type bytesParse struct {
	raw     chplan.Expr
	valid   chplan.Expr
	value   chplan.Expr
	details chplan.Expr
}

// newBytesParse builds the parse expressions for one label access,
// replicating humanize.ParseBytes's split in CH function calls.
func newBytesParse(raw chplan.Expr) bytesParse {
	// number = the leading [0-9.,] run; numStripped = that run with
	// commas removed (humanize's `strings.Replace(num, ",", "", -1)`).
	number := &chplan.FuncCall{
		Name: "extract",
		Args: []chplan.Expr{raw, &chplan.LitString{V: bytesNumberRe}},
	}
	numStripped := &chplan.FuncCall{
		Name: "replaceAll",
		Args: []chplan.Expr{number, &chplan.LitString{V: ","}, &chplan.LitString{V: ""}},
	}
	// rest = the suffix after the number run, lowercased and
	// whitespace-trimmed — humanize's `strings.ToLower(strings.TrimSpace(...))`.
	rest := &chplan.FuncCall{
		Name: "replaceRegexpAll",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "lower",
				Args: []chplan.Expr{
					&chplan.FuncCall{
						Name: "replaceRegexpAll",
						Args: []chplan.Expr{raw, &chplan.LitString{V: bytesNumberRe}, &chplan.LitString{V: ""}},
					},
				},
			},
			&chplan.LitString{V: `^\s+|\s+$`},
			&chplan.LitString{V: ""},
		},
	}
	numberValid := &chplan.FuncCall{
		Name: "isNotNull",
		Args: []chplan.Expr{
			&chplan.FuncCall{Name: "toFloat64OrNull", Args: []chplan.Expr{numStripped}},
		},
	}
	unitValid := &chplan.FuncCall{
		Name: "match",
		Args: []chplan.Expr{rest, &chplan.LitString{V: bytesUnitRe}},
	}
	valid := &chplan.Binary{Op: chplan.OpAnd, Left: numberValid, Right: unitValid}
	// value: parseReadableSize understands the same human size grammar
	// humanize.ParseBytes does for the accepted set; it is only ever
	// read under `valid`.
	value := &chplan.FuncCall{
		Name: "parseReadableSize",
		Args: []chplan.Expr{raw},
	}
	// details: classify in humanize's scan order — number first, then
	// unit. `if(numberValid, <unhandled-size>, <parsefloat-error>)`.
	details := &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			numberValid,
			&chplan.FuncCall{
				Name: "concat",
				Args: []chplan.Expr{&chplan.LitString{V: `unhandled size name: `}, rest},
			},
			&chplan.FuncCall{
				Name: "concat",
				Args: []chplan.Expr{
					&chplan.LitString{V: `strconv.ParseFloat: parsing "`},
					numStripped,
					&chplan.LitString{V: `": invalid syntax`},
				},
			},
		},
	}
	return bytesParse{raw: raw, valid: valid, value: value, details: details}
}
