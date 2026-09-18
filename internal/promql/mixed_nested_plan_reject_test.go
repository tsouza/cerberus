package promql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/schema"
)

// A mixed float/histogram plan that is produced ONE LEVEL DOWN — by a
// payload-preserving wrapper (sort_by_label, limitk, a further set operator,
// a subquery select) over a mixed `or` — must not reach the generic float
// consumers of scalar scaling, unary minus, and vector-vector arithmetic or
// comparison. Those consumers narrow through mixedRowsFloatOnly or join on
// the placeholder Value column, so they would DROP every histogram row
// (`* 2`, `/ 2`, unary `-`) or FABRICATE a float sample from a histogram's
// placeholder Value (`+ up`, `- up`, `> bool up`). Reference Prometheus scales
// histograms under `*`, `/` and unary `-`, and emits no sample at all for a
// histogram-vs-float `+`/`-`/comparison.
//
// Until the nested shapes are lowered through the discriminator-aware folds
// the direct `(h or f) * 2` / `(h or f) + up` roots already use (cerberus
// issue #3562), every such query is REJECTED at lowering with the
// not-admitted error rather than answered with the wrong rows. This test
// pins the rejection for the class; #3562 turns each row into a value
// assertion when it lands.
func TestNestedMixedPlanAtFloatConsumersIsRejected(t *testing.T) {
	const direct = `(latency_exp_hist or num_cpus)`
	const nested = `sort_by_label(latency_exp_hist or num_cpus, "job")`
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		query  string
		family mixedWrapperFamily
	}{
		{nested + ` * 2`, mixedScaleFamily},
		{`2 * ` + nested, mixedScaleFamily},
		{nested + ` / 2`, mixedScaleFamily},
		{`limitk(5, ` + direct + `) / 2`, mixedScaleFamily},
		{`last_over_time(` + direct + `[5m:1m]) * 2`, mixedScaleFamily},
		{`(` + direct + ` and up) * 2`, mixedScaleFamily},
		{`-` + nested, mixedUnaryFamily},
		{`-limitk(5, ` + direct + `)`, mixedUnaryFamily},
		{nested + ` + up`, mixedVectorArithmeticFamily},
		{`up - ` + nested, mixedVectorArithmeticFamily},
		{nested + ` + ` + nested, mixedVectorArithmeticFamily},
		{`last_over_time(` + direct + `[5m:1m]) + up`, mixedVectorArithmeticFamily},
		{nested + ` > bool up`, mixedVectorComparisonFamily},
		{`last_over_time(` + direct + `[5m:1m]) > bool up`, mixedVectorComparisonFamily},
		{`limitk(5, ` + direct + `) == up`, mixedVectorComparisonFamily},
	} {
		t.Run(tc.query, func(t *testing.T) {
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			_, err = LowerAt(context.Background(), expr, s, at, at)
			want := "mixed operand is not admitted for " + string(tc.family) + " at " + string(mixedPlanAdmission)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("nested mixed plan reached a float-only consumer: err=%v, want %q", err, want)
			}
		})
	}
}

// Unary `+` is the identity, so a nested mixed plan under it keeps every row
// and is still admitted; the direct roots the folds already answer stay
// admitted too. Pinning both keeps the rejection above narrow.
func TestNestedMixedPlanIdentityAndDirectRootsStayAdmitted(t *testing.T) {
	const direct = `(latency_exp_hist or num_cpus)`
	const nested = `sort_by_label(latency_exp_hist or num_cpus, "job")`
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, query := range []string{
		`+` + nested,
		direct + ` * 2`,
		`-` + direct,
		direct + ` + up`,
		direct + ` > bool up`,
	} {
		t.Run(query, func(t *testing.T) {
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := LowerAt(context.Background(), expr, s, at, at); err != nil {
				t.Fatalf("admitted shape rejected: %v", err)
			}
		})
	}
}
