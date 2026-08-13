package chsql_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	tql "github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/internal/traceql/ast"
)

// Phase-4 emitter coverage for the spans-scan WINDOW invariant. These tests
// mirror the requireInstantScanBound fail-closed tests: an emitter-synthetic
// recursive / grouped spans scan that opted into request-window partition
// pruning (TimestampColumn stamped by the search lowering) but reaches emit
// with a ZERO window must be rejected (ErrUnboundedSpansScan) rather than
// silently rendering a full-retention scan behind the inert `TraceId IN (seed)`
// membership. The positive cases assert the recursive arm / root-lookup scan
// carries a direct `fromUnixTimestamp64Nano(...)` Timestamp predicate when the
// window IS present.
//
// The recursive STEP/ANCHOR arms render the window as an INLINE literal
// (`fromUnixTimestamp64Nano(<nanos>)`) — only the emitter-synthetic scans do
// that; every chplan-leaf scan renders its window as a positional `?` arg. So
// `fromUnixTimestamp64Nano(<startNano>)` appearing in the SQL is a precise
// witness that the recursive arm itself is windowed (not just the seed leaf).

const (
	scanWindowEmitLimit         = 20
	scanWindowEmitStartNano     = int64(1782571392_000000000)
	scanWindowEmitEndNano       = int64(1782573192_000000000)
	scanWindowEmitStep          = 5 * time.Minute
	fromUnixNanoCall            = "fromUnixTimestamp64Nano"
	structuralRecursiveStepFrom = "`otel_traces` AS t INNER JOIN"
)

func scanWindowEmitBounds() (time.Time, time.Time) {
	return time.Unix(0, scanWindowEmitStartNano).UTC(), time.Unix(0, scanWindowEmitEndNano).UTC()
}

// lowerWindowedSearch lowers a TraceQL search query through the real lowering
// with the /api/search limit + request window threaded on the context, exactly
// as the handler does.
func lowerWindowedSearch(t *testing.T, q string, start, end time.Time) chplan.Node {
	t.Helper()
	expr, err := ast.Parse(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	ctx := tql.WithSearchTraceLimit(context.Background(), scanWindowEmitLimit)
	ctx = tql.WithSearchWindow(ctx, start, end)
	plan, err := tql.Lower(ctx, expr, schema.DefaultOTelTraces())
	if err != nil {
		t.Fatalf("lower %q: %v", q, err)
	}
	return plan
}

// emitSpansScoped emits plan with the spans table threaded onto the emit
// context (chsql.WithSpansTable), the genuine "production Tempo request"
// signal that arms the fail-closed window guard.
func emitSpansScoped(t *testing.T, plan chplan.Node) (string, error) {
	t.Helper()
	s := schema.DefaultOTelTraces()
	sql, _, err := chsql.Emit(chsql.WithSpansTable(context.Background(), s.SpansTable), plan)
	return sql, err
}

// zeroStructuralWindows clears the request window on every StructuralJoin while
// LEAVING TimestampColumn set, simulating a node that opted into windowing but
// reaches emit with a zero window — exactly what the fail-closed guard exists
// to catch.
func zeroStructuralWindows(plan chplan.Node) {
	chplan.Walk(plan, func(n chplan.Node) bool {
		if j, ok := n.(*chplan.StructuralJoin); ok {
			j.WindowStartNano = 0
			j.WindowEndNano = 0
		}
		return true
	})
}

// zeroNestedSetWindows is the NestedSetAnnotate analogue of
// zeroStructuralWindows.
func zeroNestedSetWindows(plan chplan.Node) {
	chplan.Walk(plan, func(n chplan.Node) bool {
		if ns, ok := n.(*chplan.NestedSetAnnotate); ok {
			ns.WindowStartNano = 0
			ns.WindowEndNano = 0
		}
		return true
	})
}

// TestEmitStructuralRecursiveStepWindowed pins the structural-closure step arm:
// with a window the recursive `t` scan carries the inline Timestamp prune; with
// the window zeroed (TimestampColumn still set) the emit fails closed.
func TestEmitStructuralRecursiveStepWindowed(t *testing.T) {
	t.Parallel()
	start, end := scanWindowEmitBounds()
	const q = `{ .service.name = "a" } >> { .http.status_code = 500 }`

	plan := lowerWindowedSearch(t, q, start, end)
	sql, err := emitSpansScoped(t, plan)
	if err != nil {
		t.Fatalf("windowed structural emit: %v", err)
	}
	if !strings.Contains(sql, structuralRecursiveStepFrom) {
		t.Fatalf("emitted SQL has no recursive `otel_traces AS t` step — shape changed, assertion vacuous:\n%s", sql)
	}
	lo := fmt.Sprintf("%s(%d)", fromUnixNanoCall, scanWindowEmitStartNano)
	hi := fmt.Sprintf("%s(%d)", fromUnixNanoCall, scanWindowEmitEndNano)
	if !strings.Contains(sql, lo) || !strings.Contains(sql, hi) {
		t.Errorf("recursive step `t` scan is not window-bounded (missing inline %s / %s):\n%s", lo, hi, sql)
	}

	// Fail closed: zero the window (keep TimestampColumn) → the step would read
	// full retention behind the inert TraceId-IN, so emit must reject it.
	zeroStructuralWindows(plan)
	if _, err := emitSpansScoped(t, plan); !errors.Is(err, chsql.ErrUnboundedSpansScan) {
		t.Fatalf("zero-window structural step under WithSpansTable must fail closed, got %v", err)
	}
}

// TestEmitNestedSetAnchorStepWindowed pins the nested-set numbering CTE: both
// the anchor and the step `t` scans carry the inline Timestamp prune when a
// window is present, and the emit fails closed when it is zeroed.
func TestEmitNestedSetAnchorStepWindowed(t *testing.T) {
	t.Parallel()
	start, end := scanWindowEmitBounds()
	// The Grafana Traces Drilldown structure-tab query — a bounded
	// NestedSetAnnotate numbering walk.
	const q = `({ nestedSetParent < 0 } &>> { kind = server }) || ({ nestedSetParent < 0 }) | select(nestedSetParent, nestedSetLeft, nestedSetRight)`

	plan := lowerWindowedSearch(t, q, start, end)
	sql, err := emitSpansScoped(t, plan)
	if err != nil {
		t.Fatalf("windowed nested-set emit: %v", err)
	}
	if !strings.Contains(sql, "_cerberus_ns_paths") {
		t.Fatalf("emitted SQL has no nested-set numbering CTE — shape changed, assertion vacuous:\n%s", sql)
	}
	lo := fmt.Sprintf("%s(%d)", fromUnixNanoCall, scanWindowEmitStartNano)
	hi := fmt.Sprintf("%s(%d)", fromUnixNanoCall, scanWindowEmitEndNano)
	// The numbering anchor + step both render the inline window; at minimum the
	// recursive-arm prune must be present (only emitter-synthetic scans inline
	// the nanos — chplan leaves use `?`).
	if strings.Count(sql, lo) < 2 || strings.Count(sql, hi) < 2 {
		t.Errorf("nested-set anchor+step not both window-bounded (inline %s count=%d, %s count=%d, want >= 2 each):\n%s",
			lo, strings.Count(sql, lo), hi, strings.Count(sql, hi), sql)
	}

	// Fail closed: zero the window (keep TimestampColumn + TraceLimit) → reject.
	zeroNestedSetWindows(plan)
	if _, err := emitSpansScoped(t, plan); !errors.Is(err, chsql.ErrUnboundedSpansScan) {
		t.Fatalf("zero-window nested-set numbering under WithSpansTable must fail closed, got %v", err)
	}
}

// structuralClosureSeedMarker ends the anchor arm's seed derived table
// (`(<L>) AS _seed`); everything between it and the closure's top-level
// `UNION ALL` is the anchor arm's OWN scope — the only scope whose Timestamp
// predicate prunes the seed's partitions.
const structuralClosureSeedMarker = "AS _seed"

// structuralAnchorArmScopes returns, for every recursive structural closure in
// sql, the text of the anchor arm that follows its `_seed` derived table: the
// arm's own WHERE, up to the closure's top-level `UNION ALL`. Parens are
// tracked so a subquery inside that WHERE (the candidate prefilter) cannot end
// the scan early, and the walk stops at the CTE's closing paren.
//
// Returns nil when sql has no closure at all, which the callers assert on so a
// shape change cannot make the window assertion vacuous.
func structuralAnchorArmScopes(sql string) []string {
	var out []string
	rest := sql
	for {
		i := strings.Index(rest, structuralClosureSeedMarker)
		if i < 0 {
			return out
		}
		rest = rest[i+len(structuralClosureSeedMarker):]
		out = append(out, anchorArmScope(rest))
	}
}

// anchorArmScope returns the prefix of s that belongs to the anchor arm: it
// ends at the closure's top-level `UNION ALL` (the step arm starts there) or at
// the paren that closes the CTE body, whichever comes first. Nested parens are
// skipped so a subquery inside the arm's WHERE — the candidate prefilter — is
// not mistaken for either terminator.
func anchorArmScope(s string) string {
	depth := 0
	for j := 0; j < len(s); j++ {
		switch {
		case s[j] == '(':
			depth++
		case s[j] == ')':
			if depth == 0 {
				return s[:j]
			}
			depth--
		case depth == 0 && strings.HasPrefix(s[j:], "UNION ALL"):
			return s[:j]
		}
	}
	return s
}

// assertAnchorArmsWindowed asserts every recursive structural closure in sql
// bounds its anchor arm with the inline request window. The anchor arm is a
// `WITH RECURSIVE` arm, so no enclosing predicate — not the outer search
// filter, not the metrics RangeWindow wrap — reaches its `_seed` scan;
// `otel_traces` is `PARTITION BY toDate(Timestamp)`, so without the arm's own
// bound the seed reads full retention (#1942).
func assertAnchorArmsWindowed(t *testing.T, sql string, wantClosures int) {
	t.Helper()
	arms := structuralAnchorArmScopes(sql)
	if len(arms) != wantClosures {
		t.Fatalf("found %d structural closure anchor arms, want %d — shape changed, assertion vacuous:\n%s",
			len(arms), wantClosures, sql)
	}
	lo := fmt.Sprintf("%s(%d)", fromUnixNanoCall, scanWindowEmitStartNano)
	hi := fmt.Sprintf("%s(%d)", fromUnixNanoCall, scanWindowEmitEndNano)
	for i, arm := range arms {
		if !strings.Contains(arm, lo) || !strings.Contains(arm, hi) {
			t.Errorf("closure %d: anchor arm's `_seed` scope carries no request window (missing inline %s / %s) — the seed scan reads full retention:\narm: %s\nfull SQL:\n%s",
				i, lo, hi, arm, sql)
		}
	}
}

// TestEmitStructuralRecursiveAnchorWindowed pins the structural closure's
// ANCHOR arm — the half the step-arm window (#1109) left uncovered. The anchor
// seeds from a derived table (`(<L>) AS _seed`) whose partitions are prunable
// only from the arm's own scope, and a `WITH RECURSIVE` arm admits no predicate
// from outside the CTE. Both the canonical closure and the `&>>` inverse
// closure are asserted, on both the search path (where the seed's leaf scan
// also carries the window) and the METRICS path (where it does not — the
// anchor's own bound is the ONLY thing keeping the seed off full retention).
func TestEmitStructuralRecursiveAnchorWindowed(t *testing.T) {
	t.Parallel()
	start, end := scanWindowEmitBounds()

	t.Run("search", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name     string
			q        string
			closures int
		}{
			{"descendant", `{ .service.name = "a" } >> { .http.status_code = 500 }`, 1},
			// The union form emits the canonical closure plus the inverse
			// closure that walks back the other way — two anchor arms.
			{"union_descendant", `{ .service.name = "a" } &>> { .http.status_code = 500 }`, 2},
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				sql, err := emitSpansScoped(t, lowerWindowedSearch(t, tc.q, start, end))
				if err != nil {
					t.Fatalf("windowed structural emit: %v", err)
				}
				assertAnchorArmsWindowed(t, sql, tc.closures)
			})
		}
	})

	t.Run("metrics", func(t *testing.T) {
		t.Parallel()
		// The metrics path threads the request window but folds NO window onto
		// the chplan leaves (the RangeWindow wrap bounds the metrics leaf, and
		// it cannot reach below a WITH RECURSIVE), so the anchor arm's own
		// bound is the whole fix here.
		sql, err := emitStructuralMetricsRange(t, `{ } >> { } | rate()`, start, end)
		if err != nil {
			t.Fatalf("windowed metrics structural emit: %v", err)
		}
		assertAnchorArmsWindowed(t, sql, 1)
	})
}

// emitStructuralMetricsRange lowers a metrics-over-structural pipeline with the
// request window on the context (no /api/search limit — the metrics path) and
// wraps it in the RangeWindow the /api/metrics/query_range handler builds,
// exactly as emitCompareInRangeWindow does for compare().
func emitStructuralMetricsRange(t *testing.T, q string, start, end time.Time) (string, error) {
	t.Helper()
	s := schema.DefaultOTelTraces()
	expr, err := ast.Parse(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	plan, err := tql.Lower(tql.WithSearchWindow(context.Background(), start, end), expr, s)
	if err != nil {
		t.Fatalf("lower %q: %v", q, err)
	}
	rw := &chplan.RangeWindow{
		Input:           plan,
		Range:           scanWindowEmitStep,
		Step:            scanWindowEmitStep,
		Start:           start,
		End:             end,
		TimestampColumn: s.TimestampColumn,
	}
	sql, _, err := chsql.Emit(chsql.WithSpansTable(context.Background(), s.SpansTable), chplan.Node(rw))
	return sql, err
}

// emitCompareInRangeWindow lowers `{ } | compare(...)`, wraps it in the
// RangeWindow the /api/metrics/query_range handler builds, and emits — with or
// without the spans table on the context. start/end may be zero to exercise the
// fail-closed gate.
func emitCompareInRangeWindow(t *testing.T, start, end time.Time, spansScoped bool) (string, []any, error) {
	t.Helper()
	s := schema.DefaultOTelTraces()
	expr, err := ast.Parse(`{ } | compare({ status = error })`)
	if err != nil {
		t.Fatalf("parse compare: %v", err)
	}
	plan, err := tql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("lower compare: %v", err)
	}
	rw := &chplan.RangeWindow{
		Input:           plan,
		Range:           scanWindowEmitStep,
		Step:            scanWindowEmitStep,
		Start:           start,
		End:             end,
		TimestampColumn: s.TimestampColumn,
	}
	ctx := context.Background()
	if spansScoped {
		ctx = chsql.WithSpansTable(ctx, s.SpansTable)
	}
	sql, args, err := chsql.Emit(ctx, chplan.Node(rw))
	return sql, args, err
}

// TestEmitCompareRootLookupWindowed pins the compare() root-lookup window
// push: the per-trace root aggregate's own scan gains a `TraceId IN
// (<bounded cohort>)` seed (below its GROUP BY, where CH can
// partition-prune it), whose own bound renders as two direct
// `fromUnixTimestamp64Nano(...)` Timestamp calls. windowRootLookupTraceIDSeed
// derives the spans table straight from RootLookup's own Scan
// (rootLookupSpansTable), not from the emit context, so this push fires
// identically whether or not the context threads WithSpansTable — unlike
// requireSpansScanWindow's fail-closed gate below, which is deliberately
// scoped to the real Tempo path. A zero window under the spans scope fails
// closed regardless.
func TestEmitCompareRootLookupWindowed(t *testing.T) {
	t.Parallel()
	start, end := scanWindowEmitBounds()

	withScope, withScopeArgs, err := emitCompareInRangeWindow(t, start, end, true)
	if err != nil {
		t.Fatalf("windowed compare emit (spans-scoped): %v", err)
	}
	withoutScope, withoutScopeArgs, err := emitCompareInRangeWindow(t, start, end, false)
	if err != nil {
		t.Fatalf("windowed compare emit (unscoped): %v", err)
	}

	// The base compare window renders its bounds as positional `?` args, so
	// the only `fromUnixTimestamp64Nano` calls come from the root-lookup
	// seed's own bound: two (lo + hi), present identically with or without
	// the spans scope threaded on the context.
	const wantRootSeedBoundCalls = 2
	countWith := strings.Count(withScope, fromUnixNanoCall)
	countWithout := strings.Count(withoutScope, fromUnixNanoCall)
	if countWith != wantRootSeedBoundCalls {
		t.Errorf("spans-scoped compare root-lookup seed = %d %s calls, want %d:\n%s", countWith, fromUnixNanoCall, wantRootSeedBoundCalls, withScope)
	}
	if countWithout != wantRootSeedBoundCalls {
		t.Errorf("unscoped compare root-lookup seed = %d %s calls, want %d (the seed derives its spans table from RootLookup itself, not the emit context):\n%s",
			countWithout, fromUnixNanoCall, wantRootSeedBoundCalls, withoutScope)
	}

	// The `?` shape assertion above passes regardless of the bound's actual
	// VALUE — checked here against args. The seed must mirror the 's' leg's
	// own (Start-range, End] window, not the raw [Start, End] request
	// window: a trace whose only matching span falls in the anchor lookback
	// slice (Start-range, Start) satisfies the 's' leg's bound but would be
	// silently dropped from root-name/root-service enrichment if the seed
	// used raw Start.
	wantSeedLoNano := scanWindowEmitStartNano - scanWindowEmitStep.Nanoseconds()
	wantSeedHiNano := scanWindowEmitEndNano
	for _, tc := range []struct {
		name string
		args []any
	}{
		{"spans-scoped", withScopeArgs},
		{"unscoped", withoutScopeArgs},
	} {
		if !containsInt64Arg(tc.args, wantSeedLoNano) {
			t.Errorf("%s: seed lower bound arg %d (Start - range) not found in emitted args %v", tc.name, wantSeedLoNano, tc.args)
		}
		if !containsInt64Arg(tc.args, wantSeedHiNano) {
			t.Errorf("%s: seed upper bound arg %d (End) not found in emitted args %v", tc.name, wantSeedHiNano, tc.args)
		}
		if containsInt64Arg(tc.args, scanWindowEmitStartNano) {
			t.Errorf("%s: seed lower bound must be Start-range, not raw Start (found raw Start=%d in args %v)", tc.name, scanWindowEmitStartNano, tc.args)
		}
	}

	// Fail closed: a zero request window under the spans scope must be rejected
	// rather than scanning full retention.
	if _, _, err := emitCompareInRangeWindow(t, time.Time{}, time.Time{}, true); !errors.Is(err, chsql.ErrUnboundedSpansScan) {
		t.Fatalf("zero-window compare under WithSpansTable must fail closed, got %v", err)
	}
}
