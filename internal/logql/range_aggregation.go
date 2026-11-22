package logql

import (
	"fmt"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"

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
//   - unwrap bytes(field)                                           → gated humanize.ParseBytes replica over labelsExpr[field]
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
	rows, labelsExpr, level, err := lowerLogRange(e.Left, s, innerLc)
	if err != nil {
		return nil, err
	}

	labelsExpr, unwrapValue, unwrapHasErrorMarks, err := applyUnwrapRowSemantics(e, s, rows, labelsExpr, level)
	if err != nil {
		return nil, err
	}

	value, err := rangeValueExpr(e, s, unwrapValue)
	if err != nil {
		return nil, err
	}

	// The parser-merged labels column, when the unwrap identity or an
	// outer-by key reads the merged map; see [mergedLabelsColumn]. A nil
	// mergedCol means no merged map is read and every read below stays on
	// the raw columns.
	mergedCol := mergedLabelsColumn(labelsExpr, s, lc, e.Left.Unwrap != nil)

	// What the outer-by inflation resolves its keys against. It stays the
	// raw column unless the merged map was defined as a column, so an
	// inline `mapConcat(...)` — which carries lambdas CH would reject
	// nested inside the inflation's own — can never reach the identity.
	parsedLabels := chplan.Expr(&chplan.ColumnRef{Name: s.ResourceAttributesColumn})
	if mergedCol != nil {
		parsedLabels = mergedCol
	}

	// Default identity: the raw ResourceAttributes column, augmented
	// below with the synthesized `detected_level` label so each distinct
	// severity becomes its own series. Loki's stream-identity contract
	// includes detected_level as a structural dimension whenever the
	// upstream row carries severity metadata; the RangeWindow's GROUP BY
	// (keyed on the ResourceAttributesColumn alias) then collapses one
	// row per (stream, detected_level) tuple — matching upstream Loki's
	// matrix shape (16 of the 19 loki-compat failures came from cerberus
	// collapsing 4 levels into 1 series here). `| unwrap` replaces both
	// the identity and the error bypass.
	identityBase := chplan.Expr(&chplan.ColumnRef{Name: s.ResourceAttributesColumn})
	var errorBypassLabels chplan.Expr
	if e.Left.Unwrap != nil {
		identityBase, errorBypassLabels = unwrapSeriesIdentity(e, labelsExpr, unwrapHasErrorMarks)
	}
	// `| drop` / `| keep` narrow the series identity before it becomes a
	// grouping key. On a log query the same projection runs post-fetch in
	// Go; on a metric query it has to happen here, or two streams that
	// differ only in a dropped label stay separate groups and the caller
	// gets a split matrix Loki would have merged (issue #1491).
	identityBase = narrowIdentityByProjection(identityBase, e.Left.Left)

	// The same projection also applies to the synthesized
	// `detected_level`, which reference Loki carries as ordinary
	// structured metadata and cerberus splices in below — after the
	// mapFilter above has already run, so the stage's effect on it
	// rides on the VALUE instead (issue #1607). A nil result means the
	// pipeline removes the label on every row and the key is left out
	// of the identity entirely. The value is a column of the row stage
	// ([detectedLevelColumn]); several positions below read it at once
	// (the two arms of the error bypass, one entry per `by (...)` label).
	newLevelValue := projectedLevelColumn(rows, level, e.Left.Left)

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
	groupBy, err := rangeAggregationGroupBy(e, s, identityBase, newLevelValue)
	if err != nil {
		return nil, err
	}

	identityExpr := withDetectedLevelAndColumns(s, identityBase, newLevelValue(), lc.OuterByLabels, parsedLabels)
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
		identityExpr = groupBy
	}
	if errorBypassLabels != nil {
		identityExpr = errorBypassIdentityExpr(s, errorBypassLabels, identityExpr, newLevelValue())
	}
	projections := []chplan.Projection{
		{Expr: canonicalIdentityExpr(identityExpr), Alias: s.ResourceAttributesColumn},
		{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
		{Expr: value, Alias: rangeAggSynthValueColumn},
	}
	rows.gatherAtScan(logMapColumns(s), lc.PrewhereInWhereOrder, projections...)
	projected := &chplan.Project{
		Roles:       logRoles(s),
		Input:       rows.node,
		Projections: projections,
	}
	var carry []string
	if lc.variantArmsReadLine {
		carry = []string{s.BodyColumn}
	}
	rows.seal(projected, carry...)

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
	} else if !lc.End.IsZero() {
		// Instant mode: anchor the single evaluation point at the
		// request's `time`. Left zero, the emitter anchors at `now64(9)`
		// and only the overlap of the requested window with
		// (now - range, now] is counted — `count_over_time({...}[24h])`
		// for yesterday returns the last few hours of it, or nothing.
		// Mirrors the PromQL instant evalAnchor{End: ctx.end}.
		rw.End = lc.End.UTC()
	}
	if e.Operation == syntax.OpRangeTypeQuantile {
		if e.Params == nil {
			return nil, fmt.Errorf("logql: quantile_over_time requires a phi parameter")
		}
		rw.Scalars = []float64{*e.Params}
	}
	return rw, nil
}

// canonicalIdentityExpr canonicalises the key order of the LogQL
// stream-identity Map so every whole-Map identity key downstream of it
// compares by label SET rather than by physical key order. See
// [chplan.CanonicalAttributesExpr] for why a ClickHouse Map needs this
// and why the wrap is always safe (order-only, idempotent, so it can
// never merge two genuinely different label sets).
//
// The LogQL metric path has exactly one binding family, mirroring how
// the PromQL head canonicalises at its single selector projection:
// [lowerRangeAggregation] projects the per-row stream identity under the
// ResourceAttributes alias, and the RangeWindow's GROUP BY — plus every key derived from
// it: the enclosing vector aggregation's `by (...)` subscripts and
// `without (...)` MapWithoutKeys, topk's PARTITION BY, the vector-join
// and set-op match keys — reads that alias out of the enclosing
// subquery. Canonicalising the projection fixes all of them at once, and
// the identity keys stay ALIAS references rather than re-rendered
// expressions, so they never trip ClickHouse's "a SELECT alias shadows
// the FROM column it reads" resolution order (see chsql.groupKeyFrags).
//
// It is applied once, at the projection, after the three mutually
// exclusive identity shapes have settled — the default per-stream
// identity, the `by (...)` / `without (...)` group-key override, and the
// unwrap conversion-error bypass. Wrapping there rather than per-shape
// keeps the bypass, whose Map-valued `if(...)` embeds one of the other
// two, down to a single sort covering both of its arms.
//
// Per-label subscripts (`ResourceAttributes['job']`, what `by (...)`
// lowers to) are scalar Strings and compare identically under any key
// order, so they are deliberately NOT wrapped — that would buy a sort
// and nothing else.
func canonicalIdentityExpr(identity chplan.Expr) chplan.Expr {
	return chplan.CanonicalAttributesExpr(identity)
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
	rows, labelsExpr, level, err := lowerLogRange(e.Left, s, innerLc)
	if err != nil {
		return nil, err
	}
	if _, _, _, err = applyUnwrapRowSemantics(e, s, rows, labelsExpr, level); err != nil {
		return nil, err
	}

	rows.gatherAtScan(logMapColumns(s), lc.PrewhereInWhereOrder)
	const tsAlias = sampleTimeUnixCol
	timestamps := &chplan.Project{
		Roles: logRoles(s),
		Input: rows.node,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: tsAlias},
		},
	}
	rows.seal(timestamps)
	a := &chplan.AbsentOverTime{
		Input:            timestamps,
		SynthLabels:      absentSynthLabels(e.Left.Left),
		Range:            e.Left.Interval,
		Offset:           e.Left.Offset,
		TimestampColumn:  tsAlias,
		ValueColumn:      rangeAggSynthValueColumn,
		MetricNameColumn: sampleMetricNameCol,
		AttributesColumn: sampleAttributesCol,
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
// matchers or a pipeline) into a per-row stage
// ([rowLayers]) and returns it plus the final labelsExpr the pipeline
// produced and the normalised `detected_level` column
// ([defineDetectedLevel]). Range aggregations thread this labelsExpr to
// unwrap's value extraction and any unwrap-post filters so they resolve
// against the same map that ordinary label filters do.
func lowerLogRange(lr *syntax.LogRangeExpr, s schema.Logs, lc lowerCtx) (rows *rowLayers, labelsExpr, level chplan.Expr, err error) {
	var stream *syntax.MatchersExpr
	var pipeline *syntax.PipelineExpr
	switch left := lr.Left.(type) {
	case *syntax.MatchersExpr:
		stream = left
	case *syntax.PipelineExpr:
		stream, pipeline = left.Left, left
	default:
		return nil, nil, nil, fmt.Errorf("logql: range-aggregation inner is %T (not MatchersExpr or PipelineExpr)", lr.Left)
	}
	rows = newRowLayers(lowerMatchers(stream, s, lc))
	source, level := defineDetectedLevel(rows, s)
	// No pipeline — labels-map is the ResourceAttributes column.
	labelsExpr = &chplan.ColumnRef{Name: s.ResourceAttributesColumn}
	if pipeline != nil {
		lc.row.level = level
		// A detected_level filter reads the source where WHERE order holds,
		// and ahead of a later line filter, so that it can run in the
		// SELECT that reads the table ahead of that line filter. Without
		// WHERE order that SELECT then computes the normalised level beside
		// the source ([rowLayers.gatherAtScan]). Without WHERE order, a
		// level filter beside a filter on a stored label reads the source's
		// expression rather than its column: it reads no value the stage
		// defines, so it runs in the scan's own filter, and the label
		// filter, which reads LogAttributes as a whole, folds there with it
		// ([wholeMapFiltersAhead]). Any other level filter reads the
		// normalised level, above the scan.
		switch {
		case lc.PrewhereInWhereOrder:
			lc.row.levelSource = source
		case storedLabelFilterBesideLevelFilter(pipeline):
			lc.row.levelSource = detectedLevelSourceExpr(s)
		case lineFilterFollowsLevelFilter(pipeline):
			lc.row.levelSource = source
		}
		if labelsExpr, err = lowerPipelineStages(pipeline, s, lc, rows); err != nil {
			return nil, nil, nil, err
		}
	}
	return rows, labelsExpr, level, nil
}

// lineFilterFollowsLevelFilter reports whether a line filter stage of
// pipeline follows a label filter stage that reads detected_level.
func lineFilterFollowsLevelFilter(pipeline *syntax.PipelineExpr) bool {
	level := false
	for _, stage := range pipeline.MultiStages {
		switch st := stage.(type) {
		case *syntax.LabelFilterExpr:
			level = level || filtersDetectedLevel(st.LabelFilterer)
		case *syntax.LineFilterExpr:
			if level {
				return true
			}
		}
	}
	return false
}

// storedLabelFilterBesideLevelFilter reports whether a label filter stage
// of pipeline that reads detected_level and one that does not both run
// ahead of every stage of another kind than a label or line filter, in
// either order: there every label the second one reads is a stored one.
func storedLabelFilterBesideLevelFilter(pipeline *syntax.PipelineExpr) bool {
	level, label := false, false
	for _, stage := range pipeline.MultiStages {
		switch st := stage.(type) {
		case *syntax.LabelFilterExpr:
			if filtersDetectedLevel(st.LabelFilterer) {
				level = true
			} else {
				label = true
			}
		case *syntax.LineFilterExpr:
		default:
			return level && label
		}
	}
	return level && label
}

// filtersDetectedLevel reports whether lf reads detected_level.
func filtersDetectedLevel(lf syntax.LabelFilterer) bool {
	switch v := lf.(type) {
	case *syntax.StringLabelFilter:
		return isDetectedLevelLabel(v.Name)
	case *syntax.BinaryLabelFilter:
		return filtersDetectedLevel(v.Left) || filtersDetectedLevel(v.Right)
	}
	return false
}

// applyUnwrapRowSemantics folds reference Loki's per-row unwrap
// contract (streamLabelSampleExtractor.Process,
// pkg/logql/log/metrics_extraction.go) onto the per-row stage and the
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
// All three conversions reference Loki performs at this stage — bare
// (`strconv.ParseFloat`), duration (`time.ParseDuration`) and bytes
// (`humanize.ParseBytes`) — can reject a value, and each contributes a
// mark here. The unwrap POST-filters (applyUnwrapPostFilters) route
// through labelFiltererLower, so numeric and bytes post-filters
// keep-and-mark per [numericLabelFilterExpr] / [bytesLabelFilterExpr]
// in the same way.
//
// It returns the labels map with every mark stamped, the unwrap source
// column the sample value converts, and hasErrorMarks: whether the
// returned labels expression carries any conditional `__error__` stamp —
// the identity construction uses it to gate the reference engine's
// error-series grouping bypass.
//
// No-op (and mark-free) for range aggregations without an unwrap
// clause.
func applyUnwrapRowSemantics(e *syntax.RangeAggregationExpr, s schema.Logs, rows *rowLayers, labelsExpr, level chplan.Expr) (labels, value chplan.Expr, hasErrorMarks bool, err error) {
	if e.Left.Unwrap == nil {
		return labelsExpr, nil, false, nil
	}
	if e.Left.Unwrap.Identifier == "" {
		return nil, nil, false, fmt.Errorf("logql: `| unwrap` has empty identifier")
	}
	// `| unwrap foo` pulls the value out of the live labels map for the
	// current point in the pipeline. After a parser stage the labels
	// map may carry the value under a dotted OTel-canonical form (e.g.
	// `cerberus.duration_ms` rather than `cerberus_duration_ms`), so
	// the dotted-fallback chain hits either shape.
	//
	// Resolution goes through [structuredOrStreamLookupOnMap] — the SAME
	// helper the pipeline label-filter and grouping sites use — so an
	// unwrap target that lives in structured metadata (LogAttributes)
	// resolves exactly where `| foo != ""` and `sum by (foo)` resolve it.
	// Reading the bare labels map instead would see "" for every
	// structured-metadata-only key, and rule 1 below then drops every
	// sample before conversion, so the whole matrix comes back empty
	// (issue #1887).
	//
	// The source is read by the emptiness filter, by the conversion that
	// produces the sample value and by the conversion-error mark, so it
	// is defined once as a column of the row and every one of them reads
	// that column. It is read before the mark is stamped, as reference
	// Loki reads it: the extractor takes the label value first and
	// stamps `__error__` only when converting it fails.
	source := rows.define(unwrapSourceAlias, structuredOrStreamLookupOnMap(s, labelsExpr, e.Left.Unwrap.Identifier))
	rows.filter(&chplan.Binary{
		Op:    chplan.OpNe,
		Left:  source,
		Right: &chplan.LitString{V: ""},
	})
	value, valid, details, err := unwrapConversion(e.Left.Unwrap, source, rows.definer(unwrapAliasPrefix))
	if err != nil {
		return nil, nil, false, err
	}
	// The layer sits above the emptiness filter, so every row it stamps
	// carries a non-empty source and the mark's condition is the
	// conversion's rejection alone.
	labelsExpr = rows.define(unwrapLabelsAlias, wrapLabelsWithMarks(labelsExpr, []labelFilterMark{{
		cond:    notExpr(valid),
		kind:    errSampleExtractionKind,
		details: details,
	}}))
	hasErrorMarks = true

	if len(e.Left.Unwrap.PostFilters) > 0 {
		postLabels, postMarks, err := applyUnwrapPostFilters(rows, e.Left.Unwrap.PostFilters, s, labelsExpr, level)
		if err != nil {
			return nil, nil, false, err
		}
		labelsExpr = postLabels
		hasErrorMarks = hasErrorMarks || postMarks
	}
	return labelsExpr, value, hasErrorMarks, nil
}

// Column aliases the unwrap row stage defines on its [rowLayers]: the
// unwrap source value, the labels map once the conversion-error mark is
// stamped on it, and the labels map once the post-filters' own marks are.
// The conversion's own intermediate values take unwrapAliasPrefix plus
// their role (see [defineFunc]).
const (
	unwrapAliasPrefix     = "_logql_unwrap_"
	unwrapSourceAlias     = unwrapAliasPrefix + "source"
	unwrapLabelsAlias     = unwrapAliasPrefix + "labels"
	unwrapPostLabelsAlias = unwrapAliasPrefix + "post_labels"
)

// errorBypassIdentityExpr wraps a series-identity expression with the
// error-sample bypass, mirroring reference Loki's
// LabelsBuilder.GroupedLabels (pkg/logql/log/labels.go): "When there
// are errors, GroupedLabels returns the full labels with error" — the
// by/without grouping AND the implicit without-unwrap-target strip are
// BOTH bypassed, so every error sample lands in a series keyed by its
// complete label set plus `__error__` / `__error_details__`. The
// synthesized detected_level rides along like it does on the normal
// branch (reference stamps detected_level as structured metadata on
// every row, so its error series carry it too) — under the SAME
// `levelValue` the normal branch uses, so a `| drop detected_level`
// removes it from the error series too. Reference's bypass is scoped to
// the by/without grouping; the pipeline's own drop/keep stages have
// already mutated the LabelsBuilder by then, error or not.
func errorBypassIdentityExpr(s schema.Logs, fullLabels, identity, levelValue chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{
		Fn: chplan.FnIf,
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Fn:   chplan.FnMapContainsKey,
				Args: []chplan.Expr{fullLabels, &chplan.LitString{V: syntax.ErrorLabel}},
			},
			withDetectedLevel(s, fullLabels, levelValue),
			identity,
		},
	}
}

// applyUnwrapPostFilters filters the unwrap clause's post-filters onto
// rows. Post-filters are label filters parsed
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
func applyUnwrapPostFilters(rows *rowLayers, filters []syntax.LabelFilterer, s schema.Logs, labelsExpr, level chplan.Expr) (chplan.Expr, bool, error) {
	var marks []labelFilterMark
	for _, lf := range filters {
		extra, lfMarks, err := labelFiltererLower(lf, s, labelsExpr, filterRow{level: level, rows: rows})
		if err != nil {
			return nil, false, err
		}
		marks = append(marks, lfMarks...)
		rows.filter(extra)
	}
	labelsExpr = rows.define(unwrapPostLabelsAlias, wrapLabelsWithMarks(labelsExpr, marks))
	return labelsExpr, len(marks) > 0, nil
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

// bottomsOutAtMatrixRangeWindow reports whether plan's root (walking past
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
// aggregation must re-bucket on it.
//
// It is NOT the PromQL head's bottomsOutAtMatrixRangeWindow, which it used to claim
// to mirror while answering a different question with a different arm
// set. The two differ deliberately, in both directions:
//
//   - PromQL crosses an Aggregate only when it keys on an alias of
//     `anchor_ts` (chplan.AggregatePreservesMatrixGrid), because its
//     caller goes on to READ `anchor_ts` off the output scope and a
//     grid-folding `sum by (…)` never exposes it. This one crosses any
//     Aggregate, because its callers ask only whether a per-anchor bucket
//     exists SOMEWHERE below and then resolve its name separately
//     ([matrixBucketColumn]) — LogQL's nested shape re-aliases the bucket
//     to `TimeUnix`, which is exactly the alias PromQL's guard rejects.
//   - PromQL also answers true for a RangeWindowGridNative. Nothing here can
//     see one: the native timeSeries*ToGrid nodes are built only by
//     internal/promql, so an arm for them would be unreachable.
//
// Naming the two the same invited the copy that #2031 records: a reader
// who assumes one question fixes one copy and leaves the other.
func bottomsOutAtMatrixRangeWindow(plan chplan.Node) bool {
	switch v := plan.(type) {
	case *chplan.RangeWindow:
		return v.OuterRange > 0
	case *chplan.Project:
		return bottomsOutAtMatrixRangeWindow(v.Input)
	case *chplan.Filter:
		return bottomsOutAtMatrixRangeWindow(v.Input)
	case *chplan.Aggregate:
		return bottomsOutAtMatrixRangeWindow(v.Input)
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
// Callers are expected to gate on [bottomsOutAtMatrixRangeWindow] first.
func matrixBucketColumn(plan chplan.Node) string {
	switch v := plan.(type) {
	case *chplan.Aggregate:
		return sampleTimeUnixCol
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
// Unwrap-based ops use unwrapValue, the conversion of the unwrap target
// [applyUnwrapRowSemantics] builds ([unwrapConversion]) — bare unwrap
// reads the string and casts to
// Float64, `duration` / `duration_seconds` parses Loki's Go-duration
// shape via the regex-gated [newDurationParse] expression (Float64
// seconds; 0 on parse error, matching reference convertDuration), and
// `bytes` parses the human-readable byte-size shape via the regex-gated
// [newBytesParse] expression (Float64 bytes; 0 on parse error, matching
// reference convertBytes).
//
// `length(Body)` is wrapped in `toFloat64` so the per-row Value tuple
// — `(Timestamp, Value)` shaped by the windowed-array RangeWindow
// emitter — carries a Float64 second element. Without the cast CH
// resolves the column to UInt64, and the downstream arrayMap / arraySum
// chain promotes back to Float64 with quiet rounding (UInt64 → Float64
// loses precision at ≥ 2^53). The cast keeps the units aligned with
// the line-counter path (`LitInt{V:1}` → Int64) where arithmetic
// remains exact across the [start, end] window.
func rangeValueExpr(e *syntax.RangeAggregationExpr, s schema.Logs, unwrapValue chplan.Expr) (chplan.Expr, error) {
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
		return unwrapValue, nil
	}

	switch op {
	case syntax.OpRangeTypeRate, syntax.OpRangeTypeCount:
		return &chplan.LitInt{V: 1}, nil
	case syntax.OpRangeTypeBytesRate, syntax.OpRangeTypeBytes:
		return &chplan.FuncCall{
			Fn: chplan.FnToFloat64,
			Args: []chplan.Expr{&chplan.FuncCall{
				Fn:   chplan.FnLength,
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

// unwrapConversion converts source, the unwrap target's value, the way
// the UnwrapExpr's conversion does. It returns the per-row Float64
// sample value, the predicate that the conversion accepts source, and
// the Go-shaped error message for a source it rejects.
//
// LogQL syntax:
//
//	unwrap foo                       → toFloat64OrZero(source)
//	unwrap duration(foo)             → regex-gated Go-duration parse (Float64 seconds, 0 on parse error)
//	unwrap duration_seconds(foo)     → same as duration(foo)
//	unwrap bytes(foo)                → regex-gated humanize.ParseBytes replica (Float64 bytes, 0 on parse error)
//
// All three conversions reference Loki performs at this stage can
// reject a value, and all three reject it the same way — `convFn`
// returns `(0, err)` and Process stamps `__error__="SampleExtractionErr"`
// + `__error_details__` on the sample's labels
// (pkg/logql/log/metrics_extraction.go's streamLabelSampleExtractor.Process).
// They are convertFloat (`strconv.ParseFloat`), convertDuration
// (`time.ParseDuration`) and convertBytes (`humanize.ParseBytes`);
// cerberus models each with the parse helper of the same name, so an
// unparseable value yields 0 instead of a query-aborting CH exception.
// All return Float64 so the downstream windowed-array math stays in
// Float64 throughout.
//
// define names the values a conversion reads more than once; see
// [defineFunc].
func unwrapConversion(u *syntax.UnwrapExpr, source chplan.Expr, define defineFunc) (value, valid, details chplan.Expr, err error) {
	switch u.Operation {
	case "":
		// Bare `| unwrap x`: convertFloat. Its error arm was missing, so
		// an unparseable value became a silent 0 in the NORMAL series and
		// the error series reference Loki splits off never appeared
		// (cerberus issue #3183).
		parse := newNumericParse(source)
		value = &chplan.FuncCall{
			Fn:   chplan.FnToFloat64OrZero,
			Args: []chplan.Expr{source},
		}
		return value, parse.valid, parse.details, nil
	case syntax.OpConvDuration, syntax.OpConvDurationSeconds:
		// Regex-gated Go-duration parse (see internal/logql/duration.go)
		// rather than a bare `parseTimeDelta(...)`: CH's parseTimeDelta
		// throws (code 36) on the first unparseable value — including
		// Go-valid `291.792µs` on CH 24.8 — aborting the whole query.
		// The seconds expression carries the value half of reference
		// convertDuration's contract (0 on parse failure).
		parse := newDurationParse(source, define)
		return parse.seconds, parse.valid, parse.details, nil
	case syntax.OpConvBytes:
		// Gated [newBytesParse] shape, for the same reason the duration
		// arm is gated: reference Loki's convertBytes returns `(0, err)`
		// for a value humanize.ParseBytes rejects and the sample is KEPT
		// with value 0 plus `__error__="SampleExtractionErr"`. A bare
		// `parseReadableSize(...)` instead THREW on four whole shapes
		// humanize accepts — a bare byte count with no unit (`1024`,
		// i.e. an ordinary logfmt `size=1024`), `5k`, `5 ki` and
		// `5,000 kb` — aborting the entire query with a 502, and
		// rounded rather than truncated everywhere it did parse. See
		// [newBytesParse]'s own comment for the measurements.
		//
		// The result is Float64 (humanize's `float64(uint64(f))`), so
		// the downstream windowed-array math — especially the
		// counter_delta arrayMap's `if(c < p, c, c - p)` — resolves one
		// common type, which it could not do against
		// `parseReadableSize`'s UInt64.
		parse := newBytesParse(source)
		valid = define("valid", parse.valid)
		value = &chplan.FuncCall{
			Fn:   chplan.FnIf,
			Args: []chplan.Expr{valid, parse.value, &chplan.LitFloat{V: 0}},
		}
		return value, valid, parse.details, nil
	}
	return nil, nil, nil, fmt.Errorf("logql: unsupported unwrap conversion %q", u.Operation)
}

// hasParserMergedLabels reports whether labelsExpr is anything other
// than the raw ResourceAttributes column: a labels map some pipeline
// stage produced, which [mergedLabelsColumn] hands to the readers of the
// merged map. In the metric path that map is a column of the row stage
// ([pipelineLabelsAlias], [unwrapLabelsAlias], [unwrapPostLabelsAlias]);
// any other expression is still a stage's merged map — the parser-stage
// helpers ([logfmtMergeLabels] / [jsonBareMergeLabels] /
// [regexpMergeLabels] / [logfmtExpressionMergeLabels] /
// [jsonExpressionMergeLabels]) all return a `mapConcat(prev, …)` shape —
// so the predicate captures every flavour without enumerating them.
func hasParserMergedLabels(labelsExpr chplan.Expr, s schema.Logs) bool {
	col, ok := labelsExpr.(*chplan.ColumnRef)
	if !ok {
		return true
	}
	return col.Name != s.ResourceAttributesColumn
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
// behave consistently. `newLevelValue` yields that normalisation with
// the pipeline's `| drop` / `| keep` projection already applied
// ([detectedLevelIdentityExpr]); a nil value means the pipeline removed
// the label, so `without` has nothing to inject and a `by` clause
// naming the level family groups on the empty string — the same
// collapse reference Loki produces once the projection has deleted the
// label from its LabelsBuilder. The value is a column reference
// ([projectedLevelColumn]), so every entry that names the family reads
// the same column.
func rangeAggregationGroupBy(e *syntax.RangeAggregationExpr, s schema.Logs, identityBase chplan.Expr, newLevelValue func() chplan.Expr) (chplan.Expr, error) {
	if e.Grouping == nil {
		return nil, nil
	}
	if e.Grouping.Without {
		return &chplan.MapWithoutKeys{
			Map:  withDetectedLevel(s, identityBase, newLevelValue()),
			Keys: canonicalLevelKeys(e.Grouping.Groups),
		}, nil
	}
	args := make([]chplan.Expr, 0, len(e.Grouping.Groups)*2)
	for _, label := range e.Grouping.Groups {
		args = append(
			args,
			&chplan.LitString{V: label},
			levelAwareRangeGroupKey(label, s, newLevelValue),
		)
	}
	return &chplan.FuncCall{Fn: chplan.FnMap, Args: args}, nil
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

// mergedLabelsColumn returns the labels column the pipeline produced —
// the map its parser stages merged, with any `__error__` marks its
// filters and its unwrap stamped — when the range aggregation reads that
// map as a whole, or nil when nothing does or the pipeline left the raw
// ResourceAttributes column, and every read stays on the raw columns.
//
// The row stage defines every labels map a pipeline stage produces as a
// column of its own ([rowLayers]), so labelsExpr is already a plain
// column reference. That matters here because ClickHouse rejects a
// `mapFilter((k, v) -> NOT (k IN [...]), mapConcat(RA, mapApply(...)))`
// composition with `Recursive lambda ... (UNSUPPORTED_METHOD)`: the
// inner `mapApply` lambda references the outer ResourceAttributes
// column, and CH disallows a lambda-inside-lambda-source when the inner
// lambda escapes its scope. Reading a column leaves every downstream
// wrap applying its own lambda to a plain column reference.
//
// Two shapes read the map as a whole. `| unwrap` strips the unwrap
// target out of the merged identity. An enclosing `by (...)` that names
// a parser-extracted label needs the map to resolve that key at all —
// neither raw column carries it — and without it
// `sum by (category) (count_over_time({...} | json [5m]))` collapses
// every row into a single `{category:""}` series (issue #1652, whose
// `| unpack` report is one instance of the class).
func mergedLabelsColumn(labelsExpr chplan.Expr, s schema.Logs, lc lowerCtx, hasUnwrap bool) chplan.Expr {
	outerByNeedsParsedLabels := len(structuredOuterByKeys(lc.OuterByLabels, s)) > 0
	if !hasUnwrap && !outerByNeedsParsedLabels {
		return nil
	}
	if !hasParserMergedLabels(labelsExpr, s) {
		return nil
	}
	return labelsExpr
}

// unwrapSeriesIdentity derives the `| unwrap` shape's series identity
// and its error-bypass labels map.
//
// Loki's `LabelExtractorWithStages` treats an unwrap with no explicit
// grouping as `without (unwrapIdent)`, so the identity is every
// parser-extracted label EXCEPT the unwrap target and each unique
// remaining labelset becomes its own series. Without that the outer
// matrix collapses every distinct combination into a single
// detected-level series — the symptom is `matrix length: expected=1440
// actual=4` against the loki-compat 24h unwrap-aggregations corpus,
// whose seeded payload carries a varying `request_id` / `status` /
// `user_agent` per minute.
//
// labelsExpr is the row stage's labels column once the unwrap stage has
// stamped its marks ([applyUnwrapRowSemantics]); a column, so the strip's
// `mapFilter` reads a plain column reference (see [mergedLabelsColumn]).
// When marks were stamped it is also the error bypass: an error sample
// keeps its full label set.
func unwrapSeriesIdentity(
	e *syntax.RangeAggregationExpr,
	labelsExpr chplan.Expr,
	unwrapHasErrorMarks bool,
) (identity, errorBypass chplan.Expr) {
	if unwrapHasErrorMarks {
		errorBypass = labelsExpr
	}
	return &chplan.MapWithoutKeys{Map: labelsExpr, Keys: []string{e.Left.Unwrap.Identifier}}, errorBypass
}
