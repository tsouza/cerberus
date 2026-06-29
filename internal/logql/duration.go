package logql

import (
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
// keep the row; unwrap keeps the sample with value 0). ClickHouse's
// `parseTimeDelta`, in contrast, throws (code 36) on the first value it
// can't parse — one Go-shaped `291.792µs` in a single row used to abort
// the whole query (crawl run 27327766381, Logs Drilldown fields tab).
//
// CH `parseTimeDelta` unit gaps vs Go `time.ParseDuration` (verified
// empirically against clickhouse-server 24.8.14 and chDB / server 25.8
// — the k3d, compose and compatibility stacks now all pin 25.8, see the
// spec fixtures under test/spec/logql/duration-*):
//
//   - the micro sign `µs` (U+00B5) and Greek mu `μs` (U+03BC) are
//     rejected by 24.8 ("parse unit failed") though `us` parses; 25.8
//     accepts both. Normalising to `us` before the call stays
//     forward-safe — it is a no-op on 25.8 and keeps the emit identical.
//   - a leading `-` / `+` sign is rejected on both versions. The sign
//     is stripped before the call and re-applied as a multiplier.
//   - the bare-zero special case `0` (Go: valid, no unit required) is
//     rejected on both versions. Short-circuited to 0.
//   - `.5s` ("number not found") and `1.s` ("number not found after
//     '.'") are rejected on both versions though Go accepts them
//     (Go requires digits before OR after the dot, not both).
//     Normalised to `0.5s` / `1s` before the call.
//
// `ns`, `us`, `ms`, `s`, `m`, `h`, fractional values and compound
// spans (`1h2m3.5s`) parse identically on both CH versions and match
// Go's unit set exactly (Go has no `d` / `w` — and neither does
// reference Loki at these two sites, which call `time.ParseDuration`
// directly, NOT the extended `model.ParseDuration` used for query
// range literals).
//
// Validity is decided by a Go-shaped regex BEFORE `parseTimeDelta`
// runs; the call itself is wrapped in `if(valid, …)` so CH's
// short-circuit evaluation (default `short_circuit_function_evaluation
// = enable`) never feeds it an invalid string.

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
		Name: "replaceRegexpAll",
		Args: []chplan.Expr{raw, &chplan.LitString{V: `^[+-]`}, &chplan.LitString{V: ``}},
	}
	isZero := &chplan.Binary{Op: chplan.OpEq, Left: stripped, Right: &chplan.LitString{V: "0"}}
	valid := &chplan.Binary{
		Op:   chplan.OpOr,
		Left: isZero,
		Right: &chplan.FuncCall{
			Name: "match",
			Args: []chplan.Expr{stripped, &chplan.LitString{V: goDurationValidRe}},
		},
	}

	// normalised = stripped with every CH-vs-Go gap papered over:
	// µ/μ → u, `.5s` → `0.5s` (zero-fill before a bare leading
	// fraction), `1.s` → `1s` (drop a trailing dot before the unit).
	// Only ever evaluated under `valid`, so the rewrites are
	// value-preserving by construction.
	micro := &chplan.FuncCall{
		Name: "replaceAll",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "replaceAll",
				Args: []chplan.Expr{stripped, &chplan.LitString{V: "µ"}, &chplan.LitString{V: "u"}},
			},
			&chplan.LitString{V: "μ"}, &chplan.LitString{V: "u"},
		},
	}
	leadingDotFixed := &chplan.FuncCall{
		Name: "replaceRegexpAll",
		Args: []chplan.Expr{micro, &chplan.LitString{V: `(^|[a-z])\.([0-9])`}, &chplan.LitString{V: `\10.\2`}},
	}
	normalised := &chplan.FuncCall{
		Name: "replaceRegexpAll",
		Args: []chplan.Expr{leadingDotFixed, &chplan.LitString{V: `\.([a-z])`}, &chplan.LitString{V: `\1`}},
	}

	// seconds: the bare-zero branch sits OUTSIDE the sign multiplier so
	// "-0" yields +0.0 like Go (a bare `sign * 0.` would emit IEEE -0,
	// which JSON-marshals differently from reference Loki's 0).
	sign := &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			&chplan.FuncCall{Name: "startsWith", Args: []chplan.Expr{raw, &chplan.LitString{V: "-"}}},
			&chplan.LitFloat{V: -1},
			&chplan.LitFloat{V: 1},
		},
	}
	seconds := &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			&chplan.Binary{Op: chplan.OpAnd, Left: valid, Right: notExpr(isZero)},
			&chplan.Binary{
				Op:   chplan.OpMul,
				Left: sign,
				Right: &chplan.FuncCall{
					Name: "parseTimeDelta",
					Args: []chplan.Expr{normalised},
				},
			},
			&chplan.LitFloat{V: 0},
		},
	}

	// details: classify in Go's scan order. Note both classification
	// regexes run against `stripped` (Go scans after the sign) while
	// the quoted duration is the RAW original — `time.ParseDuration("-5x")`
	// quotes "-5x", not "5x".
	quote := &chplan.LitString{V: `"`}
	details := &chplan.FuncCall{
		Name: "multiIf",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "match",
				Args: []chplan.Expr{stripped, &chplan.LitString{V: goDurationMissingUnitRe}},
			},
			&chplan.FuncCall{
				Name: "concat",
				Args: []chplan.Expr{
					&chplan.LitString{V: `time: missing unit in duration "`}, raw, quote,
				},
			},
			&chplan.FuncCall{
				Name: "match",
				Args: []chplan.Expr{stripped, &chplan.LitString{V: goDurationUnknownUnitRe}},
			},
			&chplan.FuncCall{
				Name: "concat",
				Args: []chplan.Expr{
					&chplan.LitString{V: `time: unknown unit "`},
					&chplan.FuncCall{
						Name: "extract",
						Args: []chplan.Expr{stripped, &chplan.LitString{V: goDurationUnknownUnitExtractRe}},
					},
					&chplan.LitString{V: `" in duration "`}, raw, quote,
				},
			},
			&chplan.FuncCall{
				Name: "concat",
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
	return &chplan.FuncCall{Name: "not", Args: []chplan.Expr{e}}
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
		Name: "mapContains",
		Args: []chplan.Expr{labelsExpr, &chplan.LitString{V: syntax.ErrorLabel}},
	})
	args := make([]chplan.Expr, 0, len(marks)*2+1)
	for _, m := range marks {
		args = append(
			args,
			&chplan.Binary{Op: chplan.OpAnd, Left: noPriorError, Right: m.cond},
			&chplan.FuncCall{Name: "map", Args: []chplan.Expr{
				&chplan.LitString{V: syntax.ErrorLabel},
				&chplan.LitString{V: m.kind},
				&chplan.LitString{V: syntax.ErrorDetailsLabel},
				m.details,
			}},
		)
	}
	args = append(args, &chplan.FuncCall{Name: "map"})
	branch := chplan.Expr(&chplan.FuncCall{Name: "multiIf", Args: args})
	if len(marks) == 1 {
		// multiIf demands ≥2 condition/branch pairs only in spirit; CH
		// accepts the 3-arg form but `if` is the canonical spelling.
		branch = &chplan.FuncCall{Name: "if", Args: args}
	}
	return &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{labelsExpr, branch},
	}
}
