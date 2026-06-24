//go:build chdb

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/solver"
)

// shardedResult is the SHARDED-SOLVER dimension: the headline win of the
// sharded-pushdown solver (internal/solver, docs/solver.md). It
// measures the OOM-class range query — a high anchor-fan-out matrix shape
// (sum(rate(metric[Range])) @ Step over OuterRange) whose single-statement
// route A demands more than the 1 GiB per-query memory cap, while route B
// (the solver's K disjoint anchor-grid shards) keeps every shard under the
// cap.
//
// chDB does NOT enforce a memory cap (it coerces leniently and never OOMs),
// so this dimension does NOT claim a measured OOM. What it MEASURES is the
// deterministic DRIVER of per-statement memory: the expanded (sample, anchor)
// pair count the matrix arrayJoin fan-out materializes before the GROUP BY
// collapse. Route A expands the WHOLE anchor grid against every covered
// sample; each shard expands only its anchor sub-grid, so the per-shard pair
// count drops ~1/K. The modeled GiB columns derive from the measured pair
// counts via a single published calibration constant (the worked spike's
// 2.12 GiB at 100.4M pairs), and are clearly labelled MODELED, not measured —
// the honest framing the design itself uses.
type shardedResult struct {
	// Query is the OOM-class fixture (the upstream PromQL text).
	Query string
	// Range / Step / OuterRange describe the matrix grid.
	Range      time.Duration
	Step       time.Duration
	OuterRange time.Duration
	// N is the outer anchor count (OuterRange/Step + 1); F = Range/Step.
	N int64
	F int64
	// Series / ScanRows are the seeded cardinality and the scanned sample
	// count the fan-out expands.
	Series   int64
	ScanRows int64

	// RouteAPairs is the MEASURED expanded (sample, anchor) pair count for
	// the single route-A statement over the WHOLE anchor grid.
	RouteAPairs int64
	// K is the shard count the Planner chose under Mode=sharded.
	K int
	// MaxShardPairs / SumShardPairs are the MEASURED per-shard pair counts:
	// the max is the per-statement memory driver (every shard runs
	// concurrently-bounded but each is capped independently); the sum proves
	// the shards partition the route-A pairs with no double-count.
	MaxShardPairs int64
	SumShardPairs int64

	// ShardPairs is the per-shard measured pair count, oldest-first.
	ShardPairs []int64

	// CapBytes is the prod per-query memory cap modeled against
	// (CERBERUS_CH_QUERY_MAX_MEMORY default, 1 GiB).
	CapBytes int64
	// RouteAModeledBytes / MaxShardModeledBytes are the MODELED peak memory
	// (pairs × bytesPerPair), not measured — chDB exposes no peak-memory
	// metric and never enforces a cap.
	RouteAModeledBytes   int64
	MaxShardModeledBytes int64
}

// Calibration + cap constants, all from docs/solver.md §"Slicing geometry"
// + internal/config (the published, reproducible numbers this dimension
// models against — never fabricated).
const (
	// shardCapBytes is the prod per-query cap CERBERUS_CH_QUERY_MAX_MEMORY
	// defaults to: 1 GiB. Route A must EXCEED it; every shard must fit under.
	shardCapBytes int64 = 1 << 30 // 1 GiB = 1073741824 bytes

	// The worked-spike calibration: the k3d run (27277793810) that motivated
	// the 1 GiB cap measured 2.12 GiB of route-A demand at ≈100.4M expanded
	// (sample, anchor) pairs. bytesPerPair = 2.12 GiB / 100.4M ≈ 22.7 B/pair
	// is the single published constant the modeled-GiB columns derive from.
	// It is a CALIBRATION, not a measurement of THIS run — labelled MODELED
	// everywhere it surfaces in the doc.
	spikeDemandBytes float64 = 2.12 * float64(1<<30) // 2.12 GiB
	spikePairs       float64 = 100_400_000           // ≈100.4M
)

// bytesPerPair is the published per-pair memory cost the modeled-GiB columns
// scale the measured pair counts by.
func bytesPerPair() float64 { return spikeDemandBytes / spikePairs }

// modeledBytes scales a measured pair count by the calibration constant.
func modeledBytes(pairs int64) int64 {
	return int64(float64(pairs) * bytesPerPair())
}

// The OOM-class fixture grid. F = Range/Step = 20, N = OuterRange/Step + 1 =
// 241 — the dominant routed shape the design names (sum(rate(m[5m])) @ 15s
// over 1h). The seed below provisions enough series × fan-out that the
// route-A pair count clears the 1 GiB-modeled threshold while every shard
// stays under it — the headline win the dimension exists to show.
var (
	shardedRange      = 5 * time.Minute
	shardedStep       = 15 * time.Second
	shardedStart      = time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)
	shardedEnd        = shardedStart.Add(time.Hour)
	shardedOuterRange = shardedEnd.Sub(shardedStart)
	shardedQuery      = "sum(rate(bench_shard_total[5m]))"
)

const (
	// shardedSeries is the per-metric series count. Sized so the route-A
	// (sample, anchor) pair count CLEARS the 1 GiB-modeled threshold
	// (≈47.4M pairs ⇒ ≈1.0 GiB at the calibration constant) with margin —
	// route A must OOM for the contrast to hold. Each series contributes
	// ≈N×F/… ≈4818 pairs across the dense grid, so ≈13k series puts route A
	// at ≈1.3 GiB modeled (over the cap) while every K=8 shard's sub-grid
	// stays well under it. ≈3.4M seed rows — chDB-runnable in-process.
	shardedSeries = 13000
	// shardedSampleStride is the per-series sample spacing (15s — one sample
	// per step, so every window holds F samples and the fan-out is dense).
	shardedSampleStride = 15 // seconds
	// shardedSamplesPerSeries spans the full input window: the hour grid plus
	// the 5m lookback before Start, at one sample / 15s ⇒ 261 samples.
	shardedSamplesPerSeries = 261
)

// measureSharded seeds the OOM-class dataset, lowers + optimizes the fixture
// through the real route-A pipeline, force-routes it under Mode=sharded to get
// the K shards, and MEASURES the expanded (sample, anchor) pair count for
// route A (whole grid) and every shard (sub-grid) over chDB. It returns one
// shardedResult — the dimension's single headline row.
func measureSharded(s *session, iters int) (shardedResult, error) {
	_ = iters // pair counts are deterministic; no best-of-N wall needed here.

	if err := s.execAll(shardedSeed()); err != nil {
		return shardedResult{}, fmt.Errorf("seed: %w", err)
	}
	scanRows, err := s.scalarCount("SELECT * FROM bench_shard_sum")
	if err != nil {
		return shardedResult{}, fmt.Errorf("scan count: %w", err)
	}

	ctx := context.Background()

	// Route A: lower + optimize the fixture exactly as the engine does, then
	// classify it under Mode=sharded. The plan that reaches the Planner is the
	// post-optimize plan chsql.Emit serializes — the real route-A pipeline.
	plan, err := optimizedShardPlan(ctx, shardedQuery, shardedStart, shardedEnd, shardedStep)
	if err != nil {
		return shardedResult{}, fmt.Errorf("lower+optimize: %w", err)
	}

	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeSharded // K_min routing: every eligible plan routes.
	if err := cfg.Validate(); err != nil {
		return shardedResult{}, fmt.Errorf("sharded config invalid: %w", err)
	}
	pl := solver.NewPlanner(cfg)
	gs, ge, gstep := solver.GridOf(plan)
	dec, routed := pl.Plan(plan, solver.RequestMeta{
		Lang:  solver.LangPromQL,
		Start: gs,
		End:   ge,
		Step:  gstep,
	})
	if !routed {
		return shardedResult{}, fmt.Errorf("OOM-class fixture %q did NOT route under "+
			"Mode=sharded (reason=%q) — the dimension needs a routed plan to contrast",
			shardedQuery, dec.Reason)
	}
	if dec.K < 2 {
		return shardedResult{}, fmt.Errorf("fixture routed with K=%d, want K >= 2", dec.K)
	}

	// Confirm route A's emitted SQL is valid + runs on the seed (it must — the
	// shards run the same template). We don't need its rows here; the pair
	// count is what drives memory.
	if _, _, err := chsql.Emit(ctx, plan); err != nil {
		return shardedResult{}, fmt.Errorf("emit route A: %w", err)
	}

	// MEASURE route A pairs: the whole anchor grid expanded against the seed.
	routeAPairs, err := s.scalarCount(matrixPairCount(shardedStart, shardedEnd, shardedStep, shardedRange))
	if err != nil {
		return shardedResult{}, fmt.Errorf("route-A pair count: %w", err)
	}

	// MEASURE each shard's pairs over ITS sub-grid. The slices carry their own
	// Start/End (anchor-grid-aligned); the per-shard anchor grid is
	// [Slice.Start, Slice.End] at the request Step. Emitting each shard's SQL
	// also asserts the re-anchored plan serializes (the executor would run it).
	var shardPairs []int64
	var maxPairs, sumPairs int64
	for _, sl := range dec.Slices {
		if _, _, err := chsql.Emit(ctx, sl.Plan); err != nil {
			return shardedResult{}, fmt.Errorf("emit shard %d: %w", sl.Index, err)
		}
		p, err := s.scalarCount(matrixPairCount(sl.Start, sl.End, shardedStep, shardedRange))
		if err != nil {
			return shardedResult{}, fmt.Errorf("shard %d pair count: %w", sl.Index, err)
		}
		shardPairs = append(shardPairs, p)
		sumPairs += p
		if p > maxPairs {
			maxPairs = p
		}
	}

	n := int64(shardedOuterRange/shardedStep) + 1
	f := int64(shardedRange / shardedStep)
	series, _ := s.scalarCount("SELECT DISTINCT Attributes FROM bench_shard_sum")

	return shardedResult{
		Query:                shardedQuery,
		Range:                shardedRange,
		Step:                 shardedStep,
		OuterRange:           shardedOuterRange,
		N:                    n,
		F:                    f,
		Series:               series,
		ScanRows:             scanRows,
		RouteAPairs:          routeAPairs,
		K:                    dec.K,
		MaxShardPairs:        maxPairs,
		SumShardPairs:        sumPairs,
		ShardPairs:           shardPairs,
		CapBytes:             shardCapBytes,
		RouteAModeledBytes:   modeledBytes(routeAPairs),
		MaxShardModeledBytes: modeledBytes(maxPairs),
	}, nil
}

// optimizedShardPlan lowers query at the given grid and runs the default
// optimizer, returning the post-optimize plan the Planner classifies and
// chsql.Emit serializes — the exact route-A pipeline the engine drives.
func optimizedShardPlan(ctx context.Context, query string, start, end time.Time, step time.Duration) (chplan.Node, error) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		return nil, err
	}
	plan, err := promql.LowerAtRange(ctx, expr, schema.DefaultOTelMetrics(), start, end, step)
	if err != nil {
		return nil, err
	}
	return optimizer.Default().Run(ctx, plan), nil
}

// matrixPairCount returns a counting query for the expanded (sample, anchor)
// pairs a matrix rate-window fan-out materializes over [start, end] at step,
// with a Range-wide window. It mirrors the arrayJoin fan-out the matrix
// emitters produce: each sample contributes one pair per anchor whose window
// (anchor − Range, anchor] covers it. Counting it directly over the seed is a
// MEASURED number (not modeled) and matches the per-statement intermediate the
// real route-A / per-shard SQL builds before the GROUP BY collapse.
//
// Anchors are End − i·Step for i in [0, N), N = (end−start)/step + 1 — the same
// backward-from-End grid the slicer and every matrix emitter use, so a shard's
// [Slice.Start, Slice.End] sub-grid counts exactly the shard's anchors.
func matrixPairCount(start, end time.Time, step, rng time.Duration) string {
	n := int64(end.Sub(start)/step) + 1
	stepNS := step.Nanoseconds()
	rngNS := rng.Nanoseconds()
	endLit := end.UTC().Format("2006-01-02 15:04:05.000000000")
	// scan floor: oldest anchor (start) minus the Range lookback — the input
	// window the shard would scan.
	scanFromLit := start.Add(-rng).UTC().Format("2006-01-02 15:04:05.000000000")
	// For each sample, arrayJoin the anchors whose window covers it: anchor =
	// End − i·Step, covering ⇔ TimeUnix > anchor − Range AND TimeUnix <= anchor.
	return fmt.Sprintf(`SELECT TimeUnix,
  arrayJoin(arrayFilter(
    a -> (toUnixTimestamp64Nano(TimeUnix) > a - %d AND toUnixTimestamp64Nano(TimeUnix) <= a),
    arrayMap(i -> toUnixTimestamp64Nano(toDateTime64('%s', 9)) - i * %d, range(0, toUInt64(%d))))) AS anchor_ns
  FROM bench_shard_sum
  WHERE MetricName = 'bench.shard.total'
    AND TimeUnix > toDateTime64('%s', 9)
    AND TimeUnix <= toDateTime64('%s', 9)`,
		rngNS, endLit, stepNS, n, scanFromLit, endLit)
}

// shardedSeed builds the OOM-class counter table: shardedSeries series, one
// sample per 15s spanning the input window (hour grid + 5m lookback). The
// dense fan-out makes the route-A (sample, anchor) pair count clear the 1 GiB
// modeled threshold while each shard's sub-grid stays under it. Generated
// server-side via numbers(N) — no row-by-row inserts.
func shardedSeed() string {
	total := shardedSeries * shardedSamplesPerSeries
	// Sample i belongs to series (i % shardedSeries) at timestamp
	// (Start − Range) + (i / shardedSeries)·15s, so every series gets the full
	// dense grid across the input window.
	startLit := shardedStart.Add(-shardedRange).UTC().Format("2006-01-02 15:04:05.000000000")
	return fmt.Sprintf(`DROP TABLE IF EXISTS bench_shard_sum;
CREATE TABLE bench_shard_sum (
  MetricName String, Attributes Map(String,String),
  TimeUnix DateTime64(9), Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO bench_shard_sum SELECT
  'bench.shard.total',
  map('series', concat('s', toString(number %% %d))),
  toDateTime64('%s', 9) + toIntervalSecond((intDiv(number, %d)) * %d),
  toFloat64(number)
FROM numbers(%d);`,
		shardedSeries, startLit, shardedSeries, shardedSampleStride, total)
}
