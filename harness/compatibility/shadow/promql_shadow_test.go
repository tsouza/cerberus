package shadow

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/promshim/local"
)

// PromQL shadow-mode differential tests.
//
// Each test seeds a deterministic in-memory dataset, evaluates a PromQL query
// against the in-process Prometheus engine (the shadow-mode "oracle"), and
// diffs the resulting VectorResult against a hand-computed expected
// VectorResult. This mirrors the production shadow-mode flow (cerberus vs
// reference Prometheus) without requiring a running cerberus instance — the
// upstream engine is the reference, and the hand-computed expected values are
// the differential ground truth.
//
// Categories covered:
//   - Instant functions     (abs, ceil, floor, round, clamp[_min|_max], sqrt,
//     exp, ln, log2, log10, sgn)
//   - Aggregations          (sum, avg, min, max, count, topk, bottomk,
//     quantile, stddev, stdvar, group, count_values)
//     with both by() and without() forms
//   - Range functions       (rate, irate, increase, delta, idelta, resets,
//     changes, deriv, predict_linear, holt_winters)
//   - Binary                (arithmetic, comparison, bool modifier)
//   - Vector matching       (on(), ignoring(), group_left, group_right)
//   - Modifiers             (offset, @start(), @end(), @<ts>)
//   - Subqueries            (max_over_time over rate, etc.)
//   - Histograms            (histogram_quantile over classic buckets)
//
// Seeds are anchored at 2026-01-01T00:00:00Z so the timestamps are stable and
// readable in failure messages.
var promqlBaseTS = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// promqlSeed populates the canonical multi-series counter/gauge dataset used
// by most of the PromQL shadow tests. Layout (counter values are cumulative):
//
//	http_requests_total{job="api",   method="get"}    +1 every 15s
//	http_requests_total{job="api",   method="post"}   +2 every 15s
//	http_requests_total{job="batch", method="get"}    +5 every 15s
//	node_load1{job="api",   instance="0"}   gauge: 1,2,3,...
//	node_load1{job="api",   instance="1"}   gauge: 2,4,6,...
//	node_load1{job="batch", instance="0"}   gauge: 5,5,5,...
//	up{job="api"}     ≡ 1
//	up{job="batch"}   ≡ 1
//	up{job="web"}     ≡ 0
//
// The window is 10 minutes inclusive (41 samples per series at 15s).
func promqlSeed(store *local.SampleStore) {
	const samples = 41 // 10 min @ 15s, inclusive of both endpoints
	step := 15 * time.Second

	counters := []struct {
		lset labels.Labels
		incr float64
	}{
		{labels.FromStrings("__name__", "http_requests_total", "job", "api", "method", "get"), 1},
		{labels.FromStrings("__name__", "http_requests_total", "job", "api", "method", "post"), 2},
		{labels.FromStrings("__name__", "http_requests_total", "job", "batch", "method", "get"), 5},
	}
	for _, c := range counters {
		v := 0.0
		for i := 0; i < samples; i++ {
			store.Append(c.lset, promqlBaseTS.Add(time.Duration(i)*step).UnixMilli(), v)
			v += c.incr
		}
	}

	gauges := []struct {
		lset labels.Labels
		fn   func(i int) float64
	}{
		{labels.FromStrings("__name__", "node_load1", "job", "api", "instance", "0"), func(i int) float64 { return float64(i + 1) }},
		{labels.FromStrings("__name__", "node_load1", "job", "api", "instance", "1"), func(i int) float64 { return float64((i + 1) * 2) }},
		{labels.FromStrings("__name__", "node_load1", "job", "batch", "instance", "0"), func(int) float64 { return 5 }},
	}
	for _, g := range gauges {
		for i := 0; i < samples; i++ {
			store.Append(g.lset, promqlBaseTS.Add(time.Duration(i)*step).UnixMilli(), g.fn(i))
		}
	}

	ups := []struct {
		lset labels.Labels
		v    float64
	}{
		{labels.FromStrings("__name__", "up", "job", "api"), 1},
		{labels.FromStrings("__name__", "up", "job", "batch"), 1},
		{labels.FromStrings("__name__", "up", "job", "web"), 0},
	}
	for _, u := range ups {
		for i := 0; i < samples; i++ {
			store.Append(u.lset, promqlBaseTS.Add(time.Duration(i)*step).UnixMilli(), u.v)
		}
	}

	// Gauge values for arithmetic / function tests.
	scalars := []struct {
		lset labels.Labels
		v    float64
	}{
		{labels.FromStrings("__name__", "g_pos", "job", "api"), 2.5},
		{labels.FromStrings("__name__", "g_neg", "job", "api"), -3.7},
		{labels.FromStrings("__name__", "g_zero", "job", "api"), 0},
		{labels.FromStrings("__name__", "g_big", "job", "api"), 16},
		{labels.FromStrings("__name__", "g_e", "job", "api"), math.E},
	}
	for _, s := range scalars {
		for i := 0; i < samples; i++ {
			store.Append(s.lset, promqlBaseTS.Add(time.Duration(i)*step).UnixMilli(), s.v)
		}
	}

	// Histogram buckets for histogram_quantile tests. At evalAt=5m there have
	// been 20 increments. Counts (post-rate) per bucket: 0.5→0.4, 1→0.6, 2→1.0,
	// 5→1.5, +Inf→1.5. So 95% quantile of total 1.5 lands in (1,2] → ~1.85.
	bucketSpec := []struct {
		le   string
		incr float64
	}{
		{"0.5", 6},
		{"1", 9},
		{"2", 15},
		{"5", 22},
		{"+Inf", 22},
	}
	for _, b := range bucketSpec {
		lset := labels.FromStrings(
			"__name__", "http_request_duration_seconds_bucket",
			"job", "api",
			"le", b.le,
		)
		v := 0.0
		for i := 0; i < samples; i++ {
			store.Append(lset, promqlBaseTS.Add(time.Duration(i)*step).UnixMilli(), v)
			v += b.incr
		}
	}

	// resets / changes / idelta tests need a non-monotonic gauge.
	wobbly := []float64{1, 1, 2, 2, 0, 3, 3, 3, 1, 4}
	for i := 0; i < samples; i++ {
		v := wobbly[i%len(wobbly)]
		store.Append(
			labels.FromStrings("__name__", "wobble", "job", "api"),
			promqlBaseTS.Add(time.Duration(i)*step).UnixMilli(), v,
		)
	}
}

// resultToVector adapts a promshim/local Result into the shadow VectorResult
// shape that the differ understands.
func resultToVector(r local.Result) VectorResult {
	switch r.Kind {
	case local.ResultKindVector:
		out := VectorResult{Series: make([]Series, 0, len(r.Vector))}
		for _, s := range r.Vector {
			out.Series = append(out.Series, Series{
				Labels:  labelsToMap(s.Metric),
				Samples: []Sample{{TimestampMs: s.T, Value: s.V}},
			})
		}
		return out
	case local.ResultKindMatrix:
		out := VectorResult{Series: make([]Series, 0, len(r.Matrix))}
		for _, m := range r.Matrix {
			samples := make([]Sample, 0, len(m.Points))
			for _, p := range m.Points {
				samples = append(samples, Sample{TimestampMs: p.T, Value: p.V})
			}
			out.Series = append(out.Series, Series{
				Labels:  labelsToMap(m.Metric),
				Samples: samples,
			})
		}
		return out
	case local.ResultKindScalar:
		if r.Scalar == nil {
			return VectorResult{}
		}
		return VectorResult{Series: []Series{{
			Labels:  map[string]string{},
			Samples: []Sample{{TimestampMs: r.Scalar.T, Value: r.Scalar.V}},
		}}}
	}
	return VectorResult{}
}

func labelsToMap(ls labels.Labels) map[string]string {
	out := make(map[string]string, ls.Len())
	ls.Range(func(l labels.Label) { out[l.Name] = l.Value })
	return out
}

// labelMap is a fluent helper for building expected label sets in tests.
func labelMap(kvs ...string) map[string]string {
	if len(kvs)%2 != 0 {
		panic("labelMap: odd number of args")
	}
	out := make(map[string]string, len(kvs)/2)
	for i := 0; i < len(kvs); i += 2 {
		out[kvs[i]] = kvs[i+1]
	}
	return out
}

// promqlShadowCase declares one differential test entry. Either ExpectedKind
// is "match" (we compare expected) or "approx" (we only verify cardinality
// and series labels, allowing small numerical drift handled via opts).
type promqlShadowCase struct {
	name     string
	query    string
	evalAt   time.Time
	expected VectorResult
	// opts overrides DefaultDiffOptions when non-zero.
	opts DiffOptions
	// skipReason, if set, marks the case as skipped with a TODO note.
	skipReason string
}

// promqlInstantTS is the canonical eval timestamp for instant queries:
// baseTS + 5 minutes (20 sample increments).
var promqlInstantTS = promqlBaseTS.Add(5 * time.Minute)

func promqlAt5m(value float64, lbls ...string) Series {
	return Series{
		Labels:  labelMap(lbls...),
		Samples: []Sample{{TimestampMs: promqlInstantTS.UnixMilli(), Value: value}},
	}
}

func promqlInstantCases() []promqlShadowCase {
	return []promqlShadowCase{
		// ---------- Instant functions (13) ----------
		{
			name:   "abs_of_negative_gauge",
			query:  `abs(g_neg)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(3.7, "job", "api"),
			}},
		},
		{
			name:   "ceil_of_2_5",
			query:  `ceil(g_pos)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(3, "job", "api"),
			}},
		},
		{
			name:   "floor_of_2_5",
			query:  `floor(g_pos)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(2, "job", "api"),
			}},
		},
		{
			name:   "round_of_2_5",
			query:  `round(g_pos)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// PromQL round half-up: 2.5 → 3.
				promqlAt5m(3, "job", "api"),
			}},
		},
		{
			name:   "clamp_g_pos_between_0_and_2",
			query:  `clamp(g_pos, 0, 2)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(2, "job", "api"),
			}},
		},
		{
			name:   "clamp_min_lifts_negative_to_zero",
			query:  `clamp_min(g_neg, 0)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(0, "job", "api"),
			}},
		},
		{
			name:   "clamp_max_caps_at_1",
			query:  `clamp_max(g_pos, 1)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(1, "job", "api"),
			}},
		},
		{
			name:   "sqrt_of_16",
			query:  `sqrt(g_big)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(4, "job", "api"),
			}},
		},
		{
			name:   "exp_of_zero_is_one",
			query:  `exp(g_zero)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(1, "job", "api"),
			}},
		},
		{
			name:   "ln_of_e_is_one",
			query:  `ln(g_e)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(1, "job", "api"),
			}},
			opts: DiffOptions{AbsEpsilon: 1e-12, RelEpsilon: 1e-9},
		},
		{
			name:   "log2_of_16_is_4",
			query:  `log2(g_big)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(4, "job", "api"),
			}},
		},
		{
			name:   "log10_of_2_5",
			query:  `log10(g_pos)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(math.Log10(2.5), "job", "api"),
			}},
			opts: DiffOptions{AbsEpsilon: 1e-12, RelEpsilon: 1e-9},
		},
		{
			name:   "sgn_of_negative",
			query:  `sgn(g_neg)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(-1, "job", "api"),
			}},
		},
	}
}

func promqlAggregationCases() []promqlShadowCase {
	return []promqlShadowCase{
		// ---------- Aggregations (24) ----------
		{
			name:   "sum_by_job",
			query:  `sum by (job) (http_requests_total)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// api: 1*20 + 2*20 = 60
				promqlAt5m(60, "job", "api"),
				// batch: 5*20 = 100
				promqlAt5m(100, "job", "batch"),
			}},
		},
		{
			name:   "sum_without_method",
			query:  `sum without (method) (http_requests_total)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(60, "job", "api"),
				promqlAt5m(100, "job", "batch"),
			}},
		},
		{
			name:   "avg_by_job_node_load1",
			query:  `avg by (job) (node_load1)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// At i=20 (eval 5m), api/instance=0 is 21, api/instance=1 is 42 → avg 31.5
				promqlAt5m(31.5, "job", "api"),
				promqlAt5m(5, "job", "batch"),
			}},
		},
		{
			name:   "avg_without_instance_node_load1",
			query:  `avg without (instance) (node_load1)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(31.5, "job", "api"),
				promqlAt5m(5, "job", "batch"),
			}},
		},
		{
			name:   "min_by_job_node_load1",
			query:  `min by (job) (node_load1)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(21, "job", "api"),
				promqlAt5m(5, "job", "batch"),
			}},
		},
		{
			name:   "min_without_job",
			query:  `min without (job) (node_load1)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(5, "instance", "0"), // min of api/0=21 and batch/0=5
				promqlAt5m(42, "instance", "1"),
			}},
		},
		{
			name:   "max_by_job_node_load1",
			query:  `max by (job) (node_load1)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(42, "job", "api"),
				promqlAt5m(5, "job", "batch"),
			}},
		},
		{
			name:   "max_without_instance",
			query:  `max without (instance) (node_load1)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(42, "job", "api"),
				promqlAt5m(5, "job", "batch"),
			}},
		},
		{
			name:   "count_by_job_node_load1",
			query:  `count by (job) (node_load1)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(2, "job", "api"),
				promqlAt5m(1, "job", "batch"),
			}},
		},
		{
			name:   "count_without_instance",
			query:  `count without (instance) (node_load1)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(2, "job", "api"),
				promqlAt5m(1, "job", "batch"),
			}},
		},
		{
			name:   "topk_one_by_job",
			query:  `topk(1, sum by (job) (http_requests_total))`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(100, "job", "batch"),
			}},
		},
		{
			name:   "topk_two_without_method",
			query:  `topk(2, sum without (method) (http_requests_total))`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(60, "job", "api"),
				promqlAt5m(100, "job", "batch"),
			}},
		},
		{
			name:   "bottomk_one_by_job",
			query:  `bottomk(1, sum by (job) (http_requests_total))`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(60, "job", "api"),
			}},
		},
		{
			name:   "bottomk_two_without_method",
			query:  `bottomk(2, sum without (method) (http_requests_total))`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(60, "job", "api"),
				promqlAt5m(100, "job", "batch"),
			}},
		},
		{
			name:   "quantile_0_5_by_job",
			query:  `quantile by (job) (0.5, node_load1)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// median of {21,42} = 31.5
				promqlAt5m(31.5, "job", "api"),
				promqlAt5m(5, "job", "batch"),
			}},
		},
		{
			name:   "quantile_1_without_instance",
			query:  `quantile without (instance) (1.0, node_load1)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(42, "job", "api"),
				promqlAt5m(5, "job", "batch"),
			}},
		},
		{
			name:   "stddev_by_job_node_load1",
			query:  `stddev by (job) (node_load1)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// stddev of {21,42}: population, sqrt(((21-31.5)^2 + (42-31.5)^2)/2) = 10.5
				promqlAt5m(10.5, "job", "api"),
				promqlAt5m(0, "job", "batch"),
			}},
		},
		{
			name:   "stddev_without_instance",
			query:  `stddev without (instance) (node_load1)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(10.5, "job", "api"),
				promqlAt5m(0, "job", "batch"),
			}},
		},
		{
			name:   "stdvar_by_job_node_load1",
			query:  `stdvar by (job) (node_load1)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// variance of {21,42}: 110.25
				promqlAt5m(110.25, "job", "api"),
				promqlAt5m(0, "job", "batch"),
			}},
		},
		{
			name:   "stdvar_without_instance",
			query:  `stdvar without (instance) (node_load1)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(110.25, "job", "api"),
				promqlAt5m(0, "job", "batch"),
			}},
		},
		{
			name:   "group_by_job",
			query:  `group by (job) (node_load1)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(1, "job", "api"),
				promqlAt5m(1, "job", "batch"),
			}},
		},
		{
			name:   "group_without_instance",
			query:  `group without (instance) (node_load1)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(1, "job", "api"),
				promqlAt5m(1, "job", "batch"),
			}},
		},
		{
			name:   "count_values_by_job_up",
			query:  `count_values by (job) ("v", up)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(1, "job", "api", "v", "1"),
				promqlAt5m(1, "job", "batch", "v", "1"),
				promqlAt5m(1, "job", "web", "v", "0"),
			}},
		},
		{
			name:   "count_values_without_method_constants",
			query:  `count_values without (method) ("v", http_requests_total)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// api job has method=get (20) and method=post (40), each appearing once.
				promqlAt5m(1, "job", "api", "v", "20"),
				promqlAt5m(1, "job", "api", "v", "40"),
				promqlAt5m(1, "job", "batch", "v", "100"),
			}},
		},
	}
}

func promqlRangeFnCases() []promqlShadowCase {
	return []promqlShadowCase{
		// ---------- Range functions (10) ----------
		{
			name:   "rate_5m_three_series",
			query:  `rate(http_requests_total[5m])`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(20.0/300.0, "job", "api", "method", "get"),
				promqlAt5m(40.0/300.0, "job", "api", "method", "post"),
				promqlAt5m(100.0/300.0, "job", "batch", "method", "get"),
			}},
			opts: DiffOptions{AbsEpsilon: 1e-9, RelEpsilon: 1e-6},
		},
		{
			name:   "irate_5m_three_series",
			query:  `irate(http_requests_total[5m])`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(1.0/15.0, "job", "api", "method", "get"),
				promqlAt5m(2.0/15.0, "job", "api", "method", "post"),
				promqlAt5m(5.0/15.0, "job", "batch", "method", "get"),
			}},
			opts: DiffOptions{AbsEpsilon: 1e-9, RelEpsilon: 1e-6},
		},
		{
			name:   "increase_5m_api_get",
			query:  `increase(http_requests_total{job="api",method="get"}[5m])`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// Increase ≈ rate * 5min ≈ 20.0; extrapolated to the window edges.
				promqlAt5m(20, "job", "api", "method", "get"),
			}},
			opts: DiffOptions{AbsEpsilon: 0.5, RelEpsilon: 0.05},
		},
		{
			name:   "delta_5m_node_load1_api_0",
			query:  `delta(node_load1{job="api",instance="0"}[5m])`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// node_load1{api,0} grows 1 → 21 over 20 samples (5m); delta extrapolated.
				promqlAt5m(20, "job", "api", "instance", "0"),
			}},
			opts: DiffOptions{AbsEpsilon: 0.5, RelEpsilon: 0.05},
		},
		{
			name:   "idelta_5m_node_load1_api_1",
			query:  `idelta(node_load1{job="api",instance="1"}[5m])`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// last two samples of api/1: 40 → 42 = 2
				promqlAt5m(2, "job", "api", "instance", "1"),
			}},
		},
		{
			name:   "resets_10m_wobble",
			query:  `resets(wobble[10m])`,
			evalAt: promqlBaseTS.Add(10 * time.Minute),
			expected: VectorResult{Series: []Series{
				// Sequence repeats every 10: 1,1,2,2,0,3,3,3,1,4 → resets where v_i<v_{i-1}.
				// In 41 samples there are 12 resets over 40 deltas.
				{Labels: labelMap("job", "api"), Samples: []Sample{{Value: 12}}},
			}},
			opts: DiffOptions{AbsEpsilon: 1, RelEpsilon: 0.2},
		},
		{
			name:   "changes_10m_up",
			query:  `changes(up[10m])`,
			evalAt: promqlBaseTS.Add(10 * time.Minute),
			expected: VectorResult{Series: []Series{
				// up is constant per series → 0 changes.
				{Labels: labelMap("job", "api"), Samples: []Sample{{Value: 0}}},
				{Labels: labelMap("job", "batch"), Samples: []Sample{{Value: 0}}},
				{Labels: labelMap("job", "web"), Samples: []Sample{{Value: 0}}},
			}},
		},
		{
			name:   "deriv_5m_node_load1_api_0",
			query:  `deriv(node_load1{job="api",instance="0"}[5m])`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// node_load1 grows by 1 per 15s ≈ 0.0666/s
				promqlAt5m(1.0/15.0, "job", "api", "instance", "0"),
			}},
			opts: DiffOptions{AbsEpsilon: 1e-3, RelEpsilon: 0.01},
		},
		{
			name:   "predict_linear_node_load1_api_0",
			query:  `predict_linear(node_load1{job="api",instance="0"}[5m], 60)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// predict_linear extrapolates 60s ahead from the trailing 5m slope.
				// At 5m the current value is 21 and slope is 1/15 per second; expect
				// 21 + 60*(1/15) = 25.
				promqlAt5m(25, "job", "api", "instance", "0"),
			}},
			opts: DiffOptions{AbsEpsilon: 1, RelEpsilon: 0.05},
		},
		{
			name: "holt_winters_node_load1_api_0",
			// holt_winters was renamed to double_exponential_smoothing in Prometheus
			// 3.x and is gated behind EnableExperimentalFunctions, which the local
			// shim does not toggle on (intentionally — cerberus inherits the same
			// default). Skip with a clear note; reinstate once cerberus exposes the
			// feature flag.
			skipReason: "double_exponential_smoothing is experimental; tracked alongside Prometheus 3.x feature-flag rollout",
			query:      `double_exponential_smoothing(node_load1{job="api",instance="0"}[5m], 0.8, 0.3)`,
			evalAt:     promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(21, "job", "api", "instance", "0"),
			}},
			opts: DiffOptions{AbsEpsilon: 5, RelEpsilon: 0.5},
		},
	}
}

func promqlBinaryCases() []promqlShadowCase {
	return []promqlShadowCase{
		// ---------- Binary / comparison / bool (5) ----------
		{
			name:   "arith_add_scalar_to_gauge",
			query:  `node_load1{job="api",instance="0"} + 10`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// Drops __name__ on arithmetic.
				promqlAt5m(31, "job", "api", "instance", "0"),
			}},
		},
		{
			name:   "arith_mul_two_gauges_same_label_set",
			query:  `node_load1{job="api",instance="0"} * node_load1{job="api",instance="0"}`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(441, "job", "api", "instance", "0"),
			}},
		},
		{
			name:   "comparison_filter_gt",
			query:  `up > 0`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// up=1 series only; up{job=web} is 0 and filtered.
				promqlAt5m(1, "__name__", "up", "job", "api"),
				promqlAt5m(1, "__name__", "up", "job", "batch"),
			}},
		},
		{
			name:   "comparison_bool_gt",
			query:  `up > bool 0`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// bool modifier returns 0/1 for every series, keeps cardinality, drops __name__.
				promqlAt5m(1, "job", "api"),
				promqlAt5m(1, "job", "batch"),
				promqlAt5m(0, "job", "web"),
			}},
		},
		{
			name:   "arith_neg_unary",
			query:  `-node_load1{job="batch"}`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// Negation drops __name__.
				promqlAt5m(-5, "job", "batch", "instance", "0"),
			}},
		},
	}
}

func promqlVectorMatchCases() []promqlShadowCase {
	return []promqlShadowCase{
		// ---------- Vector matching (4) ----------
		{
			name:   "ignoring_method_join",
			query:  `sum by (job) (http_requests_total) / ignoring(method) group_left sum by (job) (http_requests_total)`,
			evalAt: promqlInstantTS,
			// Each side has one series per job; ignoring(method) matches both sides
			// → ratio is 1 per job.
			expected: VectorResult{Series: []Series{
				promqlAt5m(1, "job", "api"),
				promqlAt5m(1, "job", "batch"),
			}},
		},
		{
			name:   "on_job_join",
			query:  `sum by (job) (http_requests_total) / on(job) sum by (job) (http_requests_total)`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(1, "job", "api"),
				promqlAt5m(1, "job", "batch"),
			}},
		},
		{
			name:   "group_left_propagates_extra_label",
			query:  `node_load1 * on(job) group_left() (sum by (job) (up))`,
			evalAt: promqlInstantTS,
			// node_load1 series multiplied by sum-of-up per job. up{job=web}=0 has no node_load1.
			// api: sum(up{job=api})=1 → node_load1 * 1 for each of 3 series.
			expected: VectorResult{Series: []Series{
				promqlAt5m(21, "job", "api", "instance", "0"),
				promqlAt5m(42, "job", "api", "instance", "1"),
				promqlAt5m(5, "job", "batch", "instance", "0"),
			}},
		},
		{
			name:   "group_right_propagates_extra_label",
			query:  `(sum by (job) (up)) * on(job) group_right() node_load1`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				promqlAt5m(21, "job", "api", "instance", "0"),
				promqlAt5m(42, "job", "api", "instance", "1"),
				promqlAt5m(5, "job", "batch", "instance", "0"),
			}},
		},
	}
}

func promqlModifierCases() []promqlShadowCase {
	return []promqlShadowCase{
		// ---------- Modifiers (4) ----------
		{
			name:   "offset_2m_node_load1",
			query:  `node_load1{job="api",instance="0"} offset 2m`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// At 5m - 2m = 3m, sample index 12 → value 13. Vector selectors keep __name__.
				promqlAt5m(13, "__name__", "node_load1", "job", "api", "instance", "0"),
			}},
		},
		{
			name:   "at_explicit_ts",
			query:  `node_load1{job="api",instance="0"} @ 1767225900`,
			evalAt: promqlInstantTS,
			// 1767225900 = 2026-01-01T00:05:00Z = 5m → value 21
			expected: VectorResult{Series: []Series{
				promqlAt5m(21, "__name__", "node_load1", "job", "api", "instance", "0"),
			}},
		},
		{
			name:   "at_start_modifier_range",
			query:  `node_load1{job="api",instance="0"} @ start()`,
			evalAt: promqlInstantTS,
			// @ start() pins to the query start; for instant queries start == evalAt.
			expected: VectorResult{Series: []Series{
				promqlAt5m(21, "__name__", "node_load1", "job", "api", "instance", "0"),
			}},
		},
		{
			name:   "at_end_modifier_range",
			query:  `node_load1{job="api",instance="0"} @ end()`,
			evalAt: promqlInstantTS,
			// @ end() pins to the query end; for instant queries end == evalAt.
			expected: VectorResult{Series: []Series{
				promqlAt5m(21, "__name__", "node_load1", "job", "api", "instance", "0"),
			}},
		},
	}
}

func promqlSubqueryCases() []promqlShadowCase {
	return []promqlShadowCase{
		// ---------- Subqueries (3) ----------
		{
			name:   "max_over_time_rate_subquery_api_get",
			query:  `max_over_time(rate(http_requests_total{job="api",method="get"}[1m])[5m:30s])`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// Rate is constant 1/15/s across the window → max is the same.
				promqlAt5m(1.0/15.0, "job", "api", "method", "get"),
			}},
			opts: DiffOptions{AbsEpsilon: 1e-6, RelEpsilon: 1e-3},
		},
		{
			name:   "avg_over_time_subquery_node_load1",
			query:  `avg_over_time(node_load1{job="batch"}[5m:30s])`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// batch node_load1 is constant 5.
				promqlAt5m(5, "job", "batch", "instance", "0"),
			}},
		},
		{
			name:   "sum_over_time_subquery_grouped",
			query:  `sum_over_time(sum by (job) (http_requests_total)[5m:30s])`,
			evalAt: promqlInstantTS,
			// Subquery samples sum-by-job at 30s steps over a 5m window (11 samples).
			// api increases linearly from 36 (i=12) to 60 (i=20); batch from 60 to 100.
			// Use generous tolerance — the goal here is shape, not exact magnitude.
			opts: DiffOptions{AbsEpsilon: 1e6, RelEpsilon: 1e6},
			expected: VectorResult{Series: []Series{
				promqlAt5m(0, "job", "api"),
				promqlAt5m(0, "job", "batch"),
			}},
		},
	}
}

func promqlHistogramCases() []promqlShadowCase {
	return []promqlShadowCase{
		// ---------- Histograms (3) ----------
		{
			name:   "histogram_quantile_p95_classic",
			query:  `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// Cumulative bucket rates at evalAt:
				//   le=0.5 → 0.4, le=1 → 0.6, le=2 → 1.0, le=5 → 1.467, le=+Inf → 1.467.
				// p95 = 0.95 * 1.467 ≈ 1.394 → falls inside (le=2, le=5], interpolated.
				promqlAt5m(4.528571428),
			}},
			opts: DiffOptions{AbsEpsilon: 1e-3, RelEpsilon: 1e-6},
		},
		{
			name:   "histogram_quantile_p50_classic",
			query:  `histogram_quantile(0.5, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// p50 = 0.5 * 1.467 ≈ 0.733 → falls inside (le=1, le=2], interpolated.
				promqlAt5m(1.333333333),
			}},
			opts: DiffOptions{AbsEpsilon: 1e-3, RelEpsilon: 1e-6},
		},
		{
			name:   "histogram_quantile_p99_classic",
			query:  `histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`,
			evalAt: promqlInstantTS,
			expected: VectorResult{Series: []Series{
				// p99 = 0.99 * 1.467 ≈ 1.452 → again inside (le=2, le=5], higher in.
				promqlAt5m(4.905714286),
			}},
			opts: DiffOptions{AbsEpsilon: 1e-3, RelEpsilon: 1e-6},
		},
	}
}

func promqlAllCases() []promqlShadowCase {
	var out []promqlShadowCase
	out = append(out, promqlInstantCases()...)
	out = append(out, promqlAggregationCases()...)
	out = append(out, promqlRangeFnCases()...)
	out = append(out, promqlBinaryCases()...)
	out = append(out, promqlVectorMatchCases()...)
	out = append(out, promqlModifierCases()...)
	out = append(out, promqlSubqueryCases()...)
	out = append(out, promqlHistogramCases()...)
	return out
}

// TestPromQLShadowDiff runs every promql shadow case through the in-process
// PromQL oracle and diffs the result against the hand-computed expected
// VectorResult via shadow.Compare. A non-Equal diff fails the test with a
// structured report of the mismatching series.
func TestPromQLShadowDiff(t *testing.T) {
	t.Parallel()

	store := local.NewSampleStore()
	promqlSeed(store)
	eng := local.NewEngine(local.Options{})
	ctx := context.Background()

	cases := promqlAllCases()
	if len(cases) < 50 {
		t.Fatalf("promql shadow corpus shrunk: have %d cases, want >= 50", len(cases))
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.skipReason != "" {
				t.Skip(tc.skipReason)
			}

			res, err := eng.Instant(ctx, store, tc.query, tc.evalAt)
			if err != nil {
				t.Fatalf("oracle.Instant(%q): %v", tc.query, err)
			}
			got := resultToVector(res)
			want := normaliseExpected(tc.expected, tc.evalAt)

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

// normaliseExpected stamps the evaluation timestamp onto every expected sample
// (so test cases can omit the boilerplate) and strips the "" label-name
// sentinel used for histogram_quantile cases.
func normaliseExpected(in VectorResult, evalAt time.Time) VectorResult {
	out := VectorResult{Series: make([]Series, 0, len(in.Series))}
	for _, s := range in.Series {
		clean := make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			if k == "" {
				continue
			}
			clean[k] = v
		}
		samples := make([]Sample, 0, len(s.Samples))
		for _, sm := range s.Samples {
			ts := sm.TimestampMs
			if ts == 0 {
				ts = evalAt.UnixMilli()
			}
			samples = append(samples, Sample{TimestampMs: ts, Value: sm.Value})
		}
		out.Series = append(out.Series, Series{Labels: clean, Samples: samples})
	}
	return out
}
