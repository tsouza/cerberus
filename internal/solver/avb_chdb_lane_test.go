//go:build chdb

// A-vs-B chDB differential lane — the parity proof that unlocks the Mode
// flip: route A and the K shard SQLs run on chDB and compare bit-for-bit
// (the disjoint-anchor equivalence behind docs/solver.md).
//
// For every seeded fixture whose optimized plan the Planner force-routes
// under Mode="sharded", this lane:
//
//  1. Builds the optimized plan (lower -> optimizer.Default().Run).
//  2. Routes it via the Planner under Mode="sharded" (K_min routing —
//     every ELIGIBLE plan routes at K >= 2).
//  3. Emits route A's single SQL (chsql.Emit over the whole plan) AND each
//     of the K shard SQLs (chsql.Emit per Slice.Plan), executing ALL under
//     chDB over the seeded data.
//  4. Concatenates the K shard result sets oldest-first (the order
//     shardCursor drains them) and compares to route A's result set.
//  5. Asserts ZERO diffs. The comparison is NaN-stable: NaN equals NaN by
//     bit-class (NOT reflect.DeepEqual, which makes NaN != NaN), the sort
//     uses a NaN-stable total order (key = (isNaN, value)), and duplicate
//     rows are compared with multiplicity (sorted index-aligned compare).
//  6. Coverage is MEASURED: each compared fixture must have ACTUALLY
//     force-routed (routed == true, K >= 2). A fixture the Planner declines
//     to route is a hard failure (known-untested, not silently passed), and
//     the routed count is printed.
//
// The fixtures deliberately seed a NaN-emitting shape (job=d — a dense FLAT
// counter carried IDENTICALLY on http_requests_total and http_errors_total, so
// rate() is exactly 0 on both arms and the vector-vector RATIO is 0/0 = literal
// nan at every anchor) and duplicate output timestamps (two series share every
// anchor_ts), so the comparator's NaN-bit-class and duplicate-multiplicity
// paths are exercised on real data.
//
// This file is compiled only under the `chdb` build tag (libchdb.so at
// /usr/local/lib/libchdb.so; see `just chdb-install`). It mirrors the chDB
// execution + decode contract of test/spec/runner_chdb.go.
package solver_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/solver"
)

// The lane grid every fixture anchors on. With
// Step = 15s over a 1h OuterRange the outer fan-out N = 241; F = Range/Step =
// 20 for a [5m] window — the RangeWindow matrix family the design names as
// the dominant routed shape (sum(rate(m[5m])) @ 15s over 1h). All bounds are
// pinned and now64-free so the Planner sees a grid-matched, eligible plan and
// force-sharded routes it at K_min.
var (
	laneStart = time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)
	laneStep  = 15 * time.Second
	laneEnd   = laneStart.Add(time.Hour)
)

// laneSeed populates otel_metrics_sum with a counter http_requests_total
// carrying three series:
//
//   - job=a, job=b — dense monotonic counters, one sample every 15s for the
//     full hour (241 samples each). They produce a row at every anchor for
//     both series, so EVERY anchor_ts appears with multiplicity 2 in the
//     output — the duplicate-timestamp coverage the comparator must handle.
//
//   - job=c — exactly two samples at the SAME timestamp (00:10:00). The rate
//     emitter's timestamp-dedup (dedupWindowPairsByTsFrag / arrayCompact by ts)
//     collapses the two same-timestamp samples to a single window element, so
//     every job=c window holds length 1 and fails rate's `length >= 2` filter:
//     job=c is DROPPED from every rate() output. It is the dup-input-timestamp
//     DEDUP-DROP coverage — route A and every shard must agree on the drop.
//
//   - job=d — a dense FLAT counter, one sample every 15s for the full hour
//     (241 samples), every sample the SAME value. A flat counter has zero
//     counter-delta, so rate(job=d[5m]) is exactly 0 at every anchor (a
//     finite, non-dropped value). job=d exists so the vector-vector ratio
//     fixtures below emit a GENUINE literal nan: 0/0.
//
// A parallel counter http_errors_total carries the SAME four-series shape
// (dense job=a/job=b, a dup-input-timestamp job=c, a dense flat job=d). Because
// job=d is flat on BOTH metrics, both ratio arms rate() to 0 at every anchor,
// so the join emits 0/0 = literal nan for job=d — the NaN-bit-class coverage.
// The nan is produced deterministically (0.0/0.0) and IDENTICALLY on route A
// and the owning shard (each anchor is computed in exactly one shard), so the
// comparator's NaN==NaN-by-bit-class path fires while parity stays green.
//
// The ORDER BY does not dedup (MergeTree, not ReplacingMergeTree), so both
// job=c rows persist. Statements are newline-clean (no inline `-- comment`
// lines) so splitStatements keeps each INSERT intact.
// ResourceAttributes (DEFAULT map()) mirrors the OTel-CH default schema:
// the rc.5 read path projects mapUpdate(sanitize(ResourceAttributes), …),
// so the seed table must carry the column or the chDB round-trip 502s with
// UNKNOWN_IDENTIFIER. Each INSERT is column-explicit (sans
// ResourceAttributes) so the empty default fills it.
const laneSeed = `CREATE OR REPLACE TABLE otel_metrics_sum (
  MetricName String,
  Attributes Map(String, String),
  ResourceAttributes Map(String, String) DEFAULT map(),
  ServiceName LowCardinality(String),
  TimeUnix DateTime64(9),
  Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_sum (MetricName, Attributes, ServiceName, TimeUnix, Value)
SELECT 'http_requests_total', map('job', 'a'), 'svc',
  toDateTime64('2026-06-13 00:00:00', 9) + toIntervalSecond(number * 15),
  toFloat64(number)
FROM numbers(241);
INSERT INTO otel_metrics_sum (MetricName, Attributes, ServiceName, TimeUnix, Value)
SELECT 'http_requests_total', map('job', 'b'), 'svc',
  toDateTime64('2026-06-13 00:00:00', 9) + toIntervalSecond(number * 15),
  toFloat64(number * 2)
FROM numbers(241);
INSERT INTO otel_metrics_sum (MetricName, Attributes, ServiceName, TimeUnix, Value) VALUES
  ('http_requests_total', map('job', 'c'), 'svc', toDateTime64('2026-06-13 00:10:00', 9), 5.0),
  ('http_requests_total', map('job', 'c'), 'svc', toDateTime64('2026-06-13 00:10:00', 9), 9.0);
INSERT INTO otel_metrics_sum (MetricName, Attributes, ServiceName, TimeUnix, Value)
SELECT 'http_errors_total', map('job', 'a'), 'svc',
  toDateTime64('2026-06-13 00:00:00', 9) + toIntervalSecond(number * 15),
  toFloat64(number) * 0.5
FROM numbers(241);
INSERT INTO otel_metrics_sum (MetricName, Attributes, ServiceName, TimeUnix, Value)
SELECT 'http_errors_total', map('job', 'b'), 'svc',
  toDateTime64('2026-06-13 00:00:00', 9) + toIntervalSecond(number * 15),
  toFloat64(number)
FROM numbers(241);
INSERT INTO otel_metrics_sum (MetricName, Attributes, ServiceName, TimeUnix, Value) VALUES
  ('http_errors_total', map('job', 'c'), 'svc', toDateTime64('2026-06-13 00:10:00', 9), 3.0),
  ('http_errors_total', map('job', 'c'), 'svc', toDateTime64('2026-06-13 00:10:00', 9), 7.0);
INSERT INTO otel_metrics_sum (MetricName, Attributes, ServiceName, TimeUnix, Value)
SELECT 'http_requests_total', map('job', 'd'), 'svc',
  toDateTime64('2026-06-13 00:00:00', 9) + toIntervalSecond(number * 15),
  toFloat64(100)
FROM numbers(241);
INSERT INTO otel_metrics_sum (MetricName, Attributes, ServiceName, TimeUnix, Value)
SELECT 'http_errors_total', map('job', 'd'), 'svc',
  toDateTime64('2026-06-13 00:00:00', 9) + toIntervalSecond(number * 15),
  toFloat64(100)
FROM numbers(241);`

// laneFixtures are the eligible shapes the lane proves. Each is a real shape
// the Planner routes under sharded mode over laneSeed:
//
//   - rate(...)        — bare matrix RangeWindow, per-series Attributes carried.
//   - sum(rate(...))   — cross-series total (empty-Attributes output).
//   - sum by (job)(..) — keyed aggregate (single-key Attributes output).
//   - http_requests_total — a BARE instant-vector selector that lowers to a
//     RangeLWR (last-with-respect-to) spine, the phase-3 widened routable
//     family. It exercises the RangeLWR re-anchor arm under chDB: each shard
//     re-grids its [Start, End] and emits the most-recent in-window sample per
//     anchor, and the oldest-first concatenation must equal route A's single
//     pass exactly. (No rate arithmetic → no NaN cell, but it shares every
//     anchor_ts across job=a / job=b, so it adds duplicate-timestamp coverage.)
//
// The bare matrix shapes carry duplicate output timestamps (job=a / job=b share
// every anchor_ts before aggregation; sum by (job) keeps the per-anchor
// duplication across the surviving keys). The vector-vector RATIO shapes below
// additionally carry the literal-nan cell (job=d's 0/0). Together the fixture
// set satisfies the combined NaN / duplicate-timestamp boundary-coverage gates.
var laneFixtures = []string{
	"rate(http_requests_total[5m])",
	"sum(rate(http_requests_total[5m]))",
	"sum by (job) (rate(http_requests_total[5m]))",
	"http_requests_total",
	// Step-aligned vector-vector joins — the shapes this PR unlocks. In
	// range mode both arms produce a per-step matrix, so the lowering sets
	// StepAligned=true and the join step-aligns on the per-anchor
	// TimestampColumn (the join key includes the anchor timestamp), making
	// every joined row a per-(match-key, anchor) reduce that route B slices
	// safely. The job=d flat-counter arm rate()s to 0 on BOTH metrics, so the
	// ratio for job=d is 0/0 = literal nan at every anchor — the NaN coverage.
	//
	// - one-to-one on {job}: both arms `sum by (job)` leave a single {job}
	//   key, matched one-to-one (default on-all-labels).
	"sum by (job) (rate(http_requests_total[5m])) / sum by (job) (rate(http_errors_total[5m]))",
	// - on(job) group_left(): exercises the CardManyToOne emitter path (the
	//   per-(match-key, anchor) dedup throwIf(uniqExact>1) + Include
	//   mapConcat). With one series per (job, anchor) the uniqueness guard is
	//   satisfied; the parity proof is that route A's dedup and each shard's
	//   dedup agree because the anchor timestamp is in the join key.
	"sum by (job) (rate(http_requests_total[5m])) / on (job) group_left () sum by (job) (rate(http_errors_total[5m]))",
	// - asymmetric per-arm offset: only the errors arm carries `offset 5m`, so
	//   the two arms re-anchor to the same shard grid but each keeps its OWN
	//   offset window. This is the join shape a single global (ΣRange,
	//   one-offset) scan floor would mishandle; the parity proof is that route
	//   B's per-arm re-gridding preserves each arm's offset exactly, so the
	//   sliced ratio equals route A's. The offset drops the first ~10m of
	//   anchors on the errors arm, but job=a/job=b stay dense afterward.
	"sum by (job) (rate(http_requests_total[5m])) / sum by (job) (rate(http_errors_total[5m] offset 5m))",
}

// TestSolver_AvsB_ChDB_Differential is the per-PR parity workhorse. For each
// laneFixture it force-routes the optimized plan under Mode="sharded",
// executes route A and the K shards under chDB over laneSeed, concatenates
// the shard results oldest-first, and asserts the route-B multiset equals
// route A's exactly (zero diffs) under the NaN-stable comparator.
func TestSolver_AvsB_ChDB_Differential(t *testing.T) {
	ctx := context.Background()
	db := openLaneChDB(t)
	applyLaneSeed(t, db, laneSeed)

	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeSharded // K_min routing: every eligible plan routes.
	if err := cfg.Validate(); err != nil {
		t.Fatalf("sharded Config invalid: %v", err)
	}

	routed := 0
	totalNaNCells := 0
	totalDupTimestampGroups := 0
	for _, query := range laneFixtures {
		plan := optimizedPlan(t, ctx, query)

		pl := &solver.Planner{Cfg: cfg}
		gs, ge, gstep := solver.GridOf(plan)
		dec, isRouted := pl.Plan(plan, solver.RequestMeta{
			Lang:  solver.LangPromQL,
			Start: gs,
			End:   ge,
			Step:  gstep,
		})

		// Coverage gate: a fixture the Planner declines to route is
		// known-untested, not silently passed. Fail hard, never skip.
		if !isRouted {
			t.Errorf("fixture %q did NOT force-route under Mode=sharded (reason=%q); "+
				"the A-vs-B lane only proves parity for routed plans — a non-routed "+
				"fixture is a coverage hole, not a pass", query, dec.Reason)
			continue
		}
		if dec.K < 2 {
			t.Errorf("fixture %q routed with K=%d, want K >= 2", query, dec.K)
			continue
		}

		// Route A: emit + execute the whole optimized plan.
		aSQL, aArgs, err := chsql.Emit(ctx, plan)
		if err != nil {
			t.Errorf("emit route A for %q: %v", query, err)
			continue
		}
		routeA := execLane(t, db, query, "route-A", aSQL, aArgs)

		// Route B: emit + execute every shard, concatenating oldest-first
		// (dec.Slices is oldest-first — the order shardCursor drains them).
		var routeB [][]any
		for _, sl := range dec.Slices {
			sSQL, sArgs, err := chsql.Emit(ctx, sl.Plan)
			if err != nil {
				t.Errorf("emit shard %d [%v,%v] for %q: %v", sl.Index, sl.Start, sl.End, query, err)
				routeB = nil
				break
			}
			label := fmt.Sprintf("shard-%d", sl.Index)
			routeB = append(routeB, execLane(t, db, query, label, sSQL, sArgs)...)
		}

		if len(routeA) == 0 {
			t.Errorf("fixture %q: route A returned zero rows — the seed does not "+
				"exercise this shape; an empty oracle proves nothing", query)
			continue
		}

		stats := assertRowSetsEqual(t, query, routeA, routeB)
		routed++
		totalNaNCells += stats.nanCells
		totalDupTimestampGroups += stats.dupTimestampGroups

		t.Logf("fixture %q: routed K=%d, route-A rows=%d, route-B rows=%d "+
			"(across %d shards), NaN value-cells=%d, duplicate-timestamp groups=%d — zero diffs",
			query, dec.K, len(routeA), len(routeB), len(dec.Slices),
			stats.nanCells, stats.dupTimestampGroups)
	}

	if routed != len(laneFixtures) {
		t.Fatalf("force-routed %d/%d fixtures — every lane fixture MUST route under "+
			"Mode=sharded (else it is a known-untested coverage hole)",
			routed, len(laneFixtures))
	}

	// Boundary-coverage gate: the comparator's NaN-bit-class and
	// duplicate-multiplicity paths MUST have been exercised on real compared
	// data — else a seed change could silently neuter the comparator while the
	// lane stays green. A zero count here means the boundary fixtures stopped
	// emitting the boundary case, which is a coverage regression to fix at the
	// seed, never to tolerate.
	if totalNaNCells == 0 {
		t.Fatalf("comparator NaN path UNEXERCISED: no NaN value-cell appeared in any " +
			"route-A result — the flat-counter job=d must rate() to 0 on both ratio arms so 0/0 emits literal nan")
	}
	if totalDupTimestampGroups == 0 {
		t.Fatalf("comparator duplicate-timestamp path UNEXERCISED: no timestamp appeared " +
			"in more than one route-A row — the multi-series seed must share anchor_ts")
	}

	t.Logf("A-vs-B chDB differential: %d/%d fixtures force-routed and proved route-B == route-A "+
		"(NaN cells exercised=%d, duplicate-timestamp groups exercised=%d)",
		routed, len(laneFixtures), totalNaNCells, totalDupTimestampGroups)
}

// optimizedPlan lowers query at the lane grid and runs the default optimizer,
// returning the post-optimize plan the Planner classifies and chsql.Emit
// serializes — the exact route-A pipeline.
func optimizedPlan(t *testing.T, ctx context.Context, query string) chplan.Node {
	t.Helper()
	return optimizedPlanAt(t, ctx, query, laneStart, laneEnd, laneStep)
}

// optimizedPlanAt is optimizedPlan generalized over the grid: the
// live-edge boundary case below anchors its window near a fixed synthetic
// "now" instead of the fixed laneStart/laneEnd grid every other fixture in
// this file shares.
func optimizedPlanAt(t *testing.T, ctx context.Context, query string, start, end time.Time, step time.Duration) chplan.Node {
	t.Helper()
	p := parser.NewParser(parser.Options{})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	plan, err := promql.LowerAtRange(ctx, expr, schema.DefaultOTelMetrics(), start, end, step)
	if err != nil {
		t.Fatalf("lower %q: %v", query, err)
	}
	return optimizer.Default().Run(ctx, plan)
}

// openLaneChDB returns a fresh ephemeral chDB session (empty DSN → temp-dir
// session torn down with the connection). Mirrors test/spec/runner_chdb.go's
// openChDB.
func openLaneChDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}
	return db
}

// applyLaneSeed splits the seed on top-level semicolons (single-quoted
// strings keep their semicolons literal) and exec's each statement. The
// CREATE OR REPLACE form makes a re-run inside chdb-go's shared process
// engine idempotent.
func applyLaneSeed(t *testing.T, db *sql.DB, seed string) {
	t.Helper()
	for _, stmt := range splitStatements(seed) {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed exec failed:\n--- stmt ---\n%s\n--- err ---\n%v", stmt, err)
		}
	}
}

// splitStatements splits a multi-statement script on top-level semicolons,
// keeping semicolons inside single-quoted string literals intact. Mirrors the
// helper of the same name in test/spec/runner_chdb.go.
func splitStatements(s string) []string {
	var (
		out   []string
		buf   strings.Builder
		inStr bool
		esc   bool
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
			buf.WriteByte(c)
		case c == '\\' && inStr:
			esc = true
			buf.WriteByte(c)
		case c == '\'':
			inStr = !inStr
			buf.WriteByte(c)
		case c == ';' && !inStr:
			out = append(out, buf.String())
			buf.Reset()
		default:
			buf.WriteByte(c)
		}
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}

// wrapMapColumns wraps an emitted statement so its Map-typed `Attributes`
// output column is stringified server-side via toJSONString(...). chdb-go's
// parquet driver panics on a native Map scan (the documented gap mirrored by
// test/spec/runner_chdb.go's rewriteMapProjections), so every matrix shape's
// `Attributes` projection must be flattened before the Go side scans it.
//
// The wrap is applied IDENTICALLY to route A and every shard, so it cannot
// hide a divergence: it only moves `Attributes` to the end of the projection
// and JSON-encodes it, symmetrically on both sides. Every lane fixture emits
// exactly one Map column named `Attributes`, so the single `EXCEPT` covers
// all shapes.
func wrapMapColumns(sql string) string {
	return "SELECT * EXCEPT (`Attributes`), toJSONString(`Attributes`) AS `Attributes` FROM (" + sql + ")"
}

// execLane runs one emitted statement (Map columns wrapped) against the chDB
// session and returns the decoded rows. Cells are scanned into *any so we
// receive the driver-native Go value (float64, time.Time, string, []byte,
// nil) without instantiating rows.ColumnTypes() (which panics on Map columns).
func execLane(t *testing.T, db *sql.DB, query, label, sql string, args []any) [][]any {
	t.Helper()
	wrapped := wrapMapColumns(sql)
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("%s / %s query failed:\n--- sql ---\n%s\n--- args ---\n%#v\n--- err ---\n%v",
			query, label, wrapped, args, err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("%s / %s rows.Columns: %v", query, label, err)
	}
	colCount := len(cols)

	var out [][]any
	for rows.Next() {
		cells := make([]any, colCount)
		ptrs := make([]any, colCount)
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("%s / %s scan: %v", query, label, err)
		}
		out = append(out, cells)
	}
	if err := tolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("%s / %s rows.Err: %v", query, label, err)
	}
	return out
}

// chdbEOFSentinel is the spurious end-of-iteration error chdb-go's parquet
// driver returns instead of io.EOF (chdb-go v1.11.0's `return
// fmt.Errorf("empty row")`). It surfaces on rows.Err() and must be ignored;
// any other error is real. Mirrors test/spec/runner_chdb.go.
const chdbEOFSentinel = "empty row"

func tolerantRowsErr(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), chdbEOFSentinel) {
		return nil
	}
	return err
}

// laneStats reports what the comparator actually exercised, so the lane can
// PROVE (not assume) that the seeded NaN and duplicate-timestamp boundary
// cases were present in the compared data.
type laneStats struct {
	// nanCells is the number of value cells in route A that are NaN.
	nanCells int
	// dupTimestampGroups is the number of distinct timestamp values that
	// appear in more than one route-A row (the duplicate-timestamp coverage).
	dupTimestampGroups int
}

// assertRowSetsEqual fails t unless routeB is an exact, multiplicity-faithful
// permutation of routeA under the NaN-stable comparator. It returns the
// boundary-coverage stats it observed.
//
// The comparison is a sorted, index-aligned, cell-by-cell compare:
//
//   - sortLaneRows imposes a NaN-stable TOTAL ORDER (key per cell =
//     (isNaN, value) for floats; NaN sorts after every finite value, so two
//     NaN cells are adjacent and compare equal). Sorting both sides and
//     walking them in lockstep makes the comparison faithful to row
//     MULTIPLICITY — duplicate rows (same labels, same anchor_ts, same
//     value) must appear the same number of times on both sides.
//
//   - cellsEqual treats NaN == NaN by BIT-CLASS (math.IsNaN on both), which
//     reflect.DeepEqual does NOT (IEEE NaN != NaN). Route A legitimately
//     emits literal nan (the job=c dup-input-timestamp window), so a
//     reflect.DeepEqual comparison would spuriously fail every NaN row.
func assertRowSetsEqual(t *testing.T, query string, routeA, routeB [][]any) laneStats {
	t.Helper()

	stats := laneStats{
		nanCells:           countNaNCells(routeA),
		dupTimestampGroups: countDuplicateTimestampGroups(routeA),
	}

	a := cloneRows(routeA)
	b := cloneRows(routeB)
	sortLaneRows(a)
	sortLaneRows(b)

	if len(a) != len(b) {
		t.Fatalf("fixture %q: route-A has %d rows, route-B has %d — shard union is not "+
			"a permutation of route A", query, len(a), len(b))
	}
	diffs := 0
	for i := range a {
		if !rowsEqual(a[i], b[i]) {
			diffs++
			if diffs <= 10 {
				t.Errorf("fixture %q: row %d differs\n route-A=%s\n route-B=%s",
					query, i, renderRow(a[i]), renderRow(b[i]))
			}
		}
	}
	if diffs > 0 {
		t.Fatalf("fixture %q: %d/%d rows differ between route A and concatenated route B "+
			"(ZERO diffs required for parity)", query, diffs, len(a))
	}
	return stats
}

// countNaNCells counts value cells (any float64 that is NaN) across rows.
func countNaNCells(rows [][]any) int {
	n := 0
	for _, r := range rows {
		for _, c := range r {
			if f, ok := c.(float64); ok && math.IsNaN(f) {
				n++
			}
		}
	}
	return n
}

// countDuplicateTimestampGroups counts distinct timestamp values that appear
// in more than one row — the duplicate-output-timestamp coverage. Timestamps
// surface as the driver's time.Time cells.
func countDuplicateTimestampGroups(rows [][]any) int {
	counts := map[string]int{}
	for _, r := range rows {
		for _, c := range r {
			if ts, ok := c.(time.Time); ok {
				counts[ts.UTC().Format(time.RFC3339Nano)]++
			}
		}
	}
	groups := 0
	for _, c := range counts {
		if c > 1 {
			groups++
		}
	}
	return groups
}

// cloneRows deep-copies the outer + inner slices so the in-place sort does not
// reorder the caller's slice (route A's rows are also used for the stats).
func cloneRows(rows [][]any) [][]any {
	out := make([][]any, len(rows))
	for i, r := range rows {
		nr := make([]any, len(r))
		copy(nr, r)
		out[i] = nr
	}
	return out
}

// sortLaneRows sorts rows in-place under the NaN-stable total order: rows are
// ordered by their canonical key string, where each cell contributes a
// totally-ordered token. Float cells use key (isNaN, value): NaN is tagged so
// it sorts after every finite value and two NaNs are adjacent (and so compare
// equal under rowsEqual). The key is used ONLY for ordering; equality is
// re-checked structurally by rowsEqual, never by key-string identity.
func sortLaneRows(rows [][]any) {
	sort.SliceStable(rows, func(i, j int) bool {
		return laneRowKey(rows[i]) < laneRowKey(rows[j])
	})
}

// laneRowKey renders a row as a totally-ordered key string. Float values are
// fixed-width zero-padded with a sign and a NaN tag so lexical order matches
// numeric order and NaN sorts last (key = (isNaN, value)).
func laneRowKey(row []any) string {
	var b strings.Builder
	for _, c := range row {
		b.WriteByte('|')
		b.WriteString(cellKey(c))
	}
	return b.String()
}

// cellKey renders one cell as a totally-ordered token.
func cellKey(c any) string {
	switch x := c.(type) {
	case nil:
		return "0:nil"
	case float64:
		if math.IsNaN(x) {
			// NaN tag '9' sorts after the finite tag '1'; the (isNaN, value)
			// key — NaN last, finite by value.
			return "9:NaN"
		}
		// Order-preserving fixed-width encoding: sign flag then the bit
		// pattern won't help lexical order across magnitudes, so format the
		// magnitude with a fixed exponent width via %+027.10e (sign + 0-padded
		// mantissa + exponent). %e renders -Inf/+Inf deterministically too.
		return "1:" + fmt.Sprintf("%+027.10e", x)
	case time.Time:
		return "2:" + x.UTC().Format(time.RFC3339Nano)
	case []byte:
		return "3:" + string(x)
	case string:
		return "3:" + x
	default:
		return "8:" + fmt.Sprintf("%v", x)
	}
}

// rowsEqual reports whether two same-length rows are cell-wise equal under the
// NaN-bit-class rule.
func rowsEqual(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !cellsEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// cellsEqual compares two cells with NaN == NaN by bit-class. Floats compare
// by exact equality EXCEPT both-NaN, which is true (reflect.DeepEqual /
// `==` both make NaN != NaN). time.Time compares by instant; []byte/string by
// content; everything else by ==.
func cellsEqual(a, b any) bool {
	af, aok := a.(float64)
	bf, bok := b.(float64)
	if aok && bok {
		if math.IsNaN(af) && math.IsNaN(bf) {
			return true
		}
		return af == bf
	}
	if at, ok := a.(time.Time); ok {
		bt, ok := b.(time.Time)
		return ok && at.UTC().Equal(bt.UTC())
	}
	as := asString(a)
	bs := asString(b)
	if as != nil || bs != nil {
		return as != nil && bs != nil && *as == *bs
	}
	return a == b
}

// asString returns a pointer to the string content of a string/[]byte cell,
// or nil for any other type — so cellsEqual can treat the driver's String and
// FixedString/[]byte returns uniformly.
func asString(v any) *string {
	switch x := v.(type) {
	case string:
		return &x
	case []byte:
		s := string(x)
		return &s
	default:
		return nil
	}
}

// renderRow renders a row for a diff message with NaN/Inf made visible.
func renderRow(row []any) string {
	parts := make([]string, len(row))
	for i, c := range row {
		switch x := c.(type) {
		case float64:
			switch {
			case math.IsNaN(x):
				parts[i] = "NaN"
			case math.IsInf(x, +1):
				parts[i] = "+Inf"
			case math.IsInf(x, -1):
				parts[i] = "-Inf"
			default:
				parts[i] = fmt.Sprintf("%g", x)
			}
		case time.Time:
			parts[i] = x.UTC().Format(time.RFC3339Nano)
		case []byte:
			parts[i] = string(x)
		default:
			parts[i] = fmt.Sprintf("%v", c)
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// ---------------------------------------------------------------------
// chDB-backed solver.CursorQuerier — drives the REAL Executor/shardCursor
// orchestration (admission, the composed output-row cap, the wall-clock
// deadline) over genuine chDB result cardinalities, rather than the SQL-text
// comparison the differential loop above performs. chdbQuerier adapts the
// existing execLane/wrapMapColumns machinery to solver.CursorQuerier so
// Executor.Execute runs unmodified against a real ClickHouse engine.
// ---------------------------------------------------------------------

// chdbQuerier implements solver.CursorQuerier by executing each shard's SQL
// against a live chDB session, through the same wrapMapColumns shim the
// route-A/route-B comparator above uses. sleep, when > 0, wraps every query
// behind a forced ClickHouse-side sleep() so the deadline contract case
// below can assert the wall-clock timeout path against genuine (deliberately
// slowed) execution latency instead of a synthetic clock double.
type chdbQuerier struct {
	db                  *sql.DB
	maxQueryMemoryBytes int64
	sleep               time.Duration
	// opened counts QueryCursor calls. The Executor runs its shards under an
	// errgroup with SetLimit(P_eff), so every shard's QueryCursor lands on a
	// different goroutine concurrently — a plain int here loses increments to
	// interleaved read-modify-write and under-reports the open count, which
	// reads as "a shard never opened" in the assertions below.
	opened atomic.Int64
}

func (q *chdbQuerier) MaxQueryMemoryBytes() int64 { return q.maxQueryMemoryBytes }

func (q *chdbQuerier) QueryCursor(ctx context.Context, sqlText string, args ...any) (chclient.Cursor, error) {
	q.opened.Add(1)
	wrapped := wrapMapColumns(sqlText)
	if q.sleep > 0 {
		// CROSS JOIN forces ClickHouse to evaluate the scalar sleep()
		// subquery as a real data source instead of eliding it as dead
		// code — a bare WITH-clause scalar nothing selects from gets
		// optimized away, but a CROSS JOIN operand cannot be.
		wrapped = fmt.Sprintf(
			"SELECT * FROM (%s) AS _lane CROSS JOIN (SELECT sleep(%f)) AS _delay",
			wrapped, q.sleep.Seconds(),
		)
	}
	rows, err := q.db.QueryContext(ctx, wrapped, args...)
	if err != nil {
		return nil, err
	}
	return &chdbCursor{rows: rows}, nil
}

// chdbCursor adapts a chdb-go *sql.Rows into chclient.Cursor. It decodes
// columns by NAME, not position, so extra columns — the sleep() cross-join
// dummy, or wrapMapColumns's Attributes-last reordering — are tolerated
// without hand-tuning scan order per fixture.
type chdbCursor struct {
	rows      *sql.Rows
	cols      []string
	idx       map[string]int
	cur       chclient.Sample
	err       error
	inspected int64
}

// probeColumns latches the result-set column names on first use.
func (c *chdbCursor) probeColumns() bool {
	if c.cols != nil {
		return true
	}
	cols, err := c.rows.Columns()
	if err != nil {
		c.err = fmt.Errorf("chdb cursor columns: %w", err)
		return false
	}
	c.cols = cols
	c.idx = make(map[string]int, len(cols))
	for i, name := range cols {
		c.idx[name] = i
	}
	return true
}

func (c *chdbCursor) Next() bool {
	if c.err != nil {
		return false
	}
	if !c.probeColumns() {
		return false
	}
	if !c.rows.Next() {
		if err := tolerantRowsErr(c.rows.Err()); err != nil {
			c.err = err
		}
		return false
	}
	vals := make([]any, len(c.cols))
	ptrs := make([]any, len(c.cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := c.rows.Scan(ptrs...); err != nil {
		c.err = fmt.Errorf("chdb cursor scan: %w", err)
		return false
	}
	var s chclient.Sample
	if i, ok := c.idx["MetricName"]; ok {
		s.MetricName, _ = vals[i].(string)
	}
	if i, ok := c.idx["TimeUnix"]; ok {
		s.Timestamp, _ = vals[i].(time.Time)
	}
	if i, ok := c.idx["Value"]; ok {
		s.Value = cellAsFloat64(vals[i])
	}
	if i, ok := c.idx["Attributes"]; ok {
		labels := map[string]string{}
		if raw, ok := vals[i].(string); ok && raw != "" {
			if err := json.Unmarshal([]byte(raw), &labels); err != nil {
				c.err = fmt.Errorf("chdb cursor unmarshal Attributes: %w", err)
				return false
			}
		}
		s.Labels = labels
	}
	c.cur = s
	c.inspected++
	return true
}

func (c *chdbCursor) Sample() chclient.Sample { return c.cur }
func (c *chdbCursor) Err() error              { return c.err }
func (c *chdbCursor) Close() error            { return c.rows.Close() }
func (c *chdbCursor) Inspected() int64        { return c.inspected }

// cellAsFloat64 coerces a scanned Value cell to float64. chdb-go's driver
// already hands back a float64 for a Float64 column in every fixture this
// lane exercises; the int64 fallback covers a query that happens to project
// an integer-typed Value without pulling in reflection.
func cellAsFloat64(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	default:
		return 0
	}
}

// chsqlEmitter adapts the package-level chsql.Emit function to
// solver.SQLEmitter (mirrors internal/engine.ChsqlEmitter, which this
// solver-scoped lane does not import — the lane's dependency cone stays
// solver + chsql + chDB, matching the differential loop above).
type chsqlEmitter struct{}

func (chsqlEmitter) Emit(ctx context.Context, plan chplan.Node) (string, []any, error) {
	return chsql.Emit(ctx, plan)
}

// TestSolver_AvsB_ChDB_OutputCapContract proves the REAL contract behind
// Config.MaxOutputRows against genuine chDB-produced cardinality. Reading
// executor.go and cursor.go together shows the enforcement site is the
// COMPOSED shardCursor (cursor.go's Next, gated on cfg.MaxOutputRows), never
// the Executor and never the Planner: the Planner has already committed to
// route B before any row streams (routing is a structural, cost-based
// decision independent of actual result cardinality), and the Executor
// opens every shard's cursor unconditionally. There is no route-A fallback
// anywhere in this package for an over-cap composed result — the composed
// stream instead TRUNCATES at exactly the configured cap and the cursor's
// terminal error becomes a distinct *solver.OutputCapError. This test drives
// that real code path — not a synthetic double — against a real chDB
// dataset, so a future change to where/how the cap is enforced has to keep
// a genuine chDB result set passing, not just a fake generator.
func TestSolver_AvsB_ChDB_OutputCapContract(t *testing.T) {
	ctx := context.Background()
	db := openLaneChDB(t)
	applyLaneSeed(t, db, laneSeed)

	// A bare selector projects one row per input sample with no
	// aggregation — the widest real row count among laneFixtures, giving
	// the cap plenty of room to land strictly inside the composed stream.
	const query = "http_requests_total"
	plan := optimizedPlan(t, ctx, query)

	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeSharded
	pl := &solver.Planner{Cfg: cfg}
	gs, ge, gstep := solver.GridOf(plan)
	dec, isRouted := pl.Plan(plan, solver.RequestMeta{
		Lang: solver.LangPromQL, Start: gs, End: ge, Step: gstep,
	})
	if !isRouted || dec.K < 2 {
		t.Fatalf("fixture %q did not force-route under Mode=sharded (routed=%v K=%d) — "+
			"the output-cap contract needs a real multi-shard composition", query, isRouted, dec.K)
	}

	// Learn the REAL total row count from route A before picking a cap: the
	// cap must cross a genuine composed cardinality, never a guessed one.
	aSQL, aArgs, err := chsql.Emit(ctx, plan)
	if err != nil {
		t.Fatalf("emit route A: %v", err)
	}
	total := len(execLane(t, db, query, "route-A (cap sizing)", aSQL, aArgs))
	if total < 4 {
		t.Fatalf("fixture %q returned only %d real rows — too few to prove a mid-stream cap crossing", query, total)
	}
	// capDivisor halves the real total: the cap lands strictly inside the
	// composed stream (never at or past its end), so draining MUST observe
	// the truncation rather than a clean end-of-stream.
	const capDivisor = 2
	capRows := int64(total / capDivisor)
	cfg.MaxOutputRows = capRows
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Config invalid with MaxOutputRows=%d: %v", capRows, err)
	}

	q := &chdbQuerier{db: db}
	x := &solver.Executor{Client: q, Emitter: chsqlEmitter{}, Cfg: cfg}

	cur, _, err := x.Execute(ctx, "promql", dec, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	emitted := 0
	for cur.Next() {
		_ = cur.Sample()
		emitted++
	}
	derr := cur.Err()

	var capErr *solver.OutputCapError
	if !errors.As(derr, &capErr) {
		t.Fatalf("want *solver.OutputCapError from a real over-cap chDB composition, got %v", derr)
	}
	if capErr.Limit != cfg.MaxOutputRows {
		t.Fatalf("OutputCapError.Limit = %d, want %d", capErr.Limit, cfg.MaxOutputRows)
	}
	if int64(emitted) != cfg.MaxOutputRows {
		t.Fatalf("composed stream emitted %d rows before erroring, want EXACTLY the cap %d "+
			"(truncate-then-error, not a silent short read and not a route-A fallback)", emitted, cfg.MaxOutputRows)
	}

	if err := cur.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Close() waits for every launched shard goroutine, so by now every
	// QueryCursor call the Executor was ever going to make has happened:
	// exactly one per shard, never a retried/fallback query after the cap
	// fired.
	if opened := q.opened.Load(); opened != int64(len(dec.Slices)) {
		t.Fatalf("QueryCursor opened %d times, want exactly %d (one per shard, zero fallback queries)",
			opened, len(dec.Slices))
	}

	t.Logf("output-cap contract: real chDB route-B stream (%d total rows across %d shards) "+
		"truncated at cap=%d with *OutputCapError, %d QueryCursor calls (no fallback)",
		total, len(dec.Slices), cfg.MaxOutputRows, q.opened.Load())
}

// Wall-clock deadline margins for TestSolver_AvsB_ChDB_TimeoutContract's
// deadline_exceeded subtest. shardArtificialDelay forces genuine
// ClickHouse-side latency (via chdbQuerier's sleep-cross-join) so the
// timeout path is proven against real wall-clock execution, never a
// synthetic clock double. deadlineExceededBudget sits an order of magnitude
// below it so the timeout fires deterministically on any CI runner — a
// margin, not a race.
const (
	shardArtificialDelay   = 300 * time.Millisecond
	deadlineExceededBudget = 20 * time.Millisecond
)

// TestSolver_AvsB_ChDB_TimeoutContract proves the REAL Config.Timeout
// contract read from executor.go's wall-clock deadline block: a dedicated
// context.WithCancelCause fires errSolverTimeout when the timer elapses, and
// the composed cursor maps that cause to a distinct *solver.SolverTimeoutError
// — there is no route-A retry anywhere in this package (the ONE caller-side
// consumer of a routed failure is internal/engine's route-memo dispatch,
// which is a fresh, later request choosing a DIFFERENT route, never an
// in-flight retry of this one). Within a generous budget, route B completes
// cleanly over real chDB execution; forced past a budget too tight for any
// (deliberately slowed) shard to finish, it surfaces the typed timeout error
// instead.
func TestSolver_AvsB_ChDB_TimeoutContract(t *testing.T) {
	ctx := context.Background()
	db := openLaneChDB(t)
	applyLaneSeed(t, db, laneSeed)

	const query = "sum(rate(http_requests_total[5m]))"
	plan := optimizedPlan(t, ctx, query)

	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeSharded
	pl := &solver.Planner{Cfg: cfg}
	gs, ge, gstep := solver.GridOf(plan)
	dec, isRouted := pl.Plan(plan, solver.RequestMeta{
		Lang: solver.LangPromQL, Start: gs, End: ge, Step: gstep,
	})
	if !isRouted || dec.K < 2 {
		t.Fatalf("fixture %q did not force-route under Mode=sharded (routed=%v K=%d) — "+
			"the deadline contract needs a real multi-shard composition", query, isRouted, dec.K)
	}

	t.Run("within_budget", func(t *testing.T) {
		// cfg's Timeout is DefaultConfig's generous default (60s) — real
		// chDB execution over the seeded dataset completes in low
		// milliseconds, so route B must complete with zero error.
		q := &chdbQuerier{db: db}
		x := &solver.Executor{Client: q, Emitter: chsqlEmitter{}, Cfg: cfg}
		cur, _, err := x.Execute(ctx, "promql", dec, nil)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		defer cur.Close()
		n := 0
		for cur.Next() {
			n++
		}
		if err := cur.Err(); err != nil {
			t.Fatalf("route B must complete within a generous budget over real chDB execution, got %v", err)
		}
		if n == 0 {
			t.Fatalf("route B returned zero rows over a real chDB dataset — the seed does not exercise this shape")
		}
	})

	t.Run("deadline_exceeded", func(t *testing.T) {
		tight := cfg
		tight.Timeout = deadlineExceededBudget
		if err := tight.Validate(); err != nil {
			t.Fatalf("Config invalid with Timeout=%s: %v", tight.Timeout, err)
		}
		q := &chdbQuerier{db: db, sleep: shardArtificialDelay}
		x := &solver.Executor{Client: q, Emitter: chsqlEmitter{}, Cfg: tight}
		// Execute's own synchronous work (emit + breaker pre-flight +
		// admission + gate acquire) has nothing to do with shard query
		// latency, so it returns a cursor immediately; the deadline fires
		// from WITHIN the drain below, not from this call.
		cur, _, err := x.Execute(ctx, "promql", dec, nil)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		defer cur.Close()
		for cur.Next() {
			// Drain to the terminal error. Real rows may or may not
			// surface before the artificially slowed shard loses the
			// race against the tight deadline.
		}
		derr := cur.Err()
		var timeoutErr *solver.SolverTimeoutError
		if !errors.As(derr, &timeoutErr) {
			t.Fatalf("want *solver.SolverTimeoutError from a deadline forced past real "+
				"(artificially delayed) chDB execution, got %v", derr)
		}
		if timeoutErr.Timeout != tight.Timeout.String() {
			t.Fatalf("SolverTimeoutError.Timeout = %q, want %q", timeoutErr.Timeout, tight.Timeout.String())
		}
	})
}

// liveEdgeMarginSteps mirrors internal/engine's (route_memo_wiring.go)
// unexported liveEdgeFreshnessMarginSteps: the failure-driven route memo's
// freshEnoughForRouteMemo gate treats a request as fresh enough for route B
// only once its End has aged past this many step-widths behind "now". It is
// duplicated here as a named constant — rather than imported, since
// internal/engine sits outside this file's scope and the source constant is
// unexported — because the VALUE is exactly the contract this test pins:
// End == now - liveEdgeMarginSteps*step is the FIRST (freshest) grid anchor
// the gate opens up for route B, and this test proves route A and route B
// agree byte-for-byte at that precise edge.
const liveEdgeMarginSteps = 1

// TestSolver_AvsB_ChDB_LiveEdgeBoundary proves route A and route B produce
// byte-identical results when a query's End anchor sits EXACTLY at the
// live-edge freshness boundary route_memo_wiring.go's freshEnoughForRouteMemo
// checks — the earliest (freshest) instant the gate still lets through to
// route B. This is the disjoint-anchor equivalence proof's own boundary
// condition: a shard reading a strictly newer snapshot than an earlier shard
// is the documented exception the proof does not cover, and the gate exists
// to keep every routed request's End at least one step behind "now" — this
// test pins that "at least" is satisfied, not violated, by one grid step of
// slack (the recurring window-anchor bug class: an endpoint computed off
// now()/staleness instead of the request's own [start,end] silently drifts
// this boundary).
func TestSolver_AvsB_ChDB_LiveEdgeBoundary(t *testing.T) {
	ctx := context.Background()
	db := openLaneChDB(t)

	step := 15 * time.Second
	// now is a fixed synthetic reference instant (never time.Now()) so the
	// test is fully deterministic.
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	end := now.Add(-time.Duration(liveEdgeMarginSteps) * step)
	start := end.Add(-1 * time.Hour)

	seed := fmt.Sprintf(`CREATE OR REPLACE TABLE otel_metrics_sum (
	MetricName String,
	Attributes Map(String, String),
	ResourceAttributes Map(String, String) DEFAULT map(),
	ServiceName LowCardinality(String),
	TimeUnix DateTime64(9),
	Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_sum (MetricName, Attributes, ServiceName, TimeUnix, Value)
SELECT 'http_requests_total', map('job', 'a'), 'svc',
       toDateTime64('%[1]s', 9) + toIntervalSecond(number * 15),
       toFloat64(number)
FROM numbers(241);
INSERT INTO otel_metrics_sum (MetricName, Attributes, ServiceName, TimeUnix, Value)
SELECT 'http_requests_total', map('job', 'b'), 'svc',
       toDateTime64('%[1]s', 9) + toIntervalSecond(number * 15),
       toFloat64(number * 2)
FROM numbers(241);`, start.UTC().Format("2006-01-02 15:04:05"))
	applyLaneSeed(t, db, seed)

	const query = "sum(rate(http_requests_total[5m]))"
	plan := optimizedPlanAt(t, ctx, query, start, end, step)

	gs, ge, gstep := solver.GridOf(plan)
	if !ge.Equal(end) {
		t.Fatalf("lowered plan's grid End = %v, want the exact live-edge boundary %v — "+
			"the window-anchor bug class this test guards against", ge, end)
	}

	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeSharded
	pl := &solver.Planner{Cfg: cfg}
	dec, isRouted := pl.Plan(plan, solver.RequestMeta{
		Lang: solver.LangPromQL, Start: gs, End: ge, Step: gstep,
	})
	if !isRouted || dec.K < 2 {
		t.Fatalf("live-edge query did not force-route under Mode=sharded (routed=%v K=%d) — "+
			"the boundary parity proof needs a real multi-shard composition", isRouted, dec.K)
	}

	aSQL, aArgs, err := chsql.Emit(ctx, plan)
	if err != nil {
		t.Fatalf("emit route A: %v", err)
	}
	routeA := execLane(t, db, query, "route-A (live edge)", aSQL, aArgs)
	if len(routeA) == 0 {
		t.Fatalf("live-edge fixture returned zero rows — the seed does not reach the boundary anchor")
	}

	var routeB [][]any
	for _, sl := range dec.Slices {
		sSQL, sArgs, err := chsql.Emit(ctx, sl.Plan)
		if err != nil {
			t.Fatalf("emit shard %d [%v,%v]: %v", sl.Index, sl.Start, sl.End, err)
		}
		label := fmt.Sprintf("shard-%d (live edge)", sl.Index)
		routeB = append(routeB, execLane(t, db, query, label, sSQL, sArgs)...)
	}

	assertRowSetsEqual(t, query+" @ live-edge boundary", routeA, routeB)
	t.Logf("live-edge boundary contract: End=%s sits exactly at now-%d*step (now=%s) — "+
		"route A and route B byte-identical (%d rows across %d shards)",
		end.Format(time.RFC3339), liveEdgeMarginSteps, now.Format(time.RFC3339), len(routeA), len(dec.Slices))
}
