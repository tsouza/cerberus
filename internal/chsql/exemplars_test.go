package chsql_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

func TestEmitMetricsExemplars_NilRangeWindow(t *testing.T) {
	t.Parallel()
	_, _, err := chsql.EmitMetricsExemplars(context.Background(), nil, &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpRate,
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}, "TraceId", "SpanId", 1)
	if err == nil {
		t.Fatalf("expected error for nil RangeWindow, got nil")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("expected ErrUnsupported, got %v", err)
	}
}

func TestEmitMetricsExemplars_MissingColumns(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		traceIDCol string
		spanIDCol  string
		wantSub    string
	}{
		{name: "empty_trace_id", traceIDCol: "", spanIDCol: "SpanId", wantSub: "traceIDCol"},
		{name: "empty_span_id", traceIDCol: "TraceId", spanIDCol: "", wantSub: "spanIDCol"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.RangeWindow{
				Input: &chplan.MetricsAggregate{
					Op:         chplan.MetricsOpRate,
					ValueAlias: "Value",
					Inner:      &chplan.Scan{Table: "otel_traces"},
				},
				Step:            time.Minute,
				Range:           time.Minute,
				Start:           time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
				End:             time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC),
				TimestampColumn: "Timestamp",
			}
			_, _, err := chsql.EmitMetricsExemplars(context.Background(), plan,
				plan.Input.(*chplan.MetricsAggregate),
				tc.traceIDCol, tc.spanIDCol, 1)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q missing substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestEmitMetricsExemplars_ShapeSanity(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)
	m := &chplan.MetricsAggregate{
		Op:             chplan.MetricsOpRate,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "resource.service.name"}},
		GroupByAliases: []string{"resource.service.name"},
		ValueAlias:     "Value",
		Inner:          &chplan.Scan{Table: "otel_traces"},
	}
	rw := &chplan.RangeWindow{
		Input:           m,
		Step:            time.Minute,
		Range:           time.Minute,
		Start:           start,
		End:             end,
		TimestampColumn: "Timestamp",
	}

	sql, args, err := chsql.EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 2)
	if err != nil {
		t.Fatalf("EmitMetricsExemplars: %v", err)
	}

	tokens := []string{
		"argMax",
		"exemplar_trace_id",
		"exemplar_span_id",
		"map(",
		"LIMIT 2 BY",
	}
	for _, tok := range tokens {
		if !strings.Contains(sql, tok) {
			t.Errorf("SQL missing token %q\nSQL=%s", tok, sql)
		}
	}
	if len(args) == 0 {
		t.Errorf("expected non-empty args, got nil")
	}
}
