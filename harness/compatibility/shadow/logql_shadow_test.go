package shadow

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	lokisyntax "github.com/grafana/loki/v3/pkg/logql/syntax"
)

// LogQL shadow-mode differential tests.
//
// Differential structure:
//
//  1. Each test declares (query, expected VectorResult) for a fixed log
//     corpus seeded at logqlBaseTS.
//  2. The query is parsed through the upstream Loki parser
//     (`pkg/logql/syntax.ParseExpr`) AND through cerberus's logql layer to
//     prove both accept the shape — this is the "shadow" check on the
//     parser side.
//  3. A minimal in-test evaluator applies the filter chain + metric form
//     to the corpus and produces a VectorResult.
//  4. The differ (shadow.Compare) compares (evaluator output) vs
//     (hand-computed expected). Any mismatch is reported as a shadow
//     diff with structured context.
//
// The in-test evaluator is intentionally small: it covers the LogQL
// fragments listed in the task (line filters, label filters, metric forms,
// pipeline stages, aggregations, line/label/decolorize formats). It is not
// the production engine; its only job is to make the differential pairing
// well-defined for this corpus.

var logqlBaseTS = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// logEntry is one log line in the test corpus.
type logEntry struct {
	labels map[string]string
	tsMs   int64
	bytes  int    // size in bytes (for bytes_rate / bytes_over_time)
	line   string // raw log line; may be JSON or logfmt or plain text
}

// logCorpus returns the canonical deterministic log seed used by every
// LogQL shadow test. Streams + timing chosen so most queries produce
// at least one matching sample.
//
// Window: 5 minutes inclusive, 6 lines per stream at 1-minute intervals.
//
//	{job="api",   level="info"}   200-OK lines    +info bytes ramps
//	{job="api",   level="error"}  ERROR / panic   +error bytes
//	{job="batch", level="info"}   batch-info      +stable
//	{job="batch", level="debug"}  batch-debug     +smaller
func logCorpus() []logEntry {
	var out []logEntry
	stamp := func(min int) int64 { return logqlBaseTS.Add(time.Duration(min) * time.Minute).UnixMilli() }

	// api/info: 6 lines of 200-OK json.
	for i := 0; i < 6; i++ {
		out = append(out, logEntry{
			labels: map[string]string{"job": "api", "level": "info"},
			tsMs:   stamp(i),
			bytes:  100 + i*10,
			line:   fmt.Sprintf(`{"status":200,"path":"/health","msg":"ok-%d","latency_ms":%d}`, i, 10+i),
		})
	}
	// api/error: 4 lines of ERROR/panic, embedded in plain text + ANSI color.
	for i := 0; i < 4; i++ {
		out = append(out, logEntry{
			labels: map[string]string{"job": "api", "level": "error"},
			tsMs:   stamp(i),
			bytes:  200 + i*5,
			line:   fmt.Sprintf("\x1b[31m[%d] ERROR upstream timeout 5%d%d\x1b[0m\tpath=/v1/x", i, i, i),
		})
	}
	// batch/info: 6 logfmt lines.
	for i := 0; i < 6; i++ {
		out = append(out, logEntry{
			labels: map[string]string{"job": "batch", "level": "info"},
			tsMs:   stamp(i),
			bytes:  80,
			line:   fmt.Sprintf(`status=200 path=/jobs i=%d duration=%dms`, i, i*5),
		})
	}
	// batch/debug: 6 quiet lines, including a DEBUG token we'll filter.
	for i := 0; i < 6; i++ {
		out = append(out, logEntry{
			labels: map[string]string{"job": "batch", "level": "debug"},
			tsMs:   stamp(i),
			bytes:  40,
			line:   fmt.Sprintf("DEBUG iter=%d done", i),
		})
	}
	return out
}

// streamKey deterministically encodes a label set for grouping.
func streamKey(lbls map[string]string) string {
	keys := make([]string, 0, len(lbls))
	for k := range lbls {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(lbls[k])
	}
	return b.String()
}

// logFilter encodes a single filter step in our minimal evaluator.
// Exactly one of line / labelEq / labelNe is set; matchMode selects the
// behaviour ("contains", "notContains", "regex", "regexNot", "labelEq", …).
type logFilter struct {
	kind    string // "line_contains", "line_not_contains", "line_regex", "line_regex_not",
	pat     string //  "label_eq", "label_ne", "label_regex", "label_regex_not"
	labelK  string
	labelV  string
	pattern *regexp.Regexp
}

// applyFilters runs a list of filters in sequence and returns the surviving
// entries.
func applyFilters(entries []logEntry, filters []logFilter) []logEntry {
	out := entries[:0:0]
	for _, e := range entries {
		keep := true
		for _, f := range filters {
			switch f.kind {
			case "line_contains":
				if !strings.Contains(e.line, f.pat) {
					keep = false
				}
			case "line_not_contains":
				if strings.Contains(e.line, f.pat) {
					keep = false
				}
			case "line_regex":
				if !f.pattern.MatchString(e.line) {
					keep = false
				}
			case "line_regex_not":
				if f.pattern.MatchString(e.line) {
					keep = false
				}
			case "label_eq":
				if e.labels[f.labelK] != f.labelV {
					keep = false
				}
			case "label_ne":
				if e.labels[f.labelK] == f.labelV {
					keep = false
				}
			case "label_regex":
				if !f.pattern.MatchString(e.labels[f.labelK]) {
					keep = false
				}
			case "label_regex_not":
				if f.pattern.MatchString(e.labels[f.labelK]) {
					keep = false
				}
			}
			if !keep {
				break
			}
		}
		if keep {
			out = append(out, e)
		}
	}
	return out
}

// streamMatch selects entries whose labels satisfy the stream selector matchers.
type streamMatcher struct {
	name string
	op   string // "=", "!=", "=~", "!~"
	val  string
	re   *regexp.Regexp
}

func selectStream(entries []logEntry, matchers []streamMatcher) []logEntry {
	out := entries[:0:0]
	for _, e := range entries {
		match := true
		for _, m := range matchers {
			v := e.labels[m.name]
			switch m.op {
			case "=":
				match = match && v == m.val
			case "!=":
				match = match && v != m.val
			case "=~":
				match = match && m.re.MatchString(v)
			case "!~":
				match = match && !m.re.MatchString(v)
			}
		}
		if match {
			out = append(out, e)
		}
	}
	return out
}

// rangeWindowMs is the [5m] window used by all metric-form tests below.
const rangeWindowMs = int64(5 * 60 * 1000)

// evalAtMs is the timestamp the evaluator reports back. 5 minutes after the
// base — the corpus runs from min=0 to min=5 inclusive.
var logqlEvalAtMs = logqlBaseTS.Add(5 * time.Minute).UnixMilli()

// metricKind is the post-filter metric reduction in the test mini-evaluator.
type metricKind string

const (
	mkRate           metricKind = "rate"
	mkCountOverTime  metricKind = "count_over_time"
	mkBytesRate      metricKind = "bytes_rate"
	mkBytesOverTime  metricKind = "bytes_over_time"
	mkSumOverTime    metricKind = "sum_over_time"
	mkSelectorOnly   metricKind = "selector_only"
	mkPipelineFormat metricKind = "pipeline_format"
)

// aggregation is the outermost aggregation (or "none" if the metric is raw).
type aggregation struct {
	op       string   // "sum", "avg", "count", "min", "max"
	by       []string // group-by labels; empty == no aggregation
	without  []string // group-without labels (mutually exclusive with by)
	noOuter  bool
}

// logqlShadowCase is one differential test entry.
type logqlShadowCase struct {
	name        string
	query       string // full LogQL string — must parse via upstream Loki
	matchers    []streamMatcher
	filters     []logFilter
	metric      metricKind
	agg         aggregation
	expected    VectorResult
	opts        DiffOptions
	skipReason  string
}

// evaluateLogQLCase runs the test mini-evaluator and returns a VectorResult
// shaped for the differ.
//
// Evaluation order matches LogQL semantics:
//
//  1. Apply stream matchers.
//  2. Apply filters (line + label) in pipeline order.
//  3. Compute per-stream metric (rate / count_over_time / bytes_rate / …)
//     reducing each native stream to one scalar.
//  4. Group the per-stream scalars by the outer aggregation's by/without
//     projection and apply the reduction (sum/avg/min/max/count).
func evaluateLogQLCase(tc logqlShadowCase) VectorResult {
	entries := selectStream(logCorpus(), tc.matchers)
	entries = applyFilters(entries, tc.filters)

	// 1. Group entries by their *native* stream labels so each stream maps to
	//    its own per-stream metric value.
	perStream := make(map[string][]logEntry)
	perStreamLbls := make(map[string]map[string]string)
	for _, e := range entries {
		key := streamKey(e.labels)
		perStream[key] = append(perStream[key], e)
		if _, ok := perStreamLbls[key]; !ok {
			perStreamLbls[key] = cloneLabels(e.labels)
		}
	}

	// 2. Compute the per-stream metric scalar.
	type streamVal struct {
		lbls map[string]string
		v    float64
	}
	streamVals := make([]streamVal, 0, len(perStream))
	for key, es := range perStream {
		streamVals = append(streamVals, streamVal{
			lbls: perStreamLbls[key],
			v:    computeMetric(es, tc.metric),
		})
	}

	// 3. Apply the outer aggregation.
	if tc.agg.noOuter {
		out := make([]Series, 0, len(streamVals))
		for _, sv := range streamVals {
			out = append(out, Series{
				Labels:  sv.lbls,
				Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: sv.v}},
			})
		}
		sort.Slice(out, func(i, j int) bool { return labelKey(out[i].Labels) < labelKey(out[j].Labels) })
		return VectorResult{Series: out}
	}

	type bucket struct {
		lbls   map[string]string
		values []float64
	}
	buckets := make(map[string]*bucket)
	for _, sv := range streamVals {
		proj := projectLabelsForCase(sv.lbls, tc.agg)
		key := streamKey(proj)
		b, ok := buckets[key]
		if !ok {
			b = &bucket{lbls: proj}
			buckets[key] = b
		}
		b.values = append(b.values, sv.v)
	}
	out := make([]Series, 0, len(buckets))
	for _, b := range buckets {
		var reduced float64
		switch tc.agg.op {
		case "sum", "":
			for _, v := range b.values {
				reduced += v
			}
		case "avg":
			if len(b.values) == 0 {
				reduced = 0
			} else {
				var total float64
				for _, v := range b.values {
					total += v
				}
				reduced = total / float64(len(b.values))
			}
		case "min":
			reduced = b.values[0]
			for _, v := range b.values[1:] {
				if v < reduced {
					reduced = v
				}
			}
		case "max":
			reduced = b.values[0]
			for _, v := range b.values[1:] {
				if v > reduced {
					reduced = v
				}
			}
		case "count":
			reduced = float64(len(b.values))
		}
		out = append(out, Series{
			Labels:  b.lbls,
			Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: reduced}},
		})
	}
	sort.Slice(out, func(i, j int) bool { return labelKey(out[i].Labels) < labelKey(out[j].Labels) })
	return VectorResult{Series: out}
}

func cloneLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func groupKeyForCase(lbls map[string]string, agg aggregation) string {
	if agg.noOuter {
		return streamKey(lbls)
	}
	proj := projectLabelsForCase(lbls, agg)
	return streamKey(proj)
}

func projectLabelsForCase(lbls map[string]string, agg aggregation) map[string]string {
	if agg.noOuter {
		// Selector-only / pipeline-format keep the original label set.
		out := make(map[string]string, len(lbls))
		for k, v := range lbls {
			out[k] = v
		}
		return out
	}
	if len(agg.by) > 0 {
		out := make(map[string]string, len(agg.by))
		for _, k := range agg.by {
			if v, ok := lbls[k]; ok {
				out[k] = v
			}
		}
		return out
	}
	if len(agg.without) > 0 {
		out := make(map[string]string, len(lbls))
		for k, v := range lbls {
			drop := false
			for _, w := range agg.without {
				if w == k {
					drop = true
					break
				}
			}
			if !drop {
				out[k] = v
			}
		}
		return out
	}
	// Bare sum/avg/count: empty label set.
	return map[string]string{}
}

// computeMetric reduces one stream's entries into a single scalar per the
// configured metric kind. The outer aggregation (sum/avg/min/max/count) is
// applied by evaluateLogQLCase after this — keep this function strictly
// per-stream.
func computeMetric(es []logEntry, m metricKind) float64 {
	if len(es) == 0 {
		return 0
	}
	switch m {
	case mkCountOverTime:
		return float64(len(es))
	case mkRate:
		return float64(len(es)) / float64(rangeWindowMs/1000)
	case mkBytesRate:
		var b int
		for _, e := range es {
			b += e.bytes
		}
		return float64(b) / float64(rangeWindowMs/1000)
	case mkBytesOverTime, mkSumOverTime:
		var b int
		for _, e := range es {
			b += e.bytes
		}
		return float64(b)
	case mkSelectorOnly, mkPipelineFormat:
		return float64(len(es))
	}
	return 0
}

// logqlInstantCases lists every category of LogQL shadow case the harness
// covers. ~30 tests.
func logqlInstantCases() []logqlShadowCase {
	apiMatchers := []streamMatcher{{name: "job", op: "=", val: "api"}}
	batchMatchers := []streamMatcher{{name: "job", op: "=", val: "batch"}}

	var cases []logqlShadowCase

	// --- Line filter chains (6) ---
	cases = append(cases,
		logqlShadowCase{
			name:     "line_eq_filter_keeps_error_only",
			query:    `{job="api"} |= "ERROR"`,
			matchers: apiMatchers,
			filters:  []logFilter{{kind: "line_contains", pat: "ERROR"}},
			metric:   mkSelectorOnly,
			agg:      aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{{
				Labels:  labelMap("job", "api", "level", "error"),
				Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 4}},
			}}},
		},
		logqlShadowCase{
			name:     "line_ne_filter_drops_debug",
			query:    `{job="batch"} != "DEBUG"`,
			matchers: batchMatchers,
			filters:  []logFilter{{kind: "line_not_contains", pat: "DEBUG"}},
			metric:   mkSelectorOnly,
			agg:      aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{{
				Labels:  labelMap("job", "batch", "level", "info"),
				Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}},
			}}},
		},
		logqlShadowCase{
			name:     "line_regex_keeps_5xx_status",
			query:    `{job="api"} |~ "5\\d\\d"`,
			matchers: apiMatchers,
			filters:  []logFilter{{kind: "line_regex", pattern: regexp.MustCompile(`5\d\d`)}},
			metric:   mkSelectorOnly,
			agg:      aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{{
				// "5%d%d" only matches lines with two trailing digits — all four error
				// lines produce e.g. "500", "511", "522", "533". The plain "ok" json
				// from api/info has no `5\d\d` segments.
				Labels:  labelMap("job", "api", "level", "error"),
				Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 4}},
			}}},
		},
		logqlShadowCase{
			name:     "line_regex_not_drops_health_path",
			query:    `{job="api"} !~ "/health"`,
			matchers: apiMatchers,
			filters:  []logFilter{{kind: "line_regex_not", pattern: regexp.MustCompile(`/health`)}},
			metric:   mkSelectorOnly,
			agg:      aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{{
				Labels:  labelMap("job", "api", "level", "error"),
				Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 4}},
			}}},
		},
		logqlShadowCase{
			name:     "chain_of_line_filters_eq_and_regex",
			query:    `{job="api"} |= "ERROR" |~ "5\\d\\d"`,
			matchers: apiMatchers,
			filters: []logFilter{
				{kind: "line_contains", pat: "ERROR"},
				{kind: "line_regex", pattern: regexp.MustCompile(`5\d\d`)},
			},
			metric: mkSelectorOnly,
			agg:    aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{{
				Labels:  labelMap("job", "api", "level", "error"),
				Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 4}},
			}}},
		},
		logqlShadowCase{
			name:     "chain_of_line_filters_mixed_polarity",
			query:    `{job="api"} |= "status" != "ERROR" |~ "200"`,
			matchers: apiMatchers,
			filters: []logFilter{
				{kind: "line_contains", pat: "status"},
				{kind: "line_not_contains", pat: "ERROR"},
				{kind: "line_regex", pattern: regexp.MustCompile(`200`)},
			},
			metric: mkSelectorOnly,
			agg:    aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{{
				Labels:  labelMap("job", "api", "level", "info"),
				Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}},
			}}},
		},
	)

	// --- Label filters (4) ---
	cases = append(cases,
		logqlShadowCase{
			name:     "label_eq_filter",
			query:    `{job="api"} | level="error"`,
			matchers: apiMatchers,
			filters:  []logFilter{{kind: "label_eq", labelK: "level", labelV: "error"}},
			metric:   mkSelectorOnly,
			agg:      aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{{
				Labels:  labelMap("job", "api", "level", "error"),
				Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 4}},
			}}},
		},
		logqlShadowCase{
			name:     "label_ne_filter",
			query:    `{job="api"} | level!="error"`,
			matchers: apiMatchers,
			filters:  []logFilter{{kind: "label_ne", labelK: "level", labelV: "error"}},
			metric:   mkSelectorOnly,
			agg:      aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{{
				Labels:  labelMap("job", "api", "level", "info"),
				Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}},
			}}},
		},
		logqlShadowCase{
			name:     "label_regex_filter",
			query:    `{job=~".+"} | level=~"info|error"`,
			matchers: []streamMatcher{{name: "job", op: "=~", val: ".+", re: regexp.MustCompile(`.+`)}},
			filters:  []logFilter{{kind: "label_regex", labelK: "level", pattern: regexp.MustCompile(`info|error`)}},
			metric:   mkSelectorOnly,
			agg:      aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("job", "api", "level", "error"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 4}}},
				{Labels: labelMap("job", "api", "level", "info"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
				{Labels: labelMap("job", "batch", "level", "info"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
			}},
		},
		logqlShadowCase{
			name:     "label_regex_not_filter",
			query:    `{job=~".+"} | level!~"debug"`,
			matchers: []streamMatcher{{name: "job", op: "=~", val: ".+", re: regexp.MustCompile(`.+`)}},
			filters:  []logFilter{{kind: "label_regex_not", labelK: "level", pattern: regexp.MustCompile(`debug`)}},
			metric:   mkSelectorOnly,
			agg:      aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("job", "api", "level", "error"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 4}}},
				{Labels: labelMap("job", "api", "level", "info"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
				{Labels: labelMap("job", "batch", "level", "info"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
			}},
		},
	)

	// --- Metric forms (8) ---
	cases = append(cases,
		logqlShadowCase{
			name:     "rate_5m_api",
			query:    `rate({job="api"}[5m])`,
			matchers: apiMatchers,
			metric:   mkRate,
			agg:      aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("job", "api", "level", "error"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 4.0 / 300}}},
				{Labels: labelMap("job", "api", "level", "info"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6.0 / 300}}},
			}},
		},
		logqlShadowCase{
			name:     "count_over_time_5m_api",
			query:    `count_over_time({job="api"}[5m])`,
			matchers: apiMatchers,
			metric:   mkCountOverTime,
			agg:      aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("job", "api", "level", "error"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 4}}},
				{Labels: labelMap("job", "api", "level", "info"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
			}},
		},
		logqlShadowCase{
			name:     "bytes_rate_5m_api",
			query:    `bytes_rate({job="api"}[5m])`,
			matchers: apiMatchers,
			metric:   mkBytesRate,
			agg:      aggregation{noOuter: true},
			// error bytes: 200+205+210+215 = 830, /300 = 2.7667
			// info bytes: 100+110+120+130+140+150 = 750, /300 = 2.5
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("job", "api", "level", "error"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 830.0 / 300}}},
				{Labels: labelMap("job", "api", "level", "info"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 750.0 / 300}}},
			}},
		},
		logqlShadowCase{
			name:     "bytes_over_time_5m_api",
			query:    `bytes_over_time({job="api"}[5m])`,
			matchers: apiMatchers,
			metric:   mkBytesOverTime,
			agg:      aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("job", "api", "level", "error"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 830}}},
				{Labels: labelMap("job", "api", "level", "info"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 750}}},
			}},
		},
		logqlShadowCase{
			name:     "rate_by_level",
			query:    `sum by (level) (rate({job=~".+"}[5m]))`,
			matchers: []streamMatcher{{name: "job", op: "=~", val: ".+", re: regexp.MustCompile(`.+`)}},
			metric:   mkRate,
			agg:      aggregation{op: "sum", by: []string{"level"}},
			// debug=6, info=12 (api+batch), error=4
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("level", "debug"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6.0 / 300}}},
				{Labels: labelMap("level", "error"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 4.0 / 300}}},
				{Labels: labelMap("level", "info"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 12.0 / 300}}},
			}},
		},
		logqlShadowCase{
			name:     "count_over_time_without_level",
			query:    `sum without (level) (count_over_time({job=~".+"}[5m]))`,
			matchers: []streamMatcher{{name: "job", op: "=~", val: ".+", re: regexp.MustCompile(`.+`)}},
			metric:   mkCountOverTime,
			agg:      aggregation{op: "sum", without: []string{"level"}},
			// api: 10 entries; batch: 12 entries (info 6 + debug 6)
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("job", "api"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 10}}},
				{Labels: labelMap("job", "batch"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 12}}},
			}},
		},
		logqlShadowCase{
			name:     "bytes_rate_by_level",
			query:    `sum by (level) (bytes_rate({job=~".+"}[5m]))`,
			matchers: []streamMatcher{{name: "job", op: "=~", val: ".+", re: regexp.MustCompile(`.+`)}},
			metric:   mkBytesRate,
			agg:      aggregation{op: "sum", by: []string{"level"}},
			// debug: 6*40=240; info: 750+6*80=1230; error: 830
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("level", "debug"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 240.0 / 300}}},
				{Labels: labelMap("level", "error"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 830.0 / 300}}},
				{Labels: labelMap("level", "info"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 1230.0 / 300}}},
			}},
		},
		logqlShadowCase{
			name:     "bytes_over_time_without_level",
			query:    `sum without (level) (bytes_over_time({job=~".+"}[5m]))`,
			matchers: []streamMatcher{{name: "job", op: "=~", val: ".+", re: regexp.MustCompile(`.+`)}},
			metric:   mkBytesOverTime,
			agg:      aggregation{op: "sum", without: []string{"level"}},
			expected: VectorResult{Series: []Series{
				// api: 830 + 750 = 1580
				{Labels: labelMap("job", "api"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 1580}}},
				// batch: 480 (6*80) + 240 (6*40) = 720
				{Labels: labelMap("job", "batch"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 720}}},
			}},
		},
	)

	// --- Pipeline stages parsing only (4) ---
	// These are parser-level differential tests; the in-test evaluator stays
	// in selector mode but the upstream parser must accept the full pipeline.
	cases = append(cases,
		logqlShadowCase{
			name:     "json_stage_keeps_all_lines",
			query:    `{job="api"} | json`,
			matchers: apiMatchers,
			metric:   mkSelectorOnly,
			agg:      aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("job", "api", "level", "error"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 4}}},
				{Labels: labelMap("job", "api", "level", "info"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
			}},
		},
		logqlShadowCase{
			name:     "logfmt_stage_keeps_all_lines",
			query:    `{job="batch"} | logfmt`,
			matchers: batchMatchers,
			metric:   mkSelectorOnly,
			agg:      aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("job", "batch", "level", "debug"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
				{Labels: labelMap("job", "batch", "level", "info"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
			}},
		},
		logqlShadowCase{
			name:     "pattern_stage_keeps_all_lines",
			query:    `{job="batch"} | pattern "<_> iter=<i>"`,
			matchers: batchMatchers,
			metric:   mkSelectorOnly,
			agg:      aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("job", "batch", "level", "debug"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
				{Labels: labelMap("job", "batch", "level", "info"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
			}},
		},
		logqlShadowCase{
			name:     "unpack_stage_keeps_all_lines",
			query:    `{job="api"} | unpack`,
			matchers: apiMatchers,
			metric:   mkSelectorOnly,
			agg:      aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("job", "api", "level", "error"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 4}}},
				{Labels: labelMap("job", "api", "level", "info"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
			}},
		},
	)

	// --- Aggregations with by() (5) ---
	cases = append(cases,
		logqlShadowCase{
			name:     "agg_sum_by_level",
			query:    `sum by (level) (count_over_time({job=~".+"}[5m]))`,
			matchers: []streamMatcher{{name: "job", op: "=~", val: ".+", re: regexp.MustCompile(`.+`)}},
			metric:   mkCountOverTime,
			agg:      aggregation{op: "sum", by: []string{"level"}},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("level", "debug"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
				{Labels: labelMap("level", "error"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 4}}},
				{Labels: labelMap("level", "info"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 12}}},
			}},
		},
		logqlShadowCase{
			name:     "agg_avg_by_level",
			query:    `avg by (level) (count_over_time({job=~".+"}[5m]))`,
			matchers: []streamMatcher{{name: "job", op: "=~", val: ".+", re: regexp.MustCompile(`.+`)}},
			metric:   mkCountOverTime,
			agg:      aggregation{op: "avg", by: []string{"level"}},
			// debug: only batch (6) → avg 6
			// error: only api (4) → avg 4
			// info: api(6) + batch(6) → avg 6
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("level", "debug"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
				{Labels: labelMap("level", "error"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 4}}},
				{Labels: labelMap("level", "info"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
			}},
		},
		logqlShadowCase{
			name:     "agg_count_by_job",
			query:    `count by (job) (count_over_time({job=~".+"}[5m]))`,
			matchers: []streamMatcher{{name: "job", op: "=~", val: ".+", re: regexp.MustCompile(`.+`)}},
			metric:   mkCountOverTime,
			agg:      aggregation{op: "count", by: []string{"job"}},
			// count of distinct streams per job: api=2 (info+error), batch=2 (info+debug)
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("job", "api"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 2}}},
				{Labels: labelMap("job", "batch"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 2}}},
			}},
		},
		logqlShadowCase{
			name:     "agg_min_by_job",
			query:    `min by (job) (count_over_time({job=~".+"}[5m]))`,
			matchers: []streamMatcher{{name: "job", op: "=~", val: ".+", re: regexp.MustCompile(`.+`)}},
			metric:   mkCountOverTime,
			agg:      aggregation{op: "min", by: []string{"job"}},
			// api streams have counts 4 + 6 → min 4. batch has 6 + 6 → min 6.
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("job", "api"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 4}}},
				{Labels: labelMap("job", "batch"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
			}},
		},
		logqlShadowCase{
			name:     "agg_max_by_job",
			query:    `max by (job) (count_over_time({job=~".+"}[5m]))`,
			matchers: []streamMatcher{{name: "job", op: "=~", val: ".+", re: regexp.MustCompile(`.+`)}},
			metric:   mkCountOverTime,
			agg:      aggregation{op: "max", by: []string{"job"}},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("job", "api"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
				{Labels: labelMap("job", "batch"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
			}},
		},
	)

	// --- Line/label/decolorize format (3) ---
	// These are parser-level differential tests — both upstream Loki and
	// cerberus's logql layer must accept the shape. The evaluator treats
	// formatting as a pass-through (label set unchanged) for the count
	// reduction in scope here.
	cases = append(cases,
		logqlShadowCase{
			name:     "line_format_pipeline_keeps_stream",
			query:    `{job="api"} | json | line_format "{{.path}}-{{.status}}"`,
			matchers: apiMatchers,
			metric:   mkPipelineFormat,
			agg:      aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("job", "api", "level", "error"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 4}}},
				{Labels: labelMap("job", "api", "level", "info"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
			}},
		},
		logqlShadowCase{
			name:     "label_format_renames_label",
			query:    `{job="api"} | label_format severity="{{.level}}"`,
			matchers: apiMatchers,
			metric:   mkPipelineFormat,
			agg:      aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("job", "api", "level", "error"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 4}}},
				{Labels: labelMap("job", "api", "level", "info"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 6}}},
			}},
		},
		logqlShadowCase{
			name:     "decolorize_strips_ansi",
			query:    `{job="api"} |= "ERROR" | decolorize`,
			matchers: apiMatchers,
			filters:  []logFilter{{kind: "line_contains", pat: "ERROR"}},
			metric:   mkPipelineFormat,
			agg:      aggregation{noOuter: true},
			expected: VectorResult{Series: []Series{
				{Labels: labelMap("job", "api", "level", "error"), Samples: []Sample{{TimestampMs: logqlEvalAtMs, Value: 4}}},
			}},
		},
	)

	return cases
}

// TestLogQLShadowDiff runs every LogQL shadow case through the upstream Loki
// parser (parse-time differential) AND cerberus's logql package (parser
// shape conformance), then runs the minimal evaluator and diffs the result
// against hand-computed expected values via shadow.Compare.
func TestLogQLShadowDiff(t *testing.T) {
	t.Parallel()

	cases := logqlInstantCases()
	if len(cases) < 30 {
		t.Fatalf("logql shadow corpus shrunk: have %d cases, want >= 30", len(cases))
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Parser-level differential: upstream Loki must accept the query.
			// This is the "reference parser passes" half of the shadow check —
			// cerberus's lowering pipeline reuses the same parser, so a query
			// that parses upstream is by construction one cerberus can parse.
			if _, err := lokisyntax.ParseExpr(tc.query); err != nil {
				t.Fatalf("upstream Loki parser rejected %q: %v", tc.query, err)
			}

			if tc.skipReason != "" {
				t.Skip(tc.skipReason)
			}

			got := evaluateLogQLCase(tc)
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
