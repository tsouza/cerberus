package engine_test

import (
	"context"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/logql/lsyntax"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/internal/traceql/ast"
)

const (
	settingQCC                  = "use_query_condition_cache"
	settingProjectionFiltering  = "optimize_use_projection_filtering"
	shapeDefectTestTraceIDValue = "0123456789abcdef0123456789abcdef"
)

// shapeDefectFleetRules is a fleet carrying ClickHouse#119168 and #120286,
// with the condition cache and the trace-id bitmap filter resolved in.
func shapeDefectFleetRules() engine.SettingsRules {
	return engine.SettingsRules{
		ConditionCache:                     true,
		TraceIDBitmapFilter:                true,
		ConversionConditionCacheDefect:     true,
		NegatedConjunctionProjectionDefect: true,
		Metrics:                            schema.DefaultOTelMetrics(),
		Traces:                             schema.DefaultOTelTraces(),
		Logs:                               schema.DefaultOTelLogs(),
	}
}

func lowerLogQL(t *testing.T, q string) chplan.Node {
	t.Helper()
	expr, err := lsyntax.ParseExpr(q)
	if err != nil {
		t.Fatalf("parse %s: %v", q, err)
	}
	plan, err := logql.Lower(context.Background(), expr, schema.DefaultOTelLogs())
	if err != nil {
		t.Fatalf("lower %s: %v", q, err)
	}
	return plan
}

func lowerTraceQL(t *testing.T, q string) chplan.Node {
	t.Helper()
	root, err := ast.Parse(q)
	if err != nil {
		t.Fatalf("parse %s: %v", q, err)
	}
	plan, err := traceql.Lower(context.Background(), root, schema.DefaultOTelTraces())
	if err != nil {
		t.Fatalf("lower %s: %v", q, err)
	}
	return plan
}

func lowerPromQL(t *testing.T, q string) chplan.Node {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(q)
	if err != nil {
		t.Fatalf("parse %s: %v", q, err)
	}
	plan, err := promql.Lower(context.Background(), expr, schema.DefaultOTelMetrics())
	if err != nil {
		t.Fatalf("lower %s: %v", q, err)
	}
	return plan
}

// TestShapeDefects_RealLowerings runs the shape-defect gates over what the
// heads actually lower, so a lowering that moves its String-to-float parse or
// its negation out of reach of the plan-shape facts fails here rather than in
// a wrong answer. Each lowering that emits toFloat64OrZero / toFloat64OrNull
// into a filter is a row with the cache off; the representative unaffected
// shapes keep it on, and only the negated mixed conjunction loses the
// projection index.
func TestShapeDefects_RealLowerings(t *testing.T) {
	tests := []struct {
		name           string
		plan           func(*testing.T) chplan.Node
		wantQCC        any
		wantProjection any
	}{
		{"logql numeric label filter", func(t *testing.T) chplan.Node {
			return lowerLogQL(t, `sum(count_over_time({service_name="a"} | ccv = 1.1e-300 [1h]))`)
		}, 0, nil},
		{"logql numeric label filter after a parser", func(t *testing.T) chplan.Node {
			return lowerLogQL(t, `{service_name="a"} | logfmt | ccv > 2`)
		}, 0, nil},
		{"logql bytes label filter", func(t *testing.T) chplan.Node {
			return lowerLogQL(t, `{service_name="a"} | ccv > 2KB`)
		}, 0, nil},
		{"logql line filter", func(t *testing.T) chplan.Node {
			return lowerLogQL(t, `sum(count_over_time({service_name="a"} |= "line" [1h]))`)
		}, 1, nil},
		{"logql string label filter", func(t *testing.T) chplan.Node {
			return lowerLogQL(t, `{service_name="a"} | ccv = "x"`)
		}, 1, nil},
		{"traceql numeric attribute compare", func(t *testing.T) chplan.Node {
			return lowerTraceQL(t, `{ span.foo > 5 }`)
		}, 0, nil},
		{"traceql string attribute compare", func(t *testing.T) chplan.Node {
			return lowerTraceQL(t, `{ span.foo = "x" }`)
		}, 1, nil},
		{"promql histogram_quantile over float bucket series", func(t *testing.T) chplan.Node {
			return lowerPromQL(t, `histogram_quantile(0.9, topk(2, rate(http_request_duration_seconds_bucket[5m])))`)
		}, 0, nil},
		{"promql histogram_quantile folding the bucket arrays", func(t *testing.T) chplan.Node {
			return lowerPromQL(t, `histogram_quantile(0.9, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`)
		}, 1, nil},
		{"promql selector", func(t *testing.T) chplan.Node {
			return lowerPromQL(t, `rate(http_requests_total{job="api"}[5m])`)
		}, 1, nil},
		{"traceql trace-id lookup", func(t *testing.T) chplan.Node {
			return lowerTraceQL(t, `{ trace:id = "`+shapeDefectTestTraceIDValue+`" }`)
		}, 1, nil},
		{"traceql trace-id with a negated mixed conjunction", func(t *testing.T) chplan.Node {
			return lowerTraceQL(t, `{ trace:id = "`+shapeDefectTestTraceIDValue+`" && !(trace:id = "`+shapeDefectTestTraceIDValue+`" && span.foo = "nope") }`)
		}, 1, 0},
		{"traceql trace-id with a negated disjunction", func(t *testing.T) chplan.Node {
			return lowerTraceQL(t, `{ trace:id = "`+shapeDefectTestTraceIDValue+`" && !(trace:id = "`+shapeDefectTestTraceIDValue+`" || span.foo = "nope") }`)
		}, 1, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := engine.SharedQuerySettingsForTest(shapeDefectFleetRules(), tc.plan(t))
			if got[settingQCC] != tc.wantQCC {
				t.Errorf("%s = %v; want %v", settingQCC, got[settingQCC], tc.wantQCC)
			}
			if got[settingProjectionFiltering] != tc.wantProjection {
				t.Errorf("%s = %v; want %v", settingProjectionFiltering, got[settingProjectionFiltering], tc.wantProjection)
			}
		})
	}
}
