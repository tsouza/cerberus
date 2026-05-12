package chsql

import (
	"fmt"
	"strconv"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitRangeWindow lowers a chplan.RangeWindow to ClickHouse SQL using the
// windowed-array idiom inspired by promshim-clickhouse:
//
//  1. GROUP BY the series-identity columns; build (ts, value) tuples per
//     series via groupArray + arraySort.
//  2. arrayFilter to the window [end-range, end].
//  3. Apply the function-specific aggregation on the windowed values:
//     - rate / increase: arrayPopBack + arrayPopFront pair-up with
//     `if(c < p, c, c - p)` to repair counter resets, arraySum to total.
//     - *_over_time: straight array aggregation (arrayAvg, arraySum, ...).
//
// The emitter substitutes literal timestamps for r.End inline. Zero
// values fall back to ClickHouse's `now64(9)` so fixtures stay
// deterministic and runtime queries still resolve to the current eval
// time.
//
// When r.OuterRange > 0 emission switches to the matrix shape: an
// arrayJoin fans out one row per anchor across [End-OuterRange, End]
// spaced by Step (end-inclusive), and the outer SELECT projects the
// anchor timestamp alongside the per-anchor value. Used by PromQL
// subqueries (P0 #4).
//
// When r.Identity is true, Func is ignored and the per-window value is
// the last sample in the window — used by bare-vector subqueries like
// `up[5m:1m]`.
func (e *emitter) emitRangeWindow(r *chplan.RangeWindow) error {
	if r.Identity {
		return e.emitRangeWindowIdentity(r)
	}
	switch r.Func {
	case "rate":
		return e.emitRangeWindowRate(r)
	case "increase":
		return e.emitRangeWindowIncrease(r)
	case "sum_over_time", "avg_over_time", "min_over_time", "max_over_time", "count_over_time", "last_over_time":
		return e.emitRangeWindowOverTime(r)
	case "log_rate":
		return e.emitRangeWindowLogRate(r)
	default:
		return fmt.Errorf("%w: range function %q (lands in M1.1 follow-ups)", ErrUnsupported, r.Func)
	}
}

// emitRangeWindowIdentity emits the "last value in window" shape used
// by bare-vector subqueries (`up[5m:1m]`). Functionally equivalent to
// last_over_time but lowered from a SubqueryExpr (P0 #4.5) rather than
// a Call.
func (e *emitter) emitRangeWindowIdentity(r *chplan.RangeWindow) error {
	return e.emitWindowedArray(r, "if(length(window_vals) > 0, window_vals[length(window_vals)], nan)")
}

// emitRangeWindowLogRate emits SQL for LogQL-style `rate({...}[range])`
// (and `bytes_rate`, after the lowering layer projects `length(Body)`
// as Value): `arraySum(window_vals) / range_seconds`. Distinct from
// PromQL's counter `rate`, which uses counter-reset-aware deltas.
//
// range_seconds binds as a parameter via the value-writer callback so
// the emitter stays free of new Sprintf-on-SQL instances (RC6 rule).
func (e *emitter) emitRangeWindowLogRate(r *chplan.RangeWindow) error {
	return e.emitWindowedArrayCb(r, func() error {
		e.b.WriteString("if(length(window_vals) > 0, arraySum(window_vals) / ")
		if err := e.bindArg(r.Range.Seconds()); err != nil {
			return err
		}
		e.b.WriteString(", 0.0)")
		return nil
	})
}

// emitRangeWindowRate emits SQL for `rate(metric[range])`.
//
// Form (instant eval at r.End, looking back r.Range):
//
//	SELECT
//	    series_key,
//	    if(length(window_vals) > 1, counter_delta / range_seconds, 0.0) AS value
//	FROM (
//	    SELECT
//	        series_key,
//	        arrayMap(p -> tupleElement(p, 2), window_pairs) AS window_vals,
//	        arraySum(arrayMap(
//	            (p, c) -> if(c < p, c, c - p),
//	            arrayPopBack(arrayMap(x -> tupleElement(x, 2), window_pairs)),
//	            arrayPopFront(arrayMap(x -> tupleElement(x, 2), window_pairs))
//	        )) AS counter_delta,
//	        <range_seconds> AS range_seconds
//	    FROM (
//	        SELECT
//	            series_key,
//	            arrayFilter(
//	                p -> tupleElement(p, 1) >= <end> - toIntervalNanosecond(<range_ns>)
//	                  AND tupleElement(p, 1) <= <end>,
//	                series_array
//	            ) AS window_pairs
//	        FROM (
//	            SELECT
//	                <group-by-keys> AS series_key,
//	                arraySort(groupArray((`TimeUnix`, `Value`))) AS series_array
//	            FROM (<input>)
//	            GROUP BY <group-by-keys>
//	        )
//	    )
//	)
func (e *emitter) emitRangeWindowRate(r *chplan.RangeWindow) error {
	return e.emitWindowedArray(r, rateValueExpr(r.Range.Seconds()))
}

// emitRangeWindowIncrease emits SQL for `increase(metric[range])`. Same
// as rate but without dividing by range_seconds.
func (e *emitter) emitRangeWindowIncrease(r *chplan.RangeWindow) error {
	return e.emitWindowedArray(r, "if(length(window_vals) > 1, counter_delta, 0.0)")
}

// emitRangeWindowOverTime emits SQL for the `*_over_time` family:
// sum_over_time, avg_over_time, min_over_time, max_over_time,
// count_over_time, last_over_time. These don't need counter-reset
// handling — they're straight array aggregations over the window's
// values.
func (e *emitter) emitRangeWindowOverTime(r *chplan.RangeWindow) error {
	var inner string
	switch r.Func {
	case "sum_over_time":
		inner = "arraySum(window_vals)"
	case "avg_over_time":
		inner = "if(length(window_vals) > 0, arrayAvg(window_vals), nan)"
	case "min_over_time":
		inner = "if(length(window_vals) > 0, arrayMin(window_vals), nan)"
	case "max_over_time":
		inner = "if(length(window_vals) > 0, arrayMax(window_vals), nan)"
	case "count_over_time":
		inner = "toFloat64(length(window_vals))"
	case "last_over_time":
		inner = "if(length(window_vals) > 0, window_vals[length(window_vals)], nan)"
	default:
		return fmt.Errorf("%w: over-time function %q", ErrUnsupported, r.Func)
	}
	return e.emitWindowedArray(r, inner)
}

// rateValueExpr returns the outer SELECT value expression for rate(),
// dividing the counter delta by range_seconds. Length check avoids
// dividing on a single-point window (rate is undefined there).
func rateValueExpr(rangeSeconds float64) string {
	return fmt.Sprintf("if(length(window_vals) > 1, counter_delta / %s, 0.0)",
		strconv.FormatFloat(rangeSeconds, 'f', -1, 64))
}

// emitWindowedArray writes the windowed-array SQL skeleton with valueExpr
// substituted in the outer SELECT position. valueExpr can reference
// `window_vals` (Array(Float64)) and `counter_delta` (Float64).
func (e *emitter) emitWindowedArray(r *chplan.RangeWindow, valueExpr string) error {
	return e.emitWindowedArrayCb(r, func() error {
		e.b.WriteString(valueExpr)
		return nil
	})
}

// emitWindowedArrayCb is the callback variant of emitWindowedArray. The
// valueWriter callback runs at the exact SQL position where the value
// expression lands; callers may bind args inside it (via e.bindArg) so
// `?` placeholders are emitted in lock-step with the args slice.
//
// When r.OuterRange > 0 emission switches to the matrix path: each
// series emits N rows, one per anchor across [End-OuterRange, End]
// spaced by Step (end-inclusive). The outer SELECT additionally
// projects the anchor timestamp as `anchor_ts`.
func (e *emitter) emitWindowedArrayCb(r *chplan.RangeWindow, valueWriter func() error) error {
	if r.TimestampColumn == "" {
		return fmt.Errorf("%w: RangeWindow.TimestampColumn unset", ErrUnsupported)
	}
	if r.ValueColumn == "" {
		return fmt.Errorf("%w: RangeWindow.ValueColumn unset", ErrUnsupported)
	}
	if r.OuterRange > 0 {
		if r.Step <= 0 {
			return fmt.Errorf("%w: RangeWindow.OuterRange > 0 requires Step > 0", ErrUnsupported)
		}
		return e.emitWindowedArrayMatrix(r, valueWriter)
	}

	endExpr := timeOrNow(r.End)
	if r.Offset > 0 {
		endExpr = "(" + endExpr + " - toIntervalNanosecond(" + strconv.FormatInt(r.Offset.Nanoseconds(), 10) + "))"
	}
	rangeNS := r.Range.Nanoseconds()
	groupKeys, err := e.collectGroupBy(r.GroupBy)
	if err != nil {
		return err
	}

	// Outer SELECT — final value per series.
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	e.b.WriteString(", ")
	if err := valueWriter(); err != nil {
		return err
	}
	e.b.WriteString(" AS value FROM (")

	// Middle SELECT — derives window_vals + counter_delta from window_pairs.
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	e.b.WriteString(", arrayMap(p -> tupleElement(p, 2), window_pairs) AS window_vals")
	e.b.WriteString(", arraySum(arrayMap((p, c) -> if(c < p, c, c - p), ")
	e.b.WriteString("arrayPopBack(arrayMap(x -> tupleElement(x, 2), window_pairs)), ")
	e.b.WriteString("arrayPopFront(arrayMap(x -> tupleElement(x, 2), window_pairs))")
	e.b.WriteString(")) AS counter_delta FROM (")

	// Inner-middle SELECT — arrayFilter to the [end-range, end] window.
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	fmt.Fprintf(&e.b, ", arrayFilter(p -> tupleElement(p, 1) >= %s - toIntervalNanosecond(%d) AND tupleElement(p, 1) <= %s, series_array) AS window_pairs FROM (",
		endExpr, rangeNS, endExpr)

	// Innermost SELECT — groupArray of (ts, value), sorted.
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	fmt.Fprintf(&e.b, ", arraySort(groupArray((%s, %s))) AS series_array FROM ",
		quoteIdent(r.TimestampColumn), quoteIdent(r.ValueColumn))
	if err := e.emitSubquery(r.Input); err != nil {
		return err
	}
	if len(groupKeys) > 0 {
		e.b.WriteString(" GROUP BY ")
		e.writeGroupSelectList(groupKeys)
	}
	e.b.WriteByte(')')

	e.b.WriteByte(')')
	e.b.WriteByte(')')
	return nil
}

// emitWindowedArrayMatrix is the OuterRange > 0 variant: each series
// emits N rows, one per anchor across [End-OuterRange, End] spaced by
// Step (end-inclusive). The innermost SELECT computes the per-series
// (TimeUnix, Value) array once via groupArray + arraySort, then an
// arrayJoin in the next layer fans out one row per anchor. Subsequent
// layers operate on the per-(series, anchor) tuple.
//
// SQL skeleton (with N = OuterRange/Step + 1):
//
//	SELECT series_key, anchor_ts, <valueExpr> AS value FROM (
//	  SELECT series_key, anchor_ts, <window_vals + counter_delta> FROM (
//	    SELECT series_key, anchor_ts, arrayFilter(p -> p.1 in [anchor_ts - range, anchor_ts], series_array) AS window_pairs FROM (
//	      SELECT series_key, series_array,
//	        arrayJoin(arrayMap(i -> <end> - toIntervalNanosecond(i * <step_ns>), range(0, N))) AS anchor_ts
//	      FROM (
//	        SELECT series_key, arraySort(groupArray((TimeUnix, Value))) AS series_array
//	        FROM (<input>) GROUP BY series_key
//	      )
//	    )
//	  )
//	)
func (e *emitter) emitWindowedArrayMatrix(r *chplan.RangeWindow, valueWriter func() error) error {
	endExpr := timeOrNow(r.End)
	if r.Offset > 0 {
		endExpr = "(" + endExpr + " - toIntervalNanosecond(" + strconv.FormatInt(r.Offset.Nanoseconds(), 10) + "))"
	}
	rangeNS := r.Range.Nanoseconds()
	stepNS := r.Step.Nanoseconds()
	// End-inclusive anchor count. e.g. [5m:2m] = 5m/2m + 1 = 3 anchors
	// at end, end-2m, end-4m. Truncating division matches Prom semantics.
	numAnchors := r.OuterRange.Nanoseconds()/stepNS + 1
	groupKeys, err := e.collectGroupBy(r.GroupBy)
	if err != nil {
		return err
	}

	// Outer SELECT — per-(series, anchor) row.
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	if len(groupKeys) > 0 {
		e.b.WriteString(", ")
	}
	e.b.WriteString("anchor_ts, ")
	if err := valueWriter(); err != nil {
		return err
	}
	e.b.WriteString(" AS value FROM (")

	// Middle SELECT — window_vals + counter_delta per (series, anchor).
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	if len(groupKeys) > 0 {
		e.b.WriteString(", ")
	}
	e.b.WriteString("anchor_ts, arrayMap(p -> tupleElement(p, 2), window_pairs) AS window_vals")
	e.b.WriteString(", arraySum(arrayMap((p, c) -> if(c < p, c, c - p), ")
	e.b.WriteString("arrayPopBack(arrayMap(x -> tupleElement(x, 2), window_pairs)), ")
	e.b.WriteString("arrayPopFront(arrayMap(x -> tupleElement(x, 2), window_pairs))")
	e.b.WriteString(")) AS counter_delta FROM (")

	// Inner-middle SELECT — arrayFilter to [anchor_ts - range, anchor_ts].
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	if len(groupKeys) > 0 {
		e.b.WriteString(", ")
	}
	fmt.Fprintf(&e.b, "anchor_ts, arrayFilter(p -> tupleElement(p, 1) >= anchor_ts - toIntervalNanosecond(%d) AND tupleElement(p, 1) <= anchor_ts, series_array) AS window_pairs FROM (",
		rangeNS)

	// Anchor-fanout SELECT — arrayJoin produces one row per anchor.
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	if len(groupKeys) > 0 {
		e.b.WriteString(", ")
	}
	fmt.Fprintf(&e.b, "series_array, arrayJoin(arrayMap(i -> %s - toIntervalNanosecond(i * %d), range(0, %d))) AS anchor_ts FROM (",
		endExpr, stepNS, numAnchors)

	// Innermost SELECT — groupArray of (ts, value), sorted.
	e.b.WriteString("SELECT ")
	e.writeGroupSelectList(groupKeys)
	if len(groupKeys) > 0 {
		e.b.WriteString(", ")
	}
	fmt.Fprintf(&e.b, "arraySort(groupArray((%s, %s))) AS series_array FROM ",
		quoteIdent(r.TimestampColumn), quoteIdent(r.ValueColumn))
	if err := e.emitSubquery(r.Input); err != nil {
		return err
	}
	if len(groupKeys) > 0 {
		e.b.WriteString(" GROUP BY ")
		e.writeGroupSelectList(groupKeys)
	}
	e.b.WriteByte(')')

	e.b.WriteByte(')')
	e.b.WriteByte(')')
	e.b.WriteByte(')')
	return nil
}

// collectGroupBy renders each GroupBy expression to an isolated string so
// it can be reused in SELECT list, GROUP BY, and reused for the outer
// SELECT in the windowed-array stack. Args captured by emitExpr go to the
// shared args slice (positions still increase across renders).
//
// Returns the rendered identifier list (each entry is a complete SQL
// fragment like `\`Attributes\“).
func (e *emitter) collectGroupBy(group []chplan.Expr) ([]string, error) {
	out := make([]string, 0, len(group))
	for _, g := range group {
		// Render to a separate buffer so we can reuse the string.
		sub := &emitter{args: e.args}
		if err := sub.emitExpr(g); err != nil {
			return nil, err
		}
		// Append any args captured by the sub-emitter back onto ours.
		e.args = sub.args
		out = append(out, sub.b.String())
	}
	return out, nil
}

func (e *emitter) writeGroupSelectList(group []string) {
	for i, g := range group {
		if i > 0 {
			e.b.WriteString(", ")
		}
		e.b.WriteString(g)
	}
}

// timeOrNow renders an explicit DateTime64(9) literal for a non-zero time
// or falls back to ClickHouse's `now64(9)` for the zero value (which is
// what the lowering produces today; M2.1 will start populating Start/End
// from the HTTP API's time params).
func timeOrNow(t time.Time) string {
	if t.IsZero() {
		return "now64(9)"
	}
	return "toDateTime64('" + t.UTC().Format("2006-01-02 15:04:05.000000000") + "', 9)"
}

// quoteIdent backtick-quotes a CH identifier; the existing writeIdent
// writes to a builder, so this is a tiny wrapper that returns the string.
func quoteIdent(name string) string {
	var b []byte
	b = append(b, '`')
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '`' {
			b = append(b, '`', '`')
			continue
		}
		b = append(b, c)
	}
	b = append(b, '`')
	return string(b)
}
