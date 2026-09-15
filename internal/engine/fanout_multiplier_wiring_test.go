package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// TestDispatchRouteACursor_StampsPhysicalScanMultiplier is the end-to-end
// wiring pin for the data-shard fan-out weight (cerberus issue #3128): the
// physical-scan count chsql.EmitCounted returns for the EMITTED statement
// must reach the ctx chclient's CursorQuerier receives, as the
// WithDataShardFanoutMultiplier stamp. A SearchTraceLimit plan renders its
// input twice, so the expected value is 2 — a plan-independent default of 1
// (or no stamp at all) fails, which is what makes this test non-vacuous.
func TestDispatchRouteACursor_StampsPhysicalScanMultiplier(t *testing.T) {
	t.Parallel()

	cq := &shapeCapturingCursorClient{}
	eng := &engine.Engine{Optimizer: optimizer.Default(), Client: cq}
	lang := &fakeLang{
		name: "tempo",
		parseFn: func(context.Context, string) (chplan.Node, engine.Meta, error) {
			return &chplan.SearchTraceLimit{
				Input:      &chplan.Scan{Table: "otel_traces", Columns: []string{"TraceId", "Timestamp"}, Roles: []chplan.Column{{Name: "TraceId", Role: chplan.RoleTraceID}, {Name: "Timestamp", Role: chplan.RoleTimestamp}}},
				TraceLimit: 20,
			}, engine.Meta{}, nil
		},
	}

	_, err := eng.QueryCursor(context.Background(), lang, "{ }")
	if err == nil {
		t.Fatal("QueryCursor: expected the sentinel open error to surface, got nil")
	}
	if !errors.Is(err, errShapeCaptureSentinel) {
		t.Fatalf("QueryCursor err = %v, want it to wrap errShapeCaptureSentinel", err)
	}
	if cq.gotCtx == nil {
		t.Fatal("QueryCursor: cursor client's QueryCursor was never invoked")
	}
	got, ok := chclient.DataShardFanoutMultiplierFromContext(cq.gotCtx)
	if !ok {
		t.Fatal("dispatch ctx carries no data-shard fan-out multiplier — the emitted statement's scan count never reached the CursorQuerier")
	}
	if got != 2 {
		t.Fatalf("data-shard fan-out multiplier = %d, want 2 (SearchTraceLimit renders its input on both arms)", got)
	}
}
