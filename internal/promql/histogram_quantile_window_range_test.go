// Tests in this file pin the invariant the two aggregated histogram-quantile
// lowerings rest on: every [histogramAggShape] that [matchHistogramAggIdiom]
// yields carries a STRICTLY POSITIVE windowRange. Both
// [lowerHistogramQuantileAgg] and [lowerHistogramQuantileNativeAgg] bind the
// window's left bound and its staleness lower bound unconditionally from that
// field, so a non-positive value would emit a degenerate window — a
// zero-width `TimeUnix` range on the classic path, and a scan with no lower
// time bound at all once the staleness predicate is dropped.
package promql

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/promql/promparse"
	"github.com/tsouza/cerberus/internal/schema"
)

// windowRangeProbeParser IS the parser every production cerberus entry point
// parses with, not a copy of its options: [promparse.New] is the single
// construction site, so this pin cannot stay green while a production site
// diverges. It used to spell `parser.Options{EnableExperimentalFunctions: true}`
// itself, which made it an eleventh copy asserting about the other ten.
//
// promparse.New leaves Options.ExperimentalDurationExpr unset, which is what
// [TestPromQLGrammarRefusesNonPositiveMatrixRange] turns on its head.
var windowRangeProbeParser = promparse.New()

// histogramQuantileShapeCorpus enumerates the histogram_quantile arguments
// that can reach [matchHistogramAggIdiom]: both histogram families, every
// selector spelling the matcher's own name test distinguishes, every
// aggregation wrapper, both window arms (the #1692 no-range-wrapper arm and
// each supported range-vector function), and every eval modifier.
//
// The phi argument is deliberately absent: the matcher only ever inspects
// `c.Args[1]`, so varying `c.Args[0]` would multiply the corpus without
// reaching a single new shape.
func histogramQuantileShapeCorpus() []string {
	metrics := []string{
		"http_req_duration_seconds_bucket",
		"http_req_duration_exp_hist",
	}
	selectors := []string{
		"%s",
		`%s{job="api"}`,
		`%s{le="0.5"}`,
		`%s{le=~"0\\.5|1"}`,
		`%s{job!="api", le!="+Inf"}`,
		`{__name__=~"%s"}`,
	}
	// "" is the #1692 arm: an aggregation directly over a bucket selector,
	// with no range-vector wrapper at all.
	windows := []string{
		"",
		"rate(%s[1s])",
		"rate(%s[30s])",
		"rate(%s[5m])",
		"rate(%s[1h])",
		"rate(%s[1d])",
		"increase(%s[1s])",
		"increase(%s[5m])",
		"sum_over_time(%s[5m])",
		"sum_over_time(%s[2h])",
	}
	aggregations := []string{
		"",
		"sum",
		"sum by(le)",
		"sum by(le, job)",
		"sum without(le)",
		"sum without(job)",
		"sum by(job)",
		"avg by(le)",
		"max by(le)",
		"min by(le)",
		"count by(le)",
		"group by(le)",
		"stddev by(le)",
		"quantile by(le)",
		"topk by(le)",
	}
	modifiers := []string{"", " offset 5m", " offset -5m", " @ 1700000000", " @ start()", " @ end()"}

	seen := map[string]bool{}
	var out []string
	for _, metric := range metrics {
		for _, selector := range selectors {
			base := fmt.Sprintf(selector, metric)
			for _, modifier := range modifiers {
				for _, window := range windows {
					inner := base + modifier
					if window != "" {
						// The modifier binds inside the range-vector call:
						// `rate(m[5m] offset 5m)`.
						inner = strings.TrimSuffix(fmt.Sprintf(window, base), ")") + modifier + ")"
					}
					for _, aggregation := range aggregations {
						arg := inner
						switch {
						case aggregation == "":
						case strings.HasPrefix(aggregation, "quantile"), strings.HasPrefix(aggregation, "topk"):
							arg = aggregation + "(2, " + inner + ")"
						default:
							arg = aggregation + "(" + inner + ")"
						}
						q := "histogram_quantile(0.9, " + arg + ")"
						if !seen[q] {
							seen[q] = true
							out = append(out, q)
						}
					}
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// TestMatchHistogramAggIdiomAlwaysYieldsPositiveWindowRange sweeps
// [histogramQuantileShapeCorpus] and pins that no accepted shape carries a
// non-positive windowRange.
//
// The two assignments the matcher makes are
// histogram_quantile.go:`windowRange: instantLookback` (the #1692
// no-range-wrapper arm; instantLookback is qlcommon.InstantLookback, 5m) and
// histogram_quantile.go:`windowRange: ms.Range` (the rate / increase /
// sum_over_time arm), and this test fails the moment either stops being
// strictly positive.
func TestMatchHistogramAggIdiomAlwaysYieldsPositiveWindowRange(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	corpus := histogramQuantileShapeCorpus()
	matched := 0
	for _, q := range corpus {
		expr, err := windowRangeProbeParser.ParseExpr(q)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", q, err)
		}
		call, ok := expr.(*parser.Call)
		if !ok || len(call.Args) != 2 {
			t.Fatalf("ParseExpr(%q) is not a two-argument Call: %T", q, expr)
		}
		shape, ok := matchHistogramAggIdiom(call.Args[1], s)
		if !ok {
			continue
		}
		matched++
		if shape.windowRange <= 0 {
			t.Fatalf("matchHistogramAggIdiom(%q) yielded windowRange = %v; "+
				"lowerHistogramQuantileAgg and lowerHistogramQuantileNativeAgg "+
				"bind the window's left bound and staleness lower bound from it "+
				"unconditionally, so a non-positive value emits a degenerate window",
				q, shape.windowRange)
		}
	}
	// A corpus that stopped matching would pass the loop above vacuously.
	if matched < len(corpus)/2 {
		t.Fatalf("matchHistogramAggIdiom accepted %d of %d corpus arguments; "+
			"the corpus has stopped exercising the matcher", matched, len(corpus))
	}
}

// TestPromQLGrammarRefusesNonPositiveMatrixRange pins the other half of the
// invariant: `ms.Range`, the matcher's second windowRange source, can never
// reach it as zero.
//
// The grammar's `positive_duration_expr` nonterminal rejects a non-positive
// literal outright, and a MatrixSelector's Range is populated from a
// *NumberLiteral only — a duration EXPRESSION leaves Range at zero and moves
// the value into RangeExpr. That second spelling is what
// [windowRangeProbeParser]'s options close off: with
// Options.ExperimentalDurationExpr unset, the grammar records
// "experimental duration expression is not enabled" and ParseExpr returns an
// error instead of an expression with a zero Range.
func TestPromQLGrammarRefusesNonPositiveMatrixRange(t *testing.T) {
	t.Parallel()

	for _, q := range []string{
		// Non-positive literals.
		"rate(http_req_duration_seconds_bucket[0s])",
		"rate(http_req_duration_seconds_bucket[0m])",
		"rate(http_req_duration_seconds_bucket[0])",
		"rate(http_req_duration_seconds_bucket[-5m])",
		"histogram_quantile(0.9, sum by(le)(rate(http_req_duration_seconds_bucket[0s])))",
		// Duration expressions — the only spelling that leaves
		// MatrixSelector.Range at zero on a successful parse.
		"rate(http_req_duration_seconds_bucket[1m+1m])",
		"rate(http_req_duration_seconds_bucket[2m-2m])",
		"rate(http_req_duration_seconds_bucket[1m*0])",
		"rate(http_req_duration_seconds_bucket[step()])",
		"rate(http_req_duration_seconds_bucket[range()])",
		"histogram_quantile(0.9, sum by(le)(rate(http_req_duration_seconds_bucket[2m-2m])))",
	} {
		if _, err := windowRangeProbeParser.ParseExpr(q); err == nil {
			t.Fatalf("ParseExpr(%q) succeeded; a zero MatrixSelector.Range would then "+
				"reach matchHistogramAggIdiom's `windowRange: ms.Range` assignment", q)
		}
	}
}
