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
//   - one row per matching span, TraceID set to that span's trace, for a
//     plain filter, a structural (`>`/`>>`) filter, or a filter followed
//     by `| select(...)` — /api/search groups a trace's matched spans into
//     one TraceSummary but reports the true per-trace matched-span count
//     via SpanSet.Matched (see internal/api/tempo/handler.go's
//     observeSpan), so a multiset of one row per matching span, keyed by
//     TraceID, is the projection that lines up with the wire contract;
//     select() only adds projected columns, never changes which spans
//     survive, so it shares this branch rather than getting its own;
//   - one row per matching trace whose per-trace `count()` /
//     `avg|min|max|sum(duration)` aggregate satisfies the scalar filter,
//     TraceID set to that trace. TraceQL's pipeline aggregates are
//     trace-scoped per the spec — `{ ... } | count() > 0` returns one row
//     per matching trace, not a single corpus-wide aggregate — and
//     /api/search's aggregate-shape summaries carry no SpanSet at all (the
//     Aggregate collapses to one row per trace before the wire shaper ever
//     sees it: internal/api/tempo/handler.go's isSpansetAggregateShape +
//     spansetAggregateSampleProjections branch), so this branch's
//     projection is a plain per-trace SET rather than a multiset;
//   - zero rows when no trace's aggregate satisfies the predicate;
//   - an Err otherwise (parse failure / unsupported shape).
//
// property.CompareTraceIdentityOutcomes is the comparator these two
// distinct row shapes are built for: it multiset-compares rows by TraceID,
// so a selector/select() shape's per-span multiplicity and a pipeline
// shape's per-trace uniqueness both get exactly the identity check their
// own wire projection supports — see runCerberusTraceQL in
// test/property/traceql_test.go for the cerberus-side mirror of both
// shapes off the same TraceSummary array cerberus's /api/search actually
// returns.
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
		// span, TraceID set to that span's trace — CompareTraceIdentityOutcomes
		// multiset-counts rows per TraceID, so a trace contributing N
		// matching spans produces N rows here, matching the cerberus side's
		// per-trace SpanSet.Matched count. Labels stay empty (this family
		// never uses the default label-keyed comparator); Timestamp + Value
		// stay zero so nothing beyond TraceID drives the comparison.
		rows := make([]property.OutcomeRow, 0, len(matched))
		for _, sv := range matched {
			rows = append(rows, property.OutcomeRow{Labels: map[string]string{}, TraceID: sv.traceID})
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
// TracesModel spans.
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
	// scopeResourceValue / scopeSpanValue carry the "environment" attribute
	// at the resource and span scopes respectively — the same key name at
	// two different scopes, with independently-drawn values, so
	// resource.environment and span.environment can be told apart even
	// when both are present on the same span (see gen/traceql.go's
	// TraceQLScopeCollisionAttributeKey doc for the generator side).
	scopeResourceValue string
	scopeSpanValue     string
}

// spanKey identifies a span within a trace — TraceId alone isn't
// unique (a trace has many spans), so structural lookups key on the
// (TraceId, SpanId) pair.
type spanKey struct {
	traceID string
	spanID  string
}

// spanViews pivots the dataset's TracesModel into the spanView shape
// every evaluator helper reads. See gen/traceql.go's TraceQLDataset
// doc and property.SpanRecord's doc for the typed field shape this reads.
func spanViews(d property.Dataset) []spanView {
	if d.Traces == nil {
		return nil
	}
	out := make([]spanView, 0, len(d.Traces.Spans))
	for _, s := range d.Traces.Spans {
		out = append(out, spanView{
			traceID:            s.TraceID,
			spanID:             s.SpanID,
			parentID:           s.ParentSpanID,
			service:            s.ResourceAttributes["service.name"],
			cluster:            s.ResourceAttributes["cluster"],
			httpMethod:         s.SpanAttributes["http.method"],
			name:               s.Name,
			statusCode:         s.StatusCode,
			durationNs:         s.DurationNs,
			scopeResourceValue: s.ResourceAttributes["environment"],
			scopeSpanValue:     s.SpanAttributes["environment"],
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
// outside the generator's pools (TraceQLServicePool / TraceQLClusterPool
// / TraceQLHTTPMethodPool / TraceQLScopeCollisionResourceValuePool /
// TraceQLScopeCollisionSpanValuePool) is a generator bug, not a real
// query, so it fails the parse rather than silently reading a zero
// value. resource.environment and span.environment are two distinct
// cases below, each reading its own spanView field, deliberately never
// collapsed into one shared "environment" getter — that collapse is
// exactly the scope-mixing bug this attribute exists to catch.
func attrGetter(attr string) (func(spanView) string, error) {
	switch attr {
	case "resource.service.name":
		return func(sv spanView) string { return sv.service }, nil
	case "resource.cluster":
		return func(sv spanView) string { return sv.cluster }, nil
	case "span.http.method":
		return func(sv spanView) string { return sv.httpMethod }, nil
	case "resource.environment":
		return func(sv spanView) string { return sv.scopeResourceValue }, nil
	case "span.environment":
		return func(sv spanView) string { return sv.scopeSpanValue }, nil
	}
	return nil, fmt.Errorf("unsupported attribute path %q", attr)
}

// TraceQL's `=~` / `!~` are FULLY ANCHORED, not substring searches:
// `{ resource.service.name =~ "a.*" }` matches the service `api` and
// does NOT match `batch`. Tempo evaluates every regex comparison
// through `pkg/regexp.NewRegexp` (`pkg/traceql/ast_execute.go`), which
// builds `labels.NewFastRegexMatcher`, which compiles
// `"^(?s:" + pattern + ")$"` — so the anchors and the dot-matches-
// newline flag are part of the operator's semantics, not of the
// pattern the user wrote.
//
// The non-capturing group is load-bearing for alternation: `^a|b$`
// alternates between `^a` and `b$`, so only `^(?s:a|b)$` expresses
// "the whole value is a or b". `(?s:` additionally makes `.` match
// `\n`, matching Prometheus's `syntax.DotNL` parse flag — a value
// containing a newline is still matched by `.*`.
//
// A pattern that already carries its own `^`/`$` nests safely:
// `^(?s:^api$)$` still matches only `api`, because `^`/`$` are
// zero-width assertions that compose rather than conflict.
//
// Cerberus wraps the pattern as `^(?:…)$` at the single SQL site both
// regex render paths share — `anchoredRegexPattern` in
// `internal/chsql/builder.go` — and lands on identical semantics
// because ClickHouse compiles `match()` with RE2's dot-matches-newline
// option already on, while leaving `^`/`$` non-multiline. Probed
// directly against chDB: `match('a\nc', '^(?:a.c)$')` = 1 (dot spans
// the newline) and `match('abc\nxyz', '^(?:abc)$')` = 0 (the anchors
// bind the whole value, not a line). Go's regexp defaults the other
// way on the first of those, so the oracle spells the flag out.
const (
	anchorRegexPrefix = "^(?s:"
	anchorRegexSuffix = ")$"
)

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
		re, err := regexp.Compile(anchorRegexPrefix + value + anchorRegexSuffix)
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
// pointer upward, one hop at a time (bySpanID lookups below) — this is
// already a general ancestor traversal, independent of whether the trace
// is a linear parent→child chain or a branching tree with siblings (see
// gen/traceql.go's drawTraceQLBranchingTree): every span has exactly one
// parent regardless of how many siblings or other children exist
// elsewhere in the trace, so "some ancestor matches Left" reduces to one
// walk up THIS span's own chain of parents, never a search over the
// whole tree. `>` additionally never widens beyond an immediate parent
// (see the switch below), so a sibling relationship — two spans sharing
// a parent, neither one the other's ancestor — can never satisfy either
// operator. See TestEvaluate_BranchingTree for the pinned proof of both
// claims. The Left side is restricted to spans reachable from an
// empty-parent root. TraceQL's ingest-time nested-set walk never
// numbers orphan chains, so they cannot establish structural relations.
func evalBase(spans []spanView, b baseQuery) ([]spanView, error) {
	if b.op == "" {
		return filterSpans(spans, b.right), nil
	}

	rooted := rootedSpanKeys(spans)
	leftMatched := filterSpans(spans, b.left)
	leftSet := make(map[spanKey]bool, len(leftMatched))
	for _, sv := range leftMatched {
		key := spanKey{sv.traceID, sv.spanID}
		if rooted[key] {
			leftSet[key] = true
		}
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

// rootedSpanKeys reconstructs the subset TraceQL's ingest-time nested-set
// walk can reach. Only an empty ParentSpanID starts a trace; any other value,
// including the 16-zero spelling, names a parent and leaves the span orphaned
// when no such parent row exists.
func rootedSpanKeys(spans []spanView) map[spanKey]bool {
	children := make(map[spanKey][]spanKey, len(spans))
	rooted := make(map[spanKey]bool, len(spans))
	queue := make([]spanKey, 0, len(spans))
	for _, sv := range spans {
		key := spanKey{sv.traceID, sv.spanID}
		if sv.parentID == "" {
			rooted[key] = true
			queue = append(queue, key)
			continue
		}
		parent := spanKey{sv.traceID, sv.parentID}
		children[parent] = append(children[parent], key)
	}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, child := range children[parent] {
			if rooted[child] {
				continue
			}
			rooted[child] = true
			queue = append(queue, child)
		}
	}
	return rooted
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

// countPipelineOutcome implements `| count() OP N`: one outcome row,
// TraceID set to that trace, per trace whose per-trace count of matched
// spans satisfies the predicate.
func countPipelineOutcome(spans []spanView, pl pipeline) property.Outcome {
	cmp, err := int64Comparator(pl.op)
	if err != nil {
		return property.Outcome{Err: err}
	}
	perTrace := groupByTraceID(spans)
	rows := make([]property.OutcomeRow, 0, len(perTrace))
	for traceID, count := range perTrace {
		if cmp(count, pl.n) {
			rows = append(rows, property.OutcomeRow{Labels: map[string]string{}, TraceID: traceID})
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
// Nms`: one outcome row, TraceID set to that trace, per trace whose
// per-trace duration aggregate satisfies the predicate.
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
	for traceID, durations := range perTrace {
		value, err := aggregateDurations(pl.fn, durations)
		if err != nil {
			return property.Outcome{Err: err}
		}
		if cmp(value, float64(pl.thresholdNs)) {
			rows = append(rows, property.OutcomeRow{Labels: map[string]string{}, TraceID: traceID})
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
// all 20 stable shapes). Anything outside that set returns an error — the
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
		// Shapes 14-16: scope-collision attribute — the same key at the
		// resource and span scopes, and both together.
		`{ resource.environment = "prod" }`,
		`{ span.environment = "canary" }`,
		`{ resource.environment = "prod" && span.environment = "canary" }`,
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
