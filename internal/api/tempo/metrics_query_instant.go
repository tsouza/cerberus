package tempo

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/telemetry"
	traceql_lower "github.com/tsouza/cerberus/internal/traceql"
)

// MetricsQueryInstantResponse is the body of `GET /api/metrics/query`.
// Mirrors Tempo's `tempopb.QueryInstantResponse` JSON wire shape — one
// InstantSeries per (group-by labels) tuple, each carrying a single
// `value` rather than a `samples` array. Grafana's Tempo datasource
// uses this for the "now"-style metric widgets that need a scalar per
// series rather than a matrix.
//
// Tempo's reference implementation collapses a range response to an
// instant one via translateQueryRangeToInstant (modules/frontend/
// metrics_query_handler.go): step = end - start (one bucket), then
// each series's first Samples entry becomes the InstantSeries.Value.
// Cerberus follows the same shape so the differ's CompareMetrics
// (compatibility/tempo/driver/differ_metrics.go) can canonicalise
// both backends' responses without a shape-specific branch.
type MetricsQueryInstantResponse struct {
	Series []MetricsInstantSeries `json:"series"`
}

// MetricsInstantSeries is one entry of MetricsQueryInstantResponse.Series.
// Mirrors `tempopb.InstantSeries` — the `samples` field is replaced by
// a single `value` scalar.
type MetricsInstantSeries struct {
	Labels []MetricsLabel `json:"labels"`
	Value  float64        `json:"value"`
}

// handleMetricsQueryInstant implements `GET /api/metrics/query`.
//
// Tempo's reference semantics: parse the TraceQL metrics-pipeline query
// as if it were a range query, but evaluate it over a single bucket
// spanning [start, end] (step = end - start). Each resulting series is
// projected to a single (labels, value) tuple — the first sample of
// the range envelope becomes the instant value. See translateQueryRangeToInstant
// in upstream tempo modules/frontend/metrics_query_handler.go.
//
// Contract: `q` (or `query`) = TraceQL metrics-pipeline expression;
// `start` / `end` = unix seconds or nanoseconds (parseTempoStartEnd
// also accepts RFC3339). `step` is unused at the wire level — the
// handler synthesises it from end-start so the chplan.RangeWindow
// emits exactly one anchor.
func (h *Handler) handleMetricsQueryInstant(w http.ResponseWriter, r *http.Request) {
	q := metricsInstantQueryParam(r)
	if q == "" {
		writeError(w, http.StatusBadRequest, "", "", errors.New("missing 'q' parameter"))
		return
	}
	start, end, err := parseTempoStartEnd(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "", "", err)
		return
	}
	if start.IsZero() || end.IsZero() {
		writeError(w, http.StatusBadRequest, "", "", errors.New("'start' and 'end' parameters are required"))
		return
	}
	// Single-bucket evaluation: step = end - start so the inner
	// chplan.RangeWindow emits exactly one anchor whose sample is the
	// instant value Tempo's translateQueryRangeToInstant would pick.
	step := end.Sub(start)
	if step <= 0 {
		writeError(w, http.StatusBadRequest, "", "", errors.New("'end' must be after 'start'"))
		return
	}

	ctx := r.Context()
	// Parse + lower inline (same pattern as handleMetricsQueryRange) so
	// we can wrap the lowered plan with the matrix-shape RangeWindow
	// before engine.QueryPlan runs.
	parseT := telemetry.ObserveStage(telemetry.StageParse)
	expr, perr := parseExpr(ctx, q)
	parseT.Done(ctx)
	if perr != nil {
		writeError(w, http.StatusBadRequest, "", "", perr)
		return
	}
	// Thread the request window onto ctx so the universal recursive-arm window
	// stamp (traceql.stampRecursiveScanWindow) bounds the EMITTER-SYNTHETIC
	// recursive spans scans of a metrics-over-structural / nested-set source; the
	// RangeWindow wrap below cannot reach below a WITH RECURSIVE.
	ctx = traceql_lower.WithSearchWindow(ctx, start, end)
	lowerT := telemetry.ObserveStage(telemetry.StageLower)
	plan, lerr := traceql_lower.Lower(ctx, expr, h.Schema)
	lowerT.Done(ctx)
	if lerr != nil {
		writeError(w, http.StatusUnprocessableEntity, "", "", lerr)
		return
	}

	// Second-stage transforms (`| topk(N)` / `| bottomk(N)` / `| > N`)
	// wrap the aggregate — peel them off so the RangeWindow wraps the
	// aggregate itself, then re-apply around the windowed result. The
	// instant path passes no PartitionBy: a single anchor means a
	// single global selection (`ORDER BY Value LIMIT K`), matching
	// Tempo's translateQueryRangeToInstant collapse.
	stages, inner := peelMetricsSecondStages(plan)
	metrics, ok := unwrapMetricsAggregate(inner)
	if !ok {
		// `| compare({...}, topN)` follows its own post-processing path
		// (the BaselineAggregator mirror); with a single anchor the
		// per-series sample collapses to the InstantSeries.Value —
		// translateQueryRangeToInstant semantics, same as the scalar ops.
		if cmp, cok := unwrapMetricsCompare(inner); cok {
			if len(stages) > 0 {
				writeError(w, http.StatusUnprocessableEntity, "", "",
					fmt.Errorf("traceql: second-stage %s over compare() is unsupported — compare() series carry the __meta_type split, not a scalar Value to rank or threshold", stages[0].Op))
				return
			}
			// Start = End on purpose — see the RangeWindow comment below
			// for why the instant anchor sits at `end` only.
			series, headers, cerr := h.execCompareRange(ctx, q, plan, cmp, end, end, step)
			if cerr != nil {
				writeError(w, classifyMetricsQueryRangeErr(cerr), "", "", cerr)
				return
			}
			writeEngineHeaders(w, headers)
			writeJSON(w, http.StatusOK, MetricsQueryInstantResponse{
				Series: compareSeriesToInstant(series),
			})
			return
		}
		writeError(w, http.StatusBadRequest, "", "",
			fmt.Errorf("query %q is not a TraceQL metrics-pipeline expression — /api/metrics/query requires `| rate()`, `| count_over_time()`, `| *_over_time(...)` or `| quantile_over_time(...)`", q))
		return
	}
	if len(stages) > 0 && metrics.Op == chplan.MetricsOpQuantileOverTime {
		// Same boundary as handleMetricsQueryRange: quantiles fold from
		// bucket rows Go-side, after SQL — a SQL-side rank/threshold
		// would operate on bucket counts, not quantile values.
		writeError(w, http.StatusUnprocessableEntity, "", "",
			fmt.Errorf("traceql: second-stage %s over quantile_over_time is unsupported — quantiles are computed from bucket rows after SQL execution", stages[0].Op))
		return
	}

	// Start = End on purpose: the matrix emitter generates
	// (End-Start)/Step + 1 anchors at `End - i*Step`, so Start==End
	// yields the single anchor at `end` whose right-closed window
	// (end - Range, end] IS the instant bucket [start, end] Tempo's
	// IntervalMapperInstant evaluates. Passing Start=start here would
	// fan out a second anchor at `start` covering (start-step, start]
	// — entirely before the requested window — which evaluates to 0
	// and, being the earliest sample, used to win the
	// first-sample-by-timestamp pick in toMetricsInstantSeries: every
	// instant compat case returned 0 instead of the real value. The
	// inner-scan time-bound pushdown still prunes on
	// (Start - Range, End] = (start, end], so partition pruning is
	// unaffected.
	rw := &chplan.RangeWindow{
		Input:           inner,
		Range:           step,
		Step:            step,
		Start:           end,
		End:             end,
		TimestampColumn: h.Schema.TimestampColumn,
	}
	wrapped := wrapMetricsForSample(applyMetricsSecondStages(rw, stages, nil), metrics)

	res, qerr := h.Engine.QueryPlan(ctx, metricsLang{spansTable: h.Schema.SpansTable}, wrapped, engine.Meta{
		IsMetric:      true,
		ResponseShape: "tempo-metrics-instant",
	})
	if qerr != nil {
		writeError(w, classifyMetricsQueryRangeErr(qerr), "", "", qerr)
		return
	}
	h.Logger.Debug("cerberus tempo metrics_query_instant",
		"traceql", q, "start", start, "end", end, "step", step,
		"sql", res.SQL, "args", res.Args)

	// quantile_over_time: same bucket-shape → quantile collapse as in
	// the range handler — Tempo's reference engine routes the instant
	// shape through the same HistogramAggregator, so the post-processor
	// runs before the instant projection.
	samples := res.Samples
	if metrics.Op == chplan.MetricsOpQuantileOverTime {
		samples = postProcessQuantileBuckets(samples, metrics)
	}

	writeEngineHeaders(w, res.Headers)
	writeJSON(w, http.StatusOK, MetricsQueryInstantResponse{
		Series: toMetricsInstantSeries(samples, metrics),
	})
}

// metricsInstantQueryParam mirrors Tempo's ParseQueryInstantRequest:
// accept both `q` and `query` (the latter is the original
// prom-compat alias Grafana still emits on some panels). When both
// are present the last-set parameter wins — same precedence as
// upstream's two sequential extractQueryParam calls.
func metricsInstantQueryParam(r *http.Request) string {
	vals := r.URL.Query()
	if s := vals.Get("query"); s != "" {
		return s
	}
	return vals.Get("q")
}

// toMetricsInstantSeries pivots the flat sample stream into Tempo's
// instant-series envelope. For each unique label set, the first sample
// (sorted ascending by timestamp for determinism) becomes the instant
// value — same rule Tempo's translateQueryRangeToInstant applies
// upstream. With step=end-start the chplan.RangeWindow emits exactly
// one anchor per series, so the "first sample" is also the only
// sample; the sort is defensive against a future RangeWindow that
// returns multiple anchors.
func toMetricsInstantSeries(samples []chclient.Sample, m *chplan.MetricsAggregate) []MetricsInstantSeries {
	labelNames := metricsLabelNames(m)

	type bucket struct {
		labels   []MetricsLabel
		firstTS  time.Time
		firstVal float64
		filled   bool
	}
	byKey := map[string]*bucket{}

	for _, s := range samples {
		key := format.CanonicalKey(s.Labels)
		b, ok := byKey[key]
		if !ok {
			b = &bucket{labels: labelsFromSample(s.Labels, labelNames)}
			byKey[key] = b
		}
		if !b.filled || s.Timestamp.Before(b.firstTS) {
			b.firstTS = s.Timestamp
			b.firstVal = s.Value
			b.filled = true
		}
	}

	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]MetricsInstantSeries, 0, len(byKey))
	for _, k := range keys {
		b := byKey[k]
		out = append(out, MetricsInstantSeries{Labels: b.labels, Value: b.firstVal})
	}
	return out
}
