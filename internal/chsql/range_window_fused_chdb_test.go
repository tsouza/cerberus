//go:build chdb

// Differential chDB coverage for the FUSED subquery emitter
// (range_window_fused.go) against the MATERIALIZED path it replaces.
//
// The fused emitter's contract is result-equivalence with
// emitRangeWindowOverTimeDirect{,Matrix} over
// emitWindowedArrayExtrapolatedMatrix — the shape a `query_range` over a
// PromQL subquery used to take before #1505. That materialized shape is what
// the compat lane validates against reference Prometheus, so pinning
// fused == materialized here transitively pins fused == Prometheus, including
// the property this path is most likely to get wrong: the inner subquery
// sample grid's epoch phase (PromQL evaluates inner samples at exact
// multiples of the subquery step, independent of the request grid or any
// offset — reference engine.go's evalSubquery). A one-step phase shift, the
// exact signature of the earlier materialized-path bug, changes WHICH inner
// anchors exist and therefore which values the outer reducer sees; both
// emitters are executed over the same seeded rows and every (series, anchor,
// value) triple must match.
//
// The grid axes are chosen so no case is accidentally phase-degenerate: the
// request End sits OFF the subquery-step grid, the offsets include both a
// step multiple and a non-multiple, and the sample cadence does not divide the
// subquery step.
package chsql

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

const fusedDiffTable = "otel_metrics_sum"

// fusedDiffOpenDB returns an isolated chDB database seeded with a monotone
// counter per series. `scrape` deliberately does NOT divide the subquery step
// used by the cases, so window edges land between samples and the
// extrapolation arithmetic (durationToStart / durationToEnd clamps) is
// exercised rather than skipped.
//
// The table carries the full MetricsSeedDDL column set even though this
// differential drives chplan directly and reads only Attributes / TimeUnix /
// Value. A hand-rolled three-column shape under a name in the schema's metric
// family is what #2074 was: a selector elsewhere fans over
// `merge(currentDatabase(), '^(otel_metrics_gauge|otel_metrics_sum)$')`, and a
// truncated arm makes that fan-out unresolvable.
func fusedDiffOpenDB(t *testing.T, seedStart time.Time, scrape time.Duration, nSamples int) *sql.DB {
	t.Helper()
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(chsqltest.MetricsSeedDDL(fusedDiffTable)); err != nil {
		t.Fatalf("create table: %v", err)
	}
	rows := make([]string, 0, nSamples*2)
	for i := 0; i < nSamples; i++ {
		ts := seedStart.Add(time.Duration(i) * scrape).Format("2006-01-02 15:04:05.000000000")
		// `api` is a dense counter; `worker` is sparse (every third scrape) so
		// some anchors see fewer than two samples and drop — the empty-window
		// arm both paths must agree on.
		rows = append(rows, fmt.Sprintf(
			"(map('job', 'api'), toDateTime64('%s', 9), %d.5)", ts, i*7,
		))
		if i%3 == 0 {
			rows = append(rows, fmt.Sprintf(
				"(map('job', 'worker'), toDateTime64('%s', 9), %d.25)", ts, i*3,
			))
		}
	}
	// Explicit column list: the shared seed shape has columns this differential
	// never reads, and MetricName / ResourceAttributes / ServiceName take their
	// defaults.
	insert := "INSERT INTO " + fusedDiffTable + " (Attributes, TimeUnix, Value) VALUES "
	if _, err := db.Exec(insert + strings.Join(rows, ",")); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	return db
}

// fusedDiffPlan builds the outer/inner RangeWindow pair a range-mode PromQL
// subquery lowers to. The inner grid quantities mirror
// promql.widenSubquerySpine exactly: Start = requestStart - Offset -
// subqueryRange, End = requestEnd, OuterRange = End - Start, and StepAlign set
// so the inner sample grid snaps to absolute-epoch multiples of the subquery
// step. The outer reducer keeps StepAlign false — it reports on the request's
// own start + k*step grid.
func fusedDiffPlan(
	outerFn, innerFn string,
	innerRange, subStep, subRange time.Duration,
	start, end time.Time, step, offset time.Duration,
	tsColumn string,
) *chplan.RangeWindow {
	innerStart := start.Add(-offset - subRange)
	inner := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: fusedDiffTable},
		Func:            innerFn,
		Range:           innerRange,
		Step:            subStep,
		OuterRange:      end.Sub(innerStart),
		Offset:          offset,
		StepAlign:       true,
		Start:           innerStart,
		End:             end,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	return &chplan.RangeWindow{
		Input:           inner,
		Func:            outerFn,
		Range:           subRange,
		Step:            step,
		OuterRange:      end.Sub(start),
		Offset:          offset,
		Start:           start,
		End:             end,
		TimestampColumn: tsColumn,
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// fusedDiffSQL renders the fused shape for r, asserting the entry point
// actually claimed it (a silent decline would make the differential vacuous).
func fusedDiffSQL(t *testing.T, r *chplan.RangeWindow) string {
	t.Helper()
	e := &emitter{}
	handled, err := e.tryEmitFusedSubquery(r)
	if err != nil {
		t.Fatalf("fused emit: %v", err)
	}
	if !handled {
		t.Fatalf("fused emitter declined %s(%s) — the differential would compare the "+
			"materialized path against itself", r.Func, r.Input.(*chplan.RangeWindow).Func)
	}
	return e.b.String()
}

// materializedDiffSQL renders the pre-#1505 shape: the materialized inner
// matrix under the direct-aggregate outer regroup.
func materializedDiffSQL(t *testing.T, r *chplan.RangeWindow) string {
	t.Helper()
	aggFor, ok := overTimeDirectAggFrag(r)
	if !ok {
		t.Fatalf("%s is not a direct-aggregate reducer", r.Func)
	}
	e := &emitter{}
	var err error
	if r.OuterRange > 0 {
		err = e.emitRangeWindowOverTimeDirectMatrix(r, aggFor)
	} else {
		err = e.emitRangeWindowOverTimeDirectInstant(r, aggFor)
	}
	if err != nil {
		t.Fatalf("materialized emit: %v", err)
	}
	return e.b.String()
}

// fusedDiffRow is one emitted matrix point, keyed for order-insensitive
// comparison.
type fusedDiffRow struct {
	series string
	anchor string
	value  float64
}

// fusedDiffRun executes `inner` as a subquery and returns its rows sorted by
// (series, anchor). Both emitters produce `Attributes`, `anchor_ts` and
// `Value`; row ORDER is not part of either contract (both end in a GROUP BY /
// arrayJoin whose output order ClickHouse leaves unspecified), so the
// comparison sorts.
func fusedDiffRun(t *testing.T, db *sql.DB, inner string, withAnchor bool) []fusedDiffRow {
	t.Helper()
	anchor := "''"
	if withAnchor {
		anchor = "toString(`anchor_ts`)"
	}
	q := "SELECT toString(`Attributes`) AS s, " + anchor + " AS a, `Value` AS v FROM (" + inner + ")"
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("query: %v\nSQL: %s", err, q)
	}
	defer func() { _ = rows.Close() }()
	out := make([]fusedDiffRow, 0, 64)
	for rows.Next() {
		var r fusedDiffRow
		var v any
		if err := rows.Scan(&r.series, &r.anchor, &v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		r.value = fusedDiffFloat(t, v)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v\nSQL: %s", err, q)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].series != out[j].series {
			return out[i].series < out[j].series
		}
		return out[i].anchor < out[j].anchor
	})
	return out
}

func fusedDiffFloat(t *testing.T, v any) float64 {
	t.Helper()
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case []byte:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			t.Fatalf("parse value %q: %v", x, err)
		}
		return f
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err != nil {
			t.Fatalf("parse value %q: %v", x, err)
		}
		return f
	}
	t.Fatalf("unexpected Value type %T", v)
	return 0
}

// fusedDiffValuesEqual compares two emitted values. Both paths drive the SAME
// extrapolation arithmetic, so equality is expected bit-for-bit; the relative
// tolerance only absorbs a re-association the CH optimizer is free to make
// between an inlined operand and a column alias.
func fusedDiffValuesEqual(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	if a == b {
		return true
	}
	const relTolerance = 1e-12
	scale := math.Max(math.Abs(a), math.Abs(b))
	return math.Abs(a-b) <= relTolerance*scale
}

// TestFusedSubqueryMatchesMaterialized_ChDB is the phase-alignment oracle: for
// every (outer reducer × inner func × grid phase × offset) combination the
// fused emitter must return exactly the matrix the materialized path returns.
func TestFusedSubqueryMatchesMaterialized_ChDB(t *testing.T) {
	// Sample cadence 20s from 90 minutes before the request start, i.e. 3
	// samples per minute and NOT a divisor of the 90s subquery step used
	// below, so window edges fall between samples.
	seedStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	db := fusedDiffOpenDB(t, seedStart, 20*time.Second, 400)

	// Request window: 27 minutes wide, ending `endAfter` past the seed start.
	// The default end (:47:10) sits deliberately OFF the subquery-step (90s)
	// epoch grid, so a grid that anchored on End instead of a step multiple
	// lands on different inner anchors and the differential fails.
	const requestSpan = 27 * time.Minute
	const skewedEnd = 107*time.Minute + 10*time.Second
	// alignedEnd is an exact multiple of 90s AND of 120s past the epoch, so
	// every outer anchor lands ON an inner grid anchor. That is the boundary
	// where the covered-slice bounds must round the OTHER way from a plain
	// floor: the anchor exactly AT the outer anchor is IN the right-closed
	// window and the anchor exactly at `anchor - range` is OUT of the
	// left-open one (see coveredValuesFrag / anchorGridCeilIdxFrag). Off-grid
	// ends alone never exercise that rounding.
	const alignedEnd = 102 * time.Minute

	grids := []struct {
		name     string
		endAfter time.Duration
		step     time.Duration
		subStep  time.Duration
		subRange time.Duration
		inRange  time.Duration
		offset   time.Duration
		tsColumn string
	}{
		{
			name: "aligned step, no offset", step: 5 * time.Minute, subStep: 90 * time.Second,
			subRange: 10 * time.Minute, inRange: 3 * time.Minute, tsColumn: "anchor_ts",
		},
		{
			name: "fine step, no offset", step: 30 * time.Second, subStep: 90 * time.Second,
			subRange: 6 * time.Minute, inRange: 2 * time.Minute, tsColumn: "anchor_ts",
		},
		{
			name: "offset non-multiple of sub step", step: 5 * time.Minute, subStep: 90 * time.Second,
			subRange: 10 * time.Minute, inRange: 3 * time.Minute, offset: 7 * time.Minute,
			tsColumn: "anchor_ts",
		},
		{
			name: "offset multiple of sub step", step: 5 * time.Minute, subStep: 90 * time.Second,
			subRange: 10 * time.Minute, inRange: 3 * time.Minute, offset: 4*time.Minute + 30*time.Second,
			tsColumn: "anchor_ts",
		},
		{
			name: "sub step equal to request step", step: 2 * time.Minute, subStep: 2 * time.Minute,
			subRange: 8 * time.Minute, inRange: 4 * time.Minute, tsColumn: "anchor_ts",
		},
		// Exercises the extra `<grid anchor> AS <schema ts>` projection both
		// matrix emitters add when the outer timestamp column is not the anchor
		// alias. Offset stays 0 here: with an offset the MATERIALIZED path takes
		// its per-anchor timestamp from the named column, which the inner matrix
		// emits offset-UNSHIFTED (gridAnchorFrag), and compares it against its
		// own offset-SHIFTED outer base — the two disagree by the offset. No
		// lowering builds that pair (lowerOuterRangeFnOverSubquery always names
		// the anchor alias), so the differential pins the reachable half.
		{
			name: "schema timestamp column projection", step: 5 * time.Minute, subStep: 90 * time.Second,
			subRange: 10 * time.Minute, inRange: 3 * time.Minute,
			tsColumn: "TimeUnix",
		},
		{
			name: "sub range shorter than one step", step: 5 * time.Minute, subStep: 90 * time.Second,
			subRange: 60 * time.Second, inRange: 3 * time.Minute, tsColumn: "anchor_ts",
		},
		// Boundary family: outer anchors sitting exactly ON inner grid anchors.
		{
			name: "outer anchors on the inner grid", endAfter: alignedEnd,
			step: 90 * time.Second, subStep: 90 * time.Second,
			subRange: 9 * time.Minute, inRange: 3 * time.Minute, tsColumn: "anchor_ts",
		},
		{
			name: "outer anchors on the inner grid, sub range off-grid", endAfter: alignedEnd,
			step: 3 * time.Minute, subStep: 90 * time.Second,
			subRange: 9*time.Minute + 30*time.Second, inRange: 3 * time.Minute,
			tsColumn: "anchor_ts",
		},
		{
			name: "outer anchors on the inner grid, offset on-grid", endAfter: alignedEnd,
			step: 3 * time.Minute, subStep: 90 * time.Second,
			subRange: 9 * time.Minute, inRange: 3 * time.Minute, offset: 3 * time.Minute,
			tsColumn: "anchor_ts",
		},
		{
			name: "outer anchors on the inner grid, coarse sub step", endAfter: alignedEnd,
			step: 2 * time.Minute, subStep: 2 * time.Minute,
			subRange: 10 * time.Minute, inRange: 4 * time.Minute, tsColumn: "anchor_ts",
		},
	}

	for _, outerFn := range []string{"max_over_time", "min_over_time", "count_over_time"} {
		for _, innerFn := range []string{"rate", "increase", "delta"} {
			for _, g := range grids {
				name := fmt.Sprintf("%s/%s/%s", outerFn, innerFn, g.name)
				t.Run(name, func(t *testing.T) {
					endAfter := g.endAfter
					if endAfter == 0 {
						endAfter = skewedEnd
					}
					end := seedStart.Add(endAfter)
					r := fusedDiffPlan(outerFn, innerFn, g.inRange, g.subStep, g.subRange,
						end.Add(-requestSpan), end, g.step, g.offset, g.tsColumn)
					fused := fusedDiffRun(t, db, fusedDiffSQL(t, r), true)
					mat := fusedDiffRun(t, db, materializedDiffSQL(t, r), true)
					if len(mat) == 0 {
						t.Fatalf("materialized path returned no rows — the case does not "+
							"exercise the emitters (grid %s)", g.name)
					}
					if len(fused) != len(mat) {
						t.Fatalf("row count: fused=%d materialized=%d — the fused grid "+
							"covers a different anchor set (phase shift?)\nfirst fused=%v\nfirst mat=%v",
							len(fused), len(mat), fusedDiffHead(fused), fusedDiffHead(mat))
					}
					for i := range mat {
						if fused[i].series != mat[i].series || fused[i].anchor != mat[i].anchor {
							t.Fatalf("row %d key mismatch: fused=(%s,%s) materialized=(%s,%s)",
								i, fused[i].series, fused[i].anchor, mat[i].series, mat[i].anchor)
						}
						if !fusedDiffValuesEqual(fused[i].value, mat[i].value) {
							t.Fatalf("row %d (%s @ %s): fused=%v materialized=%v",
								i, mat[i].series, mat[i].anchor, fused[i].value, mat[i].value)
						}
					}
				})
			}
		}
	}
}

func fusedDiffHead(rows []fusedDiffRow) []fusedDiffRow {
	const head = 3
	if len(rows) > head {
		return rows[:head]
	}
	return rows
}

// TestFusedSubqueryInstantMatchesMaterialized_ChDB is the instant-outer sibling
// of the matrix differential: the shape that was already fused before #1505,
// re-pinned here so the shared-grid refactor cannot silently move it.
func TestFusedSubqueryInstantMatchesMaterialized_ChDB(t *testing.T) {
	seedStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	db := fusedDiffOpenDB(t, seedStart, 20*time.Second, 400)

	end := seedStart.Add(107*time.Minute + 10*time.Second)
	for _, outerFn := range []string{"max_over_time", "min_over_time", "count_over_time"} {
		for _, innerFn := range []string{"rate", "increase", "delta"} {
			t.Run(outerFn+"/"+innerFn, func(t *testing.T) {
				r := fusedDiffPlan(outerFn, innerFn, 3*time.Minute, 90*time.Second, 10*time.Minute,
					end, end, 0, 0, "anchor_ts")
				// An instant outer reducer carries no grid.
				r.Step = 0
				r.OuterRange = 0
				r.Start = time.Time{}
				// The instant materialized arm fail-closes without the IR scan
				// bound chplan.AttachInstantScanTimeBounds establishes at Emit.
				r.InstantScanBounded = true
				fused := fusedDiffRun(t, db, fusedDiffSQL(t, r), false)
				mat := fusedDiffRun(t, db, materializedDiffSQL(t, r), false)
				if len(mat) == 0 {
					t.Fatal("materialized instant path returned no rows")
				}
				if len(fused) != len(mat) {
					t.Fatalf("row count: fused=%d materialized=%d", len(fused), len(mat))
				}
				for i := range mat {
					if fused[i].series != mat[i].series ||
						!fusedDiffValuesEqual(fused[i].value, mat[i].value) {
						t.Fatalf("row %d (%s): fused=%v materialized=%v",
							i, mat[i].series, fused[i].value, mat[i].value)
					}
				}
			})
		}
	}
}
