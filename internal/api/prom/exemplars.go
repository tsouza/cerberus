package prom

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// ExemplarSeries is the wire shape for one element of the
// `/api/v1/query_exemplars` data array — one series identified by its
// label set with the matched exemplars grouped under it.
type ExemplarSeries struct {
	SeriesLabels map[string]string `json:"seriesLabels"`
	Exemplars    []Exemplar        `json:"exemplars"`
}

// Exemplar is one exemplar inside an ExemplarSeries. `Value` is a float
// (Prom's exemplar JSON keeps it as a number, unlike Sample which
// stringifies for precision). `Timestamp` is unix seconds with fractional
// nanos.
type Exemplar struct {
	Labels    map[string]string `json:"labels"`
	Value     float64           `json:"value"`
	Timestamp float64           `json:"timestamp"`
}

// handleQueryExemplars implements `/api/v1/query_exemplars`.
//
// Upstream contract:
// https://prometheus.io/docs/prometheus/latest/querying/api/#querying-exemplars
//
// Required params: `query` (PromQL string), `start` and `end` (RFC3339 or
// unix seconds). The `query` must be a single VectorSelector (Prom rejects
// anything else); cerberus mirrors that. The response is the canonical
// Prom envelope with `data` shaped as []ExemplarSeries.
//
// Implementation flow:
//
//  1. Validate the query / start / end parameters (existing behaviour).
//  2. Parse the PromQL, walk through any ParenExpr, and require a single
//     `*parser.VectorSelector`. Anything more complex returns ErrBadData —
//     upstream Prometheus also restricts this endpoint to one selector.
//  3. Resolve the target table via [exemplarsTableFor]. Summary metrics
//     short-circuit with `data:[]` since the OTel-CH summary table has
//     no Exemplars column upstream — exemplars are a histogram concept
//     and clients should not see an error for a legitimately
//     exemplar-free metric type.
//  4. Build the matcher predicate via the same
//     [promql.BuildMatcherPredicate] helper PromQL `handleQuery` /
//     `handleQueryRange` use.
//  5. Call [chsql.EmitQueryExemplars] to render the SQL + args.
//  6. Run the SQL via Querier.QueryExemplars; decode each row positionally
//     into a [chclient.ExemplarRow].
//  7. Group rows by `(MetricName, Attributes, ServiceName)` into one
//     ExemplarSeries each, then project per-exemplar Labels with the
//     reserved-key merge: ExemplarAttributes carries the SDK-recorded
//     FilteredAttributes, and `trace_id` / `span_id` from the dedicated
//     columns are overlaid (the columns are authoritative; empty values
//     are dropped). See plan §3 + §7 "Reserved-key precedence".
//
// Returns `data:[]` (not nil) so the JSON envelope renders `"data":[]`
// rather than `"data":null`; Grafana's exemplars probe distinguishes
// the two.
func (h *Handler) handleQueryExemplars(w http.ResponseWriter, r *http.Request) {
	// r.FormValue merges URL query params with POST form-encoded body
	// (auto-calling ParseForm). Matches the consistent surface used by
	// handleQuery / handleQueryRange.
	q := r.FormValue("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}
	expr, err := h.parseExpr(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	start, err := format.ParseTimeProm(r.FormValue("start"), time.Time{})
	if err != nil || start.IsZero() {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing or invalid 'start' parameter"))
		return
	}
	end, err := format.ParseTimeProm(r.FormValue("end"), time.Time{})
	if err != nil || end.IsZero() {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing or invalid 'end' parameter"))
		return
	}
	if end.Before(start) {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("'end' must be after 'start'"))
		return
	}

	vs, err := singleVectorSelector(expr)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	metricName := exemplarMetricName(vs.LabelMatchers)
	if metricName == "" {
		// Prom requires a concrete `__name__=...` matcher on this
		// endpoint. Mirror the upstream behaviour rather than fan out
		// across every metrics table.
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("metric name is required"))
		return
	}

	table, ok := exemplarsTableFor(metricName, h.Schema)
	if !ok {
		// Summary metrics (or any other family the upstream OTel-CH
		// schema doesn't carry exemplars for) return an empty data
		// array — matches Prom's behaviour for exemplar-free metric
		// types. No ClickHouse round-trip.
		writeJSON(w, http.StatusOK, Response{
			Status: "success",
			Data:   []ExemplarSeries{},
		})
		return
	}

	predicate, err := buildExemplarsPredicate(vs.LabelMatchers, h.Schema)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrInternal, err)
		return
	}

	sql, args, err := chsql.EmitQueryExemplars(r.Context(), table, predicate, start, end, h.Schema)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrInternal, err)
		return
	}
	h.Logger.Debug("cerberus query_exemplars", "promql", q, "sql", sql, "args", args)

	rows, err := h.Client.QueryExemplars(r.Context(), sql, args...)
	if err != nil {
		h.respondError(w, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway})
		return
	}

	series := groupExemplars(rows, metricName)
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   series,
	})
}

// singleVectorSelector returns the unique [promparser.VectorSelector] in
// expr or an error if the expression is anything else. ParenExpr wrappers
// are unwrapped recursively. The upstream Prom contract restricts this
// endpoint's `query` to a single VectorSelector (see
// `prometheus/web/api/v1/api.go::queryExemplars`); anything else is
// rejected with `ErrBadData`.
func singleVectorSelector(expr promparser.Expr) (*promparser.VectorSelector, error) {
	for {
		switch e := expr.(type) {
		case *promparser.VectorSelector:
			return e, nil
		case *promparser.ParenExpr:
			expr = e.Expr
		default:
			return nil, fmt.Errorf("query_exemplars requires a single vector selector, got %T", expr)
		}
	}
}

// exemplarMetricName returns the value of the `__name__` equality
// matcher (if any) — same heuristic the PromQL lowering applies to
// pick the target metrics table. Returns "" when the selector relies
// purely on regex / non-name matchers; the exemplars handler treats
// that as `ErrBadData` because Prom's contract requires a concrete
// metric name.
//
// Mirrors `metricNameFromMatchers` in internal/promql/lower.go (kept
// in-package there to avoid the cross-package import in the PromQL hot
// path).
func exemplarMetricName(ms []*labels.Matcher) string {
	for _, m := range ms {
		if m.Name == model.MetricNameLabel && m.Type == labels.MatchEqual {
			return m.Value
		}
	}
	return ""
}

// exemplarsTableFor picks the OTel-CH metrics table whose Exemplars
// Nested column carries the queried metric's exemplars. The OTel-CH
// summary table has no Exemplars column; the boolean return is `false`
// for summary metrics (and any other case where no exemplars-carrying
// table is configured) so the handler short-circuits with empty data
// rather than emitting SQL against a missing column.
//
// Routing heuristic (extends [schema.Metrics.TableFor], which only
// returns Gauge or Sum):
//
//   - [schema.Metrics.IsExpHistogramMetric] hit → ExpHistogramTable.
//   - `_bucket` suffix → HistogramTable. Buckets carry the per-bound
//     observation counts for classic histograms; exemplars there are
//     written on the histogram table by the OTel SDK.
//   - `_count` / `_sum` suffix → HistogramTable when configured,
//     otherwise SumTable. The PromQL `TableFor` heuristic groups
//     `_count` / `_sum` with the sum table, but for exemplars they
//     more often belong to the histogram family (the SDK writes
//     `_count` / `_sum` synthetic series alongside `_bucket`).
//   - `_total` suffix → SumTable (counter convention).
//   - Otherwise → GaugeTable.
func exemplarsTableFor(metricName string, s schema.Metrics) (string, bool) {
	if s.IsExpHistogramMetric(metricName) {
		if s.ExpHistogramTable == "" {
			return "", false
		}
		return s.ExpHistogramTable, true
	}
	if hasExemplarSuffix(metricName, "_bucket") {
		if s.HistogramTable == "" {
			return "", false
		}
		return s.HistogramTable, true
	}
	if hasExemplarSuffix(metricName, "_count") || hasExemplarSuffix(metricName, "_sum") {
		if s.HistogramTable != "" {
			return s.HistogramTable, true
		}
		if s.SumTable != "" {
			return s.SumTable, true
		}
		return "", false
	}
	if hasExemplarSuffix(metricName, "_total") {
		if s.SumTable == "" {
			return "", false
		}
		return s.SumTable, true
	}
	if s.GaugeTable == "" {
		return "", false
	}
	return s.GaugeTable, true
}

func hasExemplarSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// buildExemplarsPredicate AND-folds the matcher list into a single
// [chsql.Frag] suitable for the [chsql.EmitQueryExemplars] predicate
// slot. Delegates the per-matcher → predicate translation to the
// existing PromQL [promql.BuildMatcherPredicate] helper, then adapts
// the [chplan.Expr] tree to a Frag via [chsql.Builder.Expr].
//
// The single point of conversion keeps cerberus on one matcher → SQL
// path across PromQL's `/query`, `/query_range`, and this endpoint —
// any future schema-aware matcher rewrite (e.g. routing
// `service.name="X"` to the dedicated [schema.Metrics.ServiceNameColumn]
// instead of the Attributes map) lives in the lowering helper, not
// duplicated here.
func buildExemplarsPredicate(matchers []*labels.Matcher, s schema.Metrics) (chsql.Frag, error) {
	pred := promql.BuildMatcherPredicate(matchers, s)
	if pred == nil {
		return nil, nil
	}
	// Dry-run the rendering once so a schema/expr surface that
	// Builder.Expr can't handle surfaces as a 500 here, before the SQL
	// lands in front of ClickHouse.
	if err := (&chsql.Builder{}).Expr(pred); err != nil {
		return nil, fmt.Errorf("query_exemplars: lower matcher: %w", err)
	}
	return func(b *chsql.Builder) {
		_ = b.Expr(pred)
	}, nil
}

// groupExemplars folds [chclient.ExemplarRow] rows into ExemplarSeries
// keyed by `(MetricName, Attributes, ServiceName)`. The returned slice
// is deterministically ordered by the canonical series-key so two runs
// against the same row set produce identical wire envelopes.
//
// metricName is the resolved `__name__` matcher value; row.MetricName
// is the authoritative source — they match in normal operation, but
// the matcher value is the fallback when the row carries a blank
// MetricName for any reason.
//
// Per-exemplar Labels: ExemplarAttributes (the SDK-recorded
// FilteredAttributes map) is the base, then `trace_id` / `span_id`
// from the dedicated row columns overlay. Empty TraceID / SpanID
// columns are dropped to match Prom's behaviour for exemplars without
// trace linkage.
func groupExemplars(rows []chclient.ExemplarRow, metricName string) []ExemplarSeries {
	if len(rows) == 0 {
		return []ExemplarSeries{}
	}

	type bucket struct {
		labels    map[string]string
		exemplars []Exemplar
	}
	bySeries := map[string]*bucket{}
	keys := make([]string, 0, len(rows))

	for _, r := range rows {
		name := r.MetricName
		if name == "" {
			name = metricName
		}
		seriesLabels := format.WithMetricName(r.Attributes, name)
		if r.ServiceName != "" {
			// The dedicated LowCardinality column is the OTel exporter's
			// reserved place for service.name. Stamp it under the OTel
			// key first, then let NormalizeLabelMap collapse it to the
			// Prom-grammar `service_name` form — that way an Attributes
			// entry under the same key (or its underscored sibling) is
			// honoured by the collision-policy in one place.
			seriesLabels["service.name"] = r.ServiceName
		}
		seriesLabels = format.NormalizeLabelMap(seriesLabels)
		key := format.CanonicalKey(seriesLabels)
		b, ok := bySeries[key]
		if !ok {
			b = &bucket{labels: seriesLabels}
			bySeries[key] = b
			keys = append(keys, key)
		}
		b.exemplars = append(b.exemplars, projectExemplar(r))
	}

	sort.Strings(keys)
	out := make([]ExemplarSeries, 0, len(keys))
	for _, k := range keys {
		b := bySeries[k]
		out = append(out, ExemplarSeries{
			SeriesLabels: b.labels,
			Exemplars:    b.exemplars,
		})
	}
	return out
}

// projectExemplar shapes one row into a wire-format Exemplar. The
// Labels map merges the SDK-recorded ExemplarAttributes
// (FilteredAttributes upstream) with the reserved `trace_id` /
// `span_id` keys from the dedicated columns. Per the plan §7
// "Reserved-key precedence": the dedicated columns ALWAYS win over a
// collision in ExemplarAttributes (the OTel-CH exporter writes them
// from the OTel SpanContext, not from the SDK-supplied attribute set,
// so they are authoritative). Empty TraceID / SpanID columns are
// dropped — no `"trace_id":""` keys land on the wire.
func projectExemplar(r chclient.ExemplarRow) Exemplar {
	out := make(map[string]string, len(r.ExemplarAttributes)+2)
	for k, v := range r.ExemplarAttributes {
		out[k] = v
	}
	if r.TraceID != "" {
		out["trace_id"] = r.TraceID
	}
	if r.SpanID != "" {
		out["span_id"] = r.SpanID
	}
	// Per-exemplar label keys obey the same Prom grammar — Grafana's
	// exemplar overlay parses them as label identifiers when rendering
	// the trace-link tooltip. Dotted OTel keys (`http.target`,
	// `code.namespace`) surface as `http_target` / `code_namespace`.
	return Exemplar{
		Labels:    format.NormalizeLabelMap(out),
		Value:     r.Value,
		Timestamp: timestampSeconds(r.Timestamp),
	}
}

// timestampSeconds turns a CH DateTime64(9) value into unix seconds with
// fractional nanos — the per-exemplar timestamp wire shape Prometheus
// uses (numeric, not stringified). Equivalent to
// `float64(t.UnixNano()) / 1e9` but stays well-defined past 2262 where
// nanoseconds overflow int64.
func timestampSeconds(t time.Time) float64 {
	return float64(t.Unix()) + float64(t.Nanosecond())/1e9
}
