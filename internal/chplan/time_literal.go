package chplan

import "time"

// TimeLiteralLayout is the Go layout an absolute instant is written in
// inside a CH time literal: the UTC wall clock at nanosecond precision,
// followed by the explicit `Z` (UTC) designator. The designator is what
// makes the literal name the same instant whatever timezone the
// ClickHouse server or session runs in.
const TimeLiteralLayout = "2006-01-02 15:04:05.000000000Z"

// FormatTimeLiteral renders t in TimeLiteralLayout, normalising it to UTC
// first so the wall clock and the `Z` designator always agree.
func FormatTimeLiteral(t time.Time) string {
	return t.UTC().Format(TimeLiteralLayout)
}

// TimeLiteral builds the canonical expression for the absolute instant t:
// `parseDateTime64BestEffort('<FormatTimeLiteral(t)>', 9)`.
//
// The parse honours the literal's `Z` designator, so the value is t on a
// ClickHouse server in any timezone. The result type is a DateTime64(9)
// with no explicit timezone, the same type as the OTel-CH `Timestamp` /
// `TimeUnix` columns, so day-boundary functions over a literal-derived
// expression (`toStartOfDay` and friends) keep using the server's
// implicit timezone exactly as they do over the columns and the
// materialized views keyed by them.
//
// Every absolute time bound and evaluation anchor in a plan is built
// here; chsql.Builder.DateTime64Lit renders the same literal for the
// emitter-side bounds.
func TimeLiteral(t time.Time) Expr {
	return &FuncCall{
		Fn: FnParseDateTime64BestEffort,
		Args: []Expr{
			&LitString{V: FormatTimeLiteral(t)},
			&LitInt{V: NanoScale},
		},
	}
}
