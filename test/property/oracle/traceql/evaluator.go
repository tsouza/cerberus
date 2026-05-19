package traceql

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/tsouza/cerberus/test/property"
)

// Evaluate runs the property-test oracle against d for query q. The
// query string is parsed via the package-private parser (a small
// hand-rolled recognizer keyed to the generator's accept-set) — no
// dependency on Tempo's traceql package, so the oracle stays a
// spec-derived implementation rather than a wrapper.
//
// Returns property.Outcome carrying:
//
//   - one row per matching span (labels: empty map) when the query is a
//     bare selector — Tempo's /api/search emits one TraceSummary per
//     matching trace, and the generator stamps each span on its own
//     TraceID so the per-trace and per-span counts coincide;
//   - one row per matching trace whose per-trace count satisfies the
//     `| count() OP N` predicate (labels: empty map). TraceQL's
//     pipeline aggregates are trace-scoped per the spec —
//     `{ ... } | count() > 0` returns one row per matching trace, not
//     a single corpus-wide aggregate;
//   - zero rows when no trace's per-trace count satisfies the
//     predicate;
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

	spans := filterSpans(d, parsed.selector)
	if parsed.hasCount {
		// Trace-scoped count: group surviving spans by TraceID, then
		// emit one outcome row per trace whose per-trace count
		// satisfies `count() OP N`. Mirrors the per-trace aggregate
		// cerberus emits (one chclient.Sample per matching TraceId)
		// — see PR #536. The generator stamps each span on its own
		// TraceID, so per-trace counts are usually 1; the predicate
		// arithmetic still has to hold for the trace to surface.
		perTrace := groupByTraceID(spans)
		rows := make([]property.OutcomeRow, 0, len(perTrace))
		for _, count := range perTrace {
			if compareCount(count, parsed.countOp, parsed.countN) {
				rows = append(rows, property.OutcomeRow{
					Labels:      map[string]string{},
					TimestampMs: 0,
					Value:       0,
				})
			}
		}
		return property.Outcome{Rows: rows}
	}

	// Selector-only: one row per matching span. Labels stay empty so
	// the framework's labelKey() groups all rows under "{}", and the
	// comparator's per-group row-count check is exactly the
	// span-count check we want. Timestamp + Value are stamped at zero
	// so the per-group multiset diff doesn't drift on per-span
	// metadata the comparator isn't designed to inspect (the search
	// response shape collapses span identity into `inspectedTraces`,
	// not per-span timestamps).
	rows := make([]property.OutcomeRow, 0, len(spans))
	for range spans {
		rows = append(rows, property.OutcomeRow{
			Labels:      map[string]string{},
			TimestampMs: 0,
			Value:       0,
		})
	}
	return property.Outcome{Rows: rows}
}

// groupByTraceID buckets surviving spans by TraceID and returns each
// bucket's per-trace count. The generator stamps each span on its own
// TraceID via deterministicTraceID(i, …) so most buckets are size 1,
// but the helper is shape-agnostic — a future generator widening
// (multi-span traces) flows through unchanged.
func groupByTraceID(spans []spanView) map[string]int64 {
	out := map[string]int64{}
	for _, s := range spans {
		out[s.traceID]++
	}
	return out
}

// spanView is the oracle's per-span snapshot: just the fields the
// evaluator needs to apply the selector and aggregate. Built once per
// Evaluate call from the dataset's MetricsModel series.
type spanView struct {
	traceID       string
	service       string
	name          string
	startTimeMs   int64
	durationFloat float64
}

// filterSpans pivots the dataset's MetricsModel into the spanView
// shape and applies the selector predicate. Only one selector kind is
// supported today: `resource.service.name = "<value>"`. The TraceID
// is threaded through so trace-scoped aggregates (`| count()`) can
// bucket by trace identity rather than flattening across the corpus.
func filterSpans(d property.Dataset, sel selector) []spanView {
	if d.Metrics == nil {
		return nil
	}
	out := make([]spanView, 0, len(d.Metrics.Series))
	for _, s := range d.Metrics.Series {
		svc := s.Labels["resource.service.name"]
		if !sel.match(svc) {
			continue
		}
		// SeriesData carries one Point per span (Generator invariant).
		// startTimeMs / duration come from that Point.
		var startMs int64
		var durFloat float64
		if len(s.Points) > 0 {
			startMs = s.Points[0].TimestampMs
			durFloat = s.Points[0].Value
		}
		out = append(out, spanView{
			traceID:       s.Labels["__traceID__"],
			service:       svc,
			name:          s.MetricName,
			startTimeMs:   startMs,
			durationFloat: durFloat,
		})
	}
	return out
}

// parsedQuery is what parseQuery returns. The selector + optional
// scalar-filter components are decoupled so the evaluator can stamp
// them onto separate code paths.
type parsedQuery struct {
	selector selector
	hasCount bool
	countOp  string
	countN   int64
}

// selector is the spanset-filter predicate the oracle understands.
// Only the equality variant is supported today; future generator
// widening (regex, !=, intrinsics) lands new helpers + match() arms.
type selector struct {
	attr  string // always "resource.service.name" today
	value string
}

func (s selector) match(observed string) bool {
	if s.attr == "" {
		return true
	}
	return observed == s.value
}

// queryRe matches the generator's full output shape:
//
//	{ resource.service.name = "<value>" } [| count() <op> <int>]
//
// Anchored so unexpected characters fail fast. The selector group
// captures the quoted service name; the optional pipeline group
// captures the scalar filter operator + threshold.
var queryRe = regexp.MustCompile(
	`^\s*\{\s*resource\.service\.name\s*=\s*"([^"]*)"\s*\}` +
		`(?:\s*\|\s*count\(\)\s*(>=|<=|>|<|=)\s*(\d+))?\s*$`,
)

// parseQuery is the hand-rolled recognizer keyed to the generator's
// accept-set. Anything outside that set returns an error — the
// generator never emits other shapes so a parse failure is a
// generator bug, not a real divergence. (rapid will still surface
// it as a property-test failure, but the failure log says "oracle:
// parse" which is the right pointer.)
//
// The recognizer is intentionally narrow rather than wrapping
// `tempo/pkg/traceql.Parse` because the entire purpose of the
// from-scratch oracle is to NOT share code with the side under test.
// When the cerberus pipeline imports the same parser, a parser-side
// bug becomes invisible to a property test that reuses it.
func parseQuery(q string) (parsedQuery, error) {
	m := queryRe.FindStringSubmatch(q)
	if m == nil {
		return parsedQuery{}, fmt.Errorf("query does not match expected shape %q", q)
	}
	out := parsedQuery{
		selector: selector{
			attr:  "resource.service.name",
			value: m[1],
		},
	}
	if m[2] != "" {
		out.hasCount = true
		out.countOp = m[2]
		n, err := strconv.ParseInt(m[3], 10, 64)
		if err != nil {
			return parsedQuery{}, fmt.Errorf("count threshold %q: %w", m[3], err)
		}
		out.countN = n
	}
	return out, nil
}

// compareCount applies the scalar-filter operator to (count, n).
// Operators are the five the generator emits.
func compareCount(count int64, op string, n int64) bool {
	switch op {
	case ">":
		return count > n
	case ">=":
		return count >= n
	case "<":
		return count < n
	case "<=":
		return count <= n
	case "=":
		return count == n
	}
	// Defensive: the generator only emits the five operators, but
	// strings.Contains-style false positives are harmless — surface
	// the unknown op so the property test fails loudly instead of
	// silently agreeing on the wrong predicate.
	return false
}

// init validates the recognizer once at package load. queryRe must
// not change behaviour from a typo in the literal; the test below
// pins the supported shape so a future edit that breaks parsing
// fires at package-init rather than mid-iteration.
//
// Kept lightweight: the strings exercised here are the same constants
// the generator emits, so a future generator change will surface as a
// failed property test rather than a panic here.
func init() {
	for _, q := range []string{
		`{ resource.service.name = "api" }`,
		`{ resource.service.name = "api" } | count() > 0`,
		`{ resource.service.name = "api" } | count() >= 1`,
		`{ resource.service.name = "api" } | count() < 5`,
		`{ resource.service.name = "api" } | count() <= 3`,
		`{ resource.service.name = "api" } | count() = 2`,
	} {
		if _, err := parseQuery(q); err != nil {
			panic(fmt.Sprintf("oracle/traceql: package-init recognizer regression on %q: %v", q, err))
		}
	}
	// Sanity: a query the recognizer must REJECT.
	if _, err := parseQuery(`{ span.unknown = "x" }`); err == nil {
		panic("oracle/traceql: recognizer should reject span-scope attribute query")
	}
}
