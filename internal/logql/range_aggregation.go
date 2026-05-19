package logql

import (
	"fmt"

	loglib "github.com/grafana/loki/v3/pkg/logql/log"
	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerRangeAggregation handles LogQL's metric form:
//
//	rate({selector}[5m])
//	count_over_time({selector}[5m])
//	bytes_rate({selector}[5m])
//	bytes_over_time({selector}[5m])
//	sum_over_time({selector} | logfmt | unwrap field [5m])
//	avg_over_time({selector} | logfmt | unwrap field [5m])
//	min_over_time / max_over_time / stddev_over_time / stdvar_over_time
//	quantile_over_time(<phi>, {selector} | logfmt | unwrap field [5m])
//
// Pipeline-uniform: lower the inner LogSelectorExpr to a Scan +
// optional Filter (threading the parser-stage labelsExpr through), wrap
// with a Project that synthesises a numeric `Value` column, then wrap
// with a RangeWindow that aggregates over the [end - range, end] window
// per stream identity (or per `by`/`without` grouping when provided).
//
// Value-column choice:
//   - line-counting ops (`rate`, `count_over_time`)                 → 1
//   - byte-counting ops (`bytes_rate`, `bytes_over_time`)           → toFloat64(length(Body))
//   - unwrap with no conversion                                     → toFloat64OrZero(labelsExpr[field])
//   - unwrap duration / duration_seconds(field)                     → parseTimeDelta(labelsExpr[field])
//   - unwrap bytes(field)                                           → parseReadableSize(labelsExpr[field])
//
// Grouping:
//   - no Grouping → group by ResourceAttributes (one row per stream).
//   - `by (k1, k2)` → group by `map('k1', RA['k1'], 'k2', RA['k2'])`.
//   - `without (k1, k2)` → group by `mapFilter(...)` removing the keys.
func lowerRangeAggregation(e *syntax.RangeAggregationExpr, s schema.Logs, lc lowerCtx) (chplan.Node, error) {
	if e.Left == nil {
		return nil, fmt.Errorf("logql: range-aggregation has nil inner")
	}

	inner, labelsExpr, err := lowerLogRange(e.Left, s, lc)
	if err != nil {
		return nil, err
	}

	// Apply unwrap PostFilters (label filters that ride on the unwrap
	// clause — e.g. `unwrap duration | duration > 10s`). These use the
	// same labelsExpr as the rest of the pipeline.
	if e.Left.Unwrap != nil && len(e.Left.Unwrap.PostFilters) > 0 {
		filtered, err := applyUnwrapPostFilters(inner, e.Left.Unwrap.PostFilters, s, labelsExpr)
		if err != nil {
			return nil, err
		}
		inner = filtered
	}

	value, err := rangeValueExpr(e, s, labelsExpr)
	if err != nil {
		return nil, err
	}

	groupBy, err := rangeAggregationGroupBy(e, s)
	if err != nil {
		return nil, err
	}

	identityProj := chplan.Projection{Expr: &chplan.ColumnRef{Name: s.ResourceAttributesColumn}}
	if groupBy != nil {
		// With `by (...)` / `without (...)` the inner Project replaces
		// the per-stream identity with the group-key map so the
		// RangeWindow GROUP BY (still keyed on the
		// ResourceAttributesColumn) collapses per-group rather than
		// per-stream. The alias matches the column name the outer
		// RangeWindow expects.
		identityProj = chplan.Projection{Expr: groupBy, Alias: s.ResourceAttributesColumn}
	}
	projections := []chplan.Projection{
		identityProj,
		{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
		{Expr: value, Alias: rangeAggSynthValueColumn},
	}
	projected := &chplan.Project{
		Input:       inner,
		Projections: projections,
	}

	chFunc, err := rangeFuncName(e.Operation)
	if err != nil {
		return nil, err
	}

	rw := &chplan.RangeWindow{
		Input:           projected,
		Func:            chFunc,
		Range:           e.Left.Interval,
		Offset:          e.Left.Offset,
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     rangeAggSynthValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.ResourceAttributesColumn}},
	}
	if e.Operation == syntax.OpRangeTypeQuantile {
		if e.Params == nil {
			return nil, fmt.Errorf("logql: quantile_over_time requires a phi parameter")
		}
		rw.Scalars = []float64{*e.Params}
	}
	return rw, nil
}

// lowerLogRange unwraps the LogRangeExpr's inner selector (either bare
// matchers or a pipeline) and returns the resulting Node plus the final
// labelsExpr the pipeline produced. Range aggregations thread this
// labelsExpr to unwrap's value extraction and any unwrap-post filters
// so they resolve against the same map that ordinary label filters do.
func lowerLogRange(lr *syntax.LogRangeExpr, s schema.Logs, lc lowerCtx) (chplan.Node, chplan.Expr, error) {
	switch left := lr.Left.(type) {
	case *syntax.MatchersExpr:
		// No pipeline — labels-map is the ResourceAttributes column.
		return lowerMatchers(left, s, lc), &chplan.ColumnRef{Name: s.ResourceAttributesColumn}, nil
	case *syntax.PipelineExpr:
		return lowerPipelineWithLabels(left, s, lc)
	}
	return nil, nil, fmt.Errorf("logql: range-aggregation inner is %T (not MatchersExpr or PipelineExpr)", lr.Left)
}

// applyUnwrapPostFilters AND-folds the unwrap clause's post-filters
// onto `inner`'s predicate. Post-filters are label filters parsed
// between the `unwrap` clause and the `[range]` close-bracket — e.g.
// `unwrap duration | duration > 10s`. They share the LabelFilterer
// type with regular `| label="v"` filters.
func applyUnwrapPostFilters(inner chplan.Node, filters []loglib.LabelFilterer, s schema.Logs, labelsExpr chplan.Expr) (chplan.Node, error) {
	pred := chplan.Expr(nil)
	if f, ok := inner.(*chplan.Filter); ok {
		pred = f.Predicate
		inner = f.Input
	}
	for _, lf := range filters {
		extra, err := labelFiltererToExpr(lf, s, labelsExpr)
		if err != nil {
			return nil, err
		}
		if pred == nil {
			pred = extra
		} else {
			pred = &chplan.Binary{Op: chplan.OpAnd, Left: pred, Right: extra}
		}
	}
	if pred == nil {
		return inner, nil
	}
	return &chplan.Filter{Input: inner, Predicate: pred}, nil
}

// rangeAggSynthValueColumn is the column name LogQL's range-aggregation
// lowering synthesises for the per-row metric value (constant 1 for line
// counts; length(Body) for byte counts; the unwrap value for unwrap-based
// ops). Shared with [Lang.ProjectSamples] so the engine's metric-branch
// wire-wrap can `chplan.ColumnRef` the same alias the inner RangeWindow /
// Aggregate emit at their outer SELECT site since #310. Pinning this in
// one place keeps the two layers from drifting like they did between
// #310 and the e2e-failures it surfaced.
const rangeAggSynthValueColumn = "Value"

// rangeValueExpr returns the per-row Value the RangeWindow aggregates.
//
// Line-counting ops use constant 1; byte-counting ops use length(Body).
// Unwrap-based ops use the unwrap clause's identifier resolved against
// the live labelsExpr — bare unwrap reads the string and casts to
// Float64, `duration` / `duration_seconds` parses Loki's Go-duration
// shape via CH's `parseTimeDelta` (Float64 seconds), and `bytes` parses
// the human-readable byte-size shape via CH's `parseReadableSize`
// (Float64 bytes).
//
// `length(Body)` is wrapped in `toFloat64` so the per-row Value tuple
// — `(Timestamp, Value)` shaped by the windowed-array RangeWindow
// emitter — carries a Float64 second element. Without the cast CH
// resolves the column to UInt64, and the downstream arrayMap / arraySum
// chain promotes back to Float64 with quiet rounding (UInt64 → Float64
// loses precision at ≥ 2^53). The cast keeps the units aligned with
// the line-counter path (`LitInt{V:1}` → Int64) where arithmetic
// remains exact across the [start, end] window.
func rangeValueExpr(e *syntax.RangeAggregationExpr, s schema.Logs, labelsExpr chplan.Expr) (chplan.Expr, error) {
	op := e.Operation
	// Byte counters refuse unwrap (LogQL parser rejects it already, but
	// guard at lowering to surface helpful errors if a future parser
	// loosens validation). `count_over_time` is similarly rejected at
	// parse time; we leave no special handling here.
	if e.Left.Unwrap != nil {
		switch op {
		case syntax.OpRangeTypeBytesRate, syntax.OpRangeTypeBytes:
			return nil, fmt.Errorf("logql: %s does not accept `| unwrap`", op)
		}
		// `rate(... | unwrap)` is a sum-of-values rate per Loki's
		// rateLogs(computeValues=true) — the per-row Value IS the
		// unwrapped value, and the windowed-array math sums it and
		// divides by range_seconds (the `log_rate` chsql function).
		// Same shape as `sum_over_time` etc.
		return unwrapValueExpr(e.Left.Unwrap, labelsExpr)
	}

	switch op {
	case syntax.OpRangeTypeRate, syntax.OpRangeTypeCount:
		return &chplan.LitInt{V: 1}, nil
	case syntax.OpRangeTypeBytesRate, syntax.OpRangeTypeBytes:
		return &chplan.FuncCall{
			Name: "toFloat64",
			Args: []chplan.Expr{&chplan.FuncCall{
				Name: "length",
				Args: []chplan.Expr{&chplan.ColumnRef{Name: s.BodyColumn}},
			}},
		}, nil
	}
	// Value-producing ops without an `| unwrap` are a LogQL programming
	// bug — `sum_over_time` / `avg_over_time` / etc. require a number
	// per row.
	switch op {
	case syntax.OpRangeTypeSum, syntax.OpRangeTypeAvg, syntax.OpRangeTypeMin,
		syntax.OpRangeTypeMax, syntax.OpRangeTypeStddev, syntax.OpRangeTypeStdvar,
		syntax.OpRangeTypeQuantile, syntax.OpRangeTypeFirst, syntax.OpRangeTypeLast:
		return nil, fmt.Errorf("logql: %s requires an `| unwrap` clause", op)
	}
	return nil, fmt.Errorf("logql: range op %s is not yet supported", op)
}

// unwrapValueExpr builds the per-row Float64 value expression from an
// UnwrapExpr against labelsExpr (the parser-augmented labels map).
//
// LogQL syntax:
//
//	unwrap foo                       → toFloat64OrZero(labelsExpr['foo'])
//	unwrap duration(foo)             → parseTimeDelta(labelsExpr['foo'])       (Float64 seconds)
//	unwrap duration_seconds(foo)     → parseTimeDelta(labelsExpr['foo'])       (Float64 seconds)
//	unwrap bytes(foo)                → parseReadableSize(labelsExpr['foo'])    (Float64 bytes)
//
// CH's `parseTimeDelta` accepts Loki's Go-duration spec (`1.5s`,
// `200ms`, `1m30s`, `1h`). `parseReadableSize` accepts the
// human-readable byte-size shapes Loki's `humanize.ParseBytes` covers
// (`1KB`, `1.5MiB`, `2 G`). Both return Float64 so the downstream
// windowed-array math stays in Float64 throughout.
func unwrapValueExpr(u *syntax.UnwrapExpr, labelsExpr chplan.Expr) (chplan.Expr, error) {
	if u.Identifier == "" {
		return nil, fmt.Errorf("logql: `| unwrap` has empty identifier")
	}
	access := &chplan.MapAccess{
		Map: labelsExpr,
		Key: &chplan.LitString{V: u.Identifier},
	}
	switch u.Operation {
	case "":
		return &chplan.FuncCall{
			Name: "toFloat64OrZero",
			Args: []chplan.Expr{access},
		}, nil
	case syntax.OpConvDuration, syntax.OpConvDurationSeconds:
		return &chplan.FuncCall{
			Name: "parseTimeDelta",
			Args: []chplan.Expr{access},
		}, nil
	case syntax.OpConvBytes:
		// `parseReadableSize` returns UInt64 (CH 24.x+); wrap in
		// `toFloat64` so the downstream windowed-array math (especially
		// the counter_delta arrayMap that does `if(c < p, c, c - p)`)
		// can resolve a common type — chDB refuses to mix UInt64
		// branches with their signed-subtraction siblings. Aligns with
		// the comment above and matches the `length(Body)` path in
		// `rangeValueExpr` which is also toFloat64-wrapped.
		return &chplan.FuncCall{
			Name: "toFloat64",
			Args: []chplan.Expr{&chplan.FuncCall{
				Name: "parseReadableSize",
				Args: []chplan.Expr{access},
			}},
		}, nil
	}
	return nil, fmt.Errorf("logql: unsupported unwrap conversion %q", u.Operation)
}

// rangeAggregationGroupBy returns the chplan group-key expressions for
// `by (...)` / `without (...)` on a range aggregation. The output is a
// single Map-shaped expression so the downstream Project synthesises a
// per-stream identity that the RangeWindow GROUP BY can collapse to.
//
//	no Grouping        → nil (caller defaults to grouping on RA).
//	`by (k1, k2)`      → map('k1', RA['k1'], 'k2', RA['k2'])
//	`by ()`            → map()       (single all-collapsed group)
//	`without (k1, k2)` → mapFilter((k,v) -> NOT (k IN ('k1','k2')), RA)
func rangeAggregationGroupBy(e *syntax.RangeAggregationExpr, s schema.Logs) (chplan.Expr, error) {
	if e.Grouping == nil {
		return nil, nil
	}
	if e.Grouping.Without {
		return &chplan.MapWithoutKeys{
			Map:  &chplan.ColumnRef{Name: s.ResourceAttributesColumn},
			Keys: append([]string(nil), e.Grouping.Groups...),
		}, nil
	}
	args := make([]chplan.Expr, 0, len(e.Grouping.Groups)*2)
	for _, label := range e.Grouping.Groups {
		args = append(args,
			&chplan.LitString{V: label},
			&chplan.MapAccess{
				Map: &chplan.ColumnRef{Name: s.ResourceAttributesColumn},
				Key: &chplan.LitString{V: label},
			},
		)
	}
	return &chplan.FuncCall{Name: "map", Args: args}, nil
}

// rangeFuncName maps LogQL range ops to the chplan/chsql RangeWindow
// function name.
//
//   - `rate` / `bytes_rate` use the cerberus "log_rate" func (sum /
//     range_seconds — non-counter, vs PromQL's counter "rate"). This is
//     true even for `rate({…} | unwrap)`: the per-sample contribution is
//     still 1, just gated by row presence after parse / filter; the
//     run-time semantic matches Loki's `funcRate` (count of samples /
//     range_seconds).
//   - `count_over_time` reuses PromQL's identical-shape function name.
//   - `bytes_over_time` reuses `sum_over_time` since the per-row Value
//     has already been projected to `length(Body)`.
//   - `sum_over_time` / `avg_over_time` / `min_over_time` /
//     `max_over_time` / `stddev_over_time` / `stdvar_over_time` /
//     `quantile_over_time` reuse PromQL's identical-shape function names
//     — chsql/range_window.go already handles each variant via
//     emitRangeWindowOverTime / emitRangeWindowQuantileOverTime.
func rangeFuncName(op string) (string, error) {
	switch op {
	case syntax.OpRangeTypeRate, syntax.OpRangeTypeBytesRate:
		return "log_rate", nil
	case syntax.OpRangeTypeCount:
		return "count_over_time", nil
	case syntax.OpRangeTypeBytes:
		return "sum_over_time", nil
	case syntax.OpRangeTypeSum,
		syntax.OpRangeTypeAvg,
		syntax.OpRangeTypeMin,
		syntax.OpRangeTypeMax,
		syntax.OpRangeTypeStddev,
		syntax.OpRangeTypeStdvar,
		syntax.OpRangeTypeQuantile:
		return op, nil
	}
	return "", fmt.Errorf("logql: range op %s is not yet supported", op)
}
