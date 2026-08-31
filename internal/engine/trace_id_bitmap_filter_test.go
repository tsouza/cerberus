package engine

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// traceIDBitmapFilterRules returns SettingsRules with TraceIDBitmapFilter ON
// and the default schema wired (so the eligibility check runs against a live
// TraceIDColumn), mirroring conditionCacheRules in condition_cache_test.go.
func traceIDBitmapFilterRules() SettingsRules {
	return SettingsRules{
		TraceIDBitmapFilter: true,
		Metrics:             schema.DefaultOTelMetrics(),
		Traces:              schema.DefaultOTelTraces(),
		Logs:                schema.DefaultOTelLogs(),
	}
}

// traceIDEqualityFilter builds Filter(TraceId = 'x') over Scan(otel_traces) —
// the Tempo trace-by-id GET handler's shape (internal/api/tempo/handler.go).
func traceIDEqualityFilter() chplan.Node {
	return &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_traces"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "TraceId"},
			Right: &chplan.LitString{V: "0123456789abcdef0123456789abcdef"},
		},
	}
}

// TestApply_StampsTraceIDBitmapFilter_OnEquality confirms that with the rule
// on (which only resolves in on server >= 25.11) and a top-level TraceId
// equality predicate, apply stamps min_table_rows_to_use_projection_index=0.
func TestApply_StampsTraceIDBitmapFilter_OnEquality(t *testing.T) {
	ctx := traceIDBitmapFilterRules().apply(context.Background(), traceIDEqualityFilter())
	if got := settingValue(ctx, settingMinTableRowsToUseProjectionIndex); got != traceIDBitmapFilterMinTableRows {
		t.Errorf("TraceIDBitmapFilter on + TraceId equality: setting = %v; want %v", got, traceIDBitmapFilterMinTableRows)
	}
}

// TestApply_TraceIDBitmapFilter_OffByDefault confirms the rule is DARK when
// the feature did not resolve in (e.g. server < 25.11): nothing is stamped
// even though the plan shape is eligible.
func TestApply_TraceIDBitmapFilter_OffByDefault(t *testing.T) {
	off := SettingsRules{Traces: schema.DefaultOTelTraces()}.apply(context.Background(), traceIDEqualityFilter())
	if got := settingValue(off, settingMinTableRowsToUseProjectionIndex); got != nil {
		t.Errorf("TraceIDBitmapFilter off (server < 25.11): setting = %v; want absent", got)
	}
}

// TestApply_TraceIDBitmapFilter_NotStampedWithoutTraceIDPredicate confirms
// the conservative gate: a plan touching neither TraceId nor a structural
// join gains nothing from the setting, so it is not stamped even with the
// rule on.
func TestApply_TraceIDBitmapFilter_NotStampedWithoutTraceIDPredicate(t *testing.T) {
	plan := &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_traces"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "SpanName"},
			Right: &chplan.LitString{V: "GET /home"},
		},
	}
	ctx := traceIDBitmapFilterRules().apply(context.Background(), plan)
	if got := settingValue(ctx, settingMinTableRowsToUseProjectionIndex); got != nil {
		t.Errorf("no TraceId predicate: setting = %v; want absent (conservative gate)", got)
	}
}

// TestEligibleForTraceIDBitmapFilter covers the plan-shape gate directly
// against cerberus's real emitted shapes (cerberus issue #2767), not a
// synthetic top-level equality alone: a flat IN list (the /api/search
// root-lookup), an IN-subquery, the structure-tab's BoundedTraceScope gate,
// and a StructuralJoin's recursive closure — none of which chplan.Walk alone
// would reach for the subquery-bearing cases (see eligibleForTraceIDBitmapFilter's
// own doc comment).
func TestEligibleForTraceIDBitmapFilter(t *testing.T) {
	rules := traceIDBitmapFilterRules()

	tests := []struct {
		name string
		plan chplan.Node
		want bool
	}{
		{"top-level equality", traceIDEqualityFilter(), true},
		{
			"flat IN list (/api/search root-lookup shape)",
			&chplan.Filter{
				Input: &chplan.Scan{Table: "otel_traces"},
				Predicate: &chplan.InList{
					Left: &chplan.ColumnRef{Name: "TraceId"},
					List: []chplan.Expr{&chplan.LitString{V: "aa"}, &chplan.LitString{V: "bb"}},
				},
			},
			true,
		},
		{
			"IN-subquery",
			&chplan.Filter{
				Input: &chplan.Scan{Table: "otel_traces"},
				Predicate: &chplan.InSubquery{
					Left:     &chplan.ColumnRef{Name: "TraceId"},
					Subquery: &chplan.Scan{Table: "otel_traces"},
				},
			},
			true,
		},
		{
			"BoundedTraceScope (structure-tab top-N gate)",
			&chplan.Filter{
				Input: &chplan.Scan{Table: "otel_traces"},
				Predicate: &chplan.BoundedTraceScope{
					SpansTable:         "otel_traces",
					TraceIDColumn:      "TraceId",
					ParentSpanIDColumn: "ParentSpanId",
					TraceLimit:         20,
				},
			},
			true,
		},
		{
			"StructuralJoin (recursive closure, no Expr-typed TraceId field)",
			&chplan.StructuralJoin{
				Left:               &chplan.Scan{Table: "otel_traces"},
				Right:              &chplan.Scan{Table: "otel_traces"},
				Op:                 chplan.StructuralDescendant,
				TraceIDColumn:      "TraceId",
				SpanIDColumn:       "SpanId",
				ParentSpanIDColumn: "ParentSpanId",
			},
			true,
		},
		{"bare scan, no predicate", &chplan.Scan{Table: "otel_traces"}, false},
		{
			"predicate on an unrelated column",
			&chplan.Filter{
				Input: &chplan.Scan{Table: "otel_traces"},
				Predicate: &chplan.Binary{
					Op:    chplan.OpEq,
					Left:  &chplan.ColumnRef{Name: "SpanName"},
					Right: &chplan.LitString{V: "GET /home"},
				},
			},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rules.eligibleForTraceIDBitmapFilter(tt.plan); got != tt.want {
				t.Errorf("eligibleForTraceIDBitmapFilter() = %v, want %v", got, tt.want)
			}
		})
	}
}
