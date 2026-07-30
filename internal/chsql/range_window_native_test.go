package chsql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// deferredShapingGrid is the query_range grid every deferred-shaping case below
// emits over: a 5-minute window at a 30-second step, whole-second aligned so
// the rendered DateTime literals are stable.
func deferredShapingGrid() (start, end time.Time, step, window time.Duration) {
	start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return start, start.Add(5 * time.Minute), 30 * time.Second, 5 * time.Minute
}

// sanitizedAttributeMap rebuilds the OTel -> Prometheus label-name
// sanitisation of one attribute Map: every key run through replaceRegexpAll,
// the values carried across unchanged.
func sanitizedAttributeMap(src chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Name: "mapFromArrays", Args: []chplan.Expr{
		&chplan.FuncCall{Name: "arrayMap", Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{"k"}, Body: &chplan.FuncCall{
				Name: "replaceRegexpAll",
				Args: []chplan.Expr{
					&chplan.BareIdent{Name: "k"},
					&chplan.InlineString{V: `[^a-zA-Z0-9_]`},
					&chplan.InlineString{V: "_"},
				},
			}},
			&chplan.FuncCall{Name: "mapKeys", Args: []chplan.Expr{src}},
		}},
		&chplan.FuncCall{Name: "mapValues", Args: []chplan.Expr{src}},
	}}
}

// deferredShapingTower rebuilds the OTel -> Prometheus label-shaping expression
// the PromQL lowering defers past the aggregate:
//
//	mapSort(mapConcat(mapUpdate(<sanitised ResourceAttributes>, Attributes),
//	                  map('service_name', toString(ServiceName))))
//
// It reads three raw columns (ResourceAttributes, Attributes, ServiceName) and
// is non-injective, which is the whole reason the re-collapse has to go through
// the -State / -Merge pair rather than through arithmetic on finished grids.
func deferredShapingTower() chplan.Expr {
	return &chplan.FuncCall{Name: chplan.CanonicalMapFunc, Args: []chplan.Expr{
		&chplan.FuncCall{Name: "mapConcat", Args: []chplan.Expr{
			&chplan.FuncCall{Name: "mapUpdate", Args: []chplan.Expr{
				sanitizedAttributeMap(&chplan.ColumnRef{Name: "ResourceAttributes"}),
				&chplan.ColumnRef{Name: "Attributes"},
			}},
			&chplan.FuncCall{Name: "map", Args: []chplan.Expr{
				&chplan.InlineString{V: "service_name"},
				&chplan.FuncCall{Name: "toString", Args: []chplan.Expr{
					&chplan.ColumnRef{Name: "ServiceName"},
				}},
			}},
		}},
	}}
}

// deferredShapingNode builds the hoisted rate node: grouped on the RAW identity
// columns, with the shaping tower deferred to a Recollapse projection reporting
// under the output name `Attributes`. MetricName is the pass-through identity
// key — no shaping expression reads it, so all three levels must carry it
// verbatim or two metrics pool into one series.
func deferredShapingNode() *chplan.RangeWindowNative {
	start, end, step, window := deferredShapingGrid()
	return &chplan.RangeWindowNative{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "rate",
		Range:           window,
		Step:            step,
		Start:           start,
		End:             end,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: "MetricName"},
			&chplan.ColumnRef{Name: "Attributes"},
			&chplan.ColumnRef{Name: "ResourceAttributes"},
			&chplan.ColumnRef{Name: "ServiceName"},
		},
		Recollapse: []chplan.Projection{
			{Expr: deferredShapingTower(), Alias: "Attributes"},
		},
	}
}

// deferredShapingSQL is the whole three-level rendering, pinned as one string
// rather than as substring probes: a substring assertion passes just as happily
// when the merge level groups on the wrong key, when a pass-through identity
// column is dropped from a level, or when the two parametric tuples drift
// apart — each of which is a silent wrong answer, not a syntax error.
//
// Split at the level boundaries only. What each line encodes:
//
//   - outer: `MetricName` bare plus `shaped_key_0` renamed to its Alias, then
//     the verbatim ARRAY JOIN / IS NOT NULL / toFloat64(assumeNotNull(…)) tail
//     the two-level shape emits — the wrapping Aggregate sees the same row
//     shape either way;
//   - middle: the shaping tower under the SYNTHETIC `shaped_key_0` alias (NOT
//     `Attributes`: aliasing it to the output name would rebind the GROUP BY to
//     the shaped column, so ClickHouse would compute the pre-hoist thing at the
//     pre-hoist cost while still answering correctly — a silently inert
//     rewrite), a …ToGridMerge over the ONE state column, and a GROUP BY over
//     the pass-through `MetricName` plus `shaped_key_0`;
//   - inner: GROUP BY the four RAW identity columns, …ToGridState over
//     (TimeUnix, Value) with the IDENTICAL (start, end, step, window) tuple the
//     merge carries, plus the unchanged scan time bound.
const deferredShapingSQL = "SELECT `MetricName`, `shaped_key_0` AS `Attributes`, toDateTime64(`anchor_ts`, 9) AS `anchor_ts`, toDateTime64(`anchor_ts`, 9) AS `TimeUnix`, toFloat64(assumeNotNull(`grid_val`)) AS `Value` FROM (" +
	"SELECT `MetricName`, mapSort(mapConcat(mapUpdate(mapFromArrays(arrayMap(k -> replaceRegexpAll(k, '[^a-zA-Z0-9_]', '_'), mapKeys(`ResourceAttributes`)), mapValues(`ResourceAttributes`)), `Attributes`), map('service_name', toString(`ServiceName`)))) AS `shaped_key_0`, timeSeriesRateToGridMerge(toDateTime(1767225600, 'UTC'), toDateTime(1767225900, 'UTC'), 30, 300)(`grid_state`) AS `grid`, timeSeriesRange(toDateTime(1767225600, 'UTC'), toDateTime(1767225900, 'UTC'), 30) AS `grid_ts` FROM (" +
	"SELECT `MetricName`, `Attributes`, `ResourceAttributes`, `ServiceName`, timeSeriesRateToGridState(toDateTime(1767225600, 'UTC'), toDateTime(1767225900, 'UTC'), 30, 300)(`TimeUnix`, `Value`) AS `grid_state` FROM (SELECT * FROM `otel_metrics_sum`) WHERE `TimeUnix` > toDateTime64('2026-01-01 00:00:00.000000000', 9) - toIntervalNanosecond(300000000000) AND `TimeUnix` <= toDateTime64('2026-01-01 00:05:00.000000000', 9) GROUP BY `MetricName`, `Attributes`, `ResourceAttributes`, `ServiceName`) " +
	"GROUP BY `MetricName`, `shaped_key_0`) ARRAY JOIN `grid` AS `grid_val`, `grid_ts` AS `anchor_ts` WHERE `grid_val` IS NOT NULL"

// TestEmitRangeWindowNative_DeferredShaping pins the whole three-level shape,
// SQL and args both. The args assertion is not incidental: collectGroupByFrags
// appends a captured expression's args to the emitter's slice at COLLECT time
// while returning SQL-only frags, so a shaping expression collected in the
// wrong order relative to the rendered SQL would bind its parameters to the
// wrong placeholders.
func TestEmitRangeWindowNative_DeferredShaping(t *testing.T) {
	t.Parallel()

	sql, args, err := Emit(context.Background(), deferredShapingNode())
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if sql != deferredShapingSQL {
		t.Errorf("emitted SQL\n got: %s\nwant: %s", sql, deferredShapingSQL)
	}
	if len(args) != 0 {
		t.Errorf("emitted args = %#v, want none — every literal in this shape renders inline", args)
	}
}

// deferredShapingTwoKeyNode is deferredShapingNode with a SECOND shaped key —
// the sanitised scope-attribute map under its own output name. ScopeAttributes
// joins GroupBy because the containment invariant requires the inner level to
// project every column the shaping reads; MetricName stays the pass-through key,
// so the emitted key order is the pass-through key followed by the two shaped
// ones at both the merge and the outer level.
func deferredShapingTwoKeyNode() *chplan.RangeWindowNative {
	r := deferredShapingNode()
	r.GroupBy = append(r.GroupBy, &chplan.ColumnRef{Name: "ScopeAttributes"})
	r.Recollapse = append(r.Recollapse, chplan.Projection{
		Expr: &chplan.FuncCall{Name: chplan.CanonicalMapFunc, Args: []chplan.Expr{
			sanitizedAttributeMap(&chplan.ColumnRef{Name: "ScopeAttributes"}),
		}},
		Alias: "ScopeAttributes",
	})
	return r
}

// deferredShapingTwoKeySQL is the same three-level rendering with TWO shaped
// keys. What only this pinning covers, because a single-projection node renders
// neither: the per-key `shaped_key_<i>` numbering (both aliases distinct and
// assigned in Recollapse order), and the ordering of the two shaped keys against
// each other and against the pass-through key in the middle level's SELECT list
// and GROUP BY, the outer level's SELECT list, and the rename back to each
// key's own output name. A swap there hands one output column the other key's
// map — a wrong label set on every row, with no error.
const deferredShapingTwoKeySQL = "SELECT `MetricName`, `shaped_key_0` AS `Attributes`, `shaped_key_1` AS `ScopeAttributes`, toDateTime64(`anchor_ts`, 9) AS `anchor_ts`, toDateTime64(`anchor_ts`, 9) AS `TimeUnix`, toFloat64(assumeNotNull(`grid_val`)) AS `Value` FROM (" +
	"SELECT `MetricName`, mapSort(mapConcat(mapUpdate(mapFromArrays(arrayMap(k -> replaceRegexpAll(k, '[^a-zA-Z0-9_]', '_'), mapKeys(`ResourceAttributes`)), mapValues(`ResourceAttributes`)), `Attributes`), map('service_name', toString(`ServiceName`)))) AS `shaped_key_0`, mapSort(mapFromArrays(arrayMap(k -> replaceRegexpAll(k, '[^a-zA-Z0-9_]', '_'), mapKeys(`ScopeAttributes`)), mapValues(`ScopeAttributes`))) AS `shaped_key_1`, timeSeriesRateToGridMerge(toDateTime(1767225600, 'UTC'), toDateTime(1767225900, 'UTC'), 30, 300)(`grid_state`) AS `grid`, timeSeriesRange(toDateTime(1767225600, 'UTC'), toDateTime(1767225900, 'UTC'), 30) AS `grid_ts` FROM (" +
	"SELECT `MetricName`, `Attributes`, `ResourceAttributes`, `ServiceName`, `ScopeAttributes`, timeSeriesRateToGridState(toDateTime(1767225600, 'UTC'), toDateTime(1767225900, 'UTC'), 30, 300)(`TimeUnix`, `Value`) AS `grid_state` FROM (SELECT * FROM `otel_metrics_sum`) WHERE `TimeUnix` > toDateTime64('2026-01-01 00:00:00.000000000', 9) - toIntervalNanosecond(300000000000) AND `TimeUnix` <= toDateTime64('2026-01-01 00:05:00.000000000', 9) GROUP BY `MetricName`, `Attributes`, `ResourceAttributes`, `ServiceName`, `ScopeAttributes`) " +
	"GROUP BY `MetricName`, `shaped_key_0`, `shaped_key_1`) ARRAY JOIN `grid` AS `grid_val`, `grid_ts` AS `anchor_ts` WHERE `grid_val` IS NOT NULL"

// TestEmitRangeWindowNative_DeferredShapingTwoShapedKeys pins the multi-key
// arm of the same emitter, whole SQL string and args both.
func TestEmitRangeWindowNative_DeferredShapingTwoShapedKeys(t *testing.T) {
	t.Parallel()

	sql, args, err := Emit(context.Background(), deferredShapingTwoKeyNode())
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if sql != deferredShapingTwoKeySQL {
		t.Errorf("emitted SQL\n got: %s\nwant: %s", sql, deferredShapingTwoKeySQL)
	}
	if len(args) != 0 {
		t.Errorf("emitted args = %#v, want none — every literal in this shape renders inline", args)
	}
}

// TestEmitRangeWindowNative_DeferredShapingRejections covers every guard the
// three-level emitter runs BEFORE it renders anything. Each case asserts both
// ErrUnsupported (so the HTTP layer reports it as an unsupported query rather
// than a 500) and a substring unique to that guard's message, so one guard
// cannot masquerade as another — a node rejected for the wrong reason would
// otherwise look like passing coverage for a guard that no longer fires.
func TestEmitRangeWindowNative_DeferredShapingRejections(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		mutate  func(*chplan.RangeWindowNative)
		wantMsg string
	}{
		{
			// changes/resets/deriv/predict_linear have no -State/-Merge pair
			// registered, so the re-collapse cannot be deferred; emitting the
			// two-level shape instead would answer with split raw series.
			name:    "func without state/merge pair",
			mutate:  func(r *chplan.RangeWindowNative) { r.Func = "changes" },
			wantMsg: "has no -State/-Merge pair",
		},
		{
			name:    "empty alias",
			mutate:  func(r *chplan.RangeWindowNative) { r.Recollapse[0].Alias = "" },
			wantMsg: "has an empty Alias",
		},
		{
			name: "duplicate alias",
			mutate: func(r *chplan.RangeWindowNative) {
				r.Recollapse = append(r.Recollapse, chplan.Projection{
					Expr:  deferredShapingTower(),
					Alias: r.Recollapse[0].Alias,
				})
			},
			wantMsg: "is not unique",
		},
		{
			name: "computed GroupBy key",
			mutate: func(r *chplan.RangeWindowNative) {
				r.GroupBy[1] = &chplan.FuncCall{Name: chplan.CanonicalMapFunc, Args: []chplan.Expr{
					&chplan.ColumnRef{Name: "Attributes"},
				}}
			},
			wantMsg: "requires plain column GroupBy keys",
		},
		{
			// The tower reads ServiceName; dropping it from GroupBy leaves the
			// inner level not projecting it, so the middle level's shaping
			// expression would reference an unknown identifier.
			name: "shaping reads an ungrouped column",
			mutate: func(r *chplan.RangeWindowNative) {
				r.GroupBy = r.GroupBy[:len(r.GroupBy)-1]
			},
			wantMsg: `reads column "ServiceName" which is not a GroupBy key`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			node := deferredShapingNode()
			tc.mutate(node)

			sql, _, err := Emit(context.Background(), node)
			if err == nil {
				t.Fatalf("Emit accepted an unemittable deferred-shaping node and produced:\n%s", sql)
			}
			if !errors.Is(err, ErrUnsupported) {
				t.Errorf("Emit error = %v, want ErrUnsupported", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("Emit error = %q, want it to mention %q — a different guard fired, so this case "+
					"proves nothing about the one it names", err, tc.wantMsg)
			}
		})
	}
}
