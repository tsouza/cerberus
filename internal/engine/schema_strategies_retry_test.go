package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/solver"
)

// secondsStrategiesLang is a PromQL-named Lang whose statements render for
// second-precision metrics timestamps.
type secondsStrategiesLang struct{ routedCorpusLang }

func (secondsStrategiesLang) EmitSchemaStrategies() chsql.SchemaStrategies {
	return chsql.SchemaStrategies{MetricsTimestamp: schema.MetricsTimestampSeconds}
}

// strategiesProbePlan reads a metrics timestamp column, so its SQL differs
// between the two precisions.
func strategiesProbePlan() chplan.Node {
	return &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_sum"},
		Projections: []chplan.Projection{{
			Expr:  &chplan.FuncCall{Fn: chplan.FnToUnixMillis, Args: []chplan.Expr{&chplan.ColumnRef{Name: "TimeUnix"}}},
			Alias: "ts",
		}},
	}
}

// strategiesRecordingClient is a route-A CursorQuerier that renders
// strategiesProbePlan under the ctx each dispatch opens with, recording the
// SQL so a test can read which strategies that ctx carried.
type strategiesRecordingClient struct {
	mu       sync.Mutex
	probeSQL []string
}

func (c *strategiesRecordingClient) Query(context.Context, string, ...any) ([]chclient.Sample, error) {
	return nil, nil
}

func (c *strategiesRecordingClient) QueryCursor(ctx context.Context, _ string, _ ...any) (chclient.Cursor, error) {
	sql, _, err := chsql.Emit(ctx, strategiesProbePlan())
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.probeSQL = append(c.probeSQL, sql)
	c.mu.Unlock()
	return &fakeMemoWiringCursor{}, nil
}

// TestQueryPlanCursor_MemoHitRetryRendersUnderTheLangsStrategies pins that
// the route-A fallback a memo-hit's drain-retry dispatches renders under
// the Lang's SchemaStrategies, although the retry runs under the drain's
// ctx rather than the one the request entered QueryPlanCursor with.
func TestQueryPlanCursor_MemoHitRetryRendersUnderTheLangsStrategies(t *testing.T) {
	t.Parallel()
	shards := &fakeSolverCursorClient{err: chclient.ErrMemoryLimitExceeded}
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto
	cfg.MinFanout = solverMinFanoutNeverRouted
	if err := cfg.Validate(); err != nil {
		t.Fatalf("solver config invalid: %v", err)
	}
	memo := routememo.New(time.Minute)
	routeA := &strategiesRecordingClient{}
	eng := &Engine{
		Solver:    solver.New(cfg, fakeSolverEmitter{}, solver.ExecDeps{Client: shards}),
		RouteMemo: memo,
		Client:    routeA,
		Optimizer: optimizer.New(),
	}

	plan := memoWiringClosedPlan()
	seed := memoWiringNotRoutedDecision(t)
	d := eng.deriveRouteMemoDispatch(plan, seed, memoWiringGridEnd.Add(2*memoWiringGridStep))
	memo.Observe(d.key, routememo.RouteB, routememo.OutcomeSuccess)

	result, err := eng.QueryPlanCursor(context.Background(), secondsStrategiesLang{}, plan, Meta{IsMetric: true})
	if err != nil {
		t.Fatalf("QueryPlanCursor: %v", err)
	}
	if result.Retry == nil {
		t.Fatal("the memo-hit dispatch carries no drain-retry closure")
	}
	for result.Cursor.Next() {
	}
	drainErr := result.Cursor.Err()
	if drainErr == nil {
		t.Fatal("fixture must produce a drain error (every shard open fails)")
	}
	if _, retried := result.Retry(context.Background(), drainErr); !retried {
		t.Fatal("the memo-hit drain-retry declined the route-A fallback")
	}

	routeA.mu.Lock()
	defer routeA.mu.Unlock()
	if len(routeA.probeSQL) != 1 {
		t.Fatalf("route-A dispatches = %d, want exactly the retry's one", len(routeA.probeSQL))
	}
	if !strings.Contains(routeA.probeSQL[0], "toDateTime64(") {
		t.Errorf("the retry's route-A dispatch renders for nanosecond timestamps, not the Lang's seconds precision:\n%s", routeA.probeSQL[0])
	}
}

// solverMinFanoutNeverRouted is a MinFanout no fixture plan reaches, so the
// Solver never routes a request on its own and only the route memo does.
const solverMinFanoutNeverRouted = 1_000_000

// memoWiringClosedPlan is memoWiringEligiblePlan over an input with a
// closed schema, which route A's emit needs.
func memoWiringClosedPlan() chplan.Node {
	rw := memoWiringEligiblePlan().(*chplan.RangeWindow)
	rw.Input = &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_sum"},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Attributes"}, Alias: "Attributes"},
			{Expr: &chplan.ColumnRef{Name: "TimeUnix"}, Alias: "TimeUnix"},
			{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"},
		},
		Roles: []chplan.Column{{Name: "TimeUnix", Role: chplan.RoleTimestamp}},
	}
	return rw
}

// memoryLimitCursor streams no rows and then reports a memory-limit
// failure, the drain failure route A's A->B retry answers.
type memoryLimitCursor struct{ fakeMemoWiringCursor }

func (memoryLimitCursor) Err() error { return chclient.ErrMemoryLimitExceeded }

// memoryLimitRouteAClient is a route-A CursorQuerier whose every cursor
// fails its drain with a memory-limit error.
type memoryLimitRouteAClient struct{ strategiesRecordingClient }

func (c *memoryLimitRouteAClient) QueryCursor(context.Context, string, ...any) (chclient.Cursor, error) {
	return &memoryLimitCursor{}, nil
}

// strategiesRecordingEmitter is a route-B emitter that renders
// strategiesProbePlan under the ctx each shard emits with, recording the SQL.
type strategiesRecordingEmitter struct{ client *strategiesRecordingClient }

func (e strategiesRecordingEmitter) Emit(ctx context.Context, _ chplan.Node) (string, []any, int, error) {
	if _, err := e.client.QueryCursor(ctx, ""); err != nil {
		return "", nil, 0, err
	}
	return "SELECT 1", nil, 1, nil
}

// TestQueryPlanCursor_RouteARetryRendersUnderTheLangsStrategies pins that
// the route-B shards route A's drain-retry dispatches render under the
// Lang's SchemaStrategies, although the retry runs under the drain's ctx
// rather than the one the request entered QueryPlanCursor with.
func TestQueryPlanCursor_RouteARetryRendersUnderTheLangsStrategies(t *testing.T) {
	t.Parallel()
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto
	cfg.MinFanout = solverMinFanoutNeverRouted
	if err := cfg.Validate(); err != nil {
		t.Fatalf("solver config invalid: %v", err)
	}
	shardSQL := &strategiesRecordingClient{}
	eng := &Engine{
		Solver:    solver.New(cfg, strategiesRecordingEmitter{client: shardSQL}, solver.ExecDeps{Client: &fakeSolverCursorClient{rows: 1}}),
		RouteMemo: routememo.New(time.Minute),
		Client:    &memoryLimitRouteAClient{},
		Optimizer: optimizer.New(),
	}

	// Route memo corroboration: the first route-A memory-limit failure is
	// recorded without a retry, the second one retries on route B.
	retried := false
	for attempt := 0; attempt < routememo.MinCorroboratingFailures && !retried; attempt++ {
		result, err := eng.QueryPlanCursor(context.Background(), secondsStrategiesLang{}, memoWiringClosedPlan(), Meta{IsMetric: true})
		if err != nil {
			t.Fatalf("QueryPlanCursor: %v", err)
		}
		if result.Retry == nil {
			t.Fatal("the route-A dispatch carries no drain-retry closure")
		}
		for result.Cursor.Next() {
		}
		_, retried = result.Retry(context.Background(), result.Cursor.Err())
	}
	if !retried {
		t.Fatalf("no route-B retry after %d corroborating route-A failures", routememo.MinCorroboratingFailures)
	}

	shardSQL.mu.Lock()
	defer shardSQL.mu.Unlock()
	if len(shardSQL.probeSQL) == 0 {
		t.Fatal("the route-B retry emitted no shard")
	}
	for _, sql := range shardSQL.probeSQL {
		if !strings.Contains(sql, "toDateTime64(") {
			t.Fatalf("a route-B retry shard renders for nanosecond timestamps, not the Lang's seconds precision:\n%s", sql)
		}
	}
}
