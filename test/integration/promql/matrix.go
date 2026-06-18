//go:build chdb

package promql

// exoticCase is one row of the exotic catalogue: a human-readable name, the
// PromQL string, and the instant the query evaluates at (unix seconds). A
// zero EvalTs means "use the suite default" (EvalTs near the end of the
// data window).
type exoticCase struct {
	name   string
	promql string
	evalTs int64
}

// ts returns c.evalTs or the suite default when unset.
func (c exoticCase) ts() int64 {
	if c.evalTs == 0 {
		return EvalTs
	}
	return c.evalTs
}

// ExoticMatrix is the hand-curated catalogue. Every entry is bound to a
// construct the from-scratch oracle (test/property/oracle/promql) already
// evaluates, so the assertion is a real cerberus-vs-spec comparison rather
// than a both-erroring no-op. Categories the oracle can't yet evaluate
// (set ops, subqueries, label_replace/label_join, absent/clamp/sort,
// quantile/count_values/limitk, predict_linear/deriv,
// double_exponential_smoothing, native histograms) are DEFERRED — they are
// covered either by the deterministic promqltest tail (set ops, @
// start/end, stale/NaN) or left as documented oracle gaps. See the package
// doc and the PR body for the full deferred list.
//
// CAT 1 is the priority class: vector-vector binary ops over
// rate/irate/increase/delta. It is the explicit regression net for the
// production code-47 break ("Unknown expression or function identifier in
// scope WITH") that the fix/vector-join-rate-metricname change resolves —
// the vector join used to drop the operand MetricName/TimeUnix on
// rate()+rate(), producing un-runnable SQL.
var ExoticMatrix = func() []exoticCase {
	var m []exoticCase
	m = append(m, cat1BinaryOverRate()...)
	m = append(m, cat2Histogram()...)
	m = append(m, cat3Aggregators()...)
	m = append(m, cat4SetOps()...)
	m = append(m, cat5Matching()...)
	m = append(m, cat7AtOffset()...)
	m = append(m, cat8ScalarVector()...)
	m = append(m, cat9OverTime()...)
	m = append(m, cat10ScalarOps()...)
	return m
}()

// cat1BinaryOverRate is the priority class: binary operators applied to two
// rate/increase/delta/irate range-function results. The vector join must
// preserve the operand identity (labels minus __name__) so the operands
// match on their remaining labels; series present on only one side are
// silently dropped (not an error); arithmetic strips __name__.
func cat1BinaryOverRate() []exoticCase {
	const cpu = "demo_cpu_usage_seconds_total"
	const items = "demo_items_shipped_total"
	return []exoticCase{
		{name: "cat1/rate_plus_rate", promql: "rate(" + cpu + "[5m]) + rate(" + cpu + "[5m])"},
		{name: "cat1/rate_div_rate", promql: "rate(" + cpu + "[5m]) / rate(" + cpu + "[5m])"},
		{name: "cat1/increase_div_increase", promql: "increase(" + cpu + "[5m]) / increase(" + cpu + "[5m])"},
		{name: "cat1/rate_gt_rate", promql: "rate(" + cpu + "[5m]) > rate(" + cpu + "[5m])"},
		{name: "cat1/rate_gt_bool_rate", promql: "rate(" + cpu + "[5m]) > bool rate(" + cpu + "[5m])"},
		{name: "cat1/rate_minus_rate", promql: "rate(" + cpu + "[5m]) - rate(" + cpu + "[5m])"},
		{name: "cat1/delta_minus_delta", promql: "delta(demo_memory_usage_bytes[5m]) - delta(demo_memory_usage_bytes[5m])"},
		{name: "cat1/rate_mod_rate", promql: "rate(" + cpu + "[5m]) % rate(" + cpu + "[5m])"},
		// on(instance, job) keeps the FULL items label set (items carries
		// only instance+job), so the join neither reduces nor adds labels
		// — the operand identity survives the vector join (the exact
		// property the code-47 fix restores).
		{name: "cat1/rate_on_instance_job_rate", promql: "rate(" + items + "[5m]) + on(instance, job) rate(" + items + "[5m])"},
		// Disjoint operands: a metric that exists vs one that doesn't ->
		// empty result, NOT an error.
		{name: "cat1/rate_plus_empty", promql: "rate(" + cpu + "[5m]) + rate(does_not_exist[5m])"},
		// Cross-metric rate ratio matched on the shared instance+job key.
		// items has exactly {instance,job}; the inner sum by(instance,job)
		// over cpu reduces to the same key, so on(instance,job) keeps the
		// full surviving label set and both sides agree.
		{name: "cat1/items_over_cpu_on_instance", promql: "rate(" + items + "[5m]) / on(instance, job) sum by(instance, job)(rate(" + cpu + "[5m]))"},
	}
}

// cat2Histogram exercises histogram_quantile over the classic-histogram
// fan-out. The oracle implements histogram_quantile + phi out-of-range
// (-Inf / +Inf) but not the histogram_* value family, so those are
// deferred.
func cat2Histogram() []exoticCase {
	const h = "demo_api_request_duration_seconds_bucket"
	return []exoticCase{
		{name: "cat2/hq_p90", promql: "histogram_quantile(0.9, sum by(le)(rate(" + h + "[5m])))"},
		{name: "cat2/hq_p95", promql: "histogram_quantile(0.95, sum by(le)(rate(" + h + "[5m])))"},
		{name: "cat2/hq_by_path", promql: "histogram_quantile(0.9, sum by(le, path)(rate(" + h + "[5m])))"},
		// phi out of domain (Prometheus quantile.go:114-119): phi < 0 ->
		// -Inf, phi > 1 -> +Inf. Pinned by Wave-0 fix (b); phi == 0 / 1
		// stay in domain and fall through to the normal bucket search.
		{name: "cat2/hq_phi_neg", promql: "histogram_quantile(-0.1, sum by(le)(rate(" + h + "[5m])))"},
		{name: "cat2/hq_phi_over1", promql: "histogram_quantile(1.01, sum by(le)(rate(" + h + "[5m])))"},
		// DEFERRED (cerberus-vs-oracle interpolation/edge divergences,
		// outside the code-47 scope — see PR body):
		//   - hq p50: cerberus's sub-bucket interpolation lands at a
		//     different point than the oracle's bucketQuantile for the p50
		//     rank (p90/p95 agree). Separate later PR (fix (a)).
		//   - instant buckets (no rate): cerberus drops the {} series the
		//     oracle keeps over raw cumulative bucket counts.
	}
}

// cat3Aggregators covers the aggregators the oracle implements:
// sum/avg/min/max/count/topk/bottomk, with by/without grouping and edge k.
// quantile/count_values/limitk/limit_ratio are DEFERRED (oracle gap).
func cat3Aggregators() []exoticCase {
	const mem = "demo_memory_usage_bytes"
	const cpu = "demo_cpu_usage_seconds_total"
	return []exoticCase{
		{name: "cat3/topk_3", promql: "topk(3, " + mem + ")"},
		{name: "cat3/topk_huge", promql: "topk(9999999999, " + mem + ")"},
		{name: "cat3/topk_0", promql: "topk(0, " + mem + ")"},
		{name: "cat3/topk_by_instance", promql: "topk(1, " + mem + ") by (instance)"},
		{name: "cat3/bottomk_2", promql: "bottomk(2, " + mem + ")"},
		{name: "cat3/count_topk", promql: "count(topk(3, " + mem + "))"},
		{name: "cat3/sum_by_type", promql: "sum by(type)(" + mem + ")"},
		{name: "cat3/avg_without_instance", promql: "avg without(instance)(" + mem + ")"},
		{name: "cat3/max_by_mode_rate", promql: "max by(mode)(rate(" + cpu + "[5m]))"},
		{name: "cat3/min_by_instance", promql: "min by(instance)(" + mem + ")"},
		{name: "cat3/count_by_job", promql: "count by(job)(" + mem + ")"},
		{name: "cat3/sum_no_grouping", promql: "sum(" + mem + ")"},
	}
}

// cat4SetOps covers the set operators and / or / unless, matched both on
// the full label set and via on()/ignoring(). The oracle's set-op support
// is added in test/property/oracle/promql/binary.go::setOp so the
// assertion drives the REAL cerberus handler against the spec (rather than
// routing set ops to a Prometheus-only promqltest tail that would never
// exercise cerberus's lowering).
func cat4SetOps() []exoticCase {
	const mem = "demo_memory_usage_bytes"
	const cpu = "demo_cpu_usage_seconds_total"
	return []exoticCase{
		// `and` on the full label set: disjoint type selectors -> empty.
		{name: "cat4/and_disjoint_empty", promql: mem + "{type=\"used\"} and " + mem + "{type=\"cached\"}"},
		// `and` on(instance): used-typed series kept where a cached-typed
		// series shares the instance -> keeps all used series.
		{name: "cat4/and_on_instance", promql: mem + "{type=\"used\"} and on(instance) " + mem + "{type=\"cached\"}"},
		// `unless` removes the series for one instance.
		{name: "cat4/unless_instance", promql: mem + "{type=\"used\"} unless " + mem + "{type=\"used\",instance=\"demo.promlabs.com:10000\"}"},
		// `unless on(instance)` removes every used series whose instance
		// also has a cached series (all of them) -> empty.
		{name: "cat4/unless_on_instance", promql: mem + "{type=\"used\"} unless on(instance) " + mem + "{type=\"cached\"}"},
		// `or` unions two disjoint type selectors.
		{name: "cat4/or_union", promql: mem + "{type=\"used\"} or " + mem + "{type=\"cached\"}"},
		// `or` with a fully-overlapping RHS is a no-op (LHS wins).
		{name: "cat4/or_overlap_noop", promql: mem + "{type=\"used\"} or " + mem + "{type=\"used\"}"},
		// set op over rate() results (post-rate label sets).
		{name: "cat4/rate_and_rate", promql: "rate(" + cpu + "[5m]) and rate(" + cpu + "[5m])"},
		{name: "cat4/rate_unless_empty", promql: "rate(" + cpu + "[5m]) unless rate(does_not_exist[5m])"},
	}
}

// cat5Matching covers on()/ignoring()/group_left()/group_right() with extra
// labels copied across the join.
func cat5Matching() []exoticCase {
	const mem = "demo_memory_usage_bytes"
	const disk = "demo_disk_usage_bytes"
	const diskTotal = "demo_disk_total_bytes"
	return []exoticCase{
		// disk usage / total, one-to-one ignoring only __name__ (which
		// both sides drop anyway), so no label reduction — both agree.
		{name: "cat5/disk_usage_ratio", promql: disk + " / ignoring(__name__) " + diskTotal},
		// group_left copying an extra label from the one side.
		{name: "cat5/group_left_device", promql: disk + " / on(instance, device) group_left() " + diskTotal},
		// group_right (one-to-many flipped).
		{name: "cat5/group_right_device", promql: diskTotal + " / on(instance, device) group_right() " + disk},
		// many-to-one: every memory type over the per-instance num_cpus.
		{name: "cat5/mem_over_numcpus", promql: mem + " / on(instance, job) group_left() demo_num_cpus"},
		// on() with a non-shared dummy label still matches via the empty key.
		{name: "cat5/on_dummy", promql: mem + "{type=\"used\"} * on() group_left() demo_num_cpus{instance=\"demo.promlabs.com:10000\"}"},
		// ignoring the device dimension to roll usage against total.
		{name: "cat5/usage_over_total_ignoring", promql: "sum by(instance)(" + disk + ") / on(instance) sum by(instance)(" + diskTotal + ")"},
	}
}

// cat7AtOffset covers the @ modifier + offset (absolute pin, compose with
// offset, negative offset = forward). @ start()/end() determinism for range
// queries is DEFERRED to the promqltest tail (instant @start==@end here).
func cat7AtOffset() []exoticCase {
	const mem = "demo_memory_usage_bytes"
	pin := EvalTs - 120 // 2 minutes before the default eval ts
	return []exoticCase{
		{name: "cat7/at_pin", promql: mem + "{type=\"used\"} @ " + itoa(pin)},
		{name: "cat7/at_pin_offset", promql: mem + "{type=\"used\"} @ " + itoa(pin) + " offset 30s"},
		{name: "cat7/offset_at_pin", promql: mem + "{type=\"used\"} offset 30s @ " + itoa(pin)},
		{name: "cat7/offset_30s", promql: mem + "{type=\"used\"} offset 30s"},
		{name: "cat7/rate_at_pin", promql: "rate(demo_cpu_usage_seconds_total[5m] @ " + itoa(EvalTs) + ")"},
		{name: "cat7/sum_over_time_at", promql: "sum_over_time(" + mem + "{type=\"used\"}[2m] @ " + itoa(EvalTs) + ")"},
		// negative offset = look forward; pin earlier so the forward window
		// still lands inside the data.
		{name: "cat7/negative_offset", promql: mem + "{type=\"used\"} @ " + itoa(pin) + " offset -60s"},
	}
}

// cat8ScalarVector covers scalar()/vector() and scalar-vector arithmetic.
// absent/clamp/sort are DEFERRED (oracle gap).
func cat8ScalarVector() []exoticCase {
	const mem = "demo_memory_usage_bytes"
	return []exoticCase{
		{name: "cat8/scalar_vector_3", promql: "scalar(vector(3))"},
		{name: "cat8/scalar_nonsingleton", promql: "scalar(" + mem + ")"},
		{name: "cat8/scalar_of_sum", promql: "scalar(sum(demo_num_cpus))"},
		// `vector(1) + metric`: vector(1) is a label-less instant vector
		// ({} labels), so a V-V op matches on ALL labels and {} never
		// matches {instance, job} -> EMPTY. Pinned by Wave-0 fix (c)
		// (vector-typed synthetic operand routes through VectorJoin instead
		// of broadcasting).
		{name: "cat8/vector1_plus_metric", promql: "vector(1) + demo_num_cpus"},
		// Both operands are label-less {} vectors, so they DO match ->
		// one {} row with value 3 (the both-synthetic fold).
		{name: "cat8/vector1_plus_vector2", promql: "vector(1) + vector(2)"},
	}
}

// cat9OverTime covers the *_over_time family the oracle implements. The
// deriv/predict_linear/double_exponential_smoothing/quantile_over_time/
// stddev_over_time/changes/resets/last_over_time family is DEFERRED (oracle
// gap).
func cat9OverTime() []exoticCase {
	const mem = "demo_memory_usage_bytes"
	const inter = "demo_intermittent_metric"
	return []exoticCase{
		{name: "cat9/avg_over_time", promql: "avg_over_time(" + mem + "{type=\"used\"}[5m])"},
		{name: "cat9/min_over_time", promql: "min_over_time(" + mem + "{type=\"used\"}[5m])"},
		{name: "cat9/max_over_time", promql: "max_over_time(" + mem + "{type=\"used\"}[5m])"},
		{name: "cat9/sum_over_time", promql: "sum_over_time(" + mem + "{type=\"used\"}[5m])"},
		// count_over_time on the sparse series sees fewer samples than dense.
		{name: "cat9/count_over_time_sparse", promql: "count_over_time(" + inter + "[5m])"},
		{name: "cat9/count_over_time_dense", promql: "count_over_time(" + mem + "{type=\"used\"}[5m])"},
	}
}

// cat10ScalarOps covers scalar-vector divides, NaN/Inf, nested aggregation,
// bool-vs-filter comparisons, and empty/no-match (empty is not zero, empty
// is not error).
func cat10ScalarOps() []exoticCase {
	const mem = "demo_memory_usage_bytes"
	const cpu = "demo_cpu_usage_seconds_total"
	return []exoticCase{
		{name: "cat10/div_zero_inf", promql: "sum by(instance)(" + mem + ") / 0"},
		{name: "cat10/zero_times_div_zero_nan", promql: "0 * (sum by(instance)(" + mem + ") / 0)"},
		{name: "cat10/unary_pow_precedence", promql: "-2 ^ 2"},
		{name: "cat10/filter_gt", promql: "sum by(type)(" + mem + ") > 1e10"},
		{name: "cat10/bool_gt", promql: "sum by(type)(" + mem + ") > bool 1e10"},
		{name: "cat10/nested_agg", promql: "sum(sum by(instance)(" + mem + ")) by (job)"},
		{name: "cat10/avg_topk_minus", promql: "avg(topk(2, " + mem + ")) - 52"},
		{name: "cat10/one_over_vector_zero", promql: "1 / vector(0)"},
		{name: "cat10/empty_plus", promql: mem + "{type=\"does-not-exist\"} + " + mem},
		{name: "cat10/scalar_plus_rate", promql: "1 + rate(" + cpu + "[5m])"},
		{name: "cat10/rate_gt_scalar", promql: "rate(" + cpu + "[5m]) > 0.5"},
		{name: "cat10/div_by_count", promql: "sum(" + mem + ") / count(" + mem + ")"},
	}
}

// itoa renders an int64 in base 10 without pulling strconv into the matrix
// file's import set for one call (seed.go already imports strconv).
func itoa(v int64) string {
	return formatInt(v)
}
