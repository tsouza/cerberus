package tempo

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
)

// This file wires `| histogram_over_time(<attr>)` into
// `GET /api/metrics/query_range`. The plan + SQL layers landed first
// (chplan.MetricsHistogramOverTime, chsql.emitRangeWindowHistogram —
// the matrix fan-out groups by (<user group-by>, __bucket, anchor_ts));
// this is the HTTP envelope on top.
//
// Wire shape: Tempo's reference engine routes histogram_over_time
// through the same bucketize machinery as quantile_over_time
// (pkg/traceql/ast_metrics.go: byFuncLabel = internalLabelBucket) but —
// unlike quantile, which folds buckets into per-phi series — the
// histogram's FINAL series keep `__bucket` as a real output label: one
// series per (group-by labels…, __bucket=<edge>) carrying per-anchor
// counts. Grafana's first-party Traces Drilldown app (preinstalled
// since Grafana 12.x) drives this shape on its duration-histogram
// panel.
//
// Exemplars: upstream Tempo registers `exemplarNaN` for
// histogram_over_time ("Histogram final series are counts so exemplars
// are placeholders"), so cerberus ships the empty `Exemplars: []`
// envelope — the same shape every series gets before attachment.

// unwrapMetricsHistogram returns the MetricsHistogramOverTime at the
// plan root (or directly under a single Filter wrapper — mirroring
// unwrapMetricsAggregate's forward-compat posture).
func unwrapMetricsHistogram(plan chplan.Node) (*chplan.MetricsHistogramOverTime, bool) {
	switch v := plan.(type) {
	case *chplan.MetricsHistogramOverTime:
		return v, true
	case *chplan.Filter:
		if inner, ok := v.Input.(*chplan.MetricsHistogramOverTime); ok {
			return inner, true
		}
	}
	return nil, false
}

// histogramLabelNames mirrors metricsLabelNames for the histogram node:
// the user-facing group-by label names (display-name → alias →
// "group_<i>" fallback chain) with `__bucket` appended — every
// histogram series carries the bucket-edge label, grouped or not.
func histogramLabelNames(m *chplan.MetricsHistogramOverTime) []string {
	out := make([]string, 0, len(m.GroupBy)+1)
	for i := range m.GroupBy {
		if i < len(m.GroupByDisplayNames) && m.GroupByDisplayNames[i] != "" {
			out = append(out, m.GroupByDisplayNames[i])
			continue
		}
		if i < len(m.GroupByAliases) && m.GroupByAliases[i] != "" {
			out = append(out, m.GroupByAliases[i])
			continue
		}
		out = append(out, "group_"+strconv.Itoa(i))
	}
	out = append(out, tempoQuantileBucketLabel)
	return out
}

// wrapHistogramForSample wraps the matrix-shape RangeWindow with the
// Sample projection the engine's row decoder expects — the histogram
// analogue of wrapMetricsForSample's quantile branch, except `__bucket`
// is a real wire label here (no post-fold strips it).
func wrapHistogramForSample(rw *chplan.RangeWindow, m *chplan.MetricsHistogramOverTime) chplan.Node {
	attrAliases := metricsOuterGroupAliases(m.GroupBy, m.GroupByAliases)
	labelNames := histogramLabelNames(m)

	args := make([]chplan.Expr, 0, (len(m.GroupBy)+1)*2)
	for i := range m.GroupBy {
		args = append(
			args,
			&chplan.LitString{V: labelNames[i]},
			&chplan.FuncCall{
				Name: "toString",
				Args: []chplan.Expr{&chplan.ColumnRef{Name: attrAliases[i]}},
			},
		)
	}
	args = append(
		args,
		&chplan.LitString{V: tempoQuantileBucketLabel},
		&chplan.FuncCall{
			Name: "toString",
			Args: []chplan.Expr{&chplan.ColumnRef{Name: m.BucketAlias}},
		},
	)

	return &chplan.Project{
		Input: rw,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: "MetricName"},
			{Expr: &chplan.FuncCall{Name: "map", Args: args}, Alias: "Attributes"},
			{Expr: &chplan.ColumnRef{Name: "anchor_ts"}, Alias: "TimeUnix"},
			{Expr: &chplan.ColumnRef{Name: m.ValueAlias}, Alias: "Value"},
		},
	}
}

// serveMetricsQueryRangeHistogram runs the matrix-shape pipeline for a
// lowered histogram_over_time plan and writes the Tempo
// series-of-samples envelope.
func (h *Handler) serveMetricsQueryRangeHistogram(
	ctx context.Context,
	w http.ResponseWriter,
	q string,
	plan chplan.Node,
	hist *chplan.MetricsHistogramOverTime,
	start, end time.Time,
	step time.Duration,
) {
	rw := &chplan.RangeWindow{
		Input:           plan,
		Range:           step,
		Step:            step,
		Start:           start,
		End:             end,
		TimestampColumn: h.Schema.TimestampColumn,
	}
	wrapped := wrapHistogramForSample(rw, hist)

	res, qerr := h.Engine.QueryPlan(ctx, metricsLang{}, wrapped, engine.Meta{
		IsMetric:      true,
		ResponseShape: "tempo-metrics-matrix",
	})
	if qerr != nil {
		writeError(w, classifyMetricsQueryRangeErr(qerr), "", "", qerr)
		return
	}
	h.Logger.Debug("cerberus tempo metrics_query_range histogram",
		"traceql", q, "start", start, "end", end, "step", step,
		"sql", res.SQL, "args", res.Args)

	series := toMetricsSeriesWithNames(res.Samples, histogramLabelNames(hist))

	writeEngineHeaders(w, res.Headers)
	writeJSON(w, http.StatusOK, MetricsQueryRangeResponse{
		Series: series,
	})
}
