package promql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestExpHistogramRecognizersRejectWhenLoweringUnavailable is the
// behavioural pin under cerberus issue #2963's deletion of twenty-one
// copied guards.
//
// Before that change, each exp-histogram recognizer asserted the
// availability rule for itself, as its own first statement. Twenty-one of
// those statements decided nothing — the recognizer re-derives the same
// verdict through [isExpHistogramValuedShape] / [isExpHistogramDroppingShape]
// — so deleting them changed no behaviour, which is exactly why nothing
// would have caught it if they HAD changed behaviour. What the deleted
// copies used to assert site by site, this test asserts for the whole set
// at once: with exp-histogram lowering unavailable, for EITHER of the two
// reasons, every recognizer and both predicates answer false and hand back
// the zero value for every other return.
//
// The negative assertion alone would pass vacuously — a recognizer that
// rejected every input for an unrelated reason would satisfy it. So each
// query is first put through the same matrix with lowering AVAILABLE, and
// the test fails unless the matrix as a whole recognises something there.
// That positive control is what makes the negative half evidence.
func TestExpHistogramRecognizersRejectWhenLoweringUnavailable(t *testing.T) {
	t.Parallel()

	available := schema.DefaultOTelMetrics()
	noTable := schema.DefaultOTelMetrics()
	noTable.ExpHistogramTable = ""

	unavailable := []struct {
		name string
		s    schema.Metrics
		c    lowerCtx
	}{
		{"noExpHistogramTable", noTable, baseLowerCtx()},
		{"metadataFullRange", available, metadataFullRangeLowerCtx()},
		{"both", noTable, metadataFullRangeLowerCtx()},
	}

	recognized := 0
	for _, q := range expHistogramShapeMatrix() {
		p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
		expr, err := p.ParseExpr(q)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", q, err)
		}

		for _, r := range expHistogramRecognizers() {
			if strings.HasSuffix(r.call(expr, available, baseLowerCtx()), "set") {
				recognized++
			}
			for _, u := range unavailable {
				if got := r.call(expr, u.s, u.c); got != zeroTuple(got) {
					t.Errorf("%s(%q) under %s = %s; want every return at its zero value — "+
						"exp-histogram lowering is unavailable, so no recognizer may accept "+
						"(histogram_native_availability.go:expHistogramLoweringAvailable:"+
						"`s.ExpHistogramTable != \"\" && !ctx.metadataFullRange`)",
						r.name, q, u.name, got)
				}
			}
		}
	}

	if recognized == 0 {
		t.Fatalf("positive control: no recognizer accepted any of the %d shapes with lowering "+
			"available; the negative assertions above would then hold for the wrong reason",
			len(expHistogramShapeMatrix()))
	}
}

func baseLowerCtx() lowerCtx {
	return lowerCtx{lowerers: RangeLowerers{}.withDefaults(), resourceBounds: DefaultResourceBounds()}
}

func metadataFullRangeLowerCtx() lowerCtx {
	c := baseLowerCtx()
	c.metadataFullRange = true
	return c
}

// expHistogramRecognizer pairs a recognizer's name with a call that renders
// its WHOLE return tuple comparably: "zero" or "set" per value, in order.
// The bool verdict is the last entry, so an all-"zero" rendering is exactly
// "rejected, and handed back nothing".
type expHistogramRecognizer struct {
	name string
	call func(parser.Expr, schema.Metrics, lowerCtx) string
}

func tup(vals ...any) string {
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		if v == nil || reflect.ValueOf(v).IsZero() {
			parts = append(parts, "zero")
			continue
		}
		parts = append(parts, "set")
	}
	return strings.Join(parts, ",")
}

// zeroTuple is the all-rejected rendering of a tuple the same width as got.
func zeroTuple(got string) string {
	parts := strings.Split(got, ",")
	for i := range parts {
		parts[i] = "zero"
	}
	return strings.Join(parts, ",")
}

// expHistogramShapeMatrix spans every shape family the exp-histogram
// recognizers cover — bare, aggregated, windowed, subquery, binop, set-op,
// mixed-or, label-call and dropping — plus plain float controls.
func expHistogramShapeMatrix() []string {
	return []string{
		// bare
		`latency_exp_hist`,
		`latency_exp_hist[5m]`,
		`latency_exp_hist{job="a"}`,
		// aggregated
		`sum(latency_exp_hist)`,
		`avg(latency_exp_hist)`,
		`count(latency_exp_hist)`,
		`group(latency_exp_hist)`,
		`count_values("v", latency_exp_hist)`,
		`min(latency_exp_hist)`,
		`max(latency_exp_hist)`,
		`topk(3, latency_exp_hist)`,
		`quantile(0.9, latency_exp_hist)`,
		`limitk(3, latency_exp_hist)`,
		`limit_ratio(0.5, latency_exp_hist)`,
		`sum by (job) (latency_exp_hist)`,
		// windowed
		`rate(latency_exp_hist[5m])`,
		`increase(latency_exp_hist[5m])`,
		`delta(latency_exp_hist[5m])`,
		`irate(latency_exp_hist[5m])`,
		`idelta(latency_exp_hist[5m])`,
		`sum_over_time(latency_exp_hist[5m])`,
		`avg_over_time(latency_exp_hist[5m])`,
		`last_over_time(latency_exp_hist[5m])`,
		`first_over_time(latency_exp_hist[5m])`,
		`count_over_time(latency_exp_hist[5m])`,
		`present_over_time(latency_exp_hist[5m])`,
		`resets(latency_exp_hist[5m])`,
		`changes(latency_exp_hist[5m])`,
		`ts_of_first_over_time(latency_exp_hist[5m])`,
		`ts_of_last_over_time(latency_exp_hist[5m])`,
		`sum(rate(latency_exp_hist[5m]))`,
		// subquery
		`rate(latency_exp_hist[5m:1m])`,
		`sum_over_time(latency_exp_hist[5m:1m])`,
		`last_over_time(latency_exp_hist[5m:1m])`,
		`count_over_time(latency_exp_hist[5m:1m])`,
		`resets(latency_exp_hist[5m:1m])`,
		`ts_of_last_over_time(latency_exp_hist[5m:1m])`,
		`increase(sum(latency_exp_hist)[10m:1m])`,
		// binop
		`latency_exp_hist + latency_exp_hist`,
		`latency_exp_hist - latency_exp_hist`,
		`latency_exp_hist == latency_exp_hist`,
		`latency_exp_hist != latency_exp_hist`,
		`latency_exp_hist == bool latency_exp_hist`,
		`latency_exp_hist * 2`,
		`2 * latency_exp_hist`,
		`latency_exp_hist / 2`,
		`latency_exp_hist > 2`,
		`latency_exp_hist * cpu_total`,
		`cpu_total * latency_exp_hist`,
		`latency_exp_hist / cpu_total`,
		`latency_exp_hist * on (job) group_left () cpu_total`,
		`latency_exp_hist * on (job) group_right () cpu_total`,
		`latency_exp_hist > cpu_total`,
		`-latency_exp_hist`,
		`+latency_exp_hist`,
		// set op
		`latency_exp_hist or latency_exp_hist`,
		`latency_exp_hist and latency_exp_hist`,
		`latency_exp_hist unless latency_exp_hist`,
		`latency_exp_hist or cpu_total`,
		`cpu_total or latency_exp_hist`,
		`sum(latency_exp_hist or cpu_total)`,
		`label_replace(latency_exp_hist or cpu_total, "a", "b", "c", "d")`,
		`count_over_time((latency_exp_hist or cpu_total)[5m:1m])`,
		`rate((latency_exp_hist or cpu_total)[5m:1m])`,
		`sum(count_over_time((latency_exp_hist or cpu_total)[5m:1m]))`,
		// label calls / producers
		`label_replace(latency_exp_hist, "a", "b", "c", "d")`,
		`label_join(latency_exp_hist, "a", ",", "job")`,
		`label_replace(sum(latency_exp_hist), "a", "b", "c", "d")`,
		`sort_by_label(latency_exp_hist, "job")`,
		`info(latency_exp_hist)`,
		// dropping shapes nested
		`sum(min(latency_exp_hist))`,
		`sum(latency_exp_hist > 2)`,
		`label_replace(min(latency_exp_hist), "a", "b", "c", "d")`,
		`histogram_count(latency_exp_hist)`,
		`histogram_sum(latency_exp_hist)`,
		// non-exp-hist controls
		`cpu_total`,
		`sum(rate(cpu_total[5m]))`,
		`cpu_total + cpu_total`,
	}
}

// expHistogramRecognizers is every recognizer and predicate that consults
// the availability rule, directly or through the two predicates.
func expHistogramRecognizers() []expHistogramRecognizer {
	return []expHistogramRecognizer{
		{"bareExpHistogramSelector", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, ok := bareExpHistogramSelector(e, s, c)
			return tup(v0, ok)
		}},
		{"bareExpHistogramMatrixSelector", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, v1, ok := bareExpHistogramMatrixSelector(e, s, c)
			return tup(v0, v1, ok)
		}},
		{"aggregationOverExpHistogramDroppingShape", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, ok := aggregationOverExpHistogramDroppingShape(e, s, c)
			return tup(v0, ok)
		}},
		{"labelCallOverExpHistogramDroppingShape", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, ok := labelCallOverExpHistogramDroppingShape(e, s, c)
			return tup(v0, ok)
		}},
		{"countValuesOverExpHistogramValue", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, ok := countValuesOverExpHistogramValue(e, s, c)
			return tup(v0, ok)
		}},
		{"expHistogramHistogramCompareBinop", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, v1, v2, v3, ok := expHistogramHistogramCompareBinop(e, s, c)
			return tup(v0, v1, v2, v3, ok)
		}},
		{"rangeFnOverExpHistogram", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, ok := rangeFnOverExpHistogram(e, s, c)
			return tup(v0, ok)
		}},
		{"rangeFnOverExpHistogramSubquery", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, ok := rangeFnOverExpHistogramSubquery(e, s, c)
			return tup(v0, ok)
		}},
		{"expHistogramSetOp", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, ok := expHistogramSetOp(e, s, c)
			return tup(v0, ok)
		}},
		{"resetsOrChangesOverExpHistogram", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, ok := resetsOrChangesOverExpHistogram(e, s, c)
			return tup(v0, ok)
		}},
		{"sumOrAvgOverExpHistogram", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, v1, ok := sumOrAvgOverExpHistogram(e, s, c)
			return tup(v0, v1, ok)
		}},
		{"unaryOverExpHistogram", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, v1, ok := unaryOverExpHistogram(e, s, c)
			return tup(v0, v1, ok)
		}},
		{"expHistogramFloatVectorScalingBinop", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, v1, v2, v3, v4, v5, ok := expHistogramFloatVectorScalingBinop(e, s, c)
			return tup(v0, v1, v2, v3, v4, v5, ok)
		}},
		{"droppingAggregationOverExpHistogram", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, v1, ok := droppingAggregationOverExpHistogram(e, s, c)
			return tup(v0, v1, ok)
		}},
		{"expHistogramHistogramBinop", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, v1, v2, v3, ok := expHistogramHistogramBinop(e, s, c)
			return tup(v0, v1, v2, v3, ok)
		}},
		{"expHistogramDroppingHistogramBinop", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, v1, ok := expHistogramDroppingHistogramBinop(e, s, c)
			return tup(v0, v1, ok)
		}},
		{"countPresentOverExpHistogram", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, v1, v2, ok := countPresentOverExpHistogram(e, s, c)
			return tup(v0, v1, v2, ok)
		}},
		{"expHistogramDroppingVectorBinop", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, v1, ok := expHistogramDroppingVectorBinop(e, s, c)
			return tup(v0, v1, ok)
		}},
		{"lastFirstOverExpHistogram", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, v1, v2, ok := lastFirstOverExpHistogram(e, s, c)
			return tup(v0, v1, v2, ok)
		}},
		{"mixedExpHistogramSetOp", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, ok := mixedExpHistogramSetOp(e, s, c)
			return tup(v0, ok)
		}},
		{"limitKOrRatioOverExpHistogram", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, ok := limitKOrRatioOverExpHistogram(e, s, c)
			return tup(v0, ok)
		}},
		{"overTimeOverExpHistogram", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, ok := overTimeOverExpHistogram(e, s, c)
			return tup(v0, ok)
		}},
		{"expHistogramScalarBinop", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, v1, v2, ok := expHistogramScalarBinop(e, s, c)
			return tup(v0, v1, v2, ok)
		}},
		{"expHistogramDroppingScalarBinop", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, ok := expHistogramDroppingScalarBinop(e, s, c)
			return tup(v0, ok)
		}},
		{"tsOfFirstLastOverExpHistogram", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, v1, v2, ok := tsOfFirstLastOverExpHistogram(e, s, c)
			return tup(v0, v1, v2, ok)
		}},
		{"selectFnOverExpHistogramSubquery", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, ok := selectFnOverExpHistogramSubquery(e, s, c)
			return tup(v0, ok)
		}},
		{"countOverExpHistogram", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, v1, ok := countOverExpHistogram(e, s, c)
			return tup(v0, v1, ok)
		}},
		{"countOrGroupOverExpHistogramValue", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, ok := countOrGroupOverExpHistogramValue(e, s, c)
			return tup(v0, ok)
		}},
		{"isExpHistogramValuedShape", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			return tup(isExpHistogramValuedShape(e, s, c))
		}},
		{"isExpHistogramDroppingShape", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			return tup(isExpHistogramDroppingShape(e, s, c))
		}},
		{"isExpHistogramValuedOrForwarded", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			return tup(isExpHistogramValuedOrForwarded(e, s, c))
		}},
		{"isExpHistogramForwardedThroughSetOp", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			return tup(isExpHistogramForwardedThroughSetOp(e, s, c))
		}},
		{"selectFnHistogramPreservingSubquery", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, ok := selectFnHistogramPreservingSubquery(e, s, c)
			return tup(v0, ok)
		}},
		{"labelCallOverExpHistogram", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, ok := labelCallOverExpHistogram(e, s, c)
			return tup(v0, ok)
		}},
		{"histogramValuedProducerCall", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, ok := histogramValuedProducerCall(e, s, c)
			return tup(v0, ok)
		}},
		{"sumOrAvgOverMixedExpHistogramSetOp", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, v1, ok := sumOrAvgOverMixedExpHistogramSetOp(e, s, c)
			return tup(v0, v1, ok)
		}},
		{"labelCallOverMixedExpHistogramSetOp", func(e parser.Expr, s schema.Metrics, c lowerCtx) string {
			v0, v1, ok := labelCallOverMixedExpHistogramSetOp(e, s, c)
			return tup(v0, v1, ok)
		}},
	}
}
