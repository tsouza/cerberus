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
	"github.com/tsouza/cerberus/internal/telemetry"
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
// anything else); cerberus mirrors that. Every matcher shape a selector
// can carry is answered — a regex `__name__`, or no `__name__` at all —
// because the table fan-out is resolved from the matcher set rather than
// from a literal metric name. The response is the canonical Prom envelope
// with `data` shaped as []ExemplarSeries.
//
// Implementation flow:
//
//  1. Validate the query / start / end parameters (existing behaviour).
//  2. Parse the PromQL, walk through any ParenExpr, and require a single
//     `*parser.VectorSelector`. Anything more complex returns ErrBadData —
//     upstream Prometheus also restricts this endpoint to one selector.
//  3. Resolve the candidate tables and their per-table row keys via
//     [exemplarArms], which routes through
//     [schema.Metrics.ExemplarSources]. An empty candidate set (a
//     summary-only deployment — the OTel-CH summary table has no
//     Exemplars column upstream) short-circuits with `data:[]`:
//     exemplars are a histogram concept and clients should not see an
//     error for a legitimately exemplar-free metric type.
//  4. Build each arm's matcher predicate via the same
//     [promql.BuildMatcherPredicate] helper PromQL `handleQuery` /
//     `handleQueryRange` use.
//  5. Call [chsql.EmitQueryExemplarsUnion] to render the SQL + args —
//     one arm per candidate table, unioned.
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

	arms, err := exemplarArms(vs.LabelMatchers, metricName, h.Schema)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrInternal, err)
		return
	}
	if len(arms) == 0 {
		// No configured table can carry exemplars for this selector —
		// a summary-only deployment, or a schema with every
		// exemplar-carrying table elided. Return an empty data array,
		// which is Prom's answer for an exemplar-free metric type, with
		// no ClickHouse round-trip.
		writeJSON(w, http.StatusOK, Response{
			Status: "success",
			Data:   []ExemplarSeries{},
		})
		return
	}

	sql, args, err := chsql.EmitQueryExemplarsUnion(r.Context(), arms, start, end, h.Schema)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrInternal, err)
		return
	}
	h.Logger.Debug("cerberus query_exemplars", "promql", telemetry.SanitizeForLog(q), "sql", sql, "args", telemetry.SanitizeArgsForLog(args))

	rows, err := h.Client.QueryExemplars(r.Context(), sql, args...)
	if err != nil {
		// Bare err so respondError reclassifies a drain sample-budget overage
		// (chclient.drainBudgetExceeded) to the 422 limit rejection, like the
		// other metadata drains; a generic CH fault falls through to 500.
		h.respondError(w, err)
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
// matcher (if any) — the same heuristic the PromQL lowering applies to
// pick the target metrics tables. Returns "" when the selector pins no
// literal name: a regex matcher (`{__name__=~"http_.*"}`), a negated
// one, or no `__name__` matcher at all (`{job="api"}`). Those selectors
// are answered, not rejected — the candidate set widens to
// [schema.Metrics.ExemplarSources]'s unpinned-name fan-out and the
// matchers themselves do the filtering, exactly as on `/query`.
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

// exemplarArms turns a selector into the per-table arms
// [chsql.EmitQueryExemplarsUnion] reads.
//
// Table resolution is [schema.Metrics.ExemplarSources] — the same
// schema-owned resolver the PromQL sample path consults through
// [schema.Metrics.TablesFor], so a metric name that is ambiguous across
// physical layouts (`<x>_count` can be a histogram companion, a
// hostmetrics cumulative Sum, or a standalone gauge) is read from every
// layout that can hold it instead of one guessed branch.
//
// Each source also carries the MetricName its rows are keyed by, which
// differs from the queried name on the histogram-shaped tables: the
// OTel-CH exporter serves `<base>_count` / `<base>_sum` / `<base>_bucket`
// off the columns of a row written under the bare `<base>` name. That
// arm therefore gets its `__name__` matcher retargeted through
// [promql.RewriteMetricName] — the same rewrite the sample lowering
// applies — or it would filter on a name no row carries and contribute
// nothing.
//
// An empty result means no configured table can carry exemplars for the
// selector; the caller answers with an empty data array.
func exemplarArms(matchers []*labels.Matcher, metricName string, s schema.Metrics) ([]chsql.ExemplarArm, error) {
	sources := s.ExemplarSources(metricName)
	arms := make([]chsql.ExemplarArm, 0, len(sources))
	for _, src := range sources {
		armMatchers := matchers
		if src.MetricName != "" && src.MetricName != metricName {
			armMatchers = promql.RewriteMetricName(matchers, src.MetricName)
		}
		predicate, err := buildExemplarsPredicate(armMatchers, s)
		if err != nil {
			return nil, err
		}
		arms = append(arms, chsql.ExemplarArm{Table: src.Table, Predicate: predicate})
	}
	return arms, nil
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
// metricName is the resolved `__name__` equality-matcher value, and it
// wins over row.MetricName whenever the selector pinned one: the caller
// asked for exactly that series name, while the row can be keyed by the
// bare base name of a classic histogram (the arm that serves
// `<base>_count` reads the `<base>` row) or by the dotted OTel spelling
// of the same name. An unpinned selector has no such name, and
// row.MetricName — which then varies row to row across the fan-out — is
// the only truth available.
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
		name := metricName
		if name == "" {
			name = r.MetricName
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
