package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/solver"
)

// routeBMismatchedResponseShape is a deliberately NON-matrix declaration —
// the Loki log-stream pivot key an adapter really does set
// (internal/logql/lang.go). chclient's bindColumns declines the columnar
// matrix decode for exactly this input, so threading it verbatim to the
// shards is what gives route B a gate that can actually DECLINE rather than
// one that only ever waves matrix queries through.
const routeBMismatchedResponseShape = "loki-streams"

// shapeCapturingSolverClient is a solver.CursorQuerier that records the
// ResponseShape each shard's dispatch ctx carried at the moment
// internal/solver opened that shard's cursor — the exact value
// chclient.columnarCursor reads to decide whether the columnar matrix decode
// may engage.
type shapeCapturingSolverClient struct {
	rows int

	mu     sync.Mutex
	shapes []string
}

func (c *shapeCapturingSolverClient) QueryCursor(ctx context.Context, _ string, _ ...any) (chclient.Cursor, error) {
	c.mu.Lock()
	c.shapes = append(c.shapes, chclient.ResponseShapeFromContext(ctx))
	c.mu.Unlock()
	return &fakeMemoWiringCursor{remaining: c.rows}, nil
}

func (c *shapeCapturingSolverClient) MaxQueryMemoryBytes() int64 { return 0 }

func (c *shapeCapturingSolverClient) observed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.shapes...)
}

// assertEveryShardDeclared pins that the fan-out really happened (wantShards
// cursors opened — an assertion over an empty slice would pass vacuously) and
// that EVERY shard presented want to its CursorQuerier.
func assertEveryShardDeclared(t *testing.T, c *shapeCapturingSolverClient, wantShards int, want string) {
	t.Helper()
	got := c.observed()
	if len(got) != wantShards {
		t.Fatalf("shard cursor opens = %d, want %d — the dispatch did not fan out, so the shape assertion would be vacuous",
			len(got), wantShards)
	}
	for shard, shape := range got {
		if shape != want {
			t.Errorf("shard %d: ResponseShapeFromContext(dispatch ctx) = %q, want %q — Meta.ResponseShape never reached this route-B shard",
				shard, shape, want)
		}
	}
}

// routeBDispatch names one of the FOUR call sites that hand a ctx to
// solver.Executor.Execute. Each builds its own Engine around the capturing
// client, drives its own dispatch to a complete drain (so every shard has
// certainly opened), and reports how many shards it fanned out to.
type routeBDispatch struct {
	name     string
	dispatch func(t *testing.T, cq *shapeCapturingSolverClient, meta Meta) int
}

func routeBDispatches() []routeBDispatch {
	return []routeBDispatch{
		{
			// The eager route-B path: QueryPlan's fan-out, drained into a
			// Result by the engine itself.
			name: "executeRouted",
			dispatch: func(t *testing.T, cq *shapeCapturingSolverClient, meta Meta) int {
				t.Helper()
				eng := newRoutedCorpusEngine(t, cq, nil)
				plan := memoWiringEligiblePlan()
				d := routedDecision(t, eng, plan)
				if _, err := eng.executeRouted(context.Background(), routedCorpusLang{}, meta, plan, d); err != nil {
					t.Fatalf("executeRouted: %v", err)
				}
				return len(d.Slices)
			},
		},
		{
			// The streaming sibling: the caller owns the drain.
			name: "executeRoutedCursor",
			dispatch: func(t *testing.T, cq *shapeCapturingSolverClient, meta Meta) int {
				t.Helper()
				eng := newRoutedCorpusEngine(t, cq, nil)
				plan := memoWiringEligiblePlan()
				d := routedDecision(t, eng, plan)
				cr, err := eng.executeRoutedCursor(context.Background(), routedCorpusLang{}, meta, plan, d)
				if err != nil {
					t.Fatalf("executeRoutedCursor: %v", err)
				}
				drainRouteBCursor(t, cr.Cursor)
				return len(d.Slices)
			},
		},
		{
			// The failure-driven route memo's hit path: a live PreferB
			// verdict routes B without route A running at all.
			name: "tryRouteMemoHit",
			dispatch: func(t *testing.T, cq *shapeCapturingSolverClient, meta Meta) int {
				t.Helper()
				eng, memo := newMemoWiringEngine(t, cq)
				plan := memoWiringEligiblePlan()
				seed := memoWiringNotRoutedDecision(t)
				d := eng.deriveRouteMemoDispatch(plan, seed, memoWiringGridEnd.Add(2*memoWiringGridStep))
				if !d.eligible {
					t.Fatalf("fixture plan must be structurally eligible")
				}
				memo.Observe(d.key, routememo.RouteB, routememo.OutcomeSuccess)

				cur, _, usedDecision, _, ok := eng.tryRouteMemoHit(
					context.Background(), solver.LangPromQL, meta.ResponseShape, plan, seed, nil, nil,
				)
				if !ok {
					t.Fatal("tryRouteMemoHit did not dispatch on a live PreferB verdict")
				}
				drainRouteBCursor(t, cur)
				return len(usedDecision.Slices)
			},
		},
		{
			// The A->B retry: route A failed on resource exhaustion, so this
			// probe dispatches B instead. It runs on the ctx the CALLER hands
			// in at retry time, which is why the shape has to be re-declared
			// from Meta rather than inherited from the original dispatch.
			name: "retryOnRouteAResourceFailure",
			dispatch: func(t *testing.T, cq *shapeCapturingSolverClient, meta Meta) int {
				t.Helper()
				eng, _ := newMemoWiringEngine(t, cq)
				plan := memoWiringEligiblePlan()
				seed := memoWiringNotRoutedDecision(t)

				// A single route-A resource failure never earns a probe: the
				// memo demands minCorroboratingFailures consecutive failures
				// on the same key first, so the dispatch under test is the
				// SECOND call (see
				// TestRetryOnRouteAResourceFailure_ProbesAfterCorroboration).
				if _, _, _, _, retried := eng.retryOnRouteAResourceFailure(
					context.Background(), solver.LangPromQL, meta.ResponseShape, plan, seed, nil,
					chclient.ErrMemoryLimitExceeded, 0, nil,
				); retried {
					t.Fatal("probed on the FIRST route-A resource failure — fixture no longer matches the memo's corroboration rule")
				}

				cur, _, usedDecision, observeFn, retried := eng.retryOnRouteAResourceFailure(
					context.Background(), solver.LangPromQL, meta.ResponseShape, plan, seed, nil,
					chclient.ErrMemoryLimitExceeded, 0, nil,
				)
				if !retried {
					t.Fatal("retryOnRouteAResourceFailure did not probe route B after a resource failure")
				}
				drainRouteBCursor(t, cur)
				observeFn(nil)
				return len(usedDecision.Slices)
			},
		},
	}
}

// drainRouteBCursor drains and closes a composed route-B cursor, failing on
// any drain error — a dispatch that aborted mid-drain would leave some shards
// unopened and make the per-shard assertions vacuous.
func drainRouteBCursor(t *testing.T, cur chclient.Cursor) {
	t.Helper()
	for cur.Next() {
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := cur.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestRouteBDispatch_ThreadsResponseShapeToEveryShard is the route-B half of
// #1429's defense-in-depth, which #1429 itself wired only into route A's
// dispatchRouteACursor: internal/solver owns a structurally separate
// CursorQuerier, and a cursor opened through it used to present
// ResponseShapeFromContext(ctx) == "" — the "unknown, defer to the structural
// test alone" safety valve — no matter what the adapter declared (#1753).
//
// It runs over ALL FOUR sites that dispatch through solver.Executor.Execute,
// because "route B" is not one call site: a memo hit and an A->B retry open
// route-B cursors on ctxs that never passed through executeRouted(Cursor) at
// all. Adding a fifth dispatch without stamping the shape is what this table
// exists to catch.
func TestRouteBDispatch_ThreadsResponseShapeToEveryShard(t *testing.T) {
	t.Parallel()

	for _, tc := range routeBDispatches() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cq := &shapeCapturingSolverClient{rows: 1}
			shards := tc.dispatch(t, cq, Meta{IsMetric: true, ResponseShape: chclient.ResponseShapeMatrix})
			assertEveryShardDeclared(t, cq, shards, chclient.ResponseShapeMatrix)
		})
	}
}

// TestRouteBDispatch_MismatchedResponseShapeReachesEveryShard is what proves
// the AND-gate can actually FIRE on route B rather than merely riding along:
// a declaration that does NOT match the matrix shape is the single input
// chclient.bindColumns tests to REFUSE the columnar decode
// (chclient/columnar_shape_test.go pins that half). Were the value dropped
// anywhere between Meta and the shard's QueryCursor, the shards would present
// "" and the gate would fall back to the structural name/type check alone —
// passing a matrix-shaped-by-coincidence projection straight through, the
// exact collision class #1429 exists to close.
func TestRouteBDispatch_MismatchedResponseShapeReachesEveryShard(t *testing.T) {
	t.Parallel()

	for _, tc := range routeBDispatches() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cq := &shapeCapturingSolverClient{rows: 1}
			shards := tc.dispatch(t, cq, Meta{IsMetric: true, ResponseShape: routeBMismatchedResponseShape})
			assertEveryShardDeclared(t, cq, shards, routeBMismatchedResponseShape)
		})
	}
}

// TestRouteBDispatch_UnsetResponseShapeStaysEmpty is the negative pairing to
// both tests above, mirroring route A's own pin: an adapter that never
// declared a shape must leave every shard ctx observably EMPTY. "" is
// chclient's incremental-adoption valve, so inventing a value here would
// silently change which projections the columnar decode accepts for every
// not-yet-migrated caller.
func TestRouteBDispatch_UnsetResponseShapeStaysEmpty(t *testing.T) {
	t.Parallel()

	for _, tc := range routeBDispatches() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cq := &shapeCapturingSolverClient{rows: 1}
			shards := tc.dispatch(t, cq, Meta{IsMetric: true})
			assertEveryShardDeclared(t, cq, shards, "")
		})
	}
}
