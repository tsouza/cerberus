package chsql_test

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// valueExprRe extracts the outer-SELECT Value expression — the text
// between the leading `\`Attributes\`, ` projection and the Value alias.
// The over-time emitters render the outer projection as
//
//	SELECT `Attributes`, <value-expr> AS `Value` FROM ( ... )
//
// or, for the pairs-path reducers (irate / deriv), with a bare
//
//	SELECT `Attributes`, <value-expr> AS Value FROM ( ... )
//
// The alias (` AS `Value“ or ` AS Value`) immediately precedes the first
// outer ` FROM `, so we anchor the capture on the Attributes prefix and
// terminate on ` AS Value`/` AS `Value“ before ` FROM `. The capture is
// greedy up to the *last* alias-before-FROM so a nested ` AS ` inside the
// expression itself (none today, but defensive) can't truncate it early.
var valueExprRe = regexp.MustCompile("(?s)SELECT `Attributes`, (.*) AS `?Value`? FROM ")

// floatProducingTokens are the ClickHouse constructs that GUARANTEE the
// Value expression scans into a Go *float64 (cerberus's prod clickhouse-go
// scan type — see chclient.Cursor.Sample). Each is either an explicit
// cast / float fn, or an operation whose CH return type is Float64
// because at least one operand is Float64 (window_vals is a Float64
// array, so arraySum / arrayAvg / arrayMin / arrayMax / element indexing
// over it are Float64; division promotes to Float64; nan is Float64).
//
// The list is deliberately the *float-safe allow-set*. A reducer whose
// Value expression contains NONE of these is, by construction, an
// integer-typed leak: a bare `1`, `length(window_vals)` (UInt64),
// `count(...)` (UInt64), or `countIf(...)` (UInt64) projected raw. That
// is exactly the bug that bit present_over_time (a `toFloat64(1)` →
// `1` revert): prod's strict `*float64` scan 502s on a UInt8/UInt64
// Value, while the lenient chDB roundtrip silently coerces it — so the
// chdb goldens DON'T catch it. This allow-set does.
var floatProducingTokens = []string{
	"toFloat64(",             // explicit cast — present_over_time, count_over_time, holt_winters …
	"CAST(",                  // explicit CAST(... AS Float64) — resets / changes
	"arraySum(",              // sum_over_time — Float64 array → Float64
	"arrayAvg(",              // avg_over_time
	"arrayMin(",              // min_over_time
	"arrayMax(",              // max_over_time
	"min(",                   // min_over_time direct CH aggregate over Float64 Value → Float64
	"max(",                   // max_over_time direct CH aggregate over Float64 Value → Float64
	"window_vals[",           // first/last_over_time element indexing — Float64 element
	"sqrt(",                  // stddev_over_time
	"arrayWithConstant(",     // stdvar_over_time two-pass variance (arrayAvg inside)
	"simpleLinearRegression", // deriv slope — Float64 tuple element
	"nan",                    // empty-window sentinel — Float64
	" / ",                    // division promotes to Float64 (rate, two-pass variance)
}

// integerLeakTokens are CH constructs that, when they appear in the
// Value expression *without* a surrounding float-producing wrapper,
// return an integer type (UInt8 / UInt64) and therefore 502 the prod
// `*float64` scan. We only flag them when no float token is present —
// `toFloat64(length(...))` is fine, a bare `length(...)` is not.
var integerLeakTokens = []string{
	"length(",
	"count(",
	"countIf(",
}

// bareIntLiteralRe matches a Value expression that is *exactly* an
// integer literal (e.g. `1`, `0`) with no wrapping function — the
// canonical present_over_time leak shape (`toFloat64(1)` regressed to
// `1`).
var bareIntLiteralRe = regexp.MustCompile(`^-?\d+$`)

// allPromQLOverTimeReducers is the full set of PromQL range-vector
// functions cerberus's over-time emitter renders through
// emitRangeWindowOverTime / the dedicated reducer emitters. present_over_time
// leads the list because it is the function whose UInt8 leak motivated
// this guard. last/first_over_time go through the same windowed-array
// path. rate / increase / delta / irate / idelta / deriv / resets /
// changes are the derived-shape reducers; each must also project Float64.
var allPromQLOverTimeReducers = []string{
	"present_over_time",
	"sum_over_time",
	"avg_over_time",
	"min_over_time",
	"max_over_time",
	"count_over_time",
	"last_over_time",
	"stddev_over_time",
	"stdvar_over_time",
	"rate",
	"increase",
	"delta",
	"irate",
	"idelta",
	"deriv",
	"resets",
	"changes",
}

// TestOverTimeValueColumnIsFloat64 is the shift-left guard for the
// present_over_time-class UInt8 leak: cerberus's production clickhouse-go
// scans the metrics Value column strictly as *float64, so an emitter that
// projects an integer-typed Value (a bare `1`, `length(...)`, `count(...)`,
// `countIf(...)`) returns a 502 in prod — but PASSES the lenient chDB
// roundtrip goldens, which silently coerce the integer. This unit test
// closes that gap by emitting each PromQL over-time reducer's RangeWindow
// and asserting the projected Value expression is Float64-typed.
//
// The assertion is structural, not a brittle exact-string pin: the Value
// expression must contain at least one float-PRODUCING construct
// (toFloat64 / arraySum / arrayAvg / division / nan / …) AND must not be
// a bare integer literal or an unwrapped integer aggregate. Reverting
// present_over_time's `toFloat64(1)` to `1`, or count_over_time's
// `toFloat64(length(...))` to `length(...)`, makes this test go RED on
// the always-on `check` gate — before the change can reach prod and 502.
func TestOverTimeValueColumnIsFloat64(t *testing.T) {
	t.Parallel()

	for _, fn := range allPromQLOverTimeReducers {
		fn := fn
		t.Run(fn, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.RangeWindow{
				Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
				Func:            fn,
				TimestampColumn: "TimeUnix",
				ValueColumn:     "Value",
				GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%s): %v", fn, err)
			}

			valueExpr := extractValueExpr(t, fn, sql)
			assertFloat64ValueExpr(t, fn, valueExpr, sql)
		})
	}
}

// extractValueExpr pulls the `<expr> AS \`Value\“ text out of the outer
// SELECT. Every over-time emitter renders the same outer shape; if a
// future emitter diverges, the regex miss fails loudly rather than
// silently skipping the type check.
func extractValueExpr(t *testing.T, fn, sql string) string {
	t.Helper()
	m := valueExprRe.FindStringSubmatch(sql)
	if m == nil {
		t.Fatalf("Emit(%s): could not locate `<expr> AS \\`Value\\`` in outer SELECT — "+
			"the over-time outer projection shape changed; update extractValueExpr "+
			"so the Float64 type-check keeps biting.\nSQL: %s", fn, sql)
	}
	// Trim leading/trailing whitespace for the literal checks below.
	return strings.TrimSpace(m[1])
}

// assertFloat64ValueExpr is the type-check core. The Value expression
// must contain a float-producing token AND must not be an unwrapped
// integer aggregate or bare integer literal.
func assertFloat64ValueExpr(t *testing.T, fn, valueExpr, sql string) {
	t.Helper()

	// A bare integer literal Value (`1`, `0`) is the canonical leak.
	if bareIntLiteralRe.MatchString(valueExpr) {
		t.Fatalf("Emit(%s): Value expression %q is a BARE INTEGER LITERAL — "+
			"prod clickhouse-go scans Value as *float64 and 502s on a UInt8 Value "+
			"(this is the present_over_time `toFloat64(1)`→`1` regression). Wrap in "+
			"toFloat64(...).\nSQL: %s", fn, valueExpr, sql)
	}

	hasFloat := false
	for _, tok := range floatProducingTokens {
		if strings.Contains(valueExpr, tok) {
			hasFloat = true
			break
		}
	}
	if !hasFloat {
		t.Fatalf("Emit(%s): Value expression %q contains NO float-producing construct "+
			"(%v) — it projects an integer type that prod's strict *float64 scan rejects "+
			"with a 502, while the lenient chDB roundtrip silently coerces it. Wrap the "+
			"reducer in toFloat64(...) / CAST(... AS Float64).\nSQL: %s",
			fn, valueExpr, floatProducingTokens, sql)
	}

	// Defence-in-depth: an integer aggregate that is NOT inside any float
	// wrapper. We've already confirmed hasFloat above, so if an integer
	// leak token is present, it must be wrapped — but flag the specific
	// dangerous shape `<intagg>(...) AS Value` with no float token at the
	// expression head. The hasFloat gate already covers the unwrapped
	// case (a bare `length(window_vals)` has no float token); this is an
	// explanatory cross-check that the wrapper actually encloses the
	// integer aggregate rather than sitting in an unrelated subexpression.
	for _, tok := range integerLeakTokens {
		if strings.HasPrefix(valueExpr, tok) {
			// Expression LEADS with an integer aggregate. The only safe
			// lead is a float wrapper; leading with length(/count(/countIf(
			// means the integer result is the projected Value.
			t.Fatalf("Emit(%s): Value expression %q LEADS with integer aggregate %q "+
				"(returns UInt64) — prod's *float64 scan 502s. Wrap in toFloat64(...).\n"+
				"SQL: %s", fn, valueExpr, tok, sql)
		}
	}
}
