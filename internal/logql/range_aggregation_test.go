package logql

import (
	"context"
	"strings"
	"testing"
	"time"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerRangeAggregationAppliesUnwrapPostFilters pins the
// `e.Left.Unwrap != nil && len(e.Left.Unwrap.PostFilters) > 0` guard at
// the top of [lowerRangeAggregation]. A CONDITIONALS_NEGATION mutant
// flips `> 0` to `<= 0`, so a non-empty PostFilters slice would skip
// the branch entirely and the resulting plan would lack the
// post-filter predicate.
//
// A query carrying a real post-filter (`| status > 100`) MUST surface
// that predicate in the emitted SQL. The CONDITIONALS_NEGATION mutant
// drops the post-filter; the SQL no longer carries the `status` key.
func TestLowerRangeAggregationAppliesUnwrapPostFilters(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()

	// `status > 100` rides as a post-filter on top of `unwrap
	// latency`. The post-filter MapAccess key is "status" — a
	// distinct identifier from the unwrap's "latency" so the
	// presence-check below isolates the post-filter contribution
	// from the unwrap value extraction.
	query := `sum_over_time({app="api"} | logfmt | unwrap latency | status > 100 [5m])`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	ra, ok := expr.(*syntax.RangeAggregationExpr)
	if !ok {
		t.Fatalf("ParseExpr(%q) -> %T, want *syntax.RangeAggregationExpr", query, expr)
	}
	if ra.Left.Unwrap == nil {
		t.Fatalf("ParseExpr(%q): Unwrap is nil — fixture invalid", query)
	}
	if len(ra.Left.Unwrap.PostFilters) == 0 {
		t.Fatalf("ParseExpr(%q): PostFilters is empty — fixture invalid", query)
	}

	plan, err := lowerRangeAggregation(ra, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lowerRangeAggregation: %v", err)
	}

	// Emit the SQL and confirm the post-filter `status` key
	// surfaces as a literal argument. The CONDITIONALS_NEGATION
	// mutant would skip the AND-fold entirely; without the
	// post-filter, no `status` substring appears anywhere.
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("chsql.Emit: %v", err)
	}
	if !argsContain(args, "status") {
		t.Fatalf("emitted SQL args do not carry the post-filter key %q\nargs=%v\nsql=%s", "status", args, sqlStr)
	}
}

// argsContain reports whether any arg passed to chsql.Emit contains the
// given substring. Loki's `| status > 100` post-filter becomes a
// MapAccess(RA, 'status') in the emitted SQL, surfacing 'status' as
// a bound parameter string.
func argsContain(args []any, want string) bool {
	for _, a := range args {
		if s, ok := a.(string); ok && strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// TestLowerRangeAggregationMatrixShapeUnwrap pins the matrix-shape
// RangeWindow propagation against the unwrap variants of LogQL range
// aggregations. The loki-compatibility harness's 24h/1m matrix queries
// against `sum_over_time(... | unwrap ...)` / `avg_over_time(...)` /
// `min/max_over_time(...)` / `rate(... | unwrap ...)` each evaluate to
// 1440 anchors (24h / 1m + boundary). If the lowering forgets to set
// `RangeWindow.{Start,End,Step,OuterRange}` for any of these shapes,
// the chsql emitter takes the instant path and anchors at `now64(9)`,
// shrinking the matrix to a handful of "observed-bucket" anchors and
// dropping reference-Loki parity (the 11 `matrix length: expected=1440
// actual=4`-class failures observed on the unwrap rows of
// compatibility/loki/upstream/loki-bench/queries/exhaustive/
// unwrap-aggregations.yaml).
//
// PR #533 wired the propagation for the bare-selector ops; this test
// pins the same guarantee for every other range-op shape the lowering
// supports — line-counting (`count_over_time`), byte-counting
// (`bytes_over_time` / `bytes_rate`), and the unwrap family
// (`sum_over_time` / `avg_over_time` / `min_over_time` /
// `max_over_time` / `stddev_over_time` / `stdvar_over_time` /
// `quantile_over_time` / `rate` on unwrapped values, with and without
// post-filters, parser-stage extraction, and conversions). The shared
// `lowerRangeAggregation` body always runs the propagation block, but
// a future refactor that special-cased the unwrap path could
// re-introduce the regression — the broad table coverage makes that
// loud at the layer-1 unit-test boundary instead of waiting for
// layer-9 (compatibility harness) to surface it.
func TestLowerRangeAggregationMatrixShapeUnwrap(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	step := time.Minute

	cases := []struct {
		name     string
		query    string
		wantFunc string
	}{
		// Bare-selector matrix shapes — already pinned by PR #533, but
		// the case table keeps them adjacent to the unwrap variants so
		// a one-shot scan reads: "every range op flavour propagates
		// the matrix fields uniformly".
		{name: "count_over_time", query: `count_over_time({app="api"}[5m])`, wantFunc: "count_over_time"},
		{name: "rate", query: `rate({app="api"}[5m])`, wantFunc: "log_rate"},
		{name: "bytes_over_time", query: `bytes_over_time({app="api"}[5m])`, wantFunc: "sum_over_time"},
		{name: "bytes_rate", query: `bytes_rate({app="api"}[5m])`, wantFunc: "log_rate"},
		// Unwrap family — the regression bucket. Each variant exercises
		// a different combination of parser stage + conversion + post-
		// filter + Operation so the test catches a regression that
		// special-cases on any of those axes.
		{name: "sum_over_time-unwrap-bare", query: `sum_over_time({app="api"} | logfmt | unwrap latency [5m])`, wantFunc: "sum_over_time"},
		{name: "sum_over_time-unwrap-bytes", query: `sum_over_time({app="api"} | logfmt | unwrap bytes(size) [5m])`, wantFunc: "sum_over_time"},
		{name: "sum_over_time-unwrap-duration-postfilter", query: `sum_over_time({app="api"} | logfmt | duration != "" | unwrap duration(duration) [5m])`, wantFunc: "sum_over_time"},
		{name: "avg_over_time-unwrap-json", query: `avg_over_time({app="api"} | json | unwrap duration_ms [5m])`, wantFunc: "avg_over_time"},
		{name: "avg_over_time-unwrap-duration_seconds", query: `avg_over_time({app="api"} | logfmt | duration != "" | unwrap duration_seconds(duration) [5m])`, wantFunc: "avg_over_time"},
		{name: "min_over_time-unwrap-duration", query: `min_over_time({app="api"} | logfmt | duration != "" | unwrap duration(duration) [5m])`, wantFunc: "min_over_time"},
		{name: "max_over_time-unwrap-json", query: `max_over_time({app="api"} | json | unwrap duration_ms [5m])`, wantFunc: "max_over_time"},
		{name: "stddev_over_time-unwrap", query: `stddev_over_time({app="api"} | logfmt | unwrap latency [5m])`, wantFunc: "stddev_over_time"},
		{name: "stdvar_over_time-unwrap", query: `stdvar_over_time({app="api"} | logfmt | unwrap latency [5m])`, wantFunc: "stdvar_over_time"},
		{name: "quantile_over_time-unwrap", query: `quantile_over_time(0.99, {app="api"} | logfmt | unwrap latency [5m])`, wantFunc: "quantile_over_time"},
		{name: "rate-unwrap-duration", query: `rate({app="api"} | logfmt | duration != "" | unwrap duration(duration) [5m])`, wantFunc: "log_rate"},
		{name: "rate-unwrap-duration_seconds", query: `rate({app="api"} | logfmt | duration != "" | unwrap duration_seconds(duration) [5m])`, wantFunc: "log_rate"},
		{name: "rate-unwrap-json", query: `rate({app="api"} | json | unwrap duration_ms [5m])`, wantFunc: "log_rate"},
	}

	wantOuterRange := end.Sub(start)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := syntax.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			ra, ok := expr.(*syntax.RangeAggregationExpr)
			if !ok {
				t.Fatalf("ParseExpr(%q) -> %T, want *syntax.RangeAggregationExpr", tc.query, expr)
			}

			plan, err := lowerRangeAggregation(ra, s, lowerCtx{Start: start, End: end, Step: step})
			if err != nil {
				t.Fatalf("lowerRangeAggregation(%q): %v", tc.query, err)
			}

			rw, ok := plan.(*chplan.RangeWindow)
			if !ok {
				t.Fatalf("lowerRangeAggregation(%q) -> %T, want *chplan.RangeWindow", tc.query, plan)
			}
			if rw.Func != tc.wantFunc {
				t.Errorf("%q: Func = %q, want %q", tc.query, rw.Func, tc.wantFunc)
			}
			// The four matrix-shape fields MUST be set in range mode.
			// A regression that special-cases any range op (in
			// particular, an unwrap branch that builds the
			// RangeWindow before the propagation block) would zero
			// one or more of these fields, putting the emitter back
			// on the instant `now64(9)` path. The harness symptom is
			// `matrix length: expected=1440 actual=4` — pin the
			// invariant before that surfaces 9 layers downstream.
			if rw.OuterRange != wantOuterRange {
				t.Errorf("%q: OuterRange = %v, want %v (matrix shape requires non-zero OuterRange)",
					tc.query, rw.OuterRange, wantOuterRange)
			}
			if rw.Step != step {
				t.Errorf("%q: Step = %v, want %v", tc.query, rw.Step, step)
			}
			if !rw.Start.Equal(start) {
				t.Errorf("%q: Start = %v, want %v", tc.query, rw.Start, start)
			}
			if !rw.End.Equal(end) {
				t.Errorf("%q: End = %v, want %v", tc.query, rw.End, end)
			}
			if !isMatrixRangeWindow(rw) {
				t.Errorf("%q: isMatrixRangeWindow(rw) = false, want true", tc.query)
			}

			// Emit the SQL and confirm the per-anchor matrix scaffold
			// reaches the emitter — anchor_ts column + arrayJoin
			// fanout over the request's step grid. If propagation
			// dropped, the SQL falls back to the instant shape which
			// has no anchor_ts column.
			sqlStr, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("chsql.Emit(%q): %v", tc.query, err)
			}
			if !strings.Contains(sqlStr, "anchor_ts") {
				t.Errorf("%q: emitted SQL missing anchor_ts column (matrix shape lost)\nsql=%s", tc.query, sqlStr)
			}
			if !strings.Contains(sqlStr, "arrayJoin") {
				t.Errorf("%q: emitted SQL missing arrayJoin fanout (matrix shape lost)\nsql=%s", tc.query, sqlStr)
			}
		})
	}
}

// TestLowerRangeAggregationUnwrapStripsTargetFromIdentity pins the
// stream-identity contract for the unwrap+parser path: Loki's
// `LabelExtractorWithStages` treats no-grouping as `without (labelName)`,
// so every parser-extracted label survives the metric extraction EXCEPT
// the unwrap target itself. Cerberus previously collapsed the identity
// down to bare ResourceAttributes (+ detected_level), so any query whose
// log payload carries varying parser-extracted keys returned just a
// handful of series where reference Loki returned hundreds. The
// loki-compat 24h/1m corpus surfaces this as `matrix length:
// expected=1440 actual=4` for the 11 unwrap matrix cases that drove
// PR #574 — see compatibility/loki/upstream/loki-bench/queries/
// {regression/metric-queries.yaml,exhaustive/unwrap-aggregations.yaml}.
//
// The fix materialises the parser-merged labels into the
// `_logql_merged_labels` intermediate column (see [lowerRangeAggregation])
// and strips the unwrap target via `MapWithoutKeys{..., [unwrapIdent]}`
// against that materialised column reference. CH's "Recursive lambda
// (UNSUPPORTED_METHOD)" error fires when the strip's `mapFilter` source
// is itself a `mapApply`-bearing expression (e.g. `logfmtMergeLabels`),
// so the two-stage Project is load-bearing — pin both the IR shape
// (intermediate column present, identity strips the target) and the
// downstream-SQL invariant (no recursive lambda at the strip site).
func TestLowerRangeAggregationUnwrapStripsTargetFromIdentity(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	step := time.Minute

	cases := []struct {
		name             string
		query            string
		wantStrippedKey  string
		wantMergedColumn bool
	}{
		{
			name:             "rate-unwrap-duration-logfmt",
			query:            `rate({app="api"} | logfmt | duration != "" | unwrap duration(duration) [5m])`,
			wantStrippedKey:  "duration",
			wantMergedColumn: true,
		},
		{
			name:             "rate-unwrap-duration_ms-json",
			query:            `rate({app="api"} | json | unwrap duration_ms [5m])`,
			wantStrippedKey:  "duration_ms",
			wantMergedColumn: true,
		},
		{
			name:             "sum_over_time-unwrap-latency-logfmt",
			query:            `sum_over_time({app="api"} | logfmt | unwrap latency [5m])`,
			wantStrippedKey:  "latency",
			wantMergedColumn: true,
		},
		{
			name:             "avg_over_time-unwrap-duration_seconds-logfmt",
			query:            `avg_over_time({app="api"} | logfmt | duration != "" | unwrap duration_seconds(duration) [5m])`,
			wantStrippedKey:  "duration",
			wantMergedColumn: true,
		},
		{
			name:             "min_over_time-unwrap-bytes-logfmt",
			query:            `min_over_time({app="api"} | logfmt | unwrap bytes(size) [5m])`,
			wantStrippedKey:  "size",
			wantMergedColumn: true,
		},
		{
			name:             "max_over_time-unwrap-json",
			query:            `max_over_time({app="api"} | json | unwrap duration_ms [5m])`,
			wantStrippedKey:  "duration_ms",
			wantMergedColumn: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := syntax.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			ra, ok := expr.(*syntax.RangeAggregationExpr)
			if !ok {
				t.Fatalf("ParseExpr(%q) -> %T, want *syntax.RangeAggregationExpr", tc.query, expr)
			}

			plan, err := lowerRangeAggregation(ra, s, lowerCtx{Start: start, End: end, Step: step})
			if err != nil {
				t.Fatalf("lowerRangeAggregation(%q): %v", tc.query, err)
			}

			sqlStr, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("chsql.Emit(%q): %v", tc.query, err)
			}

			// Identity strip: the `mapFilter(NOT (k IN (?)), ...)`
			// shape compiled from `MapWithoutKeys{..., [target]}`
			// must surface in the emitted SQL. Without it, the
			// inner Project's identity collapses to bare
			// ResourceAttributes and 1440-series reference-Loki
			// behaviour regresses to cerberus's pre-fix 4-series
			// shape.
			if !strings.Contains(sqlStr, "mapFilter((k, v) -> NOT (k IN") {
				t.Errorf("%q: emitted SQL missing strip-via-mapFilter shape\nsql=%s", tc.query, sqlStr)
			}
			// Materialised intermediate column: the two-stage
			// Project rewrite is load-bearing — without it, the
			// outer `mapFilter` sees a `mapApply`-bearing source
			// (logfmt's rename-on-collision shape) and CH rejects
			// with `Recursive lambda (UNSUPPORTED_METHOD)`.
			if tc.wantMergedColumn && !strings.Contains(sqlStr, "_logql_merged_labels") {
				t.Errorf("%q: emitted SQL missing _logql_merged_labels materialised column (two-stage Project lost)\nsql=%s", tc.query, sqlStr)
			}
		})
	}
}

// TestIsMatrixRangeWindowWalksNestedAggregation pins the
// nested-aggregation traversal in [isMatrixRangeWindow]: the helper
// MUST walk through `*chplan.Aggregate` so the outer
// `lowerVectorAggregation` recognises `max(avg by (level)
// (avg_over_time(...)))` and friends as matrix-shape inputs. The
// inner aggregation wraps its Aggregate in a Project that
// re-projects bucket_ts to TimeUnix — without the Aggregate case,
// the helper returns false at the inner Aggregate boundary and the
// outer aggregation drops the per-anchor bucket from GROUP BY,
// collapsing the matrix into a single row. The user-facing symptom
// is `test endpoint returned empty` on
// `compatibility/loki/.../exhaustive/unwrap-aggregations.yaml`'s
// "Nested aggregations" case (the only remaining cerberus-side Loki
// compat failure post-#577 / #574 / #578).
func TestIsMatrixRangeWindowWalksNestedAggregation(t *testing.T) {
	t.Parallel()

	// Build the minimal nested-aggregation shape: a matrix RangeWindow
	// wrapped in an Aggregate wrapped in a Project (the canonical
	// [wrapVectorAggregateForSample] output the outer aggregation
	// consumes as input). The Project alone, the Aggregate alone, and
	// the full nest must each report true so the helper covers every
	// depth between bare RangeWindow and a multi-deep stack.
	rw := &chplan.RangeWindow{Func: "avg_over_time", OuterRange: time.Hour}
	agg := &chplan.Aggregate{Input: rw, GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "anchor_ts"}}, GroupByAliases: []string{"bucket_ts"}}
	proj := &chplan.Project{Input: agg, Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "bucket_ts"}, Alias: "TimeUnix"}}}

	if !isMatrixRangeWindow(proj) {
		t.Errorf("isMatrixRangeWindow(Project(Aggregate(RangeWindow))) = false, want true — nested-aggregation matrix shape lost")
	}
	if !isMatrixRangeWindow(agg) {
		t.Errorf("isMatrixRangeWindow(Aggregate(RangeWindow)) = false, want true — Aggregate case missing from helper")
	}
	if !isMatrixRangeWindow(rw) {
		t.Errorf("isMatrixRangeWindow(RangeWindow{OuterRange>0}) = false, want true — base case regressed")
	}

	// matrixBucketColumn must dispatch on plan depth: a bare RangeWindow
	// (or a value-shape Project/Filter over one) surfaces `anchor_ts`;
	// once an Aggregate is in the stack the wrap Project re-aliases
	// the bucket to `TimeUnix` and `anchor_ts` is no longer in scope.
	if got := matrixBucketColumn(rw); got != "anchor_ts" {
		t.Errorf("matrixBucketColumn(RangeWindow) = %q, want %q", got, "anchor_ts")
	}
	if got := matrixBucketColumn(agg); got != "TimeUnix" {
		t.Errorf("matrixBucketColumn(Aggregate(RangeWindow)) = %q, want %q", got, "TimeUnix")
	}
	if got := matrixBucketColumn(proj); got != "TimeUnix" {
		t.Errorf("matrixBucketColumn(Project(Aggregate(RangeWindow))) = %q, want %q", got, "TimeUnix")
	}
}

// TestLowerVectorAggregationNestedMatrixBucketsOnTimeUnix pins the
// downstream effect of [isMatrixRangeWindow] + [matrixBucketColumn]:
// when the outer aggregation lowers
// `max(avg by (level) (avg_over_time(...)))` in range mode, the
// emitted SQL MUST GROUP BY `TimeUnix` (not `anchor_ts`, which is no
// longer in scope past the inner Aggregate's projection). A regression
// that re-introduces the bare RangeWindow check would emit either no
// bucket GROUP BY (collapsing the matrix) or reference `anchor_ts`
// (CH error: unknown column).
func TestLowerVectorAggregationNestedMatrixBucketsOnTimeUnix(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	step := time.Minute

	query := `max(avg by (level) (avg_over_time({app="api"} | logfmt | duration != "" | unwrap duration(duration) [5m])))`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	va, ok := expr.(*syntax.VectorAggregationExpr)
	if !ok {
		t.Fatalf("ParseExpr(%q) -> %T, want *syntax.VectorAggregationExpr", query, expr)
	}

	plan, err := lowerVectorAggregation(va, s, lowerCtx{Start: start, End: end, Step: step})
	if err != nil {
		t.Fatalf("lowerVectorAggregation(%q): %v", query, err)
	}

	sqlStr, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("chsql.Emit: %v", err)
	}

	// The outer max(...) is no-grouping; in range mode it must still
	// bucket on the inner aggregation's TimeUnix column. A regression
	// that re-instates the bare-RangeWindow check skips the GROUP BY
	// entirely and the emitter falls through to `emitAggregateNoGroup`
	// — the test asserts the outer SELECT carries `GROUP BY` referencing
	// `TimeUnix` and rejects an `anchor_ts`-only bucket reference (the
	// inner RangeWindow's bucket is still in scope under that name, but
	// it's been re-projected by the inner wrap, so the outer aggregate
	// cannot reach it).
	if !strings.Contains(sqlStr, "GROUP BY `TimeUnix`") {
		t.Errorf("emitted SQL missing `GROUP BY `TimeUnix`` (nested matrix bucket lost)\nsql=%s", sqlStr)
	}
}

// TestLowerRangeAggregationExtendsMatcherWindowByIntervalPlusOffset pins
// the arithmetic on line 52 of [lowerRangeAggregation]:
//
//	innerLc := lc.withMatcherWindowExtension(e.Left.Interval + e.Left.Offset)
//
// The `+` is the load-bearing operator — the inner Scan/Filter's pre-scan
// timestamp clamp must extend back by `Interval + Offset` so the leftmost
// matrix anchor sees its full `(anchor - range, anchor]` window. An
// ARITHMETIC_BASE mutant flips `+` to `-`; the extension would be
// `Interval - Offset` (a smaller value), and the leftmost anchors of a
// /query_range matrix would silently truncate.
//
// Concrete asymmetric values are required so `+` and `-` produce
// observably distinct timestamps:
//   - Interval = 10m, Offset = 3m
//   - Original (`+`): extension = 13m → inner clamp Start = T - 13m
//   - Mutant  (`-`): extension =  7m → inner clamp Start = T -  7m
//
// The pre-scan clamp's lower bound renders as a `toDateTime64(?, 9)`
// FuncCall whose first arg is the formatted timestamp string. The test
// asserts that the EXACT expected timestamp string surfaces in the
// emitted args (and that the mutant's `-` value does NOT).
func TestLowerRangeAggregationExtendsMatcherWindowByIntervalPlusOffset(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()

	// Anchor-friendly fixture: T = 2026-01-01 12:00:00 UTC.
	// Interval = 10m, Offset = 3m → expected extension = 13m.
	// Sole condition: the `+` and `-` results must be observably distinct
	// — 13m vs 7m gives two unambiguous timestamps in the args list.
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	end := start.Add(1 * time.Hour)
	step := time.Minute

	query := `rate({app="api"}[10m] offset 3m)`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	ra, ok := expr.(*syntax.RangeAggregationExpr)
	if !ok {
		t.Fatalf("ParseExpr(%q) -> %T, want *syntax.RangeAggregationExpr", query, expr)
	}
	if ra.Left.Interval != 10*time.Minute {
		t.Fatalf("fixture invalid: Interval = %v, want 10m", ra.Left.Interval)
	}
	if ra.Left.Offset != 3*time.Minute {
		t.Fatalf("fixture invalid: Offset = %v, want 3m", ra.Left.Offset)
	}

	plan, err := lowerRangeAggregation(ra, s, lowerCtx{Start: start, End: end, Step: step})
	if err != nil {
		t.Fatalf("lowerRangeAggregation: %v", err)
	}

	_, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("chsql.Emit: %v", err)
	}

	// The pre-scan clamp's lower bound MUST carry the timestamp that
	// matches `start - (Interval + Offset) = start - 13m` exactly.
	// Format mirrors [timeLiteralExpr] in lower.go.
	wantExtended := start.Add(-(ra.Left.Interval + ra.Left.Offset)).Format("2006-01-02 15:04:05.000000000")
	if !argsContain(args, wantExtended) {
		t.Errorf("emitted SQL args do not carry extended Start %q (expected `+` arithmetic)\nargs=%v", wantExtended, args)
	}

	// Defensive: the `-` mutant would produce `start - (Interval -
	// Offset) = start - 7m`. That timestamp MUST NOT appear, otherwise
	// the mutation survives. We compute the same way the mutant would
	// to keep the assertion symmetric with the killer above.
	wrongShorter := start.Add(-(ra.Left.Interval - ra.Left.Offset)).Format("2006-01-02 15:04:05.000000000")
	if argsContain(args, wrongShorter) {
		t.Errorf("emitted SQL args carry the `-` mutant's Start %q — ARITHMETIC_BASE mutation may have flipped `+` to `-`\nargs=%v", wrongShorter, args)
	}
}

// TestLowerRangeAggregationBareUnwrapSkipsMaterialisedColumn pins the
// `&&` boundary on line 116 of [lowerRangeAggregation] AND the `!=`
// invariant on line 469 of [hasParserMergedLabels]:
//
//	if e.Left.Unwrap != nil && hasParserMergedLabels(labelsExpr, s) {
//	    // materialise into `_logql_merged_labels` intermediate column
//	}
//
//	func hasParserMergedLabels(...) bool {
//	    ...
//	    return col.Name != s.ResourceAttributesColumn
//	}
//
// A bare-unwrap query (no `| logfmt` / `| json` / `| regexp` parser
// stage) reads its labels map directly from ResourceAttributes — so
// [hasParserMergedLabels] must return false, the line-116 condition
// short-circuits to the else-if branch, and the SQL never references
// the `_logql_merged_labels` intermediate alias.
//
// Two LIVED mutants both surface as the SAME observable regression on
// this fixture, so the single assertion kills both at once:
//
//   - Line 116 INVERT_LOGICAL: `&&` → `||`. With Unwrap non-nil the
//     condition is now always true regardless of hasParserMergedLabels;
//     the materialised-column branch fires even on bare unwrap.
//
//   - Line 469 CONDITIONALS_NEGATION: `!=` → `==`. With labelsExpr =
//     `ColumnRef(ResourceAttributes)`, hasParserMergedLabels returns
//     true instead of false; line 116's condition becomes true; the
//     materialised-column branch fires.
//
// Both mutations cause the bare-unwrap SQL to carry the
// `_logql_merged_labels` alias the materialised Project introduces.
// Assert its absence — the original code path keeps the SQL lean.
func TestLowerRangeAggregationBareUnwrapSkipsMaterialisedColumn(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	step := time.Minute

	// Bare unwrap: the unwrap target is a stream label (`latency` lives
	// directly in ResourceAttributes — no parser stage interpolates).
	// labelsExpr stays as `ColumnRef(ResourceAttributes)`, so
	// hasParserMergedLabels MUST return false and the SQL MUST NOT
	// reference `_logql_merged_labels`.
	query := `sum_over_time({app="api"} | unwrap latency [5m])`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	ra, ok := expr.(*syntax.RangeAggregationExpr)
	if !ok {
		t.Fatalf("ParseExpr(%q) -> %T, want *syntax.RangeAggregationExpr", query, expr)
	}
	if ra.Left.Unwrap == nil {
		t.Fatalf("fixture invalid: Unwrap is nil")
	}

	plan, err := lowerRangeAggregation(ra, s, lowerCtx{Start: start, End: end, Step: step})
	if err != nil {
		t.Fatalf("lowerRangeAggregation: %v", err)
	}

	sqlStr, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("chsql.Emit: %v", err)
	}

	// The materialised-column alias `_logql_merged_labels` is the
	// fingerprint of the two-stage Project path. If it appears in the
	// emitted SQL for a bare-unwrap query, EITHER:
	//   - line 116's `&&` flipped to `||` (mutant entered the branch
	//     despite hasParserMergedLabels == false), OR
	//   - line 469's `!=` flipped to `==` (hasParserMergedLabels
	//     reported true for a bare ResourceAttributes ColumnRef).
	if strings.Contains(sqlStr, "_logql_merged_labels") {
		t.Errorf("bare-unwrap SQL unexpectedly carries the `_logql_merged_labels` alias\n"+
			"  → INVERT_LOGICAL on line 116 (`&&` → `||`) OR\n"+
			"  → CONDITIONALS_NEGATION on line 469 (`!=` → `==`) lived\nsql=%s", sqlStr)
	}

	// Companion positive assertion: the bare-unwrap path MUST surface
	// the `MapWithoutKeys` strip via the `mapFilter((k, v) -> NOT (k IN
	// (?)), ...)` shape compiled from the else-if branch. Without it
	// the test only checks one direction of the toggle.
	if !strings.Contains(sqlStr, "mapFilter((k, v) -> NOT (k IN") {
		t.Errorf("bare-unwrap SQL missing the else-if branch's mapFilter strip shape\nsql=%s", sqlStr)
	}
}

// TestLowerRangeAggregationParserUnwrapMaterialisesIntermediateColumn
// is the dual of [TestLowerRangeAggregationBareUnwrapSkipsMaterialisedColumn]:
// when the unwrap query DOES carry a parser stage, the SQL MUST surface
// the `_logql_merged_labels` materialised-column alias. Together with
// the bare-unwrap test, this pins both legs of the line-116 `&&`
// condition and the line-469 `!=` return so a flipped operator can't
// pass both halves silently.
//
// The line-116 INVERT_LOGICAL mutant flips `&&` to `||`. With Unwrap
// non-nil AND hasParserMergedLabels true, both legs of the original
// `&&` are true so the mutant's `||` also evaluates true — the test
// covers this case to confirm the happy path stays intact (no false
// positive from the dual assertion).
//
// The line-469 CONDITIONALS_NEGATION mutant flips `!=` to `==`. With a
// parser stage labelsExpr is a `mapConcat(...)` (not a ColumnRef), so
// the ok-cast `col, ok := labelsExpr.(*chplan.ColumnRef)` is false and
// hasParserMergedLabels returns true via the first return statement —
// the mutant on the second return doesn't fire here. This test pins
// that the parser path still materialises so the bare-unwrap test
// above genuinely isolates the mutation.
func TestLowerRangeAggregationParserUnwrapMaterialisesIntermediateColumn(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	step := time.Minute

	// Parser stage `| logfmt` wraps labelsExpr in a mapConcat — the
	// non-ColumnRef path of hasParserMergedLabels — so line 116 fires
	// and the materialised intermediate column appears.
	query := `sum_over_time({app="api"} | logfmt | unwrap latency [5m])`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	ra := expr.(*syntax.RangeAggregationExpr)

	plan, err := lowerRangeAggregation(ra, s, lowerCtx{Start: start, End: end, Step: step})
	if err != nil {
		t.Fatalf("lowerRangeAggregation: %v", err)
	}

	sqlStr, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("chsql.Emit: %v", err)
	}

	if !strings.Contains(sqlStr, "_logql_merged_labels") {
		t.Errorf("parser-unwrap SQL missing the `_logql_merged_labels` materialised alias\nsql=%s", sqlStr)
	}
}

// TestLowerRangeAggregationUnwrapPostFilterPreservesStreamSelector pins
// the `pred == nil` accumulator branch on line 256 of
// [applyUnwrapPostFilters]:
//
//	if pred == nil {
//	    pred = extra
//	} else {
//	    pred = &chplan.Binary{Op: chplan.OpAnd, Left: pred, Right: extra}
//	}
//
// On entry, `pred` is initialised from the inner Filter's Predicate
// (which carries the stream-selector matchers AND any time-window
// clamp). The original `==` takes the AND-fold branch on the first
// post-filter iteration when `pred` is already non-nil — preserving
// the stream selector.
//
// A CONDITIONALS_NEGATION mutant flips `==` to `!=`:
//   - With pred = P (non-nil from f.Predicate), `P != nil` is true →
//     `pred = extra` REPLACES the accumulated predicate with just the
//     post-filter. The stream selector ("app" = "api") and the
//     time-window clamp are SILENTLY DROPPED from the emitted SQL.
//
// The killer assertion: every component of the inner predicate MUST
// surface in the emitted args:
//   - "app" + "api"            — stream matcher
//   - "status"                 — post-filter MapAccess key
//   - the formatted Start time — time-window lower bound
//
// The existing [TestLowerRangeAggregationAppliesUnwrapPostFilters] only
// checks "status" survives — a `!=` mutant would still emit "status"
// (the post-filter replaces, not omits) but would drop the stream
// selector and the time clamp. This stricter assertion catches that.
func TestLowerRangeAggregationUnwrapPostFilterPreservesStreamSelector(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	// Asymmetric Start so the formatted timestamp is unique in args.
	start := time.Date(2026, 3, 17, 9, 42, 18, 0, time.UTC)
	end := start.Add(time.Hour)
	step := time.Minute

	query := `sum_over_time({app="api"} | logfmt | unwrap latency | status > 100 [5m])`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	ra := expr.(*syntax.RangeAggregationExpr)
	if ra.Left.Unwrap == nil {
		t.Fatalf("fixture invalid: Unwrap is nil")
	}
	if len(ra.Left.Unwrap.PostFilters) == 0 {
		t.Fatalf("fixture invalid: PostFilters is empty")
	}

	plan, err := lowerRangeAggregation(ra, s, lowerCtx{Start: start, End: end, Step: step})
	if err != nil {
		t.Fatalf("lowerRangeAggregation: %v", err)
	}

	_, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("chsql.Emit: %v", err)
	}

	// The post-filter key MUST be present — both original and mutant
	// reach this point. Pinning it keeps the test symmetric with the
	// existing TestLowerRangeAggregationAppliesUnwrapPostFilters but
	// is not the load-bearing assertion.
	if !argsContain(args, "status") {
		t.Fatalf("emitted SQL args missing post-filter key %q\nargs=%v", "status", args)
	}

	// LOAD-BEARING: stream selector survives the post-filter AND-fold.
	// A `==` → `!=` mutant on line 256 would replace `pred` with the
	// post-filter on its first iteration, dropping the stream
	// selector. Both "app" (the matcher's label key) and "api" (its
	// value) MUST surface as bound args.
	for _, want := range []string{"app", "api"} {
		if !argsContain(args, want) {
			t.Errorf("emitted SQL args missing stream-selector token %q "+
				"— line 256 `pred == nil` may have flipped to `!=`, "+
				"replacing the stream predicate with just the post-filter\nargs=%v",
				want, args)
		}
	}

	// LOAD-BEARING: time-window lower-bound clamp survives the post-
	// filter AND-fold. The pre-scan clamp uses the EXTENDED
	// `innerLc.Start = start - (Interval + Offset) = start - 5m`
	// (rendered by [timeLiteralExpr]). The formatted timestamp MUST
	// appear in the args; a line-256 mutation that drops the clamp
	// would erase it.
	extendedStart := start.Add(-(ra.Left.Interval + ra.Left.Offset))
	wantExtendedStart := extendedStart.Format("2006-01-02 15:04:05.000000000")
	if !argsContain(args, wantExtendedStart) {
		t.Errorf("emitted SQL args missing pre-scan clamp lower-bound %q "+
			"— line 256 mutation may have dropped the time-window predicate\nargs=%v",
			wantExtendedStart, args)
	}
}

// TestRangeAggregationGroupByEmitsLabelKeyAndValuePairs pins the args
// shape produced by [rangeAggregationGroupBy] for the explicit `by
// (...)` grouping path:
//
//	args := make([]chplan.Expr, 0, len(e.Grouping.Groups)*2)
//	for _, label := range e.Grouping.Groups {
//	    args = append(args,
//	        &chplan.LitString{V: label},
//	        levelAwareRangeGroupKey(label, s),
//	    )
//	}
//
// Each label contributes EXACTLY TWO entries: the literal key followed
// by the value expression. For N labels the resulting `map(...)`
// FuncCall has 2*N args.
//
// An ARITHMETIC_BASE mutant on line 513 flips the initial capacity hint
// `*2` to `/2`. The behaviour is OBSERVATIONALLY EQUIVALENT at the SQL
// surface — `append` grows the slice on demand, so the final FuncCall
// carries the same args regardless of initial capacity — but the test
// still pins the 2-per-label shape so any future refactor that
// genuinely DROPS args (e.g., a typo that appends only the literal or
// only the value) would be caught at the layer-1 unit-test boundary.
//
// The shape assertion uses three group labels so the test would also
// catch a regression that capped the args at the (possibly truncated)
// initial capacity instead of growing — even though Go's `append`
// makes that scenario impossible without an explicit `[:cap]` slice.
func TestRangeAggregationGroupByEmitsLabelKeyAndValuePairs(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()

	// Three labels exercise len(Groups)*2 = 6 (mutant: 6/2 = 3
	// capacity, still grown by append to 6 final length). LogQL's
	// parser only accepts `by (...)` on the range-level for the
	// quantile/avg/min/max family — `sum_over_time` rejects it. We
	// use `avg_over_time` here so the RangeAggregationExpr's
	// Grouping is populated directly without an outer
	// VectorAggregationExpr wrap.
	query := `avg_over_time({app="api"} | logfmt | unwrap latency [5m]) by (region, tenant, env)`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	ra, ok := expr.(*syntax.RangeAggregationExpr)
	if !ok {
		t.Fatalf("ParseExpr(%q) -> %T, want *syntax.RangeAggregationExpr", query, expr)
	}
	if ra.Grouping == nil || len(ra.Grouping.Groups) != 3 {
		t.Fatalf("fixture invalid: Grouping=%v Groups=%v", ra.Grouping, ra.Grouping)
	}

	got, err := rangeAggregationGroupBy(ra, s, &chplan.ColumnRef{Name: s.ResourceAttributesColumn})
	if err != nil {
		t.Fatalf("rangeAggregationGroupBy: %v", err)
	}
	assertRangeGroupByMapShape(t, got, ra)
}

// assertRangeGroupByMapShape pins the 2-args-per-label `map(...)` shape
// for the explicit `by (...)` grouping path. Split out so the `by` and
// `without` tests share the structural assertion.
func assertRangeGroupByMapShape(t *testing.T, got chplan.Expr, ra *syntax.RangeAggregationExpr) {
	t.Helper()
	fc, ok := got.(*chplan.FuncCall)
	if !ok {
		t.Fatalf("rangeAggregationGroupBy -> %T, want *chplan.FuncCall", got)
	}
	if fc.Name != "map" {
		t.Errorf("FuncCall.Name = %q, want %q", fc.Name, "map")
	}

	// Two args per label: alternating LitString{label} + key
	// expression. Three labels → 6 args. A regression that emits
	// only one arg per label (or one for every two) would surface
	// here. The `*2` ARITHMETIC_BASE mutant on the slice-cap hint
	// stays observationally equivalent (append grows on demand)
	// but the structural assertion still pins the surface area.
	wantArgs := len(ra.Grouping.Groups) * 2
	if len(fc.Args) != wantArgs {
		t.Errorf("map(...) arg count = %d, want %d (one key + one value per label)",
			len(fc.Args), wantArgs)
	}

	// Every odd-indexed arg MUST be the label literal at its index/2
	// position; every even-indexed arg MUST be the value expression
	// (non-LitString, since [levelAwareRangeGroupKey] returns a
	// MapAccess / multiIf wrap, never a bare literal).
	for i, label := range ra.Grouping.Groups {
		keyIdx := i * 2
		valIdx := i*2 + 1
		if keyIdx >= len(fc.Args) || valIdx >= len(fc.Args) {
			break
		}
		lit, ok := fc.Args[keyIdx].(*chplan.LitString)
		if !ok {
			t.Errorf("arg[%d] = %T, want *chplan.LitString for label %q", keyIdx, fc.Args[keyIdx], label)
			continue
		}
		if lit.V != label {
			t.Errorf("arg[%d].V = %q, want %q", keyIdx, lit.V, label)
		}
		if _, isLit := fc.Args[valIdx].(*chplan.LitString); isLit {
			t.Errorf("arg[%d] is a *chplan.LitString — expected a value expression for label %q", valIdx, label)
		}
	}
}

// TestRangeAggregationGroupByWithoutStripsFromIdentityBase pins the
// `without (...)` grouping contract: the exclusion strips keys from the
// FULL series identity the caller passes in (identityBase — the
// parser-merged labelset minus the unwrap target), wrapped with the
// synthesized detected_level key, NOT from the raw ResourceAttributes
// column. Grouping `without (pod)` against raw ResourceAttributes
// silently drops every parser-extracted label from the series identity
// — the loki-compat `exhaustive/aggregations.yaml#Max avg duration by
// level without service_name` failure, where the enclosing
// `max by (level)` collapsed a 4-level matrix into one empty-level
// series.
func TestRangeAggregationGroupByWithoutStripsFromIdentityBase(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()

	query := `avg_over_time({app="api"} | logfmt | unwrap latency [5m]) without (pod)`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	ra, ok := expr.(*syntax.RangeAggregationExpr)
	if !ok {
		t.Fatalf("ParseExpr(%q) -> %T, want *syntax.RangeAggregationExpr", query, expr)
	}
	if ra.Grouping == nil || !ra.Grouping.Without {
		t.Fatalf("fixture invalid: Grouping=%v", ra.Grouping)
	}

	// The identityBase stand-in mirrors the unwrap+parser path: the
	// materialised merged-labels column minus the unwrap target.
	identityBase := &chplan.MapWithoutKeys{
		Map:  &chplan.ColumnRef{Name: "_logql_merged_labels"},
		Keys: []string{"latency"},
	}
	got, err := rangeAggregationGroupBy(ra, s, identityBase)
	if err != nil {
		t.Fatalf("rangeAggregationGroupBy: %v", err)
	}

	mwk, ok := got.(*chplan.MapWithoutKeys)
	if !ok {
		t.Fatalf("rangeAggregationGroupBy -> %T, want *chplan.MapWithoutKeys", got)
	}
	if len(mwk.Keys) != 1 || mwk.Keys[0] != "pod" {
		t.Errorf("MapWithoutKeys.Keys = %v, want [pod]", mwk.Keys)
	}

	// The stripped map MUST be the detected_level-wrapped identityBase
	// (a mapConcat FuncCall whose first arg is identityBase), so the
	// parser-extracted labels survive into the series identity and an
	// enclosing `by (level)` still resolves the severity dimension.
	wrap, ok := mwk.Map.(*chplan.FuncCall)
	if !ok || wrap.Name != "mapConcat" {
		t.Fatalf("MapWithoutKeys.Map = %T (%v), want mapConcat FuncCall wrapping identityBase", mwk.Map, mwk.Map)
	}
	if len(wrap.Args) == 0 || !wrap.Args[0].Equal(identityBase) {
		t.Errorf("mapConcat first arg != identityBase — `without` is not stripping from the full series identity")
	}
}
