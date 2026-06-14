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
// The fixtures deliberately seed a NaN-emitting series (a dup-input-timestamp
// counter whose window has sampled_interval == 0, so route A legitimately
// emits literal nan) and duplicate output timestamps (two series share every
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
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/prometheus/prometheus/promql/parser"

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
//   - job=c — exactly two samples at the SAME timestamp (00:10:00). Its only
//     populated windows hold first_ts == last_ts, so sampled_interval == 0
//     and route A's rate() arithmetic legitimately emits literal nan — the
//     NaN-bit-class coverage. The dup-input-timestamp is itself the trigger.
//
// The ORDER BY does not dedup (MergeTree, not ReplacingMergeTree), so both
// job=c rows persist. Statements are newline-clean (no inline `-- comment`
// lines) so splitStatements keeps each INSERT intact.
const laneSeed = `CREATE OR REPLACE TABLE otel_metrics_sum (
  MetricName String,
  Attributes Map(String, String),
  ServiceName LowCardinality(String),
  TimeUnix DateTime64(9),
  Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_sum
SELECT 'http_requests_total', map('job', 'a'), 'svc',
  toDateTime64('2026-06-13 00:00:00', 9) + toIntervalSecond(number * 15),
  toFloat64(number)
FROM numbers(241);
INSERT INTO otel_metrics_sum
SELECT 'http_requests_total', map('job', 'b'), 'svc',
  toDateTime64('2026-06-13 00:00:00', 9) + toIntervalSecond(number * 15),
  toFloat64(number * 2)
FROM numbers(241);
INSERT INTO otel_metrics_sum VALUES
  ('http_requests_total', map('job', 'c'), 'svc', toDateTime64('2026-06-13 00:10:00', 9), 5.0),
  ('http_requests_total', map('job', 'c'), 'svc', toDateTime64('2026-06-13 00:10:00', 9), 9.0);`

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
// The matrix shapes each carry a NaN cell (the job=c window) and duplicate
// output timestamps (job=a / job=b share every anchor_ts before aggregation;
// sum by (job) keeps the per-anchor duplication across the surviving keys); the
// combined NaN / duplicate-timestamp boundary-coverage gates are satisfied
// across the whole fixture set.
var laneFixtures = []string{
	"rate(http_requests_total[5m])",
	"sum(rate(http_requests_total[5m]))",
	"sum by (job) (rate(http_requests_total[5m]))",
	"http_requests_total",
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
			"route-A result — the dup-input-timestamp seed must emit literal nan")
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
	p := parser.NewParser(parser.Options{})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	plan, err := promql.LowerAtRange(ctx, expr, schema.DefaultOTelMetrics(),
		laneStart, laneEnd, laneStep)
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
