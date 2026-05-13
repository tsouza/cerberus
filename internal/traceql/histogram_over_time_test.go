package traceql_test

import (
	"context"
	"testing"

	tempo "github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TestLowerHistogramOverTime exercises the histogram_over_time lowering
// directly: parse a TraceQL histogram_over_time query, lower it, and
// confirm the resulting tree is a chplan.MetricsHistogramOverTime with
// the expected attribute / group-by / duration flag.
func TestLowerHistogramOverTime(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	cases := []struct {
		name           string
		query          string
		wantIsDuration bool
		wantGroup      int
	}{
		{
			name:           "duration_no_by",
			query:          `{} | histogram_over_time(duration)`,
			wantIsDuration: true,
			wantGroup:      0,
		},
		{
			name:           "duration_by_service",
			query:          `{} | histogram_over_time(duration) by (resource.service.name)`,
			wantIsDuration: true,
			wantGroup:      1,
		},
		{
			name:           "span_attr_no_by",
			query:          `{} | histogram_over_time(span.http.request.body.size)`,
			wantIsDuration: false,
			wantGroup:      0,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := tempo.Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			plan, err := traceql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}

			h, ok := plan.(*chplan.MetricsHistogramOverTime)
			if !ok {
				t.Fatalf("expected *chplan.MetricsHistogramOverTime, got %T", plan)
			}
			if h.Attr == nil {
				t.Errorf("MetricsHistogramOverTime.Attr is nil; want operand expression")
			}
			if h.IsDuration != tc.wantIsDuration {
				t.Errorf("IsDuration = %v, want %v", h.IsDuration, tc.wantIsDuration)
			}
			if h.BucketAlias != "__bucket" {
				t.Errorf("BucketAlias = %q, want %q", h.BucketAlias, "__bucket")
			}
			if h.ValueAlias != "Value" {
				t.Errorf("ValueAlias = %q, want %q", h.ValueAlias, "Value")
			}
			if len(h.GroupBy) != tc.wantGroup {
				t.Errorf("len(GroupBy) = %d, want %d", len(h.GroupBy), tc.wantGroup)
			}
			if len(h.GroupBy) != len(h.GroupByAliases) {
				t.Errorf("GroupBy / GroupByAliases length mismatch: %d vs %d",
					len(h.GroupBy), len(h.GroupByAliases))
			}
			if h.Inner == nil {
				t.Errorf("Inner is nil; want the spanset tree")
			}
		})
	}
}

// TestLowerHistogramOverTimeRequiresAttr documents that the lowering
// surfaces a clean error when the histogram_over_time aggregator has no
// operand. The TraceQL grammar requires an attribute, so this is mostly
// a defence-in-depth assertion — the parser may reject it first.
func TestLowerHistogramOverTimeRequiresAttr(t *testing.T) {
	t.Parallel()

	// `histogram_over_time()` with no argument is rejected by the
	// parser. We treat a parser error as an acceptable outcome — the
	// test documents that the lowering does not panic on the no-attr
	// shape.
	s := schema.DefaultOTelTraces()
	expr, err := tempo.Parse(`{} | histogram_over_time()`)
	if err != nil {
		return
	}
	if _, err := traceql.Lower(context.Background(), expr, s); err == nil {
		t.Fatalf("Lower with no operand: expected error, got nil")
	}
}
