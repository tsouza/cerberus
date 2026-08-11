package tempo

import (
	"context"
	"sort"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// This file wires `| compare({...}, topN[, start, end])` into
// `GET /api/metrics/query_range` + `GET /api/metrics/query` (and, via
// ExecMetricsRange / ExecMetricsInstant in grpc_exports.go, the gRPC
// streaming RPCs). The plan + SQL layers live in
// chplan.MetricsCompare / chsql's emitRangeWindowCompare; the SQL
// produces raw per-(cohort, attr, val, anchor) counts and this file
// mirrors upstream Tempo's BaselineAggregator
// (pkg/traceql/engine_metrics_compare.go) on top of the row stream:
//
//   - top-N per (cohort, attribute) by total count — the per-attribute
//     series cap (`topN`, default 10);
//   - per-(cohort, attribute) totals series counting EVERY occurrence
//     (not just top-N survivors);
//   - zero-filled anchor grids — upstream's per-series counts arrays
//     are zero-initialised across every interval, so each series ships
//     a sample at every grid anchor;
//   - the `__meta_type` label scheme: `{__meta_type="baseline" |
//     "selection", <attr>=<val>}` for value series and
//     `{__meta_type="baseline_total" | "selection_total", <attr>="nil"}`
//     for totals (the "nil" value mirrors the zero-Static label
//     upstream's BaselineAggregator round-trips as the string "nil").
//
// Upstream also tracks a `{__meta_error="__too_many_values__"}` series
// per attribute that exceeded topN — but those series carry no samples
// and SeriesSet.ToProto drops sample-less series, so they never reach
// the HTTP wire. Cerberus therefore doesn't synthesise them.

// compareMetaTypeLabel is Tempo's `__meta_type` label key
// (pkg/traceql/engine_metrics.go: internalLabelMetaType).
const compareMetaTypeLabel = "__meta_type"

// The four __meta_type values (engine_metrics_compare.go).
const (
	compareMetaBaseline       = "baseline"
	compareMetaSelection      = "selection"
	compareMetaBaselineTotal  = "baseline_total"
	compareMetaSelectionTotal = "selection_total"
)

// compareNilLabelValue is the wire form of the value-less attribute
// label on totals series: upstream emits a zero-value Static whose
// AnyValue projection is the string "nil".
const compareNilLabelValue = "nil"

// Internal row-stream label keys — the Sample projection in
// wrapCompareForSample surfaces the SQL columns under these names; the
// post-processor consumes and never re-emits them.
const (
	compareRowSelLabel  = "__is_sel"
	compareRowAttrLabel = "__attr"
	compareRowValLabel  = "__val"
)

// unwrapMetricsCompare returns the MetricsCompare at the plan root (or
// directly under a single Filter wrapper — mirroring
// unwrapMetricsAggregate's forward-compat posture).
func unwrapMetricsCompare(plan chplan.Node) (*chplan.MetricsCompare, bool) {
	switch v := plan.(type) {
	case *chplan.MetricsCompare:
		return v, true
	case *chplan.Filter:
		if inner, ok := v.Input.(*chplan.MetricsCompare); ok {
			return inner, true
		}
	}
	return nil, false
}

// wrapCompareForSample wraps the matrix-shape RangeWindow with the
// Sample projection the engine's row decoder expects. The cohort flag,
// attribute name and attribute value ride the Attributes map under the
// internal compareRow* keys; the post-processor turns them into the
// wire `__meta_type` scheme.
func wrapCompareForSample(rw *chplan.RangeWindow, m *chplan.MetricsCompare) chplan.Node {
	selAlias, attrAlias, valAlias, valueAlias := compareAliases(m)
	attrs := &chplan.FuncCall{
		Name: "map",
		Args: []chplan.Expr{
			&chplan.LitString{V: compareRowSelLabel},
			&chplan.FuncCall{Name: "toString", Args: []chplan.Expr{&chplan.ColumnRef{Name: selAlias}}},
			&chplan.LitString{V: compareRowAttrLabel},
			&chplan.FuncCall{Name: "toString", Args: []chplan.Expr{&chplan.ColumnRef{Name: attrAlias}}},
			&chplan.LitString{V: compareRowValLabel},
			&chplan.FuncCall{Name: "toString", Args: []chplan.Expr{&chplan.ColumnRef{Name: valAlias}}},
		},
	}
	return &chplan.Project{
		Input: rw,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: "MetricName"},
			{Expr: attrs, Alias: "Attributes"},
			{Expr: &chplan.ColumnRef{Name: "anchor_ts"}, Alias: "TimeUnix"},
			{Expr: &chplan.ColumnRef{Name: valueAlias}, Alias: "Value"},
		},
	}
}

// compareAliases resolves the output aliases with the same defaults
// the lowering and the chsql emitter pin.
func compareAliases(m *chplan.MetricsCompare) (sel, attr, val, value string) {
	sel, attr, val, value = m.SelAlias, m.AttrAlias, m.ValAlias, m.ValueAlias
	if sel == "" {
		sel = "is_selection"
	}
	if attr == "" {
		attr = "attr"
	}
	if val == "" {
		val = "val"
	}
	if value == "" {
		value = "Value"
	}
	return sel, attr, val, value
}

// compareAnchorGrid materialises the matrix anchor set the chsql
// emitters fan samples onto: `End - k*Step` for k = 0..N-1 where
// N = (End-Start)/Step + 1, returned ascending. Must stay in lockstep
// with the anchor arithmetic in internal/chsql/range_window.go
// (anchorFanoutFrag / sampleAnchorFanoutFrag).
func compareAnchorGrid(start, end time.Time, step time.Duration) []time.Time {
	if step <= 0 {
		return []time.Time{end}
	}
	span := end.Sub(start)
	if span < 0 {
		span = 0
	}
	n := int(span/step) + 1
	out := make([]time.Time, n)
	for k := 0; k < n; k++ {
		out[n-1-k] = end.Add(-time.Duration(k) * step)
	}
	return out
}

// compareSeriesState accumulates one (cohort, attr, val) series'
// per-anchor counts during the post-process fold.
type compareSeriesState struct {
	val    string
	counts map[int64]float64 // anchor unix-nanos → count
	total  float64
}

// requireCompareGridBudget fail-closes a compare() whose ZERO-FILLED wire grid
// would exceed the per-query sample budget (Config.MaxQuerySamples).
//
// compare() is the one metrics shape whose result size is not bounded by
// anything upstream of it. Every other shape hands ClickHouse's rows straight
// to the wire, so the Go-side result drain (chclient.SampleBudget /
// Config.MaxQuerySamples) charges each sample as it is read and aborts a
// runaway mid-drain. compare() instead SYNTHESISES its grid after the drain:
// postProcessCompare folds a SPARSE row stream (only the anchors that carry
// data) into a DENSE series x anchor matrix, zero-filling every gap. The rows
// actually read from CH can therefore be a handful while the emitted matrix is
// six orders of magnitude larger — so the drain budget never sees the samples
// it is supposed to bound, and neither does the CH-side max_memory_usage cap
// (the query is cheap; the expansion happens here, on the Go heap).
//
// The other two bounds miss it for the same structural reason:
// format.MaxResolutionPoints caps the anchor axis ALONE (11000) and
// requireSubquerySampleBudget deliberately counts one series' grid rather than
// anchors x series, documenting that the cardinality axis "is bounded elsewhere
// ... the result-drain SampleBudget". For compare() that elsewhere does not
// exist. This restores it: the product series x anchors IS the sample count the
// response will carry, so it is charged before a single MetricsSample is
// allocated.
//
// Rejecting (rather than truncating) matches every peer bound: the caller gets
// the same chclient.ErrTooManySamples that a drained overrun raises, which
// ClassifyErr maps to the same resource-exhausted 422 on both the HTTP and gRPC
// surfaces. Truncating would instead answer a comparison with a silently
// partial baseline, which is worse than an honest refusal.
//
// maxSamples <= 0 disables the budget, matching the cursor's and the subquery
// gate's semantics so the bound stays inert in tests that do not wire it.
// The comparison is written as a division rather than `series*anchors > max`
// so no input can overflow it into a wrapped negative that slips under the
// budget. Today's operands cannot reach that (anchors is already capped at
// format.MaxResolutionPoints upstream), so this is defence against a future
// caller with a wider grid, not a live hazard. For positive operands
// `anchors > max/series` (integer division) is exactly equivalent to
// `series*anchors > max`.
func requireCompareGridBudget(seriesCount, anchorCount int, maxSamples int64) error {
	if maxSamples <= 0 || seriesCount <= 0 || anchorCount <= 0 {
		return nil
	}
	if int64(anchorCount) > maxSamples/int64(seriesCount) {
		return &chclient.TooManySamplesError{Limit: maxSamples}
	}
	return nil
}

// postProcessCompare folds the raw (cohort, attr, val, anchor, count)
// row stream into the wire series set — the cerberus-side equivalent
// of upstream Tempo's BaselineAggregator.Results. `anchors` is the
// full matrix grid (every series zero-fills across it); `topN` is the
// per-(cohort, attribute) value cap.
//
// `maxSamples` bounds the zero-filled series x anchor product this builds; see
// requireCompareGridBudget for why this is the only place that product can be
// charged. A crossing returns chclient.ErrTooManySamples and no series.
func postProcessCompare(
	samples []chclient.Sample, topN int, anchors []time.Time, maxSamples int64,
) ([]MetricsSeries, error) {
	type cohortAttr struct {
		meta string // baseline | selection
		attr string
	}
	values := map[cohortAttr]map[string]*compareSeriesState{}
	totals := map[cohortAttr]map[int64]float64{}

	for _, s := range samples {
		attr := s.Labels[compareRowAttrLabel]
		if attr == "" {
			continue
		}
		meta := compareMetaBaseline
		if s.Labels[compareRowSelLabel] == "1" || s.Labels[compareRowSelLabel] == "true" {
			meta = compareMetaSelection
		}
		ca := cohortAttr{meta: meta, attr: attr}
		val := s.Labels[compareRowValLabel]
		ns := s.Timestamp.UnixNano()

		vm, ok := values[ca]
		if !ok {
			vm = map[string]*compareSeriesState{}
			values[ca] = vm
		}
		st, ok := vm[val]
		if !ok {
			st = &compareSeriesState{val: val, counts: map[int64]float64{}}
			vm[val] = st
		}
		st.counts[ns] += s.Value
		st.total += s.Value

		tm, ok := totals[ca]
		if !ok {
			tm = map[int64]float64{}
			totals[ca] = tm
		}
		tm[ns] += s.Value
	}

	// Charge the grid BEFORE building it. The emitted series set is one series
	// per surviving (cohort, attr, val) plus one totals series per
	// (cohort, attr), and every one of them zero-fills across every anchor —
	// so this product is exactly the sample count the response would carry.
	// Counting it here needs no sorting and allocates nothing, which is the
	// point: an over-budget compare() is refused before the matrix exists on
	// the heap, not after.
	emitted := len(totals)
	for _, vm := range values {
		n := len(vm)
		if topN > 0 && n > topN {
			n = topN
		}
		emitted += n
	}
	if err := requireCompareGridBudget(emitted, len(anchors), maxSamples); err != nil {
		return nil, err
	}

	gridSamples := func(counts map[int64]float64) []MetricsSample {
		out := make([]MetricsSample, 0, len(anchors))
		for _, a := range anchors {
			out = append(out, MetricsSample{
				TimestampMs: a.UnixMilli(),
				Value:       counts[a.UnixNano()],
			})
		}
		return out
	}

	series := make([]MetricsSeries, 0, len(values)*2)
	addSeries := func(meta, attr, val string, counts map[int64]float64) {
		series = append(series, MetricsSeries{
			Labels: []MetricsLabel{
				{Key: compareMetaTypeLabel, Value: meta},
				{Key: attr, Value: val},
			},
			Samples:   gridSamples(counts),
			Exemplars: []Exemplar{},
		})
	}

	// Deterministic emission order: (meta, attr) keys sorted, then the
	// top-N values per key (ties broken by value string ascending so two
	// equal-count values rank stably — upstream's sort is unstable here,
	// which only matters when topN truncates a tie).
	cas := make([]cohortAttr, 0, len(values))
	for ca := range values {
		cas = append(cas, ca)
	}
	sort.Slice(cas, func(i, j int) bool {
		if cas[i].meta != cas[j].meta {
			return cas[i].meta < cas[j].meta
		}
		return cas[i].attr < cas[j].attr
	})
	for _, ca := range cas {
		vm := values[ca]
		states := make([]*compareSeriesState, 0, len(vm))
		for _, st := range vm {
			states = append(states, st)
		}
		sort.Slice(states, func(i, j int) bool {
			if states[i].total != states[j].total {
				return states[i].total > states[j].total
			}
			return states[i].val < states[j].val
		})
		n := len(states)
		if topN > 0 && n > topN {
			n = topN
		}
		for _, st := range states[:n] {
			addSeries(ca.meta, ca.attr, st.val, st.counts)
		}
	}

	// Totals series — one per (cohort, attribute), value label "nil".
	totalMeta := func(meta string) string {
		if meta == compareMetaSelection {
			return compareMetaSelectionTotal
		}
		return compareMetaBaselineTotal
	}
	tcas := make([]cohortAttr, 0, len(totals))
	for ca := range totals {
		tcas = append(tcas, ca)
	}
	sort.Slice(tcas, func(i, j int) bool {
		if tcas[i].meta != tcas[j].meta {
			return tcas[i].meta < tcas[j].meta
		}
		return tcas[i].attr < tcas[j].attr
	})
	for _, ca := range tcas {
		addSeries(totalMeta(ca.meta), ca.attr, compareNilLabelValue, totals[ca])
	}

	return series, nil
}

// execCompareRange runs the matrix-shape pipeline for a lowered
// compare() plan and returns the post-processed series list. Every Tempo
// entrypoint reaches it through metricsPipelineRouter.Compare, so the range
// and instant surfaces cannot drift apart on the BaselineAggregator mirror.
//
// `shape` is the engine.Meta response-shape tag: the matrix tag for a range
// request, the instant tag for the single-anchor collapse, so the
// telemetry/corpus bucket names the request the caller actually made.
func (h *Handler) execCompareRange(
	ctx context.Context,
	q string,
	plan chplan.Node,
	cmp *chplan.MetricsCompare,
	start, end time.Time,
	step time.Duration,
	shape string,
) ([]MetricsSeries, map[string]string, error) {
	rw := &chplan.RangeWindow{
		Input:           plan,
		Range:           step,
		Step:            step,
		Start:           start,
		End:             end,
		TimestampColumn: h.Schema.TimestampColumn,
	}
	wrapped := wrapCompareForSample(rw, cmp)

	res, qerr := h.Engine.QueryPlan(ctx, metricsLang{spansTable: h.Schema.SpansTable}, wrapped, engine.Meta{
		IsMetric:      true,
		ResponseShape: shape,
	})
	if qerr != nil {
		return nil, nil, qerr
	}
	h.Logger.Debug("cerberus tempo metrics compare exec",
		"traceql", telemetry.SanitizeForLog(q), "shape", shape,
		"start", start, "end", end, "step", step,
		"sql", res.SQL, "args", telemetry.SanitizeArgsForLog(res.Args))

	series, err := postProcessCompare(
		res.Samples, cmp.TopN, compareAnchorGrid(start, end, step), h.Engine.MaxQuerySamples,
	)
	if err != nil {
		return nil, nil, err
	}
	return series, res.Headers, nil
}

// metricsSeriesToInstant collapses a post-processed matrix-shape series
// list to Tempo's instant envelope — with a single anchor (step = end -
// start) each series carries exactly one sample whose value becomes the
// InstantSeries.Value (translateQueryRangeToInstant semantics). Shared by
// the instant router's compare() and histogram_over_time() branches, which
// both evaluate through the matrix emitter and collapse afterwards.
func metricsSeriesToInstant(series []MetricsSeries) []MetricsInstantSeries {
	out := make([]MetricsInstantSeries, 0, len(series))
	for _, s := range series {
		v := 0.0
		if len(s.Samples) > 0 {
			v = s.Samples[0].Value
		}
		out = append(out, MetricsInstantSeries{Labels: s.Labels, Value: v})
	}
	return out
}
