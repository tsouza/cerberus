package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerDateFn maps PromQL date-component functions to their ClickHouse
// equivalents. Each function takes one instant-vector argument whose
// `Value` column is interpreted as a Unix timestamp in seconds — except
// `timestamp(v)`, which reads each sample's `TimeUnix` column and
// converts it to float seconds.
//
// When called without an argument the PromQL spec defaults the input to
// `vector(time())` — a single instant-vector entry whose value is the
// current evaluation timestamp in seconds. cerberus lowers that to a
// degenerate `OneRow` source projecting
// `(MetricName=”, Attributes={}, TimeUnix=now64(9), Value=toFloat64(<fn>(now())))`.
// The `timestamp` function does NOT have a zero-arg form in upstream
// PromQL — Prometheus rejects it during parsing — so the no-arg branch
// is unreachable for that name; we keep the same shape anyway for
// uniformity in case upstream relaxes the rule.
//
// Semantic notes:
//
//   - PromQL `day_of_week` returns 0-6 with Sunday=0; ClickHouse
//     `toDayOfWeek(d)` returns 1-7 with Monday=1, Sunday=7. We lower as
//     `toDayOfWeek(d) % 7` which yields Mon=1, Tue=2, …, Sat=6, Sun=0 —
//     exactly the PromQL semantics.
//
//   - `days_in_month` lowers to `toDayOfMonth(toLastDayOfMonth(d))`
//     because CH has no direct `daysInMonth` builtin; the day-of-month
//     of the last day in the month is the day count for that month.
//
//   - `timestamp(v)` ignores `Value` and reads the sample's TimeUnix
//     column instead, converting the DateTime64(9) to fractional Unix
//     seconds via `toUnixTimestamp64Nano(TimeUnix) / 1e9`.
//
// Type-coercion note: every CH date-component function (`toYear`,
// `toMonth`, `toDayOfMonth`, `toDayOfWeek`, `toHour`, `toMinute`)
// returns a small integer (`UInt8` / `UInt16`). The cerberus Sample
// row is decoded into `*float64`, and clickhouse-go/v2 refuses to
// convert a UInt8/UInt16 column into `*float64` (errors with
// `converting UInt16 to *float64 is unsupported`). Wrap every emit
// in `toFloat64(...)` so the Value column lands as Float64 on the
// wire — this is the same shape every other Float-typed Value
// projection ends up with. `timestamp(v)` is already Float64 (it
// divides by `1e9`) but wrapping it is harmless.
func lowerDateFn(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) > 1 {
		return nil, fmt.Errorf("promql: %s expects 0 or 1 argument, got %d", c.Func.Name, len(c.Args))
	}

	if len(c.Args) == 0 {
		return lowerDateFnNoArg(c.Func.Name, s, ctx)
	}

	inner, err := lower(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}
	newValue := dateFnExpr(c.Func.Name, valueAsDateTime(s), &chplan.ColumnRef{Name: s.TimestampColumn})
	if newValue == nil {
		return nil, fmt.Errorf("promql: unknown date function %s", c.Func.Name)
	}
	return projectValueOverInner(inner, s, asFloat64(newValue)), nil
}

// lowerDateFnNoArg synthesises a single-row constant instant vector for
// the no-arg form of a date function. PromQL spec: `year()` ≡
// `year(vector(time()))`. The result is a one-row vector with the time
// component of the current eval timestamp as its sample value.
//
// We emit `OneRow` (cerberus's no-FROM `SELECT 1` source) wrapped in a
// Project that builds the canonical Sample shape:
//
//	MetricName  = ''
//	Attributes  = CAST(map(), 'Map(String,String)')
//	TimeUnix    = now64(9)
//	Value       = toFloat64(<date-fn>(now()))
//
// — matching the shape produced by an unaggregated PromQL aggregation
// over the same single instant vector. The `toFloat64` wrap is
// load-bearing: see the type-coercion note on `lowerDateFn` for the
// rationale (the CH date-component builtins return UInt8/UInt16 and
// clickhouse-go/v2 refuses to scan those into `*float64`).
//
// Historical note: this previously emitted `Scan{Database:"system",
// Table:"one"}` — a qualified scan over CH's standard one-row table.
// The SQL works against real CH 24.x, but using the dedicated
// `chplan.OneRow` source is cleaner: it bypasses the qualified-table
// emit path entirely and reuses the same `SELECT 1` shape that
// `time()` / `vector(scalar)` already rely on.
//
// In range mode (ctx.step > 0) the source is swapped for a StepGrid
// emitting one anchor per step in `[start, end]`; `now()` references
// inside the value expression are rewritten by [syntheticScalarVector]
// to read the per-row `anchor_ts` column so each step's row reflects
// that step's date components.
func lowerDateFnNoArg(name string, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	now := &chplan.FuncCall{Name: "now"}
	expr := dateFnExpr(name, now, now)
	if expr == nil {
		return nil, fmt.Errorf("promql: unknown date function %s", name)
	}
	return syntheticScalarVector(asFloat64(expr), nil, s, ctx), nil
}

// asFloat64 wraps e in `toFloat64(...)`. Used by the date-function
// lowerings to coerce CH integer return types (toYear → UInt16,
// toMonth/toHour/etc → UInt8) into Float64, matching the Sample.Value
// column type the cursor decodes into. Idempotent for Float64 inputs
// (CH's `toFloat64` is a no-op identity on Float64) so the wrap is
// safe even on the `timestamp(v)` path that already yields Float64.
func asFloat64(e chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{e}}
}

// dateFnExpr returns the CH expression that computes the date-component
// for the given PromQL function name. valueDT is the DateTime expression
// derived from the input sample's Value (interpreted as Unix seconds);
// tsRef is the raw `TimeUnix` column reference used by `timestamp(v)`.
//
// Returns nil when name is not a recognised date function — caller
// translates that into an "unsupported" error.
func dateFnExpr(name string, valueDT, tsRef chplan.Expr) chplan.Expr {
	switch name {
	case "year":
		return &chplan.FuncCall{Name: "toYear", Args: []chplan.Expr{valueDT}}
	case "month":
		return &chplan.FuncCall{Name: "toMonth", Args: []chplan.Expr{valueDT}}
	case "day_of_month":
		return &chplan.FuncCall{Name: "toDayOfMonth", Args: []chplan.Expr{valueDT}}
	case "day_of_week":
		// CH toDayOfWeek default returns Mon=1..Sun=7 (ISO).
		// PromQL day_of_week returns Sun=0..Sat=6 (US).
		// `toDayOfWeek(d) % 7` maps 7→0 and leaves 1..6 unchanged,
		// yielding the PromQL semantics directly.
		return &chplan.Binary{
			Op:    chplan.OpMod,
			Left:  &chplan.FuncCall{Name: "toDayOfWeek", Args: []chplan.Expr{valueDT}},
			Right: &chplan.LitInt{V: 7},
		}
	case "days_in_month":
		// CH has no direct daysInMonth; the day-of-month of the last
		// day in the month IS the day count for that month.
		return &chplan.FuncCall{
			Name: "toDayOfMonth",
			Args: []chplan.Expr{
				&chplan.FuncCall{Name: "toLastDayOfMonth", Args: []chplan.Expr{valueDT}},
			},
		}
	case "hour":
		return &chplan.FuncCall{Name: "toHour", Args: []chplan.Expr{valueDT}}
	case "minute":
		return &chplan.FuncCall{Name: "toMinute", Args: []chplan.Expr{valueDT}}
	case "timestamp":
		// `timestamp(v)` returns each sample's TimeUnix as float
		// seconds — NOT a function of Value. Convert the DateTime64(9)
		// column to nanoseconds (Int64) and divide by 1e9 to get
		// fractional seconds.
		return &chplan.Binary{
			Op:    chplan.OpDiv,
			Left:  &chplan.FuncCall{Name: "toUnixTimestamp64Nano", Args: []chplan.Expr{tsRef}},
			Right: &chplan.LitFloat{V: 1e9},
		}
	}
	return nil
}

// valueAsDateTime renders `toDateTime(toInt64(Value), 'UTC')` — the
// PromQL convention that Value is Unix seconds, which CH's date-component
// functions consume after a cast through Int64 → DateTime. We pin the
// timezone to UTC explicitly so the returned components don't drift with
// the server's default timezone (PromQL specifies UTC).
func valueAsDateTime(s schema.Metrics) chplan.Expr {
	return &chplan.FuncCall{
		Name: "toDateTime",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "toInt64",
				Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}},
			},
			&chplan.LitString{V: "UTC"},
		},
	}
}
