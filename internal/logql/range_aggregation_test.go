package logql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

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
