package engine

import (
	"context"
	"regexp"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/solver"
)

// fanoutGuardTestPlan is a minimal RangeBucketFanout over a bare Scan — the
// shape whose emitted SQL carries the RangeBucketFanoutMaxRows guard as a
// `LIMIT <ceiling+1>` literal (chsql's lwrFanoutBoundedSourceFrag), which
// is how the tests below read the ceiling a ctx actually threads.
func fanoutGuardTestPlan() *chplan.RangeBucketFanout {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &chplan.RangeBucketFanout{
		Input: &chplan.Scan{
			Table:   "otel_metrics_gauge",
			Columns: []string{"TimeUnix", "Attributes", "Value"},
			Roles: []chplan.Column{
				{Name: "TimeUnix", Role: chplan.RoleTimestamp},
				{Name: "Attributes"},
				{Name: "Value"},
			},
		},
		Start:        start,
		End:          start.Add(5 * time.Minute),
		Step:         30 * time.Second,
		Lookback:     5 * time.Minute,
		GroupBy:      []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		AnchorAlias:  "anchor_ts",
		TimestampCol: "TimeUnix",
		AggFuncs: []chplan.AggFunc{{
			Fn:    chplan.FnArgMax,
			Alias: "Value",
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: "Value"}, &chplan.ColumnRef{Name: "TimeUnix"}},
		}},
	}
}

var fanoutLimitLiteral = regexp.MustCompile(`LIMIT (\d+)`)

// emittedFanoutCeiling emits fanoutGuardTestPlan under ctx and returns the
// RangeBucketFanoutMaxRows ceiling the SQL carries. Every LIMIT literal in
// the statement is the same guard (rendered once per read of the fan-out),
// so they must all agree.
func emittedFanoutCeiling(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	sql, _, err := chsql.Emit(ctx, fanoutGuardTestPlan())
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	matches := fanoutLimitLiteral.FindAllStringSubmatch(sql, -1)
	if len(matches) == 0 {
		t.Fatalf("emitted SQL carries no LIMIT literal; the fan-out guard is missing\nSQL:\n%s", sql)
	}
	ceiling := int64(-1)
	for _, m := range matches {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			t.Fatalf("parse %q: %v", m[1], err)
		}
		if ceiling >= 0 && n-1 != ceiling {
			t.Fatalf("emitted SQL carries two different fan-out ceilings (%d and %d)\nSQL:\n%s", ceiling, n-1, sql)
		}
		ceiling = n - 1
	}
	return ceiling
}

// TestRouteB_FanoutGuardVerdictMatchesShardMemoryShare is the class test for
// the un-apportioned route-B ceiling: for every (K, Parallel, D, rows) the
// verdict a shard's fan-out guard reaches must equal "the shard's rows fit
// in the shard's memory share", and route A's verdict must be unchanged.
//
// The model is the one the guards are calibrated on: a whole-query ceiling
// R admits rows <= R under the whole cap, so a shard running under
// cap/(kEff x D) — kEff = min(K, Parallel) here, no gate — fits its rows/K
// exactly when rows/K <= R/(kEff x D). A shard guard that admits more than
// that hands ClickHouse a query it will abort on code 241; one that admits
// less empties the rescue window the A->B escalation exists for.
//
// The (Parallel=3, K=8) rows dominate the table because they are the
// shipped defaults: a route-A rejection there splits into shards of rows/8
// that a whole-query ceiling admits up to rows = 8R while the shard's
// memory holds only rows <= 2.67R — every escalation in (2.67R, 8R] used to
// pass the guard and die.
func TestRouteB_FanoutGuardVerdictMatchesShardMemoryShare(t *testing.T) {
	t.Parallel()

	// R is divisible by every K, kEff and D below, so rows/K and
	// R/(kEff x D) are exact and the verdicts compare without rounding.
	const wholeQueryCeiling = int64(2_400_000)

	type shape struct {
		k, parallel, dataShards int
	}
	shapes := []shape{
		{k: 8, parallel: 3, dataShards: 1}, // the shipped defaults
		{k: 8, parallel: 3, dataShards: 2},
		{k: 8, parallel: 8, dataShards: 1}, // kEff == K: no rescue window
		{k: 4, parallel: 3, dataShards: 1},
		{k: 2, parallel: 3, dataShards: 1}, // K below Parallel: kEff == K
		{k: 6, parallel: 2, dataShards: 3},
	}
	// rows as multiples of R/24, sweeping well past every window edge
	// (2.67R for the defaults, 8R for the largest K).
	var rowsSweep []int64
	for i := int64(1); i <= 24*9; i++ {
		rowsSweep = append(rowsSweep, i*wholeQueryCeiling/24)
	}

	for _, sh := range shapes {
		e := &Engine{
			RangeBucketFanoutMaxRows: wholeQueryCeiling,
			Solver: &solver.Solver{Executor: &solver.Executor{
				Cfg: solver.Config{Parallel: sh.parallel, DataShardCount: sh.dataShards},
			}},
		}
		decision := &solver.Decision{K: sh.k}
		kEff := min(sh.k, sh.parallel)
		shardShare := wholeQueryCeiling / int64(kEff*sh.dataShards)

		routeACtx := applyResourceBoundOverrides(context.Background(), e.resourceBoundOverrides())
		if got := emittedFanoutCeiling(t, routeACtx); got != wholeQueryCeiling {
			t.Fatalf("K=%d P=%d D=%d: route A's ceiling = %d, want the whole-query ceiling %d unchanged",
				sh.k, sh.parallel, sh.dataShards, got, wholeQueryCeiling)
		}
		shardCtx := routeBExecCtx(context.Background(), "promql", chclient.ResponseShapeMatrix, decision,
			fanoutGuardTestPlan(), 0, SettingsRules{}, 0, false,
			e.resourceBoundOverrides(), e.shardMemoryDivisor(decision), 0, 0, nil, nil)
		shardCeiling := emittedFanoutCeiling(t, shardCtx)

		for _, rows := range rowsSweep {
			shardRows := rows / int64(sh.k)
			guardAdmits := shardRows <= shardCeiling
			fits := shardRows <= shardShare
			if guardAdmits != fits {
				t.Errorf("K=%d Parallel=%d D=%d rows=%.2fR: shard guard admits=%v (ceiling %d) but shard rows %d fit the shard's memory share %d = %v",
					sh.k, sh.parallel, sh.dataShards, float64(rows)/float64(wholeQueryCeiling),
					guardAdmits, shardCeiling, shardRows, shardShare, fits)
			}
		}
	}
}

// TestRouteB_FanoutGuardRescueWindow pins the window the escalation can
// keep at the shipped defaults, in the terms cerberus issue #3564 states
// it: with Parallel=3 and K=8 a route-A rejection at rows in (R, 2.67R]
// passes every shard's guard and fits every shard's memory, while rows in
// (2.67R, 8R] is refused pre-flight by the shard guard instead of admitted
// and aborted.
func TestRouteB_FanoutGuardRescueWindow(t *testing.T) {
	t.Parallel()

	const (
		wholeQueryCeiling = int64(2_400_000)
		k                 = 8
		parallel          = 3
	)
	e := &Engine{
		RangeBucketFanoutMaxRows: wholeQueryCeiling,
		Solver: &solver.Solver{Executor: &solver.Executor{
			Cfg: solver.Config{Parallel: parallel, DataShardCount: 1},
		}},
	}
	decision := &solver.Decision{K: k}
	shardCtx := routeBExecCtx(context.Background(), "promql", chclient.ResponseShapeMatrix, decision,
		fanoutGuardTestPlan(), 0, SettingsRules{}, 0, false,
		e.resourceBoundOverrides(), e.shardMemoryDivisor(decision), 0, 0, nil, nil)
	shardCeiling := emittedFanoutCeiling(t, shardCtx)
	if want := wholeQueryCeiling / parallel; shardCeiling != want {
		t.Fatalf("shard ceiling = %d, want R/min(K, Parallel) = %d/%d = %d", shardCeiling, wholeQueryCeiling, parallel, want)
	}

	rescued := k * wholeQueryCeiling / parallel // 2.67R: the top of the window
	for _, rows := range []int64{wholeQueryCeiling + 1, rescued} {
		if rows/k > shardCeiling {
			t.Errorf("rows=%d (%.2fR) is inside the rescue window but the shard guard refuses it", rows, float64(rows)/float64(wholeQueryCeiling))
		}
	}
	for _, rows := range []int64{rescued + k, 5 * wholeQueryCeiling, k * wholeQueryCeiling} {
		if rows/k <= shardCeiling {
			t.Errorf("rows=%d (%.2fR) exceeds every shard's memory share but the shard guard admits it — ClickHouse would abort it on code 241",
				rows, float64(rows)/float64(wholeQueryCeiling))
		}
	}
}

// TestRouteB_FanoutCeilingsUseTheGateHalf pins that the divisor routeBExecCtx
// applies is the Executor's own clamp, gate/2 included, not a re-derivation
// from Parallel alone: a gate small enough to clamp kEff below Parallel
// must widen the shard's ceiling accordingly, because that shard really
// does run under a larger share.
func TestRouteB_FanoutCeilingsUseTheGateHalf(t *testing.T) {
	t.Parallel()

	const (
		wholeQueryCeiling = int64(2_400_000)
		k                 = 8
		parallel          = 6
		gateCap           = int64(4) // gate/2 = 2 < Parallel
	)
	e := &Engine{
		RangeBucketFanoutMaxRows: wholeQueryCeiling,
		Solver: &solver.Solver{Executor: &solver.Executor{
			Cfg:     solver.Config{Parallel: parallel, DataShardCount: 1},
			Gate:    semaphore.NewWeighted(gateCap),
			GateCap: gateCap,
		}},
	}
	decision := &solver.Decision{K: k}
	shardCtx := routeBExecCtx(context.Background(), "promql", chclient.ResponseShapeMatrix, decision,
		fanoutGuardTestPlan(), 0, SettingsRules{}, 0, false,
		e.resourceBoundOverrides(), e.shardMemoryDivisor(decision), 0, 0, nil, nil)
	if got, want := emittedFanoutCeiling(t, shardCtx), wholeQueryCeiling/(gateCap/2); got != want {
		t.Errorf("shard ceiling = %d, want R/(gate/2) = %d", got, want)
	}
}

// TestApportionFanoutBounds pins the resolve-then-divide contract for all
// four fan-out ceilings and the pass-through of the two fields that are not
// memory ceilings: a zero override resolves to chsql's default (or, for the
// fold cost, to the cap-derived default) BEFORE dividing, every result
// floors at 1, and MaxEmittedSQLBytes / CHQueryMaxMemory are untouched.
func TestApportionFanoutBounds(t *testing.T) {
	t.Parallel()

	const gib = int64(1 << 30)
	explicit := ResourceBoundOverrides{
		RangeBucketFanoutMaxRows:          100,
		RangeLWRFanoutMaxRows:             200,
		RateWindowFanoutMaxRows:           300,
		RangeBucketFanoutFoldCostMaxUnits: 400,
		MaxEmittedSQLBytes:                500,
		CHQueryMaxMemory:                  gib,
	}
	got := apportionFanoutBounds(explicit, 4)
	want := ResourceBoundOverrides{
		RangeBucketFanoutMaxRows:          25,
		RangeLWRFanoutMaxRows:             50,
		RateWindowFanoutMaxRows:           75,
		RangeBucketFanoutFoldCostMaxUnits: 100,
		MaxEmittedSQLBytes:                500,
		CHQueryMaxMemory:                  gib,
	}
	if got != want {
		t.Errorf("explicit overrides / 4 = %+v, want %+v", got, want)
	}

	unset := apportionFanoutBounds(ResourceBoundOverrides{CHQueryMaxMemory: 2 * gib}, 5)
	wantUnset := ResourceBoundOverrides{
		RangeBucketFanoutMaxRows:          chsql.ResolveRangeBucketFanoutMaxRows(0) / 5,
		RangeLWRFanoutMaxRows:             chsql.ResolveRangeLWRFanoutMaxRows(0) / 5,
		RateWindowFanoutMaxRows:           chsql.ResolveRateWindowFanoutMaxRows(0) / 5,
		RangeBucketFanoutFoldCostMaxUnits: chsql.RangeBucketFanoutFoldCostUnitsForMemory(2*gib) / 5,
		CHQueryMaxMemory:                  2 * gib,
	}
	if unset != wantUnset {
		t.Errorf("unset overrides / 5 = %+v, want the resolved defaults divided: %+v", unset, wantUnset)
	}
	for _, f := range []int64{unset.RangeBucketFanoutMaxRows, unset.RangeLWRFanoutMaxRows, unset.RateWindowFanoutMaxRows, unset.RangeBucketFanoutFoldCostMaxUnits} {
		if f <= 0 {
			t.Fatalf("an unset override apportioned to %d — chsql would read that back as 'unset' and use the whole-query default", f)
		}
	}

	tiny := apportionFanoutBounds(ResourceBoundOverrides{RangeBucketFanoutMaxRows: 3}, 10)
	if tiny.RangeBucketFanoutMaxRows != 1 {
		t.Errorf("3 / 10 apportioned to %d, want the floor 1", tiny.RangeBucketFanoutMaxRows)
	}
}
