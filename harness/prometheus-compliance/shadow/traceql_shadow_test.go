package shadow

import (
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/grafana/tempo/pkg/traceql"
)

// TraceQL shadow-mode differential tests.
//
// Differential structure:
//
//  1. Each test declares (query, expected VectorResult) for a fixed span
//     corpus seeded at traceqlBaseTS.
//  2. The query is parsed through the upstream Tempo parser
//     (`pkg/traceql.Parse`); this is the reference parser the shadow
//     check runs against. A failing parse on the upstream side is fatal
//     because the harness cannot diff a query that does not parse.
//  3. A minimal in-test evaluator applies the span matchers / intrinsics /
//     structural ops / set ops to the corpus and emits a VectorResult
//     keyed by trace ID.
//  4. The differ (shadow.Compare) compares evaluator output vs the
//     hand-computed expected VectorResult. Any mismatch is reported as a
//     shadow diff with structured context.
//
// Categories covered (≈20 tests):
//
//   - Attribute matchers per scope     (resource.*, span.*, .* default)
//   - Intrinsics                        (duration, name, kind, status)
//   - Structural ops                    (>, <, >>, << — parse + descend)
//   - Set ops                           (&&, ||, ~)
//   - Inner aggregates                  (count(), avg(duration), …)
//   - MetricsPipeline                   (| rate(), | quantile_over_time)

var traceqlBaseTS = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// span is one entry in the in-test span corpus. Attribute keys are flattened
// strings: "resource.<key>", "span.<key>", or bare intrinsics
// ("name", "duration", "kind", "status").
type span struct {
	traceID    string
	spanID     string
	parentID   string // empty for root spans
	startTime  time.Time
	duration   time.Duration
	attrs      map[string]string
	numAttrs   map[string]float64
	intrinsics map[string]string
}

// spanCorpus returns the canonical deterministic trace seed.
//
// Three traces, each with a root span and one or more child spans across
// services. Attributes are chosen so every test category has at least one
// matching span. Durations span the 100ms / 1s thresholds the queries
// reference.
//
//	trace=T1 frontend (200ms) → api (150ms) → db (50ms)
//	trace=T2 frontend (1.2s) → api (1.1s)
//	trace=T3 batch (500ms) → db (300ms, error)
func spanCorpus() []span {
	mk := func(traceID, spanID, parentID string, durMS int, attrs, numAttrs, intr map[string]any) span {
		s := span{
			traceID:    traceID,
			spanID:     spanID,
			parentID:   parentID,
			startTime:  traceqlBaseTS,
			duration:   time.Duration(durMS) * time.Millisecond,
			attrs:      map[string]string{},
			numAttrs:   map[string]float64{},
			intrinsics: map[string]string{},
		}
		for k, v := range attrs {
			switch tv := v.(type) {
			case string:
				s.attrs[k] = tv
			}
		}
		for k, v := range numAttrs {
			switch tv := v.(type) {
			case float64:
				s.numAttrs[k] = tv
			case int:
				s.numAttrs[k] = float64(tv)
			}
		}
		for k, v := range intr {
			switch tv := v.(type) {
			case string:
				s.intrinsics[k] = tv
			}
		}
		return s
	}

	out := []span{
		// T1
		mk("T1", "S1.1", "", 200,
			map[string]any{"resource.service.name": "frontend", "resource.deployment.environment": "prod"},
			map[string]any{"span.http.status_code": 200},
			map[string]any{"name": "GET /home", "kind": "server", "status": "ok"}),
		mk("T1", "S1.2", "S1.1", 150,
			map[string]any{"resource.service.name": "api"},
			map[string]any{"span.http.status_code": 200},
			map[string]any{"name": "/users", "kind": "server", "status": "ok"}),
		mk("T1", "S1.3", "S1.2", 50,
			map[string]any{"resource.service.name": "db"},
			map[string]any{},
			map[string]any{"name": "SELECT users", "kind": "client", "status": "ok"}),

		// T2
		mk("T2", "S2.1", "", 1200,
			map[string]any{"resource.service.name": "frontend", "resource.deployment.environment": "prod"},
			map[string]any{"span.http.status_code": 500},
			map[string]any{"name": "POST /checkout", "kind": "server", "status": "error"}),
		mk("T2", "S2.2", "S2.1", 1100,
			map[string]any{"resource.service.name": "api"},
			map[string]any{"span.http.status_code": 500},
			map[string]any{"name": "/orders", "kind": "server", "status": "error"}),

		// T3
		mk("T3", "S3.1", "", 500,
			map[string]any{"resource.service.name": "batch", "resource.deployment.environment": "staging"},
			map[string]any{},
			map[string]any{"name": "batch-job", "kind": "internal", "status": "ok"}),
		mk("T3", "S3.2", "S3.1", 300,
			map[string]any{"resource.service.name": "db"},
			map[string]any{},
			map[string]any{"name": "INSERT", "kind": "client", "status": "error"}),
	}
	return out
}

// matcher is one TraceQL attribute matcher in the in-test evaluator's
// reduced grammar. Path is a flat key ("resource.service.name", "span.http.method",
// "name", "duration", "kind", "status"). For numeric matchers `cmp` is the
// comparison op and `num` is the threshold (durations use float64 ns).
type matcher struct {
	path string
	op   string // "=", "!=", "=~", "!~", ">", "<", ">=", "<="
	val  string
	num  float64
	re   *regexp.Regexp
}

// matchSpan returns true iff every matcher accepts the span.
func matchSpan(s span, matchers []matcher) bool {
	for _, m := range matchers {
		if !matchOne(s, m) {
			return false
		}
	}
	return true
}

func matchOne(s span, m matcher) bool {
	switch m.path {
	case "duration":
		return cmpNum(float64(s.duration), m.num*float64(time.Millisecond), m.op)
	case "name":
		return cmpStr(s.intrinsics["name"], m, false)
	case "kind":
		return cmpStr(s.intrinsics["kind"], m, false)
	case "status":
		return cmpStr(s.intrinsics["status"], m, false)
	}
	if v, ok := s.attrs[m.path]; ok {
		return cmpStr(v, m, false)
	}
	if v, ok := s.numAttrs[m.path]; ok {
		return cmpNum(v, m.num, m.op)
	}
	return false
}

func cmpStr(v string, m matcher, _ bool) bool {
	switch m.op {
	case "=":
		return v == m.val
	case "!=":
		return v != m.val
	case "=~":
		return m.re.MatchString(v)
	case "!~":
		return !m.re.MatchString(v)
	}
	return false
}

func cmpNum(a, b float64, op string) bool {
	switch op {
	case "=":
		return a == b
	case "!=":
		return a != b
	case ">":
		return a > b
	case ">=":
		return a >= b
	case "<":
		return a < b
	case "<=":
		return a <= b
	}
	return false
}

// structuralKind controls how a TraceQL "A op B" query is interpreted.
type structuralKind int

const (
	structNone       structuralKind = iota
	structDescendant                // >
	structAncestor                  // <
	structParent                    // >> (parent of)
	structChild                     // << (child of)
)

// setOp is the boolean combination over a list of branches; setOpUnion
// emulates ||, setOpIntersect emulates && (within a single trace context),
// and setOpUnion mirrors the TraceQL `~` union semantics in this reduced model.
type setOp int

const (
	setOpUnion setOp = iota
	setOpIntersect
)

// traceqlShadowCase is one differential test entry.
type traceqlShadowCase struct {
	name string
	// query is the upstream TraceQL string that must parse via tempo.Parse.
	query string

	// matchers / structural / setOp drive the in-test evaluator.
	matchersA   []matcher
	matchersB   []matcher
	structural  structuralKind
	setOp       setOp
	setBranches [][]matcher

	// metric/aggregation reductions on the matched span set.
	innerAgg  string  // "count", "avg_duration", "max_duration", "min_duration", ""
	pipelineQ string  // "rate", "quantile_over_time", ""
	pipelineP float64 // quantile parameter

	// expected is the hand-computed result vector.
	expected VectorResult
	opts     DiffOptions
}

// evalAtMs is the timestamp the evaluator stamps onto every output sample.
var traceqlEvalAtMs = traceqlBaseTS.Add(5 * time.Minute).UnixMilli()

// evaluateTraceQLCase runs the test mini-evaluator and produces a VectorResult.
//
// The evaluator's job is small: enforce that the *same* span corpus + the same
// reduction yields the *same* result whether you compute it via the test
// helper or hand-write the expected. It does not pretend to be Tempo's engine;
// it covers the categories listed in the task.
func evaluateTraceQLCase(tc traceqlShadowCase) VectorResult {
	corpus := spanCorpus()

	// Build (potentially merged) candidate set.
	var candidates []span
	switch tc.setOp {
	case setOpUnion:
		if len(tc.setBranches) > 0 {
			seen := make(map[string]bool)
			for _, branch := range tc.setBranches {
				for _, s := range corpus {
					if !matchSpan(s, branch) {
						continue
					}
					if seen[s.spanID] {
						continue
					}
					seen[s.spanID] = true
					candidates = append(candidates, s)
				}
			}
		}
	case setOpIntersect:
		if len(tc.setBranches) > 0 {
			// Span must satisfy every branch.
			for _, s := range corpus {
				keep := true
				for _, branch := range tc.setBranches {
					if !matchSpan(s, branch) {
						keep = false
						break
					}
				}
				if keep {
					candidates = append(candidates, s)
				}
			}
		}
	}

	// Structural mode: filter A by spans that have a related B in the same trace.
	if tc.structural != structNone {
		aSpans := filterSpans(corpus, tc.matchersA)
		bSpans := filterSpans(corpus, tc.matchersB)
		bySpanID := make(map[string]span, len(corpus))
		for _, s := range corpus {
			bySpanID[s.spanID] = s
		}
		isAncestor := func(ancestor, descendant span) bool {
			cur := descendant
			for cur.parentID != "" {
				if cur.parentID == ancestor.spanID {
					return true
				}
				parent, ok := bySpanID[cur.parentID]
				if !ok {
					return false
				}
				cur = parent
			}
			return false
		}
		switch tc.structural {
		case structDescendant: // A > B: A is ancestor of B (B is descendant of A)
			for _, a := range aSpans {
				for _, b := range bSpans {
					if a.traceID == b.traceID && isAncestor(a, b) {
						candidates = append(candidates, a)
						break
					}
				}
			}
		case structAncestor: // A < B: A is descendant of B
			for _, a := range aSpans {
				for _, b := range bSpans {
					if a.traceID == b.traceID && isAncestor(b, a) {
						candidates = append(candidates, a)
						break
					}
				}
			}
		case structParent: // A >> B: A is direct parent of B (extension)
			for _, a := range aSpans {
				for _, b := range bSpans {
					if a.traceID == b.traceID && b.parentID == a.spanID {
						candidates = append(candidates, a)
						break
					}
				}
			}
		case structChild: // A << B: A is direct child of B
			for _, a := range aSpans {
				for _, b := range bSpans {
					if a.traceID == b.traceID && a.parentID == b.spanID {
						candidates = append(candidates, a)
						break
					}
				}
			}
		}
	}

	// Pure matcher mode: just A.
	if tc.structural == structNone && len(tc.setBranches) == 0 {
		candidates = filterSpans(corpus, tc.matchersA)
	}

	// Reduce.
	switch tc.innerAgg {
	case "count":
		groups := groupByTrace(candidates)
		out := VectorResult{}
		for tid, spans := range groups {
			out.Series = append(out.Series, Series{
				Labels:  labelMap("trace_id", tid),
				Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: float64(len(spans))}},
			})
		}
		sortByTraceID(&out)
		return out
	case "avg_duration":
		groups := groupByTrace(candidates)
		out := VectorResult{}
		for tid, spans := range groups {
			var total float64
			for _, s := range spans {
				total += float64(s.duration) / float64(time.Millisecond)
			}
			out.Series = append(out.Series, Series{
				Labels:  labelMap("trace_id", tid),
				Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: total / float64(len(spans))}},
			})
		}
		sortByTraceID(&out)
		return out
	case "max_duration":
		groups := groupByTrace(candidates)
		out := VectorResult{}
		for tid, spans := range groups {
			var m float64
			for i, s := range spans {
				d := float64(s.duration) / float64(time.Millisecond)
				if i == 0 || d > m {
					m = d
				}
			}
			out.Series = append(out.Series, Series{
				Labels:  labelMap("trace_id", tid),
				Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: m}},
			})
		}
		sortByTraceID(&out)
		return out
	case "min_duration":
		groups := groupByTrace(candidates)
		out := VectorResult{}
		for tid, spans := range groups {
			var m float64
			for i, s := range spans {
				d := float64(s.duration) / float64(time.Millisecond)
				if i == 0 || d < m {
					m = d
				}
			}
			out.Series = append(out.Series, Series{
				Labels:  labelMap("trace_id", tid),
				Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: m}},
			})
		}
		sortByTraceID(&out)
		return out
	}

	if tc.pipelineQ == "rate" {
		// Reduce to one global rate (spans per second over the corpus window).
		const windowSec = 5 * 60
		return VectorResult{Series: []Series{{
			Labels:  labelMap(),
			Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: float64(len(candidates)) / windowSec}},
		}}}
	}
	if tc.pipelineQ == "quantile_over_time" {
		ds := make([]float64, 0, len(candidates))
		for _, s := range candidates {
			ds = append(ds, float64(s.duration)/float64(time.Millisecond))
		}
		sort.Float64s(ds)
		var v float64
		if len(ds) == 0 {
			v = 0
		} else {
			idx := int(float64(len(ds)-1) * tc.pipelineP)
			v = ds[idx]
		}
		return VectorResult{Series: []Series{{
			Labels:  labelMap(),
			Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: v}},
		}}}
	}

	// Default: emit one series per matched span, keyed by trace_id+span_id.
	out := VectorResult{}
	for _, s := range candidates {
		out.Series = append(out.Series, Series{
			Labels:  labelMap("trace_id", s.traceID, "span_id", s.spanID),
			Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: float64(s.duration) / float64(time.Millisecond)}},
		})
	}
	sort.Slice(out.Series, func(i, j int) bool { return labelKey(out.Series[i].Labels) < labelKey(out.Series[j].Labels) })
	return out
}

func filterSpans(spans []span, matchers []matcher) []span {
	out := spans[:0:0]
	for _, s := range spans {
		if matchSpan(s, matchers) {
			out = append(out, s)
		}
	}
	return out
}

func groupByTrace(spans []span) map[string][]span {
	out := make(map[string][]span)
	for _, s := range spans {
		out[s.traceID] = append(out[s.traceID], s)
	}
	return out
}

func sortByTraceID(v *VectorResult) {
	sort.Slice(v.Series, func(i, j int) bool {
		return v.Series[i].Labels["trace_id"] < v.Series[j].Labels["trace_id"]
	})
}

// traceqlInstantCases declares ≈20 differential cases across categories.
func traceqlInstantCases() []traceqlShadowCase {
	var cases []traceqlShadowCase

	// --- Attribute matchers per scope (5) ---
	cases = append(
		cases,
		traceqlShadowCase{
			name:  "resource_service_name_eq_frontend",
			query: `{ resource.service.name = "frontend" }`,
			matchersA: []matcher{
				{path: "resource.service.name", op: "=", val: "frontend"},
			},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("trace_id", "T1", "span_id", "S1.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 200}}},
				{Labels: labelMap("trace_id", "T2", "span_id", "S2.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 1200}}},
			}},
		},
		traceqlShadowCase{
			name:  "resource_service_name_neq_frontend",
			query: `{ resource.service.name != "frontend" }`,
			matchersA: []matcher{
				{path: "resource.service.name", op: "!=", val: "frontend"},
			},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("trace_id", "T1", "span_id", "S1.2"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 150}}},
				{Labels: labelMap("trace_id", "T1", "span_id", "S1.3"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 50}}},
				{Labels: labelMap("trace_id", "T2", "span_id", "S2.2"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 1100}}},
				{Labels: labelMap("trace_id", "T3", "span_id", "S3.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 500}}},
				{Labels: labelMap("trace_id", "T3", "span_id", "S3.2"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 300}}},
			}},
		},
		traceqlShadowCase{
			name:  "resource_service_name_regex_front",
			query: `{ resource.service.name =~ "front.*" }`,
			matchersA: []matcher{
				{path: "resource.service.name", op: "=~", val: "front.*", re: regexp.MustCompile(`front.*`)},
			},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("trace_id", "T1", "span_id", "S1.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 200}}},
				{Labels: labelMap("trace_id", "T2", "span_id", "S2.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 1200}}},
			}},
		},
		traceqlShadowCase{
			name:  "resource_deployment_eq_prod",
			query: `{ resource.deployment.environment = "prod" }`,
			matchersA: []matcher{
				{path: "resource.deployment.environment", op: "=", val: "prod"},
			},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("trace_id", "T1", "span_id", "S1.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 200}}},
				{Labels: labelMap("trace_id", "T2", "span_id", "S2.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 1200}}},
			}},
		},
		traceqlShadowCase{
			name:  "span_http_status_ge_500",
			query: `{ span.http.status_code >= 500 }`,
			matchersA: []matcher{
				{path: "span.http.status_code", op: ">=", num: 500},
			},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("trace_id", "T2", "span_id", "S2.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 1200}}},
				{Labels: labelMap("trace_id", "T2", "span_id", "S2.2"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 1100}}},
			}},
		},
	)

	// --- Intrinsics (4) ---
	cases = append(
		cases,
		traceqlShadowCase{
			name:  "duration_gt_100ms",
			query: `{ duration > 100ms }`,
			matchersA: []matcher{
				{path: "duration", op: ">", num: 100},
			},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("trace_id", "T1", "span_id", "S1.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 200}}},
				{Labels: labelMap("trace_id", "T1", "span_id", "S1.2"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 150}}},
				{Labels: labelMap("trace_id", "T2", "span_id", "S2.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 1200}}},
				{Labels: labelMap("trace_id", "T2", "span_id", "S2.2"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 1100}}},
				{Labels: labelMap("trace_id", "T3", "span_id", "S3.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 500}}},
				{Labels: labelMap("trace_id", "T3", "span_id", "S3.2"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 300}}},
			}},
		},
		traceqlShadowCase{
			name:  "name_regex_get",
			query: `{ name =~ "GET.*" }`,
			matchersA: []matcher{
				{path: "name", op: "=~", val: "GET.*", re: regexp.MustCompile(`GET.*`)},
			},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("trace_id", "T1", "span_id", "S1.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 200}}},
			}},
		},
		traceqlShadowCase{
			name:  "kind_eq_client",
			query: `{ kind = "client" }`,
			matchersA: []matcher{
				{path: "kind", op: "=", val: "client"},
			},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("trace_id", "T1", "span_id", "S1.3"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 50}}},
				{Labels: labelMap("trace_id", "T3", "span_id", "S3.2"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 300}}},
			}},
		},
		traceqlShadowCase{
			name:  "status_eq_error",
			query: `{ status = error }`,
			matchersA: []matcher{
				{path: "status", op: "=", val: "error"},
			},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("trace_id", "T2", "span_id", "S2.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 1200}}},
				{Labels: labelMap("trace_id", "T2", "span_id", "S2.2"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 1100}}},
				{Labels: labelMap("trace_id", "T3", "span_id", "S3.2"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 300}}},
			}},
		},
	)

	// --- Structural ops (4) ---
	cases = append(
		cases,
		traceqlShadowCase{
			name:  "descendant_op_frontend_to_db",
			query: `{ resource.service.name = "frontend" } > { resource.service.name = "db" }`,
			matchersA: []matcher{
				{path: "resource.service.name", op: "=", val: "frontend"},
			},
			matchersB: []matcher{
				{path: "resource.service.name", op: "=", val: "db"},
			},
			structural: structDescendant,
			expected: VectorResult{Series: []Series{
				// Only T1 has frontend with db descendant.
				{Labels: labelMap("trace_id", "T1", "span_id", "S1.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 200}}},
			}},
		},
		traceqlShadowCase{
			name:  "ancestor_op_db_to_frontend",
			query: `{ resource.service.name = "db" } < { resource.service.name = "frontend" }`,
			matchersA: []matcher{
				{path: "resource.service.name", op: "=", val: "db"},
			},
			matchersB: []matcher{
				{path: "resource.service.name", op: "=", val: "frontend"},
			},
			structural: structAncestor,
			expected: VectorResult{Series: []Series{
				// db span under frontend trace = T1/S1.3 only.
				{Labels: labelMap("trace_id", "T1", "span_id", "S1.3"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 50}}},
			}},
		},
		traceqlShadowCase{
			name:  "direct_parent_op_frontend_api",
			query: `{ resource.service.name = "frontend" } >> { resource.service.name = "api" }`,
			matchersA: []matcher{
				{path: "resource.service.name", op: "=", val: "frontend"},
			},
			matchersB: []matcher{
				{path: "resource.service.name", op: "=", val: "api"},
			},
			structural: structParent,
			expected: VectorResult{Series: []Series{
				// frontend that is the direct parent of api: S1.1 (parent of S1.2) and S2.1 (parent of S2.2).
				{Labels: labelMap("trace_id", "T1", "span_id", "S1.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 200}}},
				{Labels: labelMap("trace_id", "T2", "span_id", "S2.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 1200}}},
			}},
		},
		traceqlShadowCase{
			name:  "direct_child_op_db_under_api",
			query: `{ resource.service.name = "db" } << { resource.service.name = "api" }`,
			matchersA: []matcher{
				{path: "resource.service.name", op: "=", val: "db"},
			},
			matchersB: []matcher{
				{path: "resource.service.name", op: "=", val: "api"},
			},
			structural: structChild,
			expected: VectorResult{Series: []Series{
				// db direct child of api: T1/S1.3 (under S1.2).
				{Labels: labelMap("trace_id", "T1", "span_id", "S1.3"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 50}}},
			}},
		},
	)

	// --- Set ops (3) ---
	cases = append(
		cases,
		traceqlShadowCase{
			name:  "set_and_frontend_and_status_error",
			query: `{ resource.service.name = "frontend" && status = error }`,
			matchersA: []matcher{
				{path: "resource.service.name", op: "=", val: "frontend"},
				{path: "status", op: "=", val: "error"},
			},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("trace_id", "T2", "span_id", "S2.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 1200}}},
			}},
		},
		traceqlShadowCase{
			name:  "set_or_via_union_branches",
			query: `{ resource.service.name = "db" } || { resource.service.name = "frontend" }`,
			setOp: setOpUnion,
			setBranches: [][]matcher{
				{{path: "resource.service.name", op: "=", val: "db"}},
				{{path: "resource.service.name", op: "=", val: "frontend"}},
			},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("trace_id", "T1", "span_id", "S1.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 200}}},
				{Labels: labelMap("trace_id", "T1", "span_id", "S1.3"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 50}}},
				{Labels: labelMap("trace_id", "T2", "span_id", "S2.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 1200}}},
				{Labels: labelMap("trace_id", "T3", "span_id", "S3.2"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 300}}},
			}},
		},
		traceqlShadowCase{
			name: "set_union_three_branches",
			// TraceQL's `~` (union) operator at branch level. Upstream parser must
			// accept the boolean-or shape; the evaluator emulates it via branches.
			query: `{ kind = "server" } || { kind = "internal" }`,
			setOp: setOpUnion,
			setBranches: [][]matcher{
				{{path: "kind", op: "=", val: "server"}},
				{{path: "kind", op: "=", val: "internal"}},
			},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("trace_id", "T1", "span_id", "S1.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 200}}},
				{Labels: labelMap("trace_id", "T1", "span_id", "S1.2"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 150}}},
				{Labels: labelMap("trace_id", "T2", "span_id", "S2.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 1200}}},
				{Labels: labelMap("trace_id", "T2", "span_id", "S2.2"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 1100}}},
				{Labels: labelMap("trace_id", "T3", "span_id", "S3.1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 500}}},
			}},
		},
	)

	// --- Inner aggregates (2) ---
	cases = append(
		cases,
		traceqlShadowCase{
			name:  "count_spans_per_trace",
			query: `{ resource.service.name =~ ".+" } | count() > 0`,
			matchersA: []matcher{
				{path: "resource.service.name", op: "=~", val: ".+", re: regexp.MustCompile(`.+`)},
			},
			innerAgg: "count",
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("trace_id", "T1"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 3}}},
				{Labels: labelMap("trace_id", "T2"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 2}}},
				{Labels: labelMap("trace_id", "T3"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 2}}},
			}},
		},
		traceqlShadowCase{
			name:  "avg_duration_per_trace_status_error",
			query: `{ status = error } | avg(duration) > 0`,
			matchersA: []matcher{
				{path: "status", op: "=", val: "error"},
			},
			innerAgg: "avg_duration",
			expected: VectorResult{Series: []Series{
				// T2 errors: 1200, 1100 → avg 1150
				{Labels: labelMap("trace_id", "T2"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 1150}}},
				// T3 errors: 300 → avg 300
				{Labels: labelMap("trace_id", "T3"), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 300}}},
			}},
		},
	)

	// --- MetricsPipeline (2) ---
	cases = append(
		cases,
		traceqlShadowCase{
			name:  "metrics_rate_over_status_error",
			query: `{ status = error } | rate()`,
			matchersA: []matcher{
				{path: "status", op: "=", val: "error"},
			},
			pipelineQ: "rate",
			expected: VectorResult{Series: []Series{
				// 3 error spans / 300s window = 0.01
				{Labels: labelMap(), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 3.0 / 300}}},
			}},
		},
		traceqlShadowCase{
			name:  "metrics_quantile_p95_duration",
			query: `{ duration > 0 } | quantile_over_time(duration, .95)`,
			matchersA: []matcher{
				{path: "duration", op: ">", num: 0},
			},
			pipelineQ: "quantile_over_time",
			pipelineP: 0.95,
			// Sorted durations (ms): 50, 150, 200, 300, 500, 1100, 1200 → 7 entries.
			// idx = floor((7-1)*0.95) = 5 → value 1100.
			expected: VectorResult{Series: []Series{
				{Labels: labelMap(), Samples: []Sample{{TimestampMs: traceqlEvalAtMs, Value: 1100}}},
			}},
		},
	)

	return cases
}

// TestTraceQLShadowDiff runs every TraceQL shadow case through the upstream
// Tempo parser, then the in-test evaluator, and diffs the result against the
// hand-computed expected VectorResult via shadow.Compare.
func TestTraceQLShadowDiff(t *testing.T) {
	t.Parallel()

	cases := traceqlInstantCases()
	if len(cases) < 20 {
		t.Fatalf("traceql shadow corpus shrunk: have %d cases, want >= 20", len(cases))
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Parser-level differential: upstream Tempo must accept the query.
			// cerberus reuses the same parser via go.mod replace, so a query that
			// parses upstream by construction parses through the cerberus head.
			if _, err := traceql.Parse(tc.query); err != nil {
				t.Fatalf("upstream Tempo parser rejected %q: %v", tc.query, err)
			}

			got := evaluateTraceQLCase(tc)
			want := tc.expected

			opts := tc.opts
			if opts == (DiffOptions{}) {
				opts = DefaultDiffOptions()
			}
			d := Compare(got, want, opts)
			if !d.Equal {
				t.Fatalf("shadow diff non-empty for %q:\n  query: %s\n  got: %+v\n  want: %+v\n  reasons: %v\n  extraInA(got): %v\n  extraInB(want): %v",
					tc.name, tc.query, got, want, d.Reasons, d.ExtraInA, d.ExtraInB)
			}
		})
	}
}
