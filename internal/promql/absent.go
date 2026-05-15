package promql

import (
	"fmt"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerAbsent implements PromQL `absent(v instant-vector)`. The function
// returns:
//
//   - an empty vector when `v` has any sample matching its label matchers,
//     and
//   - a single 1-row vector whose Value is 1.0 and whose label set is the
//     set of equality matchers explicitly named on `v` (mirroring Prom's
//     `createLabelsForAbsentFunction` in promql/functions.go), when `v`
//     has no matching samples.
//
// The chplan tree:
//
//	Project [synthesised Sample columns]
//	  Filter predicate=(_cerb_n = 0)
//	    Aggregate groupBy=[] funcs=[count() AS _cerb_n]   (DropEmptyOnNoGroup=false)
//	      Filter? predicate=<v's matchers>                (omitted when no
//	                                                       matchers — bare
//	                                                       Scan)
//	        Scan(<table>)
//
// The inner Aggregate sets `DropEmptyOnNoGroup=false` deliberately:
// CH's "1-row-per-aggregate-only-query" semantics emit a single
// `count = 0` row even when the filtered scan is empty, which is
// exactly the signal the outer Filter / Project chain needs to flip
// from "no result" to "synthesised absent row".
//
// Compared to Prom's `funcAbsent`, this lowering checks for the
// existence of *any* sample matching `v`'s matchers in the configured
// metric table — it does not (yet) apply the same instant-vector
// staleness window the bare-selector LWR wrap uses. That tightening is
// safe to layer on later: for the compatibility-harness fixtures and
// the OTel-CH gauge tables, "metric has zero rows in the table" is the
// signal that matters.
func lowerAbsent(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) != 1 {
		return nil, fmt.Errorf("promql: absent() expects 1 argument, got %d", len(c.Args))
	}
	// Unwrap `absent((v))` — the parser surfaces redundant parens as
	// ParenExpr nodes that the bare-selector check below would
	// otherwise reject.
	arg := unwrapParens(c.Args[0])
	vs, ok := arg.(*parser.VectorSelector)
	if !ok {
		return nil, fmt.Errorf("promql: absent() argument must be an instant-vector selector, got %T", c.Args[0])
	}

	// Build the inner filtered Scan via the standard selector
	// lowering with the range-vector flag set: that path skips the
	// LWR wrap and only applies the matchers (plus the @/offset time
	// bound when present). The wrapping Aggregate doesn't need a
	// per-series collapse — it just counts rows.
	//
	// Strip the modifier so the inner Filter doesn't carry a
	// duplicate time-bound predicate; absent() doesn't currently
	// honour the @/offset modifiers (parity with the surrounding
	// instant-vector callsites that the count-only check makes
	// semantically equivalent until LWR is plumbed in).
	vsNoMod := *vs
	vsNoMod.Timestamp = nil
	vsNoMod.OriginalOffset = 0
	vsNoMod.Offset = 0
	vsNoMod.StartOrEnd = 0
	rangeCtx := ctx
	rangeCtx.inRangeVector = true
	inner, err := lowerVectorSelector(&vsNoMod, s, rangeCtx)
	if err != nil {
		return nil, err
	}

	const cntAlias = "_cerb_n"
	agg := &chplan.Aggregate{
		Input:   inner,
		GroupBy: nil,
		AggFuncs: []chplan.AggFunc{{
			Name:  "count",
			Args:  nil,
			Alias: cntAlias,
		}},
		// FALSE so an empty filtered scan still produces the 1-row
		// `count = 0` we need to drive the no-series branch. With TRUE
		// the emitter would wrap the aggregate in `WHERE _cerb_n > 0`,
		// stripping the no-series row before the outer Filter ever
		// sees it.
		DropEmptyOnNoGroup: false,
	}

	// In range mode (ctx.step > 0) fan the 1-row count-check across the
	// query_range step grid via CROSS JOIN. The Cartesian product is
	// either N rows × 1 row (absent → emit N samples) or N rows × 0
	// rows (present → emit nothing). The outer Filter still operates
	// on `_cerb_n`; with CH 24.x CROSS JOIN the right side's columns
	// are visible to the outer SELECT.
	//
	// In instant mode the existing 1-row aggregate suffices: a single
	// sample at the eval anchor with Value = 1 / no rows when present.
	var filterInput chplan.Node = agg
	var timeExpr chplan.Expr
	if ctx.step > 0 {
		filterInput = &chplan.CrossJoin{
			Left:  &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step},
			Right: agg,
		}
		timeExpr = &chplan.ColumnRef{Name: "anchor_ts"}
	} else if !ctx.end.IsZero() {
		timeExpr = anchorBaseExpr(evalAnchor{End: ctx.end.UTC()})
	} else {
		timeExpr = anchorBaseExpr(evalAnchor{})
	}

	onlyEmpty := &chplan.Filter{
		Input: filterInput,
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: cntAlias},
			Right: &chplan.LitInt{V: 0},
		},
	}

	// Synthesise the canonical Sample-row contract:
	//   MetricName=''                                  (absent drops __name__)
	//   Attributes=map(<eq-matchers from v>)           (Prom funcAbsent label rule)
	//   TimeUnix=<eval anchor>                         (now64(9) or anchor_ts)
	//   Value=toFloat64(1)                             (Prom's spec value)
	//
	// The Value expression is wrapped in `toFloat64(...)` because the
	// clickhouse-go/v2 driver renders Go `float64(1.0)` as the SQL
	// literal `1` (no decimal), and CH narrows that to `UInt8`. The
	// downstream cursor scans into `*float64`, and the driver refuses
	// to convert UInt8 → *float64 (errors with `converting UInt8 to
	// *float64 is unsupported`). Wrapping in `toFloat64(?)` forces
	// CH to project Float64 on the wire regardless of the bound
	// literal's inferred type.
	return &chplan.Project{
		Input: onlyEmpty,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: absentAttrsMap(vs.LabelMatchers), Alias: s.AttributesColumn},
			{Expr: timeExpr, Alias: s.TimestampColumn},
			{Expr: &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{&chplan.LitFloat{V: 1.0}}}, Alias: s.ValueColumn},
		},
	}, nil
}

// lowerAbsentOverTime implements PromQL `absent_over_time(v[range])`.
// The function emits the absence-indicator series whose labels are
// derived from the input vector-selector's equality matchers (same
// rule as instant `absent(...)`, mirroring Prom's
// `createLabelsForAbsentFunction`). Samples are placed at every
// per-step anchor whose `(anchor - range, anchor]` lookback window
// has zero matching samples in the table; anchors with any matching
// sample contribute no output.
//
// Lowered shape:
//
//	AbsentOverTime synth=<matcher-eq-set> range=<R> step=<step>
//	               start=<start> end=<end> offset=<offset>
//	  Filter? predicate=<v's matchers without the time bound>
//	    Scan(<table>)
//
// The chsql emitter for AbsentOverTime renders the per-anchor lookback
// check via a groupArray of the per-sample timestamps + arrayCount(t
// -> in_window, sample_ts_arr). See
// internal/chsql/absent_over_time.go for the SQL skeleton.
//
// Cerberus previously routed `absent_over_time` through the regular
// per-series RangeWindow path which emitted `if(length(window_vals) >
// 0, NaN, 1.0)` per (series, anchor) — wrong labels (original series
// labels, not the matcher-synthesised set) AND wrong shape (per-series
// NaN rows the matrix pivot didn't drop). Bucket 4 of
// docs/compat-residual-audit-25898791664.md attributes 6 compat lane
// diffs to this divergence; replacing the lowering with the dedicated
// AbsentOverTime node closes those diffs by emitting Prom's
// single-synthesised-series shape directly.
func lowerAbsentOverTime(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) != 1 {
		return nil, fmt.Errorf("promql: absent_over_time() expects 1 argument, got %d", len(c.Args))
	}
	ms, ok := c.Args[0].(*parser.MatrixSelector)
	if !ok {
		return nil, fmt.Errorf("promql: absent_over_time() argument must be a range-vector selector, got %T", c.Args[0])
	}
	vs, ok := ms.VectorSelector.(*parser.VectorSelector)
	if !ok {
		return nil, fmt.Errorf("promql: matrix selector's inner must be a VectorSelector, got %T", ms.VectorSelector)
	}

	anchor, err := anchorFromSelector(vs, ctx)
	if err != nil {
		return nil, err
	}

	// Build the inner filtered Scan with the modifier stripped — the
	// AbsentOverTime node embeds its own per-anchor window bound, and
	// any duplicate time predicate on the inner Filter would over-narrow
	// the global prefilter the emitter applies above the inner scan.
	vsNoMod := *vs
	vsNoMod.Timestamp = nil
	vsNoMod.OriginalOffset = 0
	vsNoMod.Offset = 0
	vsNoMod.StartOrEnd = 0
	rangeCtx := ctx
	rangeCtx.inRangeVector = true
	inner, err := lowerVectorSelector(&vsNoMod, s, rangeCtx)
	if err != nil {
		return nil, err
	}

	// Resolve the eval anchor: a non-zero `@`/`@start`/`@end` modifier
	// pins `anchor.End` directly; otherwise fall through to ctx.end (the
	// query's eval time for instant queries). Zero ctx.end + zero
	// anchor.End falls back to CH's `now64(9)` at emit time — mirrors
	// the lowerAbsent instant-mode contract.
	endTime := anchor.End
	if endTime.IsZero() && !ctx.end.IsZero() {
		endTime = ctx.end.UTC()
	}

	a := &chplan.AbsentOverTime{
		Input:            inner,
		SynthLabels:      synthLabelsFromMatchers(vs.LabelMatchers),
		Range:            ms.Range,
		End:              endTime,
		Offset:           anchor.Offset,
		TimestampColumn:  s.TimestampColumn,
		ValueColumn:      s.ValueColumn,
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
	}
	// In range mode (Pool-AK's per-anchor query_range plumbing) fan the
	// per-anchor lookback check across the request's step grid. The
	// emitter pivots between the single-anchor (Step == 0) and
	// arrayJoin-fanout (Step > 0) shapes via this Step value.
	if ctx.step > 0 && !ctx.start.IsZero() && !ctx.end.IsZero() {
		a.Start = ctx.start.UTC()
		a.End = ctx.end.UTC()
		a.Step = ctx.step
	}
	return a, nil
}

// synthLabelsFromMatchers builds the matcher-derived label set Prom's
// funcAbsentOverTime / funcAbsent lift onto the synthesised output
// series. Reuses the same rules `absentAttrsMap` applies but returns
// the ordered (key, value) pair list directly so the chsql emitter can
// thread each kv through `Lit(...)` for `?`-bound rendering.
//
// Skip `__name__`; include only equality matchers; drop labels appearing
// more than once on the same name (`absent_over_time(x{job="a",
// job="b"}[5m])` → `{}`).
func synthLabelsFromMatchers(matchers []*labels.Matcher) []chplan.SynthLabel {
	has := make(map[string]bool, len(matchers))
	order := make([]string, 0, len(matchers))
	values := make(map[string]string, len(matchers))
	for _, m := range matchers {
		if m.Name == model.MetricNameLabel {
			continue
		}
		if m.Type != labels.MatchEqual {
			if has[m.Name] {
				delete(values, m.Name)
			}
			continue
		}
		if has[m.Name] {
			delete(values, m.Name)
			continue
		}
		has[m.Name] = true
		values[m.Name] = m.Value
		order = append(order, m.Name)
	}
	out := make([]chplan.SynthLabel, 0, len(order))
	for _, name := range order {
		v, ok := values[name]
		if !ok {
			continue
		}
		out = append(out, chplan.SynthLabel{Key: name, Value: v})
	}
	return out
}

// absentAttrsMap renders the absent-output label set as a CH
// Map(String, String) literal, mirroring Prom's
// `createLabelsForAbsentFunction` rules:
//
//   - skip `__name__` (absent always drops the metric name);
//   - include only equality matchers (regex / not-equal don't pin a
//     unique label value, so Prom skips them);
//   - duplicate-name matchers (`up{job="a", job="b"}`) drop the
//     label entirely — the `has` map tracks the first name seen and
//     a second occurrence triggers a delete.
//
// Returns the canonical empty-map literal (`CAST(map(), 'Map(String,
// String)')`) when no eligible equality matchers exist, matching the
// shape used by time() / vector() / aggregations-without-by.
func absentAttrsMap(matchers []*labels.Matcher) chplan.Expr {
	type kv struct {
		k, v string
	}
	has := make(map[string]bool, len(matchers))
	order := make([]string, 0, len(matchers))
	values := make(map[string]string, len(matchers))
	for _, m := range matchers {
		if m.Name == model.MetricNameLabel {
			continue
		}
		if m.Type != labels.MatchEqual {
			// Non-equality matchers don't pin a unique value; drop
			// the label so the output reflects "only the explicitly-
			// set equality matchers" rule.
			if has[m.Name] {
				delete(values, m.Name)
			}
			continue
		}
		if has[m.Name] {
			// Duplicate name with equality matchers — Prom drops the
			// label entirely. (`absent(x{job="a",job="b"})` returns
			// `{} 1`.)
			delete(values, m.Name)
			continue
		}
		has[m.Name] = true
		values[m.Name] = m.Value
		order = append(order, m.Name)
	}
	pairs := make([]kv, 0, len(order))
	for _, name := range order {
		v, ok := values[name]
		if !ok {
			continue
		}
		pairs = append(pairs, kv{k: name, v: v})
	}
	if len(pairs) == 0 {
		return emptyAttrsMap()
	}
	args := make([]chplan.Expr, 0, len(pairs)*2)
	for _, p := range pairs {
		args = append(args, &chplan.LitString{V: p.k}, &chplan.LitString{V: p.v})
	}
	return &chplan.FuncCall{Name: "map", Args: args}
}

// unwrapParens peels off any wrapping ParenExpr nodes. PromQL's parser
// preserves syntactic parens as ParenExpr; absent() expects a bare
// instant-vector selector underneath but the user may write
// `absent((up))`.
func unwrapParens(e parser.Expr) parser.Expr {
	for {
		p, ok := e.(*parser.ParenExpr)
		if !ok {
			return e
		}
		e = p.Expr
	}
}
