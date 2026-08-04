package traceql

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/tsouza/cerberus/test/property"
)

// nsPerMillisecond converts a `Nms` query-literal threshold into
// nanoseconds, matching how cerberus's TraceQL lowering scales
// duration literals before comparing against the Duration column.
const nsPerMillisecond = 1_000_000

// Evaluate runs the property-test oracle against d for query q. The
// query string is parsed via the package-private parser (a small
// hand-rolled recognizer keyed to the generator's accept-set — see
// test/property/gen/traceql.go's TraceQLQuery doc for the full shape
// list) — no dependency on Tempo's traceql package, so the oracle
// stays a spec-derived implementation rather than a wrapper.
//
// Returns property.Outcome carrying:
//
//   - one row per matching span (labels: empty map) for a plain
//     filter, a structural (`>`/`>>`) filter, or a filter followed by
//     `| select(...)` — Tempo's /api/search inspected-span count
//     tracks the query's result row count regardless of shape (see
//     internal/api/tempo/search_metrics.go's SearchMetricsFor), and
//     select() only adds projected columns, never changes which spans
//     survive;
//   - one row per matching trace whose per-trace `count()` /
//     `avg|min|max|sum(duration)` aggregate satisfies the scalar
//     filter (labels: empty map). TraceQL's pipeline aggregates are
//     trace-scoped per the spec — `{ ... } | count() > 0` returns one
//     row per matching trace, not a single corpus-wide aggregate;
//   - zero rows when no trace's aggregate satisfies the predicate;
//   - an Err otherwise (parse failure / unsupported shape).
//
// The per-trace row shape mirrors Tempo's /api/search wire shape after
// cerberus PR #536: the inner Aggregate groups by TraceId and emits
// one chclient.Sample per matching trace (see
// internal/api/tempo/handler.go's isSpansetAggregateShape +
// spansetAggregateSampleProjections branch). The framework's
// comparator counts rows-per-empty-label-key, so emitting one
// empty-label row per matching trace lines up with the cerberus side's
// `inspectedTraces == len(res.Samples)`.
func Evaluate(d property.Dataset, q property.Query) property.Outcome {
	parsed, err := parseQuery(q.String)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("oracle: parse %q: %w", q.String, err)}
	}

	matched, err := evalBase(spanViews(d), parsed.base)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("oracle: eval %q: %w", q.String, err)}
	}

	switch parsed.pipeline.kind {
	case pipelineNone, pipelineSelect:
		// Selector-only (or selector + select()): one row per matching
		// span. Labels stay empty so the framework's labelKey() groups
		// all rows under "{}", and the comparator's per-group row-count
		// check is exactly the span-count check we want. Timestamp +
		// Value are stamped at zero so the per-group multiset diff
		// doesn't drift on per-span metadata the comparator isn't
		// designed to inspect (the search response shape collapses span
		// identity into `inspectedSpans`, not per-span timestamps).
		rows := make([]property.OutcomeRow, 0, len(matched))
		for range matched {
			rows = append(rows, property.OutcomeRow{Labels: map[string]string{}})
		}
		return property.Outcome{Rows: rows}
	case pipelineCount:
		return countPipelineOutcome(matched, parsed.pipeline)
	case pipelineMetric:
		return metricPipelineOutcome(matched, parsed.pipeline)
	}
	return property.Outcome{Err: fmt.Errorf("oracle: unhandled pipeline kind %v", parsed.pipeline.kind)}
}

// spanView is the oracle's per-span snapshot: just the fields the
// evaluator needs to apply matchers, intrinsics, structural relations,
// and aggregates. Built once per Evaluate call from the dataset's
// MetricsModel series.
type spanView struct {
	traceID    string
	spanID     string
	parentID   string
	service    string
	cluster    string
	httpMethod string
	name       string
	statusCode string
	durationNs int64
}

// spanKey identifies a span within a trace — TraceId alone isn't
// unique (a trace has many spans), so structural lookups key on the
// (TraceId, SpanId) pair.
type spanKey struct {
	traceID string
	spanID  string
}

// spanViews pivots the dataset's MetricsModel into the spanView shape
// every evaluator helper reads. See gen/traceql.go's TraceQLDataset
// doc for the reserved-label-key convention this mirrors.
func spanViews(d property.Dataset) []spanView {
	if d.Metrics == nil {
		return nil
	}
	out := make([]spanView, 0, len(d.Metrics.Series))
	for _, s := range d.Metrics.Series {
		var durationNs int64
		if raw, ok := s.Labels["__duration_ns__"]; ok {
			if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
				durationNs = v
			}
		}
		out = append(out, spanView{
			traceID:    s.Labels["__traceID__"],
			spanID:     s.Labels["__spanID__"],
			parentID:   s.Labels["__parentSpanID__"],
			service:    s.Labels["resource.service.name"],
			cluster:    s.Labels["resource.cluster"],
			httpMethod: s.Labels["span.http.method"],
			name:       s.MetricName,
			statusCode: s.Labels["__status__"],
			durationNs: durationNs,
		})
	}
	return out
}

// condition is one matcher in a spanset filter — an attribute
// equality/regex test or a duration/status/name intrinsic test.
// Represented as a closure rather than a tagged struct because every
// shape reduces to "does this span satisfy me", and the parser already
// carries the type-specific error handling.
type condition struct {
	match func(sv spanView) bool
}

// matchAll reports whether sv satisfies every condition in conds — the
// `&&` combination the multi-condition filter shape draws.
func matchAll(sv spanView, conds []condition) bool {
	for _, c := range conds {
		if !c.match(sv) {
			return false
		}
	}
	return true
}

// filterSpans returns the spans satisfying every condition in conds.
func filterSpans(spans []spanView, conds []condition) []spanView {
	var out []spanView
	for _, sv := range spans {
		if matchAll(sv, conds) {
			out = append(out, sv)
		}
	}
	return out
}

// attrGetter resolves a `resource.<attr>` / `span.<attr>` path to the
// spanView field it reads. Kept as an explicit allow-list — a path
// outside the generator's pool (TraceQLServicePool /
// TraceQLClusterPool / TraceQLHTTPMethodPool) is a generator bug, not
// a real query, so it fails the parse rather than silently reading a
// zero value.
func attrGetter(attr string) (func(spanView) string, error) {
	switch attr {
	case "resource.service.name":
		return func(sv spanView) string { return sv.service }, nil
	case "resource.cluster":
		return func(sv spanView) string { return sv.cluster }, nil
	case "span.http.method":
		return func(sv spanView) string { return sv.httpMethod }, nil
	}
	return nil, fmt.Errorf("unsupported attribute path %q", attr)
}

// newAttrCondition builds an attribute matcher condition for one of
// the four operators the generator draws: `=`, `!=`, `=~`, `!~`.
func newAttrCondition(attr, op, value string) (condition, error) {
	getter, err := attrGetter(attr)
	if err != nil {
		return condition{}, err
	}
	switch op {
	case "=":
		return condition{match: func(sv spanView) bool { return getter(sv) == value }}, nil
	case "!=":
		return condition{match: func(sv spanView) bool { return getter(sv) != value }}, nil
	case "=~", "!~":
		re, err := regexp.Compile(value)
		if err != nil {
			return condition{}, fmt.Errorf("bad regex %q: %w", value, err)
		}
		negate := op == "!~"
		return condition{match: func(sv spanView) bool { return re.MatchString(getter(sv)) != negate }}, nil
	}
	return condition{}, fmt.Errorf("unsupported attribute operator %q", op)
}

// int64Comparator returns the comparison function for one of the
// generator's five scalar operators, applied to int64 operands
// (duration-in-ns and count()'s integer threshold).
func int64Comparator(op string) (func(a, b int64) bool, error) {
	switch op {
	case ">":
		return func(a, b int64) bool { return a > b }, nil
	case ">=":
		return func(a, b int64) bool { return a >= b }, nil
	case "<":
		return func(a, b int64) bool { return a < b }, nil
	case "<=":
		return func(a, b int64) bool { return a <= b }, nil
	case "=":
		return func(a, b int64) bool { return a == b }, nil
	}
	return nil, fmt.Errorf("unsupported comparison operator %q", op)
}

// float64Comparator is int64Comparator's sibling for the
// avg|min|max|sum(duration) metric pipeline, whose avg branch produces
// a non-integer value.
func float64Comparator(op string) (func(a, b float64) bool, error) {
	switch op {
	case ">":
		return func(a, b float64) bool { return a > b }, nil
	case ">=":
		return func(a, b float64) bool { return a >= b }, nil
	case "<":
		return func(a, b float64) bool { return a < b }, nil
	case "<=":
		return func(a, b float64) bool { return a <= b }, nil
	case "=":
		return func(a, b float64) bool { return a == b }, nil
	}
	return nil, fmt.Errorf("unsupported comparison operator %q", op)
}

// newDurationCondition builds the `duration OP Nms` intrinsic filter.
func newDurationCondition(op string, thresholdNs int64) (condition, error) {
	cmp, err := int64Comparator(op)
	if err != nil {
		return condition{}, err
	}
	return condition{match: func(sv spanView) bool { return cmp(sv.durationNs, thresholdNs) }}, nil
}

// statusCHValue maps the TraceQL query literal (ok/error/unset) to the
// CH-stored StatusCode value (Ok/Error/Unset) — the inverse of
// gen/traceql.go's statusQueryLiteral.
func statusCHValue(literal string) (string, error) {
	switch literal {
	case "ok":
		return "Ok", nil
	case "error":
		return "Error", nil
	case "unset":
		return "Unset", nil
	}
	return "", fmt.Errorf("unsupported status literal %q", literal)
}

// newStatusCondition builds the `status OP <ok|error|unset>` intrinsic
// filter.
func newStatusCondition(op, literal string) (condition, error) {
	chValue, err := statusCHValue(literal)
	if err != nil {
		return condition{}, err
	}
	switch op {
	case "=":
		return condition{match: func(sv spanView) bool { return sv.statusCode == chValue }}, nil
	case "!=":
		return condition{match: func(sv spanView) bool { return sv.statusCode != chValue }}, nil
	}
	return condition{}, fmt.Errorf("unsupported status operator %q", op)
}

// newNameCondition builds the `name = "<v>"` intrinsic filter — exact
// match against the span's full generated name (pool value + ordinal
// suffix, see gen/traceql.go).
func newNameCondition(value string) condition {
	return condition{match: func(sv spanView) bool { return sv.name == value }}
}

// condition-recognizer patterns, one per intrinsic/attribute shape the
// generator draws. Anchored so an unexpected shape fails fast rather
// than silently matching a prefix. Values are always plain pool
// strings (letters, digits, `/`, spaces) — the generator never emits a
// quote or backslash inside a string literal, so a bare `[^"]*` capture
// (no backslash-escape handling) is exact, not an approximation.
var (
	durationCondRe = regexp.MustCompile(`^duration\s*(>=|<=|>|<|=)\s*(\d+)ms$`)
	statusCondRe   = regexp.MustCompile(`^status\s*(=|!=)\s*(ok|error|unset)$`)
	nameCondRe     = regexp.MustCompile(`^name\s*=\s*"([^"]*)"$`)
	attrCondRe     = regexp.MustCompile(`^((?:resource|span)\.[a-zA-Z_.]+)\s*(=~|!~|!=|=)\s*"([^"]*)"$`)
)

// parseCondition recognizes one atomic condition inside a spanset
// filter's `{ ... }` body (the multi-condition shape splits on ` && `
// before calling this).
func parseCondition(s string) (condition, error) {
	s = strings.TrimSpace(s)
	if m := durationCondRe.FindStringSubmatch(s); m != nil {
		ms, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			return condition{}, fmt.Errorf("duration threshold %q: %w", m[2], err)
		}
		return newDurationCondition(m[1], ms*nsPerMillisecond)
	}
	if m := statusCondRe.FindStringSubmatch(s); m != nil {
		return newStatusCondition(m[1], m[2])
	}
	if m := nameCondRe.FindStringSubmatch(s); m != nil {
		return newNameCondition(m[1]), nil
	}
	if m := attrCondRe.FindStringSubmatch(s); m != nil {
		return newAttrCondition(m[1], m[2], m[3])
	}
	return condition{}, fmt.Errorf("condition does not match a known shape %q", s)
}

// parseSpansetFilter parses one `{ cond (&& cond)* }` block into its
// component conditions.
func parseSpansetFilter(s string) ([]condition, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, fmt.Errorf("spanset filter missing braces %q", s)
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return nil, fmt.Errorf("empty spanset filter %q", s)
	}
	parts := strings.Split(inner, " && ")
	conds := make([]condition, 0, len(parts))
	for _, p := range parts {
		c, err := parseCondition(p)
		if err != nil {
			return nil, err
		}
		conds = append(conds, c)
	}
	return conds, nil
}

// baseQuery is the non-pipeline part of a query: either a single
// spanset filter (Op == "", conditions in Right) or a structural
// relation between two spanset filters (Op == ">" or ">>", Left is the
// ancestor/parent-side filter and Right is the descendant/child-side
// filter the result set is drawn from).
type baseQuery struct {
	op    string
	left  []condition
	right []condition
}

// baseRe recognizes the generator's base-query shapes: a lone spanset
// filter, or two filters joined by `>` or `>>`. Conditions never
// contain `{`/`}` (values are plain pool strings), so a non-greedy
// `[^{}]*` capture per spanset block is exact.
var baseRe = regexp.MustCompile(`^(\{[^{}]*\})(?:\s*(>>|>)\s*(\{[^{}]*\}))?$`)

// parseBase parses the base-query portion of a query string (the part
// before any ` | ` pipeline suffix).
func parseBase(s string) (baseQuery, error) {
	m := baseRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return baseQuery{}, fmt.Errorf("base query does not match expected shape %q", s)
	}
	firstConds, err := parseSpansetFilter(m[1])
	if err != nil {
		return baseQuery{}, err
	}
	if m[2] == "" {
		return baseQuery{right: firstConds}, nil
	}
	secondConds, err := parseSpansetFilter(m[3])
	if err != nil {
		return baseQuery{}, err
	}
	return baseQuery{op: m[2], left: firstConds, right: secondConds}, nil
}

// evalBase evaluates a baseQuery against spans, returning the matching
// spans (op == "") or the right-side spans that satisfy the structural
// relation to some left-side match (op == ">" / ">>").
//
// Both structural operators are evaluated by walking a span's parent
// chain — sound because the generator only ever produces linear
// parent→child chains (see gen/traceql.go's TraceQLDataset doc), so
// "some ancestor matches Left" reduces to a chain walk rather than a
// general tree search.
func evalBase(spans []spanView, b baseQuery) ([]spanView, error) {
	if b.op == "" {
		return filterSpans(spans, b.right), nil
	}

	leftMatched := filterSpans(spans, b.left)
	leftSet := make(map[spanKey]bool, len(leftMatched))
	for _, sv := range leftMatched {
		leftSet[spanKey{sv.traceID, sv.spanID}] = true
	}

	bySpanID := make(map[spanKey]spanView, len(spans))
	for _, sv := range spans {
		bySpanID[spanKey{sv.traceID, sv.spanID}] = sv
	}

	rightMatched := filterSpans(spans, b.right)
	var out []spanView
	switch b.op {
	case ">":
		// Immediate child: r's own parent must be a Left match.
		for _, r := range rightMatched {
			if leftSet[spanKey{r.traceID, r.parentID}] {
				out = append(out, r)
			}
		}
	case ">>":
		// Descendant at any depth: walk r's ancestor chain looking for
		// a Left match.
		for _, r := range rightMatched {
			parentKey := spanKey{r.traceID, r.parentID}
			for {
				parent, ok := bySpanID[parentKey]
				if !ok {
					break // reached the chain root; no more ancestors
				}
				if leftSet[parentKey] {
					out = append(out, r)
					break
				}
				parentKey = spanKey{parent.traceID, parent.parentID}
			}
		}
	default:
		return nil, fmt.Errorf("unsupported structural operator %q", b.op)
	}
	return out, nil
}

// pipelineKind discriminates the four pipeline shapes the generator
// draws after a base query.
type pipelineKind int

const (
	pipelineNone pipelineKind = iota
	pipelineCount
	pipelineMetric
	pipelineSelect
)

// pipeline is the parsed `| ...` suffix, if any.
type pipeline struct {
	kind pipelineKind
	// op is the scalar comparison operator for pipelineCount / pipelineMetric.
	op string
	// n is the count() integer threshold (pipelineCount only).
	n int64
	// fn is avg/min/max/sum (pipelineMetric only).
	fn string
	// thresholdNs is the avg|min|max|sum(duration) threshold, already
	// scaled to nanoseconds (pipelineMetric only).
	thresholdNs int64
}

var (
	countPipelineRe  = regexp.MustCompile(`^count\(\)\s*(>=|<=|>|<|=)\s*(\d+)$`)
	metricPipelineRe = regexp.MustCompile(`^(avg|min|max|sum)\(duration\)\s*(>=|<=|>|<|=)\s*(\d+)ms$`)
	selectPipelineRe = regexp.MustCompile(`^select\(.*\)$`)
)

// parsePipeline recognizes the pipeline suffix (everything after the
// first ` | `).
func parsePipeline(s string) (pipeline, error) {
	s = strings.TrimSpace(s)
	if m := countPipelineRe.FindStringSubmatch(s); m != nil {
		n, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			return pipeline{}, fmt.Errorf("count threshold %q: %w", m[2], err)
		}
		return pipeline{kind: pipelineCount, op: m[1], n: n}, nil
	}
	if m := metricPipelineRe.FindStringSubmatch(s); m != nil {
		ms, err := strconv.ParseInt(m[3], 10, 64)
		if err != nil {
			return pipeline{}, fmt.Errorf("metric threshold %q: %w", m[3], err)
		}
		return pipeline{kind: pipelineMetric, fn: m[1], op: m[2], thresholdNs: ms * nsPerMillisecond}, nil
	}
	if selectPipelineRe.MatchString(s) {
		return pipeline{kind: pipelineSelect}, nil
	}
	return pipeline{}, fmt.Errorf("pipeline stage does not match a known shape %q", s)
}

// groupByTraceID buckets spans by TraceID and returns each bucket's
// per-trace count. The generator stamps each chain's spans on a shared
// TraceID (see gen/traceql.go), so a multi-span chain now produces a
// bucket with size > 1 — the shape count() is meant to discriminate on.
func groupByTraceID(spans []spanView) map[string]int64 {
	out := map[string]int64{}
	for _, s := range spans {
		out[s.traceID]++
	}
	return out
}

// countPipelineOutcome implements `| count() OP N`: one outcome row
// per trace whose per-trace count of matched spans satisfies the
// predicate.
func countPipelineOutcome(spans []spanView, pl pipeline) property.Outcome {
	cmp, err := int64Comparator(pl.op)
	if err != nil {
		return property.Outcome{Err: err}
	}
	perTrace := groupByTraceID(spans)
	rows := make([]property.OutcomeRow, 0, len(perTrace))
	for _, count := range perTrace {
		if cmp(count, pl.n) {
			rows = append(rows, property.OutcomeRow{Labels: map[string]string{}})
		}
	}
	return property.Outcome{Rows: rows}
}

// aggregateDurations reduces a trace's matched-span durations (ns)
// with the named function — the same four reducers cerberus's
// avg|min|max|sum(duration) pipeline lowers to (see e.g.
// test/spec/traceql/avg_duration.txtar's `Aggregate ... funcs=[avg(Duration) ...]`).
func aggregateDurations(fn string, durations []int64) (float64, error) {
	if len(durations) == 0 {
		return 0, fmt.Errorf("aggregateDurations: empty durations for fn %q", fn)
	}
	switch fn {
	case "avg":
		var sum int64
		for _, d := range durations {
			sum += d
		}
		return float64(sum) / float64(len(durations)), nil
	case "min":
		m := durations[0]
		for _, d := range durations[1:] {
			if d < m {
				m = d
			}
		}
		return float64(m), nil
	case "max":
		m := durations[0]
		for _, d := range durations[1:] {
			if d > m {
				m = d
			}
		}
		return float64(m), nil
	case "sum":
		var sum int64
		for _, d := range durations {
			sum += d
		}
		return float64(sum), nil
	}
	return 0, fmt.Errorf("unsupported metric function %q", fn)
}

// metricPipelineOutcome implements `| avg|min|max|sum(duration) OP
// Nms`: one outcome row per trace whose per-trace duration aggregate
// satisfies the predicate.
func metricPipelineOutcome(spans []spanView, pl pipeline) property.Outcome {
	perTrace := map[string][]int64{}
	for _, sv := range spans {
		perTrace[sv.traceID] = append(perTrace[sv.traceID], sv.durationNs)
	}
	cmp, err := float64Comparator(pl.op)
	if err != nil {
		return property.Outcome{Err: err}
	}
	rows := make([]property.OutcomeRow, 0, len(perTrace))
	for _, durations := range perTrace {
		value, err := aggregateDurations(pl.fn, durations)
		if err != nil {
			return property.Outcome{Err: err}
		}
		if cmp(value, float64(pl.thresholdNs)) {
			rows = append(rows, property.OutcomeRow{Labels: map[string]string{}})
		}
	}
	return property.Outcome{Rows: rows}
}

// parsedQuery is what parseQuery returns: the base spanset/structural
// query plus an optional pipeline stage.
type parsedQuery struct {
	base     baseQuery
	pipeline pipeline
}

// parseQuery is the hand-rolled recognizer keyed to the generator's
// accept-set (test/property/gen/traceql.go's TraceQLQuery doc lists
// all 14 shapes). Anything outside that set returns an error — the
// generator never emits other shapes so a parse failure is a generator
// bug, not a real divergence. (rapid will still surface it as a
// property-test failure, but the failure log says "oracle: parse"
// which is the right pointer.)
//
// The recognizer is intentionally narrow rather than wrapping
// `internal/traceql/ast`'s parser because the entire purpose of the
// from-scratch oracle is to NOT share code with the side under test.
// When the cerberus pipeline imports the same parser, a parser-side
// bug becomes invisible to a property test that reuses it.
func parseQuery(q string) (parsedQuery, error) {
	baseStr, pipelineStr, hasPipeline := splitPipeline(strings.TrimSpace(q))
	base, err := parseBase(baseStr)
	if err != nil {
		return parsedQuery{}, err
	}
	pl := pipeline{kind: pipelineNone}
	if hasPipeline {
		pl, err = parsePipeline(pipelineStr)
		if err != nil {
			return parsedQuery{}, err
		}
	}
	return parsedQuery{base: base, pipeline: pl}, nil
}

// splitPipeline splits q on its first top-level ` | `. Base-query
// values never contain `|` (plain pool strings), so the first
// occurrence is always the pipeline boundary.
func splitPipeline(q string) (base, pipelineStr string, hasPipeline bool) {
	idx := strings.Index(q, " | ")
	if idx < 0 {
		return q, "", false
	}
	return q[:idx], q[idx+len(" | "):], true
}

// init validates the recognizer once at package load, one query per
// generator shape (test/property/gen/traceql.go's TraceQLQuery doc).
// Pins the supported grammar so a future edit that breaks parsing
// fires at package-init rather than mid-iteration.
func init() {
	for _, q := range []string{
		// Shape 0: bare selector.
		`{ resource.service.name = "api" }`,
		// Shape 1: selector + count() filter, all five operators.
		`{ resource.service.name = "api" } | count() > 0`,
		`{ resource.service.name = "api" } | count() >= 1`,
		`{ resource.service.name = "api" } | count() < 5`,
		`{ resource.service.name = "api" } | count() <= 3`,
		`{ resource.service.name = "api" } | count() = 2`,
		// Shape 2: resource attribute beyond service.name.
		`{ resource.cluster = "east" }`,
		// Shape 3: span attribute matcher.
		`{ span.http.method = "GET" }`,
		// Shape 4: duration intrinsic.
		`{ duration > 100ms }`,
		`{ duration = 50ms }`,
		// Shape 5: status intrinsic, eq and negated.
		`{ status = ok }`,
		`{ status != error }`,
		// Shape 6: name intrinsic.
		`{ name = "GET /api/0" }`,
		// Shape 7: regex attribute matcher.
		`{ resource.service.name =~ "a.*" }`,
		// Shape 8: negated attribute matcher.
		`{ resource.service.name != "api" }`,
		// Shape 9: multi-condition (&&) filter.
		`{ resource.service.name = "api" && status = ok }`,
		// Shape 10 / 11: structural child / descendant.
		`{ resource.service.name = "api" } > { resource.service.name = "web" }`,
		`{ resource.service.name = "api" } >> { resource.service.name = "web" }`,
		// Shape 12: metric beyond count().
		`{ resource.service.name = "api" } | avg(duration) > 50ms`,
		`{ resource.service.name = "api" } | min(duration) <= 10ms`,
		`{ resource.service.name = "api" } | max(duration) >= 300ms`,
		`{ resource.service.name = "api" } | sum(duration) = 120ms`,
		// Shape 13: select() pipeline stage.
		`{ resource.service.name = "api" } | select(span.http.method)`,
	} {
		if _, err := parseQuery(q); err != nil {
			panic(fmt.Sprintf("oracle/traceql: package-init recognizer regression on %q: %v", q, err))
		}
	}
	// Sanity: a query the recognizer must REJECT (unknown attribute path).
	if _, err := parseQuery(`{ span.unknown = "x" }`); err == nil {
		panic("oracle/traceql: recognizer should reject an unlisted attribute path")
	}
}
