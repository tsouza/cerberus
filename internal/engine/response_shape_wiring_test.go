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

// errShapeCaptureSentinel is the fixed error shapeCapturingCursorClient
// returns from QueryCursor. It never opens a real cursor — the test only
// needs to observe the ctx dispatchRouteACursor built before the open
// attempt, not a working Cursor.
var errShapeCaptureSentinel = errors.New("shapeCapturingCursorClient: sentinel open error")

// shapeCapturingCursorClient implements engine.Querier (required by the
// Engine.Client field type) plus engine.CursorQuerier (the optional
// interface dispatchRouteACursor type-asserts for). Query is never exercised
// by these tests — only QueryCursor, which records the ctx it was handed so
// a test can read back whatever chclient.WithResponseShape stamped onto it.
type shapeCapturingCursorClient struct {
	gotCtx context.Context
}

func (c *shapeCapturingCursorClient) Query(context.Context, string, ...any) ([]chclient.Sample, error) {
	return nil, errors.New("shapeCapturingCursorClient: Query not exercised by these tests")
}

func (c *shapeCapturingCursorClient) QueryCursor(ctx context.Context, _ string, _ ...any) (chclient.Cursor, error) {
	c.gotCtx = ctx
	return nil, errShapeCaptureSentinel
}

// TestDispatchRouteACursor_ThreadsResponseShape is the end-to-end wiring pin
// for #1429: it proves engine.Meta.ResponseShape returned by Lang.Parse
// actually reaches the ctx chclient's CursorQuerier receives via
// dispatchRouteACursor's chclient.WithResponseShape call — not merely that
// the two endpoints (lang.Parse setting Meta.ResponseShape, and
// columnarCursor.bindColumns reading it back) each behave correctly in
// isolation. Uses the plain nil-Solver / nil-RouteMemo Engine so
// QueryCursor dispatches straight through dispatchRouteACursor, the exact
// call site #1429 threads through.
func TestDispatchRouteACursor_ThreadsResponseShape(t *testing.T) {
	t.Parallel()

	cq := &shapeCapturingCursorClient{}
	eng := &engine.Engine{Optimizer: optimizer.Default(), Client: cq}
	lang := &fakeLang{
		name: "promql",
		parseFn: func(context.Context, string) (chplan.Node, engine.Meta, error) {
			return &chplan.Scan{Table: "otel_metrics_gauge"},
				engine.Meta{IsMetric: true, ResponseShape: chclient.ResponseShapeMatrix},
				nil
		},
	}

	_, err := eng.QueryCursor(context.Background(), lang, "up")
	if err == nil {
		t.Fatal("QueryCursor: expected the sentinel open error to surface, got nil")
	}
	if !errors.Is(err, errShapeCaptureSentinel) {
		t.Fatalf("QueryCursor err = %v, want it to wrap errShapeCaptureSentinel", err)
	}
	if cq.gotCtx == nil {
		t.Fatal("QueryCursor: cursor client's QueryCursor was never invoked")
	}
	if got := chclient.ResponseShapeFromContext(cq.gotCtx); got != chclient.ResponseShapeMatrix {
		t.Errorf("ResponseShapeFromContext(execCtx) = %q, want %q (Meta.ResponseShape never reached the CursorQuerier's ctx)",
			got, chclient.ResponseShapeMatrix)
	}
}

// TestDispatchRouteACursor_UnsetResponseShapeStaysEmpty is the negative
// pairing: a Lang that hasn't opted into declaring ResponseShape (the
// zero-value Meta) must thread an EMPTY string, not a stale or default
// value — chclient's gate treats "" as "unknown, defer to the structural
// check alone" (#1429's incremental-adoption safety valve), so this must
// stay observably empty for any not-yet-migrated caller.
func TestDispatchRouteACursor_UnsetResponseShapeStaysEmpty(t *testing.T) {
	t.Parallel()

	cq := &shapeCapturingCursorClient{}
	eng := &engine.Engine{Optimizer: optimizer.Default(), Client: cq}
	lang := &fakeLang{
		name: "promql",
		parseFn: func(context.Context, string) (chplan.Node, engine.Meta, error) {
			return &chplan.Scan{Table: "otel_metrics_gauge"}, engine.Meta{IsMetric: true}, nil
		},
	}

	_, err := eng.QueryCursor(context.Background(), lang, "up")
	if err == nil {
		t.Fatal("QueryCursor: expected the sentinel open error to surface, got nil")
	}
	if cq.gotCtx == nil {
		t.Fatal("QueryCursor: cursor client's QueryCursor was never invoked")
	}
	if got := chclient.ResponseShapeFromContext(cq.gotCtx); got != "" {
		t.Errorf("ResponseShapeFromContext(execCtx) = %q, want \"\" for a Lang that never set Meta.ResponseShape", got)
	}
}
