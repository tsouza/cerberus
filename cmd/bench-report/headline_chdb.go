//go:build chdb

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// headlineWin is one before/after optimization measurement. The numbers
// pair a hand-written NAIVE shape (the documented pre-fix SQL) against
// the OPTIMIZED shape cerberus actually emits (driven through the real
// parse -> lower -> emit pipeline wherever a lowering produces it). The
// speedup ratio is the stable, machine-independent headline.
type headlineWin struct {
	Name      string
	Param     string // the structural knob the win scales against
	Naive     string // short description of the pre-fix shape
	Optimized string // short description of the post-fix shape

	NaiveWall time.Duration
	OptWall   time.Duration
	Speedup   float64 // NaiveWall / OptWall (timing — present as a ratio)

	// Structural (deterministic) signal, when the win has one.
	StructLabel string // e.g. "peak intermediate rows" or "granules read"
	NaiveStruct int64
	OptStruct   int64
	StructRatio float64

	// Primary selects which metric is the headline speedup in the summary
	// table: "wall" (the wall-time ratio is the win) or "structural" (the
	// deterministic ratio is the win — used where the wall does not
	// capture the optimization, e.g. a per-level multiplier or an
	// SQL-size / re-execution win whose optimized shape carries an
	// unrelated residual cost).
	Primary string

	// WallNote, when set, explains why the wall ratio is not the headline
	// (shown in the per-win detail instead of a misleading speedup).
	WallNote string
}

// measureHeadlines runs all four headline wins and returns them in a
// stable order. iters is the best-of repetition count for each wall
// timing.
func measureHeadlines(s *session, iters int) ([]headlineWin, error) {
	var wins []headlineWin

	w, err := measureRangeLWR(s, iters)
	if err != nil {
		return nil, fmt.Errorf("range-LWR: %w", err)
	}
	wins = append(wins, w)

	w, err = measureSetOp(s, iters)
	if err != nil {
		return nil, fmt.Errorf("set-op: %w", err)
	}
	wins = append(wins, w)

	w, err = measureOrderByPrune(s, iters)
	if err != nil {
		return nil, fmt.Errorf("orderby-prune: %w", err)
	}
	wins = append(wins, w)

	w, err = measureBoundedRecursion(s, iters)
	if err != nil {
		return nil, fmt.Errorf("bounded-recursion: %w", err)
	}
	wins = append(wins, w)

	return wins, nil
}

// --- Win 1: range-LWR collapse ------------------------------------------
//
// A bare-selector query_range over a wide step grid. The naive shape is
// the pre-#804 N-anchor StepGrid CROSS JOIN: every sample is paired with
// every one of the N step anchors, an O(rows x anchors) intermediate. The
// optimized shape is the production single-pass RangeLWR (driven through
// the real promql.LowerAtRange -> chsql.Emit), whose intermediate is
// bounded by rows x (lookback/step + 1).

const rangeLWRMetric = "demo_memory_usage_bytes"

func measureRangeLWR(s *session, iters int) (headlineWin, error) {
	const lookback = 5 * time.Minute
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	const anchors = 241 // dense grid: 24h / 6m
	step := end.Sub(start) / time.Duration(anchors-1)

	seed := `DROP TABLE IF EXISTS bench_lwr_gauge;
CREATE TABLE bench_lwr_gauge (
  MetricName String, Attributes Map(String,String),
  TimeUnix DateTime64(9), Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO bench_lwr_gauge SELECT
  '` + rangeLWRMetric + `',
  map('instance', concat('i', toString(number % 200))),
  toDateTime64('2026-01-01 00:00:00', 9) + toIntervalSecond((intDiv(number, 200)) * 96),
  toFloat64(number)
FROM numbers(180000);`
	if err := s.execAll(seed); err != nil {
		return headlineWin{}, err
	}

	// OPTIMIZED: lower the bare selector through the real pipeline against
	// the same table name the seed creates.
	optSQL, err := emitRangeOverTable(start, end, step, "bench_lwr_gauge")
	if err != nil {
		return headlineWin{}, err
	}
	optWall, err := s.bestWall(optSQL, iters)
	if err != nil {
		return headlineWin{}, fmt.Errorf("opt wall: %w", err)
	}
	optInner := rangeLWRFanoutInner(end, step, lookback, anchors)
	optPeak, err := s.scalarCount(optInner)
	if err != nil {
		return headlineWin{}, fmt.Errorf("opt peak: %w", err)
	}

	// NAIVE: the pre-#804 step-grid CROSS JOIN. Each sample crosses with
	// every anchor in the N-point grid, then a staleness filter keeps the
	// in-window pairs. We measure the full cross product as the
	// intermediate and time the whole shape.
	naiveSQL := naiveStepGridCrossJoin(end, step, lookback, anchors, "bench_lwr_gauge")
	naiveWall, err := s.bestWall(naiveSQL, iters)
	if err != nil {
		return headlineWin{}, fmt.Errorf("naive wall: %w", err)
	}
	naivePeak, err := s.scalarCount(naiveStepGridCross(anchors, "bench_lwr_gauge"))
	if err != nil {
		return headlineWin{}, fmt.Errorf("naive peak: %w", err)
	}

	return headlineWin{
		Name:        "range-LWR collapse",
		Param:       fmt.Sprintf("query_range over a bare selector, %d step anchors", anchors),
		Naive:       "N-anchor StepGrid CROSS JOIN (O(rows × anchors))",
		Optimized:   "single-pass RangeLWR, sample-side fan-out bounded by lookback/step",
		NaiveWall:   naiveWall,
		OptWall:     optWall,
		Speedup:     ratioDur(naiveWall, optWall),
		StructLabel: "peak intermediate rows",
		NaiveStruct: naivePeak,
		OptStruct:   optPeak,
		StructRatio: ratioI(naivePeak, optPeak),
		Primary:     "wall",
	}, nil
}

// emitRangeOverTable lowers the bare selector and rewrites the emitted SQL
// to read the bench table (the lowering targets the schema's default
// gauge table name; the bench seed uses a private name to avoid colliding
// with other measurements in the same session).
func emitRangeOverTable(start, end time.Time, step time.Duration, table string) (string, error) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(rangeLWRMetric)
	if err != nil {
		return "", err
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, end, step)
	if err != nil {
		return "", err
	}
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		return "", err
	}
	sqlText = inlineArgs(sqlText, args)
	return strings.ReplaceAll(sqlText, "otel_metrics_gauge", table), nil
}

// rangeLWRFanoutInner is the bounded sample-side fan-out the RangeLWR
// emitter produces before the GROUP BY collapse — one row per (sample,
// covered anchor). Mirrors the scaling harness's measurement of the same
// stage.
func rangeLWRFanoutInner(end time.Time, step, lookback time.Duration, numAnchors int64) string {
	stepNS := step.Nanoseconds()
	endLit := end.UTC().Format("2006-01-02 15:04:05.000000000")
	dist := fmt.Sprintf("dateDiff('nanosecond', TimeUnix, toDateTime64('%s', 9))", endLit)
	floorIdx := func(addNS int64) string {
		num := dist
		if addNS < 0 {
			num = fmt.Sprintf("%s - %d", dist, -addNS)
		}
		return fmt.Sprintf("intDiv(%s, toInt64(%d)) - (modulo(%s, toInt64(%d)) < 0) + 1",
			num, stepNS, num, stepNS)
	}
	return fmt.Sprintf(`SELECT TimeUnix, Value,
  arrayJoin(arrayMap(i -> toDateTime64('%s', 9) - toIntervalNanosecond(i * %d),
    range(greatest(0, %s), least(%d, %s)))) AS anchor_ts
  FROM bench_lwr_gauge WHERE MetricName = '`+rangeLWRMetric+`'`,
		endLit, stepNS, floorIdx(-lookback.Nanoseconds()), numAnchors, floorIdx(0))
}

// naiveStepGridCross is the pre-fix cross product: every sample paired
// with every one of the N step anchors (the full O(rows × anchors)
// intermediate, before any staleness filter prunes it).
func naiveStepGridCross(numAnchors int64, table string) string {
	return fmt.Sprintf(`SELECT s.TimeUnix, s.Value, g.anchor_ts
  FROM %s AS s
  CROSS JOIN (SELECT arrayJoin(range(0, %d)) AS anchor_ts) AS g
  WHERE s.MetricName = '%s'`, table, numAnchors, rangeLWRMetric)
}

// naiveStepGridCrossJoin wraps the cross product with the per-anchor
// staleness filter + argMax collapse the pre-fix shape applied — the full
// query whose wall the naive number reports.
func naiveStepGridCrossJoin(end time.Time, step, lookback time.Duration, numAnchors int64, table string) string {
	endNS := end.UnixNano()
	stepNS := step.Nanoseconds()
	lbNS := lookback.Nanoseconds()
	cross := fmt.Sprintf(`SELECT s.TimeUnix AS ts, s.Value AS v, s.Attributes AS attrs,
    (%d - g.idx * %d) AS anchor_ns
  FROM %s AS s
  CROSS JOIN (SELECT arrayJoin(range(0, %d)) AS idx) AS g
  WHERE s.MetricName = '%s'`, endNS, stepNS, table, numAnchors, rangeLWRMetric)
	return fmt.Sprintf(`SELECT attrs, anchor_ns, argMax(v, ts) AS val
  FROM (%s)
  WHERE toUnixTimestamp64Nano(ts) <= anchor_ns
    AND toUnixTimestamp64Nano(ts) > anchor_ns - %d
  GROUP BY attrs, anchor_ns`, cross, lbNS)
}

// --- Win 2: set-op single-pass ------------------------------------------
//
// A binary PromQL `or` over two disjoint-armed selectors. The naive shape
// re-materializes each arm independently (the pre-#814 per-arm CTE that
// CH re-executes inline at every reference); the optimized shape is the
// production single-pass UNION-ALL + window the real lowering emits, where
// each arm is scanned exactly once.

func measureSetOp(s *session, iters int) (headlineWin, error) {
	evalTime := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	// K=6: the left-assoc `or` chain `m0 or m1 or ... or m6`. The pre-#810
	// shape DUPLICATED the whole LHS subplan at every `or` level, so the
	// SQL text — and the data re-execution — was EXPONENTIAL in K (arm 0
	// re-rendered 2^K times); it breached CH's 256KB parse limit by K~8.
	// K=6 keeps the naive exponential model runnable while the contrast
	// stays decisive.
	const nArms = 6

	// Seed a sum table with one row per arm, each carrying a disjoint
	// `arm` label so the `or` chain reduces to the pure union.
	var b strings.Builder
	b.WriteString("DROP TABLE IF EXISTS bench_setop_sum;")
	b.WriteString(`CREATE TABLE bench_setop_sum (
  MetricName String, Attributes Map(String,String),
  TimeUnix DateTime64(9), Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, Attributes, TimeUnix);`)
	ts := evalTime.Add(-time.Second).UTC().Format("2006-01-02 15:04:05.000000000")
	// Each arm holds many rows (a realistic series count) so the per-arm
	// re-scan the naive shape pays is non-trivial.
	for i := 0; i <= nArms; i++ {
		fmt.Fprintf(&b, "\nINSERT INTO bench_setop_sum SELECT 'setop.chain.metric.%d', "+
			"map('arm','%d','series',concat('s',toString(number))), toDateTime64('%s',9), toFloat64(number) "+
			"FROM numbers(2000);", i, i, ts)
	}
	if err := s.execAll(b.String()); err != nil {
		return headlineWin{}, err
	}

	// OPTIMIZED: lower the real `or` chain through the pipeline.
	optSQL, err := emitSetOpChain("or", nArms, evalTime)
	if err != nil {
		return headlineWin{}, err
	}
	optSQL = strings.ReplaceAll(optSQL, "otel_metrics_sum", "bench_setop_sum")
	optWall, err := s.bestWall(optSQL, iters)
	if err != nil {
		return headlineWin{}, fmt.Errorf("opt wall: %w", err)
	}

	// NAIVE: the pre-#810 left-assoc chain DUPLICATED the whole LHS subplan
	// at every `or` level, so arm i is re-rendered (and CH re-executes it)
	// 2^(K-i) times — EXPONENTIAL in K. We model that re-execution as the
	// arms UNION-ALL'd with arm i repeated 2^(K-i) times.
	naiveSQL := naiveSetOpRematerialize(nArms)
	naiveWall, err := s.bestWall(naiveSQL, iters)
	if err != nil {
		return headlineWin{}, fmt.Errorf("naive wall: %w", err)
	}

	// Structural: rows scanned (deterministic). The optimized shape scans
	// each arm EXACTLY once (sum of arm sizes); the naive exponential
	// duplication re-scans arm i 2^(K-i) times.
	optScan, err := s.scalarCount("SELECT * FROM bench_setop_sum")
	if err != nil {
		return headlineWin{}, err
	}
	naiveScan, err := s.scalarCount(naiveSetOpScanRows(nArms))
	if err != nil {
		return headlineWin{}, err
	}

	return headlineWin{
		Name:        "set-op single-pass",
		Param:       fmt.Sprintf("left-assoc `or` chain, depth K=%d", nArms),
		Naive:       "exponential LHS duplication (arm i re-scanned 2^(K−i) times)",
		Optimized:   "single-pass UNION-ALL + window (each arm scanned once)",
		NaiveWall:   naiveWall,
		OptWall:     optWall,
		Speedup:     ratioDur(naiveWall, optWall),
		StructLabel: "arm rows scanned",
		NaiveStruct: naiveScan,
		OptStruct:   optScan,
		StructRatio: ratioI(naiveScan, optScan),
		Primary:     "structural",
		WallNote: "the optimized single-pass shape still carries the unrelated " +
			"left-assoc K-nesting residual (#90), so the wall ratio understates the win; " +
			"the deterministic re-execution ratio is the headline",
	}, nil
}

func emitSetOpChain(op string, k int, evalTime time.Time) (string, error) {
	parts := make([]string, k+1)
	for i := range parts {
		parts[i] = fmt.Sprintf("setop_chain_metric_%d", i)
	}
	q := strings.Join(parts, " "+op+" ")
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(q)
	if err != nil {
		return "", err
	}
	plan, err := promql.LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(), evalTime, evalTime)
	if err != nil {
		return "", err
	}
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		return "", err
	}
	return inlineArgs(sqlText, args), nil
}

// naiveSetOpRematerialize models the pre-#810 exponential re-execution:
// arm i is re-rendered 2^(k-i) times via repeated UNION ALL, mirroring
// how the left-assoc chain duplicated the accumulated LHS at every level.
func naiveSetOpRematerialize(k int) string {
	return strings.Join(naiveSetOpArms(k, "Attributes, Value"), "\nUNION ALL\n")
}

// naiveSetOpScanRows counts the rows the naive exponential duplication
// reads (arm i counted 2^(k-i) times).
func naiveSetOpScanRows(k int) string {
	return strings.Join(naiveSetOpArms(k, "1"), "\nUNION ALL\n")
}

func naiveSetOpArms(k int, proj string) []string {
	var arms []string
	for i := 0; i <= k; i++ {
		reps := 1 << (k - i) // 2^(k-i)
		for r := 0; r < reps; r++ {
			arms = append(arms, fmt.Sprintf(
				"SELECT %s FROM bench_setop_sum WHERE MetricName = 'setop.chain.metric.%d'", proj, i,
			))
		}
	}
	return arms
}

// --- Win 3: MetricName-first ORDER BY granule prune ---------------------
//
// Deterministic granule-prune win, lifted directly from the live
// ServiceName-first vs MetricName-first comparison in
// test/perf/orderby_chdb_test.go. Two byte-identical tables differ only in
// sort key; a metric-only query (no service.name matcher — the Grafana
// default) PK-range-prunes on the metric-first key but falls to a generic
// exclusion scan on the service-first key.

func measureOrderByPrune(s *session, iters int) (headlineWin, error) {
	const (
		nServices = 25
		nMetrics  = 40
		nAttr     = 20
		nTime     = 30
	)
	total := nServices * nMetrics * nAttr * nTime

	type tbl struct{ name, orderBy string }
	tables := []tbl{
		{"bench_m_svcfirst", "(ServiceName, MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix))"},
		{"bench_m_metricfirst", "(MetricName, Attributes, ServiceName, toUnixTimestamp64Nano(TimeUnix))"},
	}
	for _, t := range tables {
		ddl := fmt.Sprintf(`CREATE OR REPLACE TABLE %s (
  ServiceName String, MetricName String, Attributes Map(String,String),
  TimeUnix DateTime64(9), Value Float64
) ENGINE = MergeTree() ORDER BY %s SETTINGS index_granularity = 8192;`, t.name, t.orderBy)
		ins := fmt.Sprintf(`INSERT INTO %s SELECT
  concat('service.', leftPad(toString(intDiv(number, %d) %% %d), 3, '0')),
  concat('metric_', leftPad(toString(intDiv(number, %d) %% %d), 3, '0')),
  map('host', concat('h', toString(intDiv(number, %d) %% %d))),
  toDateTime64('2026-05-11 12:00:00', 9) + INTERVAL (number %% %d) SECOND,
  toFloat64(number)
FROM numbers(%d);`, t.name,
			nMetrics*nAttr*nTime, nServices, nAttr*nTime, nMetrics, nTime, nAttr, nTime, total)
		if err := s.exec(ddl); err != nil {
			return headlineWin{}, err
		}
		if err := s.exec(ins); err != nil {
			return headlineWin{}, err
		}
		if err := s.exec("OPTIMIZE TABLE " + t.name + " FINAL"); err != nil {
			return headlineWin{}, err
		}
	}

	const metricOnly = "SELECT sum(Value), count() FROM %s WHERE MetricName = 'metric_020'"
	svcQ := fmt.Sprintf(metricOnly, "bench_m_svcfirst")
	metQ := fmt.Sprintf(metricOnly, "bench_m_metricfirst")

	svcGran, _, err := s.explainSelectedGranules(svcQ)
	if err != nil {
		return headlineWin{}, fmt.Errorf("svc explain: %w", err)
	}
	metGran, _, err := s.explainSelectedGranules(metQ)
	if err != nil {
		return headlineWin{}, fmt.Errorf("metric explain: %w", err)
	}
	svcWall, err := s.bestWall(svcQ, iters)
	if err != nil {
		return headlineWin{}, err
	}
	metWall, err := s.bestWall(metQ, iters)
	if err != nil {
		return headlineWin{}, err
	}

	return headlineWin{
		Name:        "MetricName-first ORDER BY",
		Param:       fmt.Sprintf("metric-only query (no service.name), %d rows", total),
		Naive:       "ServiceName-first sort key → generic exclusion scan",
		Optimized:   "MetricName-first sort key → PK range prune",
		NaiveWall:   svcWall,
		OptWall:     metWall,
		Speedup:     ratioDur(svcWall, metWall),
		StructLabel: "granules read",
		NaiveStruct: int64(svcGran),
		OptStruct:   int64(metGran),
		StructRatio: ratioI(int64(svcGran), int64(metGran)),
		Primary:     "structural",
	}, nil
}

// --- Win 4: bounded recursion -------------------------------------------
//
// The TraceQL structural `>>` closure. The naive shape re-scans the bare
// full traces table on every recursion level (O(depth × full-scan)); the
// #808 fix pushes the candidate trace-id set into the recursive arm so
// each level reads only candidate rows. The win here is reported as the
// per-level frontier the recursion materializes: bounded by the candidate
// rows, independent of depth.

func measureBoundedRecursion(s *session, iters int) (headlineWin, error) {
	const (
		depth     = 48
		totalRows = 60000
		candRows  = 1200 // ~2% of the table matches
	)
	candTraces := candRows / depth
	noiseTraces := (totalRows - candRows) / depth

	seed := boundedRecursionSeed(candTraces, noiseTraces, depth)
	if err := s.execAll(seed); err != nil {
		return headlineWin{}, err
	}

	// OPTIMIZED frontier: each recursive level walks only the candidate
	// rows (parent ∈ candidate trace set). Bounded by candidate rows.
	optFrontier := `SELECT t.TraceId, t.SpanId FROM bench_traces AS t
  WHERE t.TraceId IN (SELECT TraceId FROM bench_traces WHERE ParentSpanId = '' AND SpanName = 'root_marker')`
	// NAIVE frontier: each level re-scans the WHOLE table (no trace-id
	// restriction), so the per-level work is the full table, ×depth.
	naiveFrontier := `SELECT t.TraceId, t.SpanId FROM bench_traces AS t`

	optScan, err := s.scalarCount(optFrontier)
	if err != nil {
		return headlineWin{}, err
	}
	fullScan, err := s.scalarCount(naiveFrontier)
	if err != nil {
		return headlineWin{}, err
	}
	// Per-level cost: optimized stays at optScan every level; naive reads
	// the full table every level — over `depth` levels that's depth ×
	// fullScan vs depth × optScan, i.e. the per-level ratio is the win.
	optWall, err := s.bestWall(optFrontier, iters)
	if err != nil {
		return headlineWin{}, err
	}
	naiveWall, err := s.bestWall(naiveFrontier, iters)
	if err != nil {
		return headlineWin{}, err
	}

	return headlineWin{
		Name:        "bounded recursion",
		Param:       fmt.Sprintf("structural `>>` closure, depth D=%d", depth),
		Naive:       "bare full-table re-scan per level (O(depth × full-scan))",
		Optimized:   "candidate trace-id set pushed into recursive arm",
		NaiveWall:   naiveWall,
		OptWall:     optWall,
		Speedup:     ratioDur(naiveWall, optWall),
		StructLabel: "rows read per recursion level",
		NaiveStruct: fullScan,
		OptStruct:   optScan,
		StructRatio: ratioI(fullScan, optScan),
		Primary:     "structural",
		WallNote: fmt.Sprintf("the win is the PER-LEVEL row count, multiplied across all %d "+
			"recursion levels; a single-frontier wall does not capture the depth multiplier, "+
			"so the deterministic per-level ratio is the headline", depth),
	}, nil
}

func boundedRecursionSeed(candTraces, noiseTraces, depth int) string {
	var b strings.Builder
	b.WriteString("DROP TABLE IF EXISTS bench_traces;")
	b.WriteString(`CREATE TABLE bench_traces (
  TraceId String, SpanId String, ParentSpanId String, SpanName String DEFAULT ''
) ENGINE = Memory;`)
	hexID := func(n int) string { return fmt.Sprintf("%016x", n+1) }
	hexTrace := func(prefix byte, n int) string { return fmt.Sprintf("%c%031x", prefix, n+1) }
	b.WriteString("\nINSERT INTO bench_traces (TraceId, SpanId, ParentSpanId, SpanName) VALUES\n")
	first := true
	gid := 0
	emit := func(traceID string, marker bool) {
		base := gid
		for lvl := 0; lvl < depth; lvl++ {
			var parent string
			if lvl > 0 {
				parent = hexID(base + lvl - 1)
			}
			name := "noise"
			if marker {
				switch lvl {
				case 0:
					name = "root_marker"
				case depth - 1:
					name = "leaf_marker"
				default:
					name = "mid"
				}
			}
			if !first {
				b.WriteString(",\n")
			}
			first = false
			fmt.Fprintf(&b, "  ('%s','%s','%s','%s')", traceID, hexID(base+lvl), parent, name)
		}
		gid += depth
	}
	for i := 0; i < candTraces; i++ {
		emit(hexTrace('c', i), true)
	}
	for i := 0; i < noiseTraces; i++ {
		emit(hexTrace('n', i), false)
	}
	b.WriteString(";")
	return b.String()
}
