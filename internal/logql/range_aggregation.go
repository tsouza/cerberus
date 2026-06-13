package logql

import (
	"fmt"

	loglib "github.com/grafana/loki/v3/pkg/logql/log"
	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/grafana/loki/v3/pkg/logqlmodel"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"

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
//   - unwrap duration / duration_seconds(field)                     → regex-gated Go-duration parse (0 on error)
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

	// `absent_over_time` is not a per-series window aggregation — it
	// synthesises ONE matcher-derived series whose samples sit at the
	// anchors where the inner selector produced no samples at all.
	// Route it to the dedicated AbsentOverTime plan node before the
	// per-series RangeWindow machinery below.
	if e.Operation == syntax.OpRangeTypeAbsent {
		return lowerAbsentOverTime(e, s, lc)
	}

	// Extend the pre-scan timestamp clamp's left bound by the range
	// aggregation's [interval] (plus offset, if any) so the leftmost
	// anchors of a matrix-mode evaluation see the full
	// `(anchor_ts - range, anchor_ts]` window rather than a truncated
	// `[start, anchor_ts]` slice. Mirrors reference Loki / Prom, which
	// have no equivalent pre-scan clamp. See lowerCtx.withMatcherWindowExtension.
	innerLc := lc.withMatcherWindowExtension(e.Left.Interval + e.Left.Offset)
	inner, labelsExpr, err := lowerLogRange(e.Left, s, innerLc)
	if err != nil {
		return nil, err
	}

	inner, labelsExpr, unwrapHasErrorMarks, err := applyUnwrapRowSemantics(e, s, inner, labelsExpr)
	if err != nil {
		return nil, err
	}

	value, err := rangeValueExpr(e, s, labelsExpr)
	if err != nil {
		return nil, err
	}

	// Default identity: the raw ResourceAttributes column, augmented with
	// the synthesized `detected_level` label so each distinct severity
	// becomes its own series. Loki's stream-identity contract includes
	// detected_level as a structural dimension whenever the upstream row
	// carries severity metadata; the RangeWindow's GROUP BY (keyed on
	// the ResourceAttributesColumn alias) then collapses one row per
	// (stream, detected_level) tuple — matching upstream Loki's matrix
	// shape (16 of the 19 loki-compat failures came from cerberus
	// collapsing 4 levels into 1 series here).
	//
	// Unwrap variant: when the range aggregation carries an `| unwrap`
	// clause and no explicit `by/without` grouping, the series identity
	// MUST include every parser-extracted label EXCEPT the unwrap
	// target — Loki's `LabelExtractorWithStages` treats no-grouping as
	// `without (unwrapIdent)`, so each unique (parser-extracted-keys
	// minus target) labelset becomes its own series. Without this the
	// outer matrix collapses every distinct combination into a single
	// detected-level series — the symptom is `matrix length: expected=
	// 1440 actual=4` against the loki-compat 24h unwrap-aggregations
	// corpus (each minute of seeded data produces a unique post-parser
	// labelset because the JSON/logfmt payload carries varying fields
	// like `request_id`, `status`, `user_agent`).
	//
	// Two-stage Project for the unwrap+parser shape: ClickHouse rejects a
	// `mapFilter((k, v) -> NOT (k IN [...]), mapConcat(RA, mapApply(...)))`
	// composition with `Recursive lambda ... (UNSUPPORTED_METHOD)` because
	// the inner `mapApply` lambda references the outer `ResourceAttributes`
	// column — CH disallows lambdas-inside-lambda-source when the inner
	// lambda escapes its scope. We materialise the parser-merged labels
	// into an intermediate `_logql_merged_labels` column via an inner
	// Project, then the outer Project applies the strip + detected_level
	// wrap against a plain column reference (no nested lambda). The
	// matching value-expression also reads from the materialised column
	// so the per-row unwrap value stays consistent with the identity.
	innerNode := inner
	identityBase := chplan.Expr(&chplan.ColumnRef{Name: s.ResourceAttributesColumn})
	valueExpr := value
	var errorBypassLabels chplan.Expr
	if e.Left.Unwrap != nil && hasParserMergedLabels(labelsExpr, s) {
		const mergedAlias = "_logql_merged_labels"
		projections := []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.ResourceAttributesColumn}},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
			{Expr: &chplan.ColumnRef{Name: s.BodyColumn}},
			{Expr: &chplan.ColumnRef{Name: s.SeverityColumn}},
			{Expr: labelsExpr, Alias: mergedAlias},
		}
		// Carry the structured-metadata (LogAttributes) column through the
		// materialise step so the downstream identity wrap
		// ([withDetectedLevelAndColumns]) can still coalesce a non-top-level
		// outer-by key (e.g. `by (namespace)`) from it — without this the
		// outer identity references `LogAttributes`, which the intermediate
		// Project would have dropped, and CH aborts with `Unknown
		// expression or function identifier 'LogAttributes'` (task #59).
		if s.AttributesColumn != "" {
			projections = append(projections, chplan.Projection{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}})
		}
		innerNode = &chplan.Project{
			Input:       inner,
			Projections: projections,
		}
		mergedCol := &chplan.ColumnRef{Name: mergedAlias}
		identityBase = &chplan.MapWithoutKeys{Map: mergedCol, Keys: []string{e.Left.Unwrap.Identifier}}
		valueExpr, err = rangeValueExprFromMerged(e, mergedCol)
		if err != nil {
			return nil, err
		}
		if unwrapHasErrorMarks {
			errorBypassLabels = mergedCol
		}
	} else if e.Left.Unwrap != nil {
		// No parser stage (or empty merge): the unwrap target is a
		// stream label that already lives in ResourceAttributes. Strip
		// it directly — CH handles `mapFilter(NOT IN, RA)` natively
		// since RA is a plain column reference.
		identityBase = &chplan.MapWithoutKeys{Map: labelsExpr, Keys: []string{e.Left.Unwrap.Identifier}}
	}
	// The grouping key is computed AGAINST identityBase — the full
	// series identity (parser-merged labels minus the unwrap target
	// when applicable) — not against the raw ResourceAttributes column.
	// `without (service_name)` on `avg_over_time({...} | logfmt |
	// unwrap duration_seconds(duration) [5m])` must keep every
	// logfmt-extracted label (level, status, ...) in the series
	// identity; grouping on RA-minus-keys silently dropped the
	// extracted labels and the enclosing `max by (level)` collapsed the
	// matrix to a single empty-level series (loki-compat
	// exhaustive/aggregations.yaml#Max avg duration by level without
	// service_name).
	groupBy, err := rangeAggregationGroupBy(e, s, identityBase)
	if err != nil {
		return nil, err
	}

	identityProj := chplan.Projection{
		Expr:  withDetectedLevelAndColumns(s, identityBase, lc.OuterByLabels),
		Alias: s.ResourceAttributesColumn,
	}
	if groupBy != nil {
		// With `by (...)` / `without (...)` the inner Project replaces
		// the per-stream identity with the group-key map so the
		// RangeWindow GROUP BY (still keyed on the
		// ResourceAttributesColumn) collapses per-group rather than
		// per-stream. The user-supplied grouping is authoritative: we
		// do NOT auto-inject detected_level on top of an explicit
		// `by (...)` / `without (...)` clause — that would defeat the
		// caller's intent. The alias matches the column name the outer
		// RangeWindow expects.
		identityProj = chplan.Projection{Expr: groupBy, Alias: s.ResourceAttributesColumn}
	}
	if errorBypassLabels != nil {
		identityProj = chplan.Projection{
			Expr:  errorBypassIdentityExpr(s, errorBypassLabels, identityProj.Expr),
			Alias: s.ResourceAttributesColumn,
		}
	}
	projections := []chplan.Projection{
		identityProj,
		{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
		{Expr: valueExpr, Alias: rangeAggSynthValueColumn},
	}
	projected := &chplan.Project{
		Input:       innerNode,
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
	// Range mode: fan the range function across the request's step grid
	// so each anchor in [Start, End] (spaced by Step) emits one row per
	// series with the per-anchor function value. Without this, the
	// emitter anchors at `now64(9)` — a query whose [start, end] window
	// lies outside the last 5 minutes of wall-clock (e.g. compatibility
	// harnesses seeding day-old data) yields an empty matrix because the
	// windowed-array filter
	// `arrayFilter(p -> tupleElement(p,1) > now64(9) - <range>, ...)`
	// drops every sample. Mirrors the PromQL side in lowerMatrixCall
	// (internal/promql/lower.go::lowerMatrixCall) which sets the same
	// fields when ctx.step > 0.
	//
	// This applies UNIFORMLY across both the bare-selector matrix
	// shapes (`count_over_time({...}[5m])`, `rate({...}[5m])`) and the
	// pipeline / unwrap matrix shapes (`sum_over_time({...} | logfmt |
	// unwrap duration [5m])`, `avg_over_time({...} | json | unwrap
	// duration_ms [5m])`, `min_over_time` / `max_over_time` / `rate` on
	// unwrapped values, etc.). The single propagation block above runs
	// after lowerLogRange returns — which collapses both the
	// MatchersExpr-only path and the PipelineExpr-with-unwrap path into
	// the same `inner` Node — so the matrix RangeWindow shape is
	// guaranteed for every range-aggregation flavour the lowering
	// supports. The unwrap-PostFilter wrap above mutates `inner` into a
	// Filter(...) but does NOT touch `rw`, so the matrix-shape fields
	// stay set regardless of how many post-filters the unwrap clause
	// carries. See [TestLowerRangeAggregationMatrixShapeUnwrap] for the
	// pin against a regression that re-introduces the day-old-data
	// instant-anchor bug for the unwrap path.
	if lc.rangeMode() {
		rw.Start = lc.Start.UTC()
		rw.End = lc.End.UTC()
		rw.Step = lc.Step
		rw.OuterRange = lc.End.Sub(lc.Start)
	}
	if e.Operation == syntax.OpRangeTypeQuantile {
		if e.Params == nil {
			return nil, fmt.Errorf("logql: quantile_over_time requires a phi parameter")
		}
		rw.Scalars = []float64{*e.Params}
	}
	return rw, nil
}

// lowerAbsentOverTime implements LogQL `absent_over_time(<log-range>)`.
//
// Reference semantics (pkg/logql/evaluator.go::AbsentRangeVectorEvaluator
// + absentLabels): per step anchor, when the inner log range — selector
// plus pipeline stages plus the optional `| unwrap` extraction —
// contributes ZERO samples in the `(anchor - range, anchor]` lookback
// window, emit one sample with value 1 whose label set is derived from
// the stream-selector matchers (equality matchers kept first-seen;
// any label with a non-equality matcher or a duplicate occurrence is
// dropped entirely). Anchors with at least one sample contribute no
// output.
//
// The lowering reuses chplan.AbsentOverTime — the head-agnostic plan
// node PromQL's absent_over_time introduced (see
// internal/chsql/absent_over_time.go for the SQL skeleton). The inner
// pipeline-lowered node is wrapped in a 1-column Project aliasing the
// logs timestamp column to the canonical `TimeUnix`, so the node's
// TimestampColumn slot serves both its input-read and output-alias
// roles with the same name and the output lands in the canonical
// Sample 4-column shape [Lang.ProjectSamples] forwards verbatim.
//
// `| unwrap` participation: reference Loki extracts the sample BEFORE
// the absence check, and an empty (or absent) unwrap source drops the
// row (streamLabelSampleExtractor.Process) — so a window whose every
// row has an empty unwrap value IS absent. [applyUnwrapRowSemantics]
// folds exactly that contract (plus unwrap post-filters) onto the
// inner node; the conversion-error keep-with-mark half doesn't affect
// row presence, so the labels/marks outputs are deliberately unused.
func lowerAbsentOverTime(e *syntax.RangeAggregationExpr, s schema.Logs, lc lowerCtx) (chplan.Node, error) {
	innerLc := lc.withMatcherWindowExtension(e.Left.Interval + e.Left.Offset)
	inner, labelsExpr, err := lowerLogRange(e.Left, s, innerLc)
	if err != nil {
		return nil, err
	}
	inner, _, _, err = applyUnwrapRowSemantics(e, s, inner, labelsExpr)
	if err != nil {
		return nil, err
	}

	const tsAlias = "TimeUnix"
	a := &chplan.AbsentOverTime{
		Input: &chplan.Project{
			Input: inner,
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: tsAlias},
			},
		},
		SynthLabels:      absentSynthLabels(e.Left.Left),
		Range:            e.Left.Interval,
		Offset:           e.Left.Offset,
		TimestampColumn:  tsAlias,
		ValueColumn:      rangeAggSynthValueColumn,
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
	}
	if lc.rangeMode() {
		a.Start = lc.Start.UTC()
		a.End = lc.End.UTC()
		a.Step = lc.Step
	} else if !lc.End.IsZero() {
		a.End = lc.End.UTC()
	}
	return a, nil
}

// absentSynthLabels derives the synthesised label set reference Loki
// lifts onto absent_over_time's output series — pkg/logql/evaluator.go
// ::absentLabels: walk the stream-selector matchers; an equality
// matcher pins its (name, value) on first sight; ANY other occurrence
// of the name — non-equality matcher, or a second matcher on the same
// name — deletes the label from the output entirely. `__name__` is
// skipped (LogQL selectors can't carry it, but the reference guard
// exists; mirror it).
func absentSynthLabels(sel syntax.LogSelectorExpr) []chplan.SynthLabel {
	if sel == nil {
		return nil
	}
	matchers := sel.Matchers()
	values := make(map[string]string, len(matchers))
	dropped := make(map[string]bool, len(matchers))
	order := make([]string, 0, len(matchers))
	for _, m := range matchers {
		if m.Name == model.MetricNameLabel {
			continue
		}
		if _, seen := values[m.Name]; m.Type == labels.MatchEqual && !seen && !dropped[m.Name] {
			values[m.Name] = m.Value
			order = append(order, m.Name)
			continue
		}
		dropped[m.Name] = true
	}
	out := make([]chplan.SynthLabel, 0, len(order))
	for _, name := range order {
		if dropped[name] {
			continue
		}
		out = append(out, chplan.SynthLabel{Key: name, Value: values[name]})
	}
	return out
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

// applyUnwrapRowSemantics folds reference Loki's per-row unwrap
// contract (streamLabelSampleExtractor.Process,
// pkg/logql/log/metrics_extraction.go) onto the lowered inner node and
// labels expression:
//
//  1. an EMPTY (or absent — Get on a CH Map yields ”) unwrap source
//     drops the sample before any conversion runs;
//  2. a non-empty value the conversion rejects keeps the sample with
//     value 0 and stamps `__error__="SampleExtractionErr"` +
//     `__error_details__` onto its labels — BEFORE the unwrap
//     post-filters run, so `| unwrap duration(d) | __error__ = ""`
//     drops exactly the error samples;
//  3. the unwrap post-filters then AND-fold against the
//     conversion-stamped labels (see [applyUnwrapPostFilters]).
//
// Only the duration conversions can reject a value at the unwrap
// conversion stage under cerberus's lowering today (bare unwrap reads
// through toFloat64OrZero, bytes through parseReadableSize), so only
// they contribute a mark here; the unwrap POST-filters
// (applyUnwrapPostFilters) route through labelFiltererLower, so numeric
// and bytes post-filters keep-and-mark per [numericLabelFilterExpr] /
// [bytesLabelFilterExpr] like the duration ones.
// hasErrorMarks reports whether the returned labels expression carries
// any conditional `__error__` stamp — the identity construction uses
// it to gate the reference engine's error-series grouping bypass.
//
// No-op (and mark-free) for range aggregations without an unwrap
// clause.
func applyUnwrapRowSemantics(e *syntax.RangeAggregationExpr, s schema.Logs, inner chplan.Node, labelsExpr chplan.Expr) (chplan.Node, chplan.Expr, bool, error) {
	if e.Left.Unwrap == nil {
		return inner, labelsExpr, false, nil
	}
	hasErrorMarks := false
	unwrapAccess := attributeLookupExpr(labelsExpr, e.Left.Unwrap.Identifier)
	switch e.Left.Unwrap.Operation {
	case syntax.OpConvDuration, syntax.OpConvDurationSeconds:
		parse := newDurationParse(unwrapAccess)
		labelsExpr = wrapLabelsWithMarks(labelsExpr, []labelFilterMark{{
			cond: &chplan.Binary{
				Op: chplan.OpAnd,
				Left: &chplan.Binary{
					Op:    chplan.OpNe,
					Left:  unwrapAccess,
					Right: &chplan.LitString{V: ""},
				},
				Right: notExpr(parse.valid),
			},
			kind:    errSampleExtractionKind,
			details: parse.details,
		}})
		hasErrorMarks = true
	}
	inner = andFilter(inner, &chplan.Binary{
		Op:    chplan.OpNe,
		Left:  unwrapAccess,
		Right: &chplan.LitString{V: ""},
	})

	if len(e.Left.Unwrap.PostFilters) > 0 {
		filtered, postLabels, postMarks, err := applyUnwrapPostFilters(inner, e.Left.Unwrap.PostFilters, s, labelsExpr)
		if err != nil {
			return nil, nil, false, err
		}
		inner = filtered
		labelsExpr = postLabels
		hasErrorMarks = hasErrorMarks || postMarks
	}
	return inner, labelsExpr, hasErrorMarks, nil
}

// errorBypassIdentityExpr wraps a series-identity expression with the
// error-sample bypass, mirroring reference Loki's
// LabelsBuilder.GroupedLabels (pkg/logql/log/labels.go): "When there
// are errors, GroupedLabels returns the full labels with error" — the
// by/without grouping AND the implicit without-unwrap-target strip are
// BOTH bypassed, so every error sample lands in a series keyed by its
// complete label set plus `__error__` / `__error_details__`. The
// synthesized detected_level rides along like it does on the normal
// branch (reference stamps detected_level as structured metadata on
// every row, so its error series carry it too).
func errorBypassIdentityExpr(s schema.Logs, fullLabels, identity chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "mapContains",
				Args: []chplan.Expr{fullLabels, &chplan.LitString{V: logqlmodel.ErrorLabel}},
			},
			withDetectedLevel(s, fullLabels),
			identity,
		},
	}
}

// applyUnwrapPostFilters AND-folds the unwrap clause's post-filters
// onto `inner`'s predicate. Post-filters are label filters parsed
// between the `unwrap` clause and the `[range]` close-bracket — e.g.
// `unwrap duration | duration > 10s`. They share the LabelFilterer
// type with regular `| label="v"` filters.
//
// The returned labels expression carries any `__error__` marks the
// post-filters themselves stamp (duration filters keep-and-mark rows
// whose value Go's time.ParseDuration rejects — reference Loki's
// postFilter stage runs against the extractor's LabelsBuilder, so its
// error stamps surface on the sample's labels exactly like pipeline-
// stage filters). hasMarks reports whether any mark was folded in.
func applyUnwrapPostFilters(inner chplan.Node, filters []loglib.LabelFilterer, s schema.Logs, labelsExpr chplan.Expr) (chplan.Node, chplan.Expr, bool, error) {
	pred := chplan.Expr(nil)
	if f, ok := inner.(*chplan.Filter); ok {
		pred = f.Predicate
		inner = f.Input
	}
	var marks []labelFilterMark
	for _, lf := range filters {
		extra, lfMarks, err := labelFiltererLower(lf, s, labelsExpr)
		if err != nil {
			return nil, nil, false, err
		}
		marks = append(marks, lfMarks...)
		if pred == nil {
			pred = extra
		} else {
			pred = &chplan.Binary{Op: chplan.OpAnd, Left: pred, Right: extra}
		}
	}
	labelsExpr = wrapLabelsWithMarks(labelsExpr, marks)
	if pred == nil {
		return inner, labelsExpr, len(marks) > 0, nil
	}
	return &chplan.Filter{Input: inner, Predicate: pred}, labelsExpr, len(marks) > 0, nil
}

// andFilter AND-folds one predicate onto `inner`, reusing an existing
// top-level Filter node when present so the plan keeps a single Filter
// layer.
func andFilter(inner chplan.Node, extra chplan.Expr) chplan.Node {
	if f, ok := inner.(*chplan.Filter); ok {
		return &chplan.Filter{
			Input:     f.Input,
			Predicate: &chplan.Binary{Op: chplan.OpAnd, Left: f.Predicate, Right: extra},
		}
	}
	return &chplan.Filter{Input: inner, Predicate: extra}
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

// isMatrixRangeWindow reports whether plan's root (walking past
// value-rewrite Projects / Filters / Aggregates that preserve the inner
// matrix shape) bottoms out at a matrix-shape RangeWindow — one emitting
// N rows per series across [Start, End] spaced by Step, exposing
// `anchor_ts` as a per-row column. Used by both [Lang.ProjectSamples]
// (to forward `anchor_ts` into the canonical TimeUnix slot) and
// [lowerVectorAggregation] (to include the per-anchor column in the
// GROUP BY so per-step rows don't collapse). The Aggregate case lets
// nested aggregations (`max(avg by (level) (matrix))`) recognise the
// matrix shape — the inner Aggregate carries its own per-anchor bucket
// (re-aliased to TimeUnix by the wrap Project), and the outer
// aggregation must re-bucket on it. Mirrors prom's isMatrixRangeWindow
// in internal/api/prom/handler.go.
func isMatrixRangeWindow(plan chplan.Node) bool {
	switch v := plan.(type) {
	case *chplan.RangeWindow:
		return v.OuterRange > 0
	case *chplan.Project:
		return isMatrixRangeWindow(v.Input)
	case *chplan.Filter:
		return isMatrixRangeWindow(v.Input)
	case *chplan.Aggregate:
		return isMatrixRangeWindow(v.Input)
	}
	return false
}

// matrixBucketColumn returns the per-anchor bucket-timestamp column
// name visible to the outer SELECT for a matrix-shape plan. The
// LogQL matrix pipeline exposes the anchor column under two distinct
// names depending on how deeply nested the aggregation chain is:
//
//   - Direct matrix RangeWindow (or one wrapped in a value-shape Project
//     / Filter) → "anchor_ts": the RangeWindow emits one row per
//     (series, anchor) carrying `anchor_ts` as a plain column.
//
//   - Vector-aggregation matrix wrap → "TimeUnix": the inner Aggregate
//     groups by `anchor_ts AS bucket_ts`, then [wrapVectorAggregateForSample]
//     re-aliases `bucket_ts` to `TimeUnix` so the canonical Sample shape
//     surfaces. Outer aggregations consume that Project as input, so
//     their bucket reference is `TimeUnix`, not `anchor_ts` (which is no
//     longer in scope past the inner Aggregate's projection).
//
// Callers are expected to gate on [isMatrixRangeWindow] first.
func matrixBucketColumn(plan chplan.Node) string {
	switch v := plan.(type) {
	case *chplan.Aggregate:
		return "TimeUnix"
	case *chplan.Project:
		return matrixBucketColumn(v.Input)
	case *chplan.Filter:
		return matrixBucketColumn(v.Input)
	}
	return "anchor_ts"
}

// rangeValueExpr returns the per-row Value the RangeWindow aggregates.
//
// Line-counting ops use constant 1; byte-counting ops use length(Body).
// Unwrap-based ops use the unwrap clause's identifier resolved against
// the live labelsExpr — bare unwrap reads the string and casts to
// Float64, `duration` / `duration_seconds` parses Loki's Go-duration
// shape via the regex-gated [newDurationParse] expression (Float64
// seconds; 0 on parse error, matching reference convertDuration), and
// `bytes` parses the human-readable byte-size shape via CH's
// `parseReadableSize` (Float64 bytes).
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
		syntax.OpRangeTypeQuantile, syntax.OpRangeTypeFirst, syntax.OpRangeTypeLast,
		syntax.OpRangeTypeRateCounter:
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
//	unwrap duration(foo)             → regex-gated Go-duration parse (Float64 seconds, 0 on parse error)
//	unwrap duration_seconds(foo)     → same as duration(foo)
//	unwrap bytes(foo)                → parseReadableSize(labelsExpr['foo'])    (Float64 bytes)
//
// The duration conversions go through [newDurationParse] — Go's exact
// time.ParseDuration unit set incl. `µs`/`μs`, with unparseable values
// yielding 0 instead of a query-aborting CH exception (reference
// semantics: convertDuration returns (0, err) and the sample is kept
// with `__error__="SampleExtractionErr"`). `parseReadableSize` accepts
// the human-readable byte-size shapes Loki's `humanize.ParseBytes`
// covers (`1KB`, `1.5MiB`, `2 G`). All return Float64 so the
// downstream windowed-array math stays in Float64 throughout.
func unwrapValueExpr(u *syntax.UnwrapExpr, labelsExpr chplan.Expr) (chplan.Expr, error) {
	if u.Identifier == "" {
		return nil, fmt.Errorf("logql: `| unwrap` has empty identifier")
	}
	// `| unwrap foo` pulls the value out of the live labels map for the
	// current point in the pipeline. After a parser stage the labels
	// map may carry the value under a dotted OTel-canonical form (e.g.
	// `cerberus.duration_ms` rather than `cerberus_duration_ms`), so
	// the dotted-fallback chain hits either shape.
	access := attributeLookupExpr(labelsExpr, u.Identifier)
	switch u.Operation {
	case "":
		return &chplan.FuncCall{
			Name: "toFloat64OrZero",
			Args: []chplan.Expr{access},
		}, nil
	case syntax.OpConvDuration, syntax.OpConvDurationSeconds:
		// Regex-gated Go-duration parse (see internal/logql/duration.go)
		// rather than a bare `parseTimeDelta(...)`: CH's parseTimeDelta
		// throws (code 36) on the first unparseable value — including
		// Go-valid `291.792µs` on CH 24.8 — aborting the whole query.
		// Reference Loki's convertDuration
		// (pkg/logql/log/metrics_extraction.go) returns 0 for the
		// sample value and stamps `__error__="SampleExtractionErr"` on
		// the sample's labels instead; the seconds expression carries
		// the value half of that contract (0 on parse failure), and
		// [lowerRangeAggregation] folds the error stamp into the
		// labels map.
		return newDurationParse(access).seconds, nil
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

// hasParserMergedLabels reports whether labelsExpr carries a parser-stage
// `mapConcat(...)` wrap on top of the raw ResourceAttributes column. Used
// by [lowerRangeAggregation] to gate the two-stage Project rewrite: when
// the parser-merged labels expression nests a lambda-bearing call (e.g.
// `mapApply` from `logfmtMergeLabels`'s rename-on-collision shape),
// wrapping it inside the outer identity's `mapFilter` triggers
// ClickHouse's `Recursive lambda (UNSUPPORTED_METHOD)` error. The fix
// materialises labelsExpr into an intermediate column so the outer
// `mapFilter`'s source is a plain column reference.
//
// Detection: any expression that ISN'T a bare ColumnRef on the resource-
// attributes column needs the materialise rewrite. The parser-stage
// helpers ([logfmtMergeLabels] / [jsonBareMergeLabels] / [regexpMergeLabels]
// / [logfmtExpressionMergeLabels] / [jsonExpressionMergeLabels]) all
// return a `mapConcat(prev, …)` shape, so this predicate captures every
// parser flavour without enumerating them.
func hasParserMergedLabels(labelsExpr chplan.Expr, s schema.Logs) bool {
	col, ok := labelsExpr.(*chplan.ColumnRef)
	if !ok {
		return true
	}
	return col.Name != s.ResourceAttributesColumn
}

// rangeValueExprFromMerged is the materialised-column variant of
// [rangeValueExpr]: the unwrap value extraction reads from the
// pre-materialised `_logql_merged_labels` column instead of re-evaluating
// the full parser-merge expression. Used in the two-stage Project path
// when [hasParserMergedLabels] returns true.
//
// Mirrors [unwrapValueExpr] but binds the merged-labels reference to a
// column ref. Byte / line counters and the no-unwrap branches go
// unchanged — they don't reference labelsExpr.
func rangeValueExprFromMerged(e *syntax.RangeAggregationExpr, mergedCol chplan.Expr) (chplan.Expr, error) {
	if e.Left.Unwrap == nil {
		return nil, fmt.Errorf("logql: rangeValueExprFromMerged called without `| unwrap`")
	}
	return unwrapValueExpr(e.Left.Unwrap, mergedCol)
}

// rangeAggregationGroupBy returns the chplan group-key expressions for
// `by (...)` / `without (...)` on a range aggregation. The output is a
// single Map-shaped expression so the downstream Project synthesises a
// per-stream identity that the RangeWindow GROUP BY can collapse to.
//
//	no Grouping        → nil (caller defaults to grouping on identityBase).
//	`by (k1, k2)`      → map('k1', RA['k1'], 'k2', RA['k2'])
//	`by ()`            → map()       (single all-collapsed group)
//	`without (k1, k2)` → mapFilter((k,v) -> NOT (k IN ('k1','k2')),
//	                               withDetectedLevel(identityBase))
//
// `without` is exclusion semantics: it strips keys from the FULL series
// identity the pipeline produced — `identityBase` (the parser-merged
// labelset minus the unwrap target when the aggregation carries
// `| unwrap`), augmented with the synthesized `detected_level` key the
// no-grouping path also injects (so an enclosing `by (level)` /
// `by (detected_level)` vector aggregation still resolves the severity
// dimension; the injection happens BEFORE the strip so
// `without (detected_level)` / `without (level)` removes it again).
// Grouping on the raw ResourceAttributes column instead would silently
// drop every parser-extracted label from the series identity.
//
// The `detected_level` family (`detected_level` + its `level` alias) is
// resolved to the SeverityText-derived `multiIf(...)` normalisation
// rather than a literal map lookup the seeder doesn't write — mirrors
// the vector-aggregation alias surface so the two grouping layers
// behave consistently.
func rangeAggregationGroupBy(e *syntax.RangeAggregationExpr, s schema.Logs, identityBase chplan.Expr) (chplan.Expr, error) {
	if e.Grouping == nil {
		return nil, nil
	}
	if e.Grouping.Without {
		return &chplan.MapWithoutKeys{
			Map:  withDetectedLevel(s, identityBase),
			Keys: canonicalLevelKeys(e.Grouping.Groups),
		}, nil
	}
	args := make([]chplan.Expr, 0, len(e.Grouping.Groups)*2)
	for _, label := range e.Grouping.Groups {
		args = append(
			args,
			&chplan.LitString{V: label},
			levelAwareRangeGroupKey(label, s),
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
//     `quantile_over_time` / `first_over_time` / `last_over_time`
//     reuse PromQL's identical-shape function names — chsql/
//     range_window.go handles each variant via emitRangeWindowOverTime
//     / emitRangeWindowQuantileOverTime. `first_over_time` /
//     `last_over_time` pick the time-earliest / time-latest unwrapped
//     value in the window (reference Loki's FirstOverTime /
//     LastOverTime streaming aggregators, pkg/logql/range_vector.go),
//     which the emitter renders as `window_vals[1]` /
//     `window_vals[length(window_vals)]` over the time-sorted window
//     array; empty windows drop the series on both sides.
func rangeFuncName(op string) (string, error) {
	switch op {
	case syntax.OpRangeTypeRate, syntax.OpRangeTypeBytesRate:
		return "log_rate", nil
	case syntax.OpRangeTypeRateCounter:
		// `rate_counter(... | unwrap v [r])` treats the unwrapped values
		// as a Prometheus counter: reference Loki's rateCounter
		// (pkg/logql/range_vector.go) is a verbatim copy of Prometheus's
		// promql/functions.go extrapolatedRate(isCounter=true,
		// isRate=true), so it shares cerberus's PromQL "rate" emitter
		// (counter-reset repair + boundary extrapolation, ≥ 2 samples).
		return "rate", nil
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
		syntax.OpRangeTypeQuantile,
		syntax.OpRangeTypeFirst,
		syntax.OpRangeTypeLast:
		return op, nil
	}
	return "", fmt.Errorf("logql: range op %s is not yet supported", op)
}
