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

// TestEmitMetricsExemplars_StructuralUnwindowedInnerRejected is the FIX B
// regression for the exemplars emit-path bypass. The inner of a
// `{ } >> { } | rate()` structural metric is a recursive descendant closure
// over otel_traces. With no request window stamped on that closure's step scan
// (TimestampColumn / WindowStartNano unset), the recursive `FROM otel_traces`
// arm reads full retention — the traces-OOM class. The OUTER exemplars grid IS
// windowed (rw.Start/End set), so the statement carries a request window and
// the node-level requireInnerSpansScanBound — which only fires on a zero OUTER
// window — passes. Before FIX B, EmitMetricsExemplars built and returned this
// SQL (the handler executed it directly, never through chsql.Emit, so the
// universal guard never saw it). EmitMetricsExemplars must now run
// GuardEmittedSQL on its own output and reject the unwindowed recursive arm.
func TestEmitMetricsExemplars_StructuralUnwindowedInnerRejected(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)
	inner := &chplan.StructuralJoin{
		Left:               &chplan.Scan{Table: "otel_traces"},
		Right:              &chplan.Scan{Table: "otel_traces"},
		Op:                 chplan.StructuralDescendant,
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
		// TimestampColumn / WindowStartNano / WindowEndNano deliberately
		// unset: the recursive step scan renders without a window predicate.
	}
	m := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpRate,
		ValueAlias: "Value",
		Inner:      inner,
	}
	rw := &chplan.RangeWindow{
		Input:           m,
		Step:            time.Minute,
		Range:           time.Minute,
		Start:           start,
		End:             end,
		TimestampColumn: "Timestamp",
	}
	_, _, err := chsql.EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1, "otel_traces")
	if err == nil {
		t.Fatalf("expected ErrUnboundedSpansScan for unwindowed recursive exemplars inner, got nil")
	}
	if !errors.Is(err, chsql.ErrUnboundedSpansScan) {
		t.Fatalf("expected ErrUnboundedSpansScan, got %v", err)
	}
}

// TestEmitMetricsExemplars_PlainWindowedInnerAccepted is the negative control:
// with the spans table explicitly set (matching production, which threads
// h.Schema.SpansTable), a plain windowed inner scan still emits cleanly. The
// guard is shape-gated, not table-gated — a windowed otel_traces scan is not
// rejected just because the table name is known.
func TestEmitMetricsExemplars_PlainWindowedInnerAccepted(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)
	m := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpRate,
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}
	rw := &chplan.RangeWindow{
		Input:           m,
		Step:            time.Minute,
		Range:           time.Minute,
		Start:           start,
		End:             end,
		TimestampColumn: "Timestamp",
	}
	if _, _, err := chsql.EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1, "otel_traces"); err != nil {
		t.Fatalf("windowed plain inner must emit cleanly, got %v", err)
	}
}
