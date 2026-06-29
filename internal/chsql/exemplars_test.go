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
	}, "TraceId", "SpanId", 1, "")
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
				tc.traceIDCol, tc.spanIDCol, 1, "")
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

	sql, args, err := chsql.EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 2, "")
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
	// The outer SELECT MUST project exactly four aliased columns —
	// MetricName, Attributes, TimeUnix, Value — because chclient.Cursor
	// binds the result-set rows positionally to those four fields of
	// chclient.Sample. Group-by attributes ride inside the Attributes
	// map (toString(<alias>) per by(...) key) rather than as extra
	// columns; a 5+ column projection would crash the cursor scan with
	// a "sql: expected 4 destination arguments in Scan" failure at run
	// time. Pin the contract via substring checks on the aliased shape.
	for _, alias := range []string{"AS `MetricName`", "AS `Attributes`", "AS `TimeUnix`", "AS `Value`"} {
		if !strings.Contains(sql, alias) {
			t.Errorf("SQL missing required column alias %q\nSQL=%s", alias, sql)
		}
	}
	// And the redundant raw group-alias projection MUST be absent — i.e.
	// the outer SELECT must not project the by(...) attribute alone as
	// a column. The group alias only legitimately appears inside the
	// Attributes map's toString(...) wrapper. Pin on a sequence that
	// can only show up in the outer SELECT (MetricName comes only from
	// the outer-shape literal projection).
	if strings.Contains(sql, "AS `MetricName`, `resource.service.name`") {
		t.Errorf("SQL still emits group alias as a separate column (breaks 4-column Sample shape)\nSQL=%s", sql)
	}
}
