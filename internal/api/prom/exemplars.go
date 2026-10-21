package prom

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"sort"
	"strconv"
	"strings"
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
// unix seconds). The `query` is any PromQL expression: as upstream does,
// every vector selector in it is extracted ([promparser.ExtractSelectors])
// and the response carries the exemplars of every series any of those
// selectors match, each series once. An expression with no vector
// selector (`vector(1)`, a scalar) selects no series and answers a
// success envelope with no `data` key, upstream's exact body. Every
// matcher shape a selector can carry is answered — a regex `__name__`,
// or no `__name__` at all — because the table fan-out is resolved from
// the matcher set rather than from a literal metric name. The response
// is the canonical Prom envelope with `data` shaped as []ExemplarSeries.
//
// Implementation flow:
//
//  1. Validate the query / start / end parameters (existing behaviour).
//  2. Parse the PromQL (a parse error is ErrBadData carrying the parser's
//     message) and extract its distinct vector selectors via
//     [exemplarSelectors].
//  3. Resolve, per selector, the candidate tables and their per-table row
//     keys via [exemplarArms], which routes through
//     [schema.Metrics.ExemplarSources]. An empty candidate set (a
//     summary-only deployment — the OTel-CH summary table has no
//     Exemplars column upstream) short-circuits with `data:[]`:
//     exemplars are a histogram concept and clients should not see an
//     error for a legitimately exemplar-free metric type.
//  4. Build each arm's matcher predicate via the same
//     [promql.BuildMatcherPredicate] helper PromQL `handleQuery` /
//     `handleQueryRange` use.
//  5. Call [chsql.EmitQueryExemplarsUnion] to render the SQL + args —
//     one arm per (selector, candidate table), unioned into one statement.
//  6. Run the SQL via Querier.QueryExemplars; decode each row positionally
//     into a [chclient.ExemplarRow].
//  7. Group rows by `(MetricName, Attributes, ServiceName)` into one
//     ExemplarSeries each — dropping the duplicate an exemplar gets when
//     two selectors' arms read the same row — then project per-exemplar
//     Labels with the reserved-key merge: ExemplarAttributes carries the
//     SDK-recorded FilteredAttributes, and `trace_id` / `span_id` from
//     the dedicated columns are overlaid (the columns are authoritative;
//     empty values are dropped). See plan §3 + §7 "Reserved-key
//     precedence".
//
// An expression with at least one selector that matches nothing returns
// `data:[]` (not nil) so the JSON envelope renders `"data":[]` rather
// than `"data":null`; Grafana's exemplars probe distinguishes the two.
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

	selectors := exemplarSelectors(expr)
	if len(selectors) == 0 {
		// No vector selector selects no series. Upstream answers with a
		// nil data payload, which its envelope omits: `{"status":"success"}`.
		writeJSON(w, http.StatusOK, Response{Status: "success"})
		return
	}

	var arms []chsql.ExemplarArm
	for _, matchers := range selectors {
		selectorArms, err := exemplarArms(matchers, exemplarMetricName(matchers), h.Schema)
		if err != nil {
			writeError(w, http.StatusInternalServerError, ErrInternal, err)
			return
		}
		arms = append(arms, selectorArms...)
	}
	if len(arms) == 0 {
		// No configured table can carry exemplars for any selector —
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

	sql, args, physicalScans, err := chsql.EmitQueryExemplarsUnion(chsql.WithSchemaStrategies(r.Context(), h.metricsStrategies(r.Context())), arms, start, end, h.Schema)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrInternal, err)
		return
	}
	h.Logger.Debug("cerberus query_exemplars", "promql", telemetry.SanitizeForLog(q), "sql", sql, "args", telemetry.SanitizeArgsForLog(args))

	// Engine bypass: the union renders one arm per exemplar source table,
	// and each arm is its own Distributed fan-out on a multi-data-shard
	// deployment, so the gate weight is stamped here from the emitted count.
	ctx, cancel := h.metadataContext(r)
	defer cancel()
	ctx = chclient.WithDataShardFanoutMultiplier(ctx, physicalScans)
	rows, err := h.Client.QueryExemplars(ctx, sql, args...)
	if err != nil {
		// Bare err so respondError reclassifies a drain sample-budget overage
		// (chclient.drainBudgetExceeded) to the 422 limit rejection, like the
		// other metadata drains; a generic CH fault falls through to 500.
		h.respondError(r.Context(), w, err)
		return
	}

	series := groupExemplars(rows)
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   series,
	})
}

// exemplarSelectors returns the matcher set of every vector selector in
// expr, in [promparser.ExtractSelectors] order — upstream's own
// extraction for this endpoint — with repeats dropped. A selector named
// twice (`x / x offset 1h`; offset and `@` modifiers do not change the
// series a selector matches) selects the same series both times, so its
// second copy would only add a duplicate scan per candidate table.
func exemplarSelectors(expr promparser.Expr) [][]*labels.Matcher {
	all := promparser.ExtractSelectors(expr)
	out := make([][]*labels.Matcher, 0, len(all))
	seen := make(map[string]struct{}, len(all))
	for _, matchers := range all {
		key := matcherSetKey(matchers)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, matchers)
	}
	return out
}

// matcherSetKey renders a matcher set as an order-independent identity
// key: the sorted String() forms, which quote each value, joined by a
// separator no quoted matcher can contain unescaped.
func matcherSetKey(matchers []*labels.Matcher) string {
	parts := make([]string, len(matchers))
	for i, m := range matchers {
		parts[i] = m.String()
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// singleVectorSelector returns the unique [promparser.VectorSelector] in
// expr or an error if the expression is anything else. ParenExpr wrappers
// are unwrapped recursively. The `match[]`-shaped helpers use it to read
// the one selector a matcher string carries.
func singleVectorSelector(expr promparser.Expr) (*promparser.VectorSelector, error) {
	for {
		switch e := expr.(type) {
		case *promparser.VectorSelector:
			return e, nil
		case *promparser.ParenExpr:
			expr = e.Expr
		default:
			return nil, fmt.Errorf("expected a single vector selector, got %T", expr)
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
// Every arm reports its rows under metricName, the selector's pinned
// name ([chsql.ExemplarArm.SeriesName]): the caller asked for exactly
// that series, while the row can be keyed by the bare base name of a
// classic histogram (the arm that serves `<base>_count` reads the `<base>`
// row) or by the dotted OTel spelling of the same name. An unpinned
// selector has no such name, and the row's own MetricName is reported.
//
// An empty result means no configured table can carry exemplars for the
// selector.
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
		arms = append(arms, chsql.ExemplarArm{Table: src.Table, Predicate: predicate, SeriesName: metricName})
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
// row.MetricName is already the reported series name — each arm projects
// its selector's pinned name there ([exemplarArms]).
//
// A series two selectors both match is read once per selector, so the
// same exemplar arrives on two rows; an exemplar identical to one the
// series already holds is dropped, leaving each series with each
// exemplar once — what upstream's exemplar storage returns, as it keeps
// a series when it matches some matcher set and never stores a
// duplicate exemplar.
//
// Per-exemplar Labels: ExemplarAttributes (the SDK-recorded
// FilteredAttributes map) is the base, then `trace_id` / `span_id`
// from the dedicated row columns overlay. Empty TraceID / SpanID
// columns are dropped to match Prom's behaviour for exemplars without
// trace linkage.
func groupExemplars(rows []chclient.ExemplarRow) []ExemplarSeries {
	if len(rows) == 0 {
		return []ExemplarSeries{}
	}

	type bucket struct {
		labels    map[string]string
		exemplars []Exemplar
		seen      map[string]struct{}
	}
	bySeries := map[string]*bucket{}
	keys := make([]string, 0, len(rows))

	for _, r := range rows {
		seriesLabels := format.WithMetricName(r.Attributes, r.MetricName)
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
			b = &bucket{labels: seriesLabels, seen: map[string]struct{}{}}
			bySeries[key] = b
			keys = append(keys, key)
		}
		ex := projectExemplar(r)
		exKey := exemplarKey(r, ex)
		if _, dup := b.seen[exKey]; dup {
			continue
		}
		b.seen[exKey] = struct{}{}
		b.exemplars = append(b.exemplars, ex)
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

// exemplarKey is an exemplar's identity within its series: the row's
// nanosecond timestamp (the wire float would merge exemplars a few
// hundred nanoseconds apart), its value, and the projected label set
// (trace_id / span_id included).
func exemplarKey(r chclient.ExemplarRow, ex Exemplar) string {
	return r.Timestamp.UTC().Format(time.RFC3339Nano) + " " +
		strconv.FormatFloat(r.Value, 'g', -1, 64) + " " +
		format.CanonicalKey(ex.Labels)
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
	maps.Copy(out, r.ExemplarAttributes)
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
