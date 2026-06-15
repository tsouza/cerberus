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

// The rate-range strategy table. The focus query is the end-to-end rate
// range query_range — the one shape where BOTH the sharded-pushdown solver
// (internal/solver, route A vs B) AND the experimental ClickHouse-native
// timeSeriesRateToGrid lowering (CERBERUS_EXPERIMENTAL_TS_GRID_RANGE)
// matter. Every other e2e shape is route-invariant (an instant query has no
// anchor grid to slice; a series lookup / log filter has no rate fan-out),
// so the strategy table lives here, on its own focus query, rather than
// fanning the whole e2e table.
//
// There are only THREE genuinely distinct strategies, not a route × tsgrid
// grid: native rate and sharding are two ALTERNATIVE remedies for the same
// row fan-out, NOT independent dials. Enabling native rate removes the
// fan-out entirely (timeSeriesRateToGrid never builds the (sample,anchor)
// matrix), so the sharded solver finds no fan-out spine to slice and any
// route collapses to a single statement. That is why the whole "native on"
// column of the old 3×2 matrix was one repeated cell — it is dropped here.
//
// Each strategy is driven through the FULL production pipeline for its
// configuration:
//
//	parse → lower(LowerOpts{tsgrid}) → optimize → route(solver mode)
//	      → emit → execute(chDB) → (sequential shard composition)
//
// The three strategies:
//
//   - single (native off)  → solver.ModeSingle, tsgrid=false: the solver is
//     dark, the whole anchor grid runs as ONE ClickHouse statement (route
//     A) — the full fan-out in one query, the baseline and the problem.
//   - sharded (native off) → solver.ModeSharded, tsgrid=false: the plan is
//     force-routed onto K disjoint anchor-grid shards (route B), each a
//     re-anchored copy of the same plan over its anchor sub-grid; the worst
//     shard's peak fits under the per-query memory cap.
//   - native rate          → solver.ModeAuto, tsgrid=true: the realistic
//     production config (auto routing + the experimental native-rate flag).
//     The native timeSeriesRateToGrid aggregate computes every grid point's
//     rate in ONE pass with NO row fan-out (requires ClickHouse ≥ 25.6 — the
//     chDB substrate is 25.8), so the fan-out vanishes and auto correctly
//     collapses to a single statement (route is moot).
//
// Two numbers are reported per strategy:
//
//   - Wall  — best-of-N wall time, MEASURED. For the sharded strategy this
//     is the SEQUENTIAL sum of the shard walls (one in-process chDB engine
//     runs them back-to-back); production runs them on parallel CH
//     connections, so the on-engine sharded wall is a CONSERVATIVE upper
//     bound, never a speedup claim.
//   - PeakMem — the per-statement peak memory, MODELED from the MEASURED
//     intermediate (sample,anchor) pair count via the same published
//     calibration constant the sharded section uses (chDB exposes no
//     peak-memory metric and never enforces a cap). For the sharded
//     strategy it is the WORST shard's modeled peak — the per-query driver,
//     since each shard is capped independently. The native-rate strategy
//     collapses the intermediate to ~scan rows, so its modeled peak is far
//     lower.
type matrixCell struct {
	Name     string // strategy label, e.g. "single (native off)"
	Route    string // single / sharded / auto
	TSGrid   bool   // native timeSeriesRateToGrid lowering on/off
	Routed   bool   // did the solver actually take route B?
	K        int    // shard count (1 when not routed)
	Wall     time.Duration
	PeakRows int64 // measured intermediate (sample,anchor) pairs (worst statement)
	PeakMem  int64 // modeled peak bytes (PeakRows × bytesPerPair)
}

// matrixResult is the three-strategy table plus the focus-query description.
type matrixResult struct {
	Query      string
	Step       time.Duration
	OuterRange time.Duration
	Range      time.Duration
	N          int64 // outer anchor count
	F          int64 // per-window fan-out (Range/Step)
	ScanRows   int64
	Cells      []matrixCell // one per matrixStrategy, in presentation order
}

// matrixStrategy is one row of the strategy table: a (route, tsgrid)
// configuration with a published label.
type matrixStrategy struct {
	Name   string // presentation label
	Route  string // solver mode
	TSGrid bool   // native timeSeriesRateToGrid lowering on/off
}

// matrixStrategies enumerates the three genuinely distinct strategies in a
// stable presentation order: the baseline single statement, the sharded
// cap-fit, and the native-rate fan-out removal (run under auto, the
// production default, to show auto + native → single).
var matrixStrategies = []matrixStrategy{
	{Name: "single (native off)", Route: solver.ModeSingle, TSGrid: false},
	{Name: "sharded (native off)", Route: solver.ModeSharded, TSGrid: false},
	{Name: "native rate", Route: solver.ModeAuto, TSGrid: true},
}

// matrixQuery is the focus query: the e2e rate range query_range. It is the
// same shape as the e2e "range query (240 steps)" row, run over the same
// 500k-row e2e_metrics_gauge seed, so the matrix and the e2e table agree.
const matrixQuery = `sum(rate(e2e_http_requests[5m]))`

var (
	matrixRange = 5 * time.Minute
	matrixStep  = 15 * time.Second
	matrixEnd   = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	matrixStart = matrixEnd.Add(-time.Hour)
)

// measureMatrix drives the three rate-range strategies over the e2e metrics
// seed (already created by measureE2E, which runs first). It returns one
// matrixResult — one measured cell per matrixStrategy.
func measureMatrix(s *session, iters int) (matrixResult, error) {
	ctx := context.Background()

	// The native timeSeriesRateToGrid aggregate is experimental; enable it
	// session-wide so the native-rate strategy runs. It is inert for the
	// non-tsgrid statements, so a single session-level SET is the cleanest
	// gate (the production engine threads it per-request via WithTSGridSetting;
	// here the in-process session carries it for the whole strategy run).
	if err := s.exec("SET allow_experimental_time_series_aggregate_functions = 1"); err != nil {
		return matrixResult{}, fmt.Errorf("enable native-rate setting: %w", err)
	}

	scanRows, err := s.scalarCount("SELECT * FROM e2e_metrics_gauge WHERE MetricName = 'e2e.http.requests'")
	if err != nil {
		return matrixResult{}, fmt.Errorf("matrix scan count: %w", err)
	}

	res := matrixResult{
		Query:      matrixQuery,
		Step:       matrixStep,
		OuterRange: matrixEnd.Sub(matrixStart),
		Range:      matrixRange,
		N:          int64(matrixEnd.Sub(matrixStart)/matrixStep) + 1,
		F:          int64(matrixRange / matrixStep),
		ScanRows:   scanRows,
	}

	for _, st := range matrixStrategies {
		cell, err := measureMatrixCell(ctx, s, iters, st)
		if err != nil {
			return matrixResult{}, fmt.Errorf("strategy %q (route=%s tsgrid=%v): %w",
				st.Name, st.Route, st.TSGrid, err)
		}
		res.Cells = append(res.Cells, cell)
	}
	return res, nil
}

// measureMatrixCell drives one strategy (route, tsgrid) configuration end to
// end and labels the resulting cell with the strategy's published name.
func measureMatrixCell(ctx context.Context, s *session, iters int, st matrixStrategy) (matrixCell, error) {
	plan, err := lowerMatrixPlan(ctx, st.TSGrid)
	if err != nil {
		return matrixCell{}, fmt.Errorf("lower+optimize: %w", err)
	}

	route, tsgrid := st.Route, st.TSGrid
	cell := matrixCell{Name: st.Name, Route: route, TSGrid: tsgrid, K: 1}

	cfg := solver.DefaultConfig()
	cfg.Mode = route
	if err := cfg.Validate(); err != nil {
		return matrixCell{}, fmt.Errorf("solver config: %w", err)
	}
	pl := &solver.Planner{Cfg: cfg}
	gs, ge, gstep := solver.GridOf(plan)
	dec, routed := pl.Plan(plan, solver.RequestMeta{
		Lang: solver.LangPromQL, Start: gs, End: ge, Step: gstep,
	})
	cell.Routed = routed

	if routed {
		// Route B: run every shard's re-anchored plan sequentially, sum the
		// walls, and take the worst shard's intermediate as the per-query
		// memory driver (each shard is capped independently).
		cell.K = dec.K
		var sumWall time.Duration
		var maxPairs int64
		for _, sl := range dec.Slices {
			sql, err := emitMatrix(ctx, sl.Plan, tsgrid)
			if err != nil {
				return matrixCell{}, fmt.Errorf("emit shard %d: %w", sl.Index, err)
			}
			w, err := s.bestWall(sql, iters)
			if err != nil {
				return matrixCell{}, fmt.Errorf("exec shard %d: %w", sl.Index, err)
			}
			sumWall += w
			pairs, err := matrixIntermediate(s, sl.Start, sl.End, tsgrid)
			if err != nil {
				return matrixCell{}, fmt.Errorf("shard %d intermediate: %w", sl.Index, err)
			}
			if pairs > maxPairs {
				maxPairs = pairs
			}
		}
		cell.Wall = sumWall
		cell.PeakRows = maxPairs
	} else {
		// Route A / single: one statement over the whole anchor grid.
		sql, err := emitMatrix(ctx, plan, tsgrid)
		if err != nil {
			return matrixCell{}, fmt.Errorf("emit route A: %w", err)
		}
		w, err := s.bestWall(sql, iters)
		if err != nil {
			return matrixCell{}, fmt.Errorf("exec route A: %w", err)
		}
		cell.Wall = w
		pairs, err := matrixIntermediate(s, matrixStart, matrixEnd, tsgrid)
		if err != nil {
			return matrixCell{}, fmt.Errorf("route-A intermediate: %w", err)
		}
		cell.PeakRows = pairs
	}

	cell.PeakMem = modeledBytes(cell.PeakRows)
	return cell, nil
}

// lowerMatrixPlan lowers + optimizes the focus query for the given tsgrid
// state, exactly as the query_range handler does (LowerAtRangeOpts threads
// LowerOpts.ExperimentalTSGridRange from Config.ExperimentalTSGridRange).
func lowerMatrixPlan(ctx context.Context, tsgrid bool) (chplan.Node, error) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(matrixQuery)
	if err != nil {
		return nil, err
	}
	plan, err := promql.LowerAtRangeOpts(ctx, expr, schema.DefaultOTelMetrics(),
		matrixStart, matrixEnd, matrixStep, promql.LowerOpts{ExperimentalTSGridRange: tsgrid})
	if err != nil {
		return nil, err
	}
	return optimizer.Default().Run(ctx, plan), nil
}

// emitMatrix emits a plan to executable SQL against the e2e seed. The native
// timeSeriesRateToGrid aggregate is experimental, but the session-level SET
// in measureMatrix already enabled it, so the emitted SQL needs no per-query
// SETTINGS wrapper (which the bestWall count() wrapper would strip anyway).
func emitMatrix(ctx context.Context, plan chplan.Node, _ bool) (string, error) {
	sqlText, args, err := chsql.Emit(ctx, plan)
	if err != nil {
		return "", err
	}
	return retargetMetrics(inlineArgs(sqlText, args)), nil
}

// matrixIntermediate counts the per-statement intermediate cardinality the
// fan-out materializes before the GROUP BY collapse — the deterministic
// driver of per-statement memory.
//
//   - tsgrid=off: the (sample,anchor) matrix — one row per sample per
//     covering anchor (matrixPairCount, reused from the sharded section).
//   - tsgrid=on: the native aggregate computes every grid point in one pass
//     with NO row fan-out, so the intermediate is the scanned-sample count
//     itself — the whole win the native path delivers.
func matrixIntermediate(s *session, start, end time.Time, tsgrid bool) (int64, error) {
	if tsgrid {
		return s.scalarCount(matrixNativeScan(start, end))
	}
	return s.scalarCount(matrixPairCountE2E(start, end))
}

// matrixPairCountE2E counts the expanded (sample, anchor) pairs a matrix
// rate-window fan-out materializes over [start, end] at the focus grid,
// against the e2e_metrics_gauge seed. It mirrors matrixPairCount (the
// sharded section's counter) retargeted to the e2e table + metric name:
// each sample contributes one pair per anchor whose window
// (anchor − Range, anchor] covers it, with anchors End − i·Step.
func matrixPairCountE2E(start, end time.Time) string {
	n := int64(end.Sub(start)/matrixStep) + 1
	stepNS := matrixStep.Nanoseconds()
	rngNS := matrixRange.Nanoseconds()
	endLit := end.UTC().Format("2006-01-02 15:04:05.000000000")
	scanFromLit := start.Add(-matrixRange).UTC().Format("2006-01-02 15:04:05.000000000")
	return fmt.Sprintf(`SELECT TimeUnix,
  arrayJoin(arrayFilter(
    a -> (toUnixTimestamp64Nano(TimeUnix) > a - %d AND toUnixTimestamp64Nano(TimeUnix) <= a),
    arrayMap(i -> toUnixTimestamp64Nano(toDateTime64('%s', 9)) - i * %d, range(0, toUInt64(%d))))) AS anchor_ns
  FROM e2e_metrics_gauge
  WHERE MetricName = 'e2e.http.requests'
    AND TimeUnix > toDateTime64('%s', 9)
    AND TimeUnix <= toDateTime64('%s', 9)`,
		rngNS, endLit, stepNS, n, scanFromLit, endLit)
}

// matrixNativeScan counts the samples the native path scans over the input
// window [start − Range, end] — its intermediate, with no anchor fan-out.
func matrixNativeScan(start, end time.Time) string {
	fromLit := start.Add(-matrixRange).UTC().Format("2006-01-02 15:04:05.000000000")
	endLit := end.UTC().Format("2006-01-02 15:04:05.000000000")
	return fmt.Sprintf(`SELECT TimeUnix FROM e2e_metrics_gauge
  WHERE MetricName = 'e2e.http.requests'
    AND TimeUnix > toDateTime64('%s', 9)
    AND TimeUnix <= toDateTime64('%s', 9)`, fromLit, endLit)
}
