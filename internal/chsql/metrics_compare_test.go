package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// containsInt64Arg reports whether want appears among args as an int64 —
// used to check a parameterized bound's actual VALUE, since a substring
// match against the SQL text only proves a `fromUnixTimestamp64Nano(?)`
// placeholder is present, not which value it was bound to.
func containsInt64Arg(args []any, want int64) bool {
	for _, a := range args {
		if v, ok := a.(int64); ok && v == want {
			return true
		}
	}
	return false
}

// compareNode builds a minimal valid MetricsCompare (no root lookup —
// the join shape is covered by the lowering-level tests + TXTAR
// fixtures; this file pins the emitter's own contract).
func compareNode() *chplan.MetricsCompare {
	return &chplan.MetricsCompare{
		Selection: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "StatusCode"},
			Right: &chplan.LitString{V: "Error"},
		},
		TopN: 10,
		Pairs: &chplan.FuncCall{Name: "array", Args: []chplan.Expr{
			&chplan.FuncCall{Name: "tuple", Args: []chplan.Expr{
				&chplan.LitString{V: "name"},
				&chplan.ColumnRef{Name: "SpanName"},
			}},
		}},
		SelAlias:   "is_selection",
		AttrAlias:  "attr",
		ValAlias:   "val",
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}
}

// compareNodeWithRoot extends compareNode with the per-trace root
// lookup leg (the LEFT JOIN shape that the production traceql
// drilldown compare emits). It mirrors internal/traceql's
// compareRootLookup: an Aggregate over a Filter(ParentSpanId empty)
// GROUP BY TraceId. The join shape is what makes scan-bound pushdown
// non-trivial (a window filter above `s LEFT JOIN r` cannot prune
// either MergeTree leg), so the matrix pushdown test below exercises
// this node rather than the join-free compareNode.
func compareNodeWithRoot() *chplan.MetricsCompare {
	m := compareNode()
	m.TraceIDColumn = "TraceId"
	m.RootLookup = &chplan.Aggregate{
		Input: &chplan.Filter{
			Input: &chplan.Scan{Table: "otel_traces"},
			Predicate: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: "ParentSpanId"},
				Right: &chplan.LitString{V: ""},
			},
		},
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "TraceId"}},
		AggFuncs: []chplan.AggFunc{
			{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: "SpanName"}}, Alias: "__root_name"},
		},
	}
	return m
}

// TestEmitRangeWindowCompare_JoinScanPushdown pins the scan-bounding
// pushdown for the join (RootLookup) shape — the prod traces-drilldown
// OOM. The (Start - range, End] Timestamp window must land INSIDE each
// MergeTree scan of `s LEFT JOIN r`, never on the SELECT wrapping the
// join (CH 24.12 cannot push a join-level predicate into either leg):
//
//   - the `s` span leg carries the bound in its own WHERE, immediately
//     above the `AS s` alias;
//   - the `r` root leg is seeded with `TraceId IN (<bounded cohort
//     trace-ids>)` pushed directly into the Filter beneath RootLookup's
//     own physical Scan — i.e. BELOW its GROUP BY TraceId aggregate, not
//     as a wrap around the aggregate's output — so the membership predicate
//     scopes the aggregate's inputs instead of filtering its output (the
//     scan itself is pruned only in the root-scoped arm's Timestamp bound).
//     A `TraceId IN (...)` filter sitting ABOVE the GROUP BY (the earlier
//     boundedRootLeg-only shape) restricts only the aggregate's output
//     and does not prune the scan; that's what kept OOMing in prod
//     despite the wrap already existing (see windowRootLookupTraceIDSeed).
func TestEmitRangeWindowCompare_JoinScanPushdown(t *testing.T) {
	t.Parallel()

	rw := &chplan.RangeWindow{
		Input:           compareNodeWithRoot(),
		Range:           time.Minute,
		Step:            time.Minute,
		Start:           time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
		End:             time.Date(2026, 5, 12, 10, 3, 0, 0, time.UTC),
		TimestampColumn: "Timestamp",
	}
	sql, args, err := chsql.Emit(context.Background(), rw)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	lo := "`Timestamp` > toDateTime64('2026-05-12 10:00:00.000000000', 9) - toIntervalNanosecond(60000000000)"
	hi := "`Timestamp` <= toDateTime64('2026-05-12 10:03:00.000000000', 9)"

	// The 's' span leg: bound sits in the WHERE immediately preceding the
	// `AS s` alias — i.e. inside the scan, below the join.
	sLeg := "WHERE " + lo + " AND " + hi + ") AS s"
	if !strings.Contains(sql, sLeg) {
		t.Errorf("matrix join SQL must bound the 's' scan inside the join (want %q):\n%s", sLeg, sql)
	}

	// The 'r' root leg: RootLookup's own Filter(ParentSpanId = '') gains a
	// `TraceId IN (<bounded cohort>)` conjunct, seeding the scan directly
	// — BELOW the GROUP BY that follows it — so the predicate scopes the
	// aggregate's inputs rather than filtering its output. This non-root shape
	// has no direct Timestamp bound, so the scan itself is NOT pruned (that is
	// #1214's lossless tradeoff; only the root-scoped arm prunes).
	// PREWHERE promotion (internal/optimizer)
	// splits the two conjuncts of the Filter across PREWHERE/WHERE rather
	// than AND-ing them into one clause; either placement still lands the
	// seed inside the scan, below the GROUP BY. The seed's own bound
	// (tsBoundExprs) renders as fromUnixTimestamp64Nano(?) — the same
	// parameterized shape the search lowering's tsBound uses — not the
	// inlined toDateTime64(...) literal the 's' leg's lo/hi above use.
	rSeededScan := "PREWHERE (`ParentSpanId` = ?) WHERE `TraceId` IN (SELECT `TraceId` FROM (SELECT * FROM `otel_traces` " +
		"WHERE (`Timestamp` >= fromUnixTimestamp64Nano(?)) AND (`Timestamp` <= fromUnixTimestamp64Nano(?)))))"
	if !strings.Contains(sql, rSeededScan) {
		t.Errorf("matrix join SQL must seed the 'r' root leg's own scan by bounded trace-ids (want %q):\n%s", rSeededScan, sql)
	}
	if !strings.Contains(sql, rSeededScan+" GROUP BY `TraceId`") {
		t.Errorf("the trace-id seed must sit BELOW RootLookup's GROUP BY, not wrap its output:\n%s", sql)
	}

	// The seed's own bound is parameterized (fromUnixTimestamp64Nano(?)), so
	// the shape assertion above passes regardless of the actual bound VALUE —
	// it must independently be checked against args. The seed has to mirror
	// the 's' leg's own (Start-range, End] window (see innerScanTsBoundsFrags
	// in range_window.go), not the raw [Start, End] request window: a trace
	// whose only matching span falls in the anchor lookback slice
	// (Start-range, Start) — the normal case for every anchor before the
	// last — satisfies the 's' leg's bound but would miss a seed bounded to
	// raw Start, silently dropping that trace's root-name/root-service
	// enrichment. This is the same shape as prior window-anchor bugs in this
	// codebase (mismatched request-window bound vs. actual scan bound).
	wantSeedLoNano := rw.Start.UnixNano() - rw.Range.Nanoseconds()
	wantSeedHiNano := rw.End.UnixNano()
	if !containsInt64Arg(args, wantSeedLoNano) {
		t.Errorf("seed lower bound arg %d (Start - range) not found in emitted args %v", wantSeedLoNano, args)
	}
	if !containsInt64Arg(args, wantSeedHiNano) {
		t.Errorf("seed upper bound arg %d (End) not found in emitted args %v", wantSeedHiNano, args)
	}
	if containsInt64Arg(args, rw.Start.UnixNano()) {
		t.Errorf("seed lower bound must be Start-range, not raw Start (found raw Start.UnixNano()=%d in args %v)", rw.Start.UnixNano(), args)
	}

	// Regression guard: once the scan-level seed lands, boundedRootLeg's
	// redundant post-aggregate wrap (`) AS r` immediately preceded by a
	// bare `WHERE TraceId IN (...)`, with no intervening Filter/Scan) must
	// not also appear — that shape re-filters an already-seeded result by
	// the identical trace-id set for no benefit.
	if strings.Contains(sql, "_cmp_seed") {
		t.Errorf("boundedRootLeg's post-aggregate wrap must be skipped once the scan-level seed succeeds (unexpected _cmp_seed):\n%s", sql)
	}

	// Regression guard: the bound must NOT sit on the SELECT that wraps
	// the whole `s LEFT JOIN r` (the original un-prunable placement). The
	// join's ON clause is the last token before the wrapping SELECT's
	// own scope; assert no Timestamp predicate trails the join's ON.
	onIdx := strings.Index(sql, "ON s.`TraceId` = r.`TraceId`")
	if onIdx < 0 {
		t.Fatalf("expected the LEFT JOIN ON clause in:\n%s", sql)
	}
	if strings.Contains(sql[onIdx:], "`Timestamp` >") || strings.Contains(sql[onIdx:], "`Timestamp` <=") {
		t.Errorf("Timestamp bound must not sit above the join (found after ON clause):\n%s", sql[onIdx:])
	}
}

// TestEmitRangeWindowCompare_RootScopedEnrichmentTimestampBound pins the
// prod fix for the traces-drilldown "Comparison" OOM: when the selection is
// root-scoped (InnerRootScoped), the enrichment ('r') root-lookup scan gains a
// DIRECT request-window Timestamp bound conjoined onto its own Filter, so CH
// partition/PK-prunes it. #1214's TraceId-IN seed alone cannot prune (TraceId
// is not the sort key). The bound is lossless here because the seed's roots are
// all in-window; a non-root selection (InnerRootScoped == false, the
// JoinScanPushdown test above) keeps the scan unbounded to preserve #1214's
// no-drop guarantee.
func TestEmitRangeWindowCompare_RootScopedEnrichmentTimestampBound(t *testing.T) {
	t.Parallel()

	// The discriminating signal is the DIRECT Timestamp bound conjoined onto the
	// root scan's own Filter, which renders BEFORE the `TraceId IN (…)` seed.
	// The seed subquery already carries its OWN nested Timestamp bound (#1214),
	// so a test that only greps the whole filter region for a Timestamp bound is
	// hollow — it passes on the non-root shape too. Slicing at the seed opener
	// pins the prefix that differs between the two arms.
	prefixBeforeSeed := func(t *testing.T, innerRootScoped bool) string {
		t.Helper()
		m := compareNodeWithRoot()
		m.InnerRootScoped = innerRootScoped
		rw := &chplan.RangeWindow{
			Input:           m,
			Range:           time.Minute,
			Step:            time.Minute,
			Start:           time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
			End:             time.Date(2026, 5, 12, 10, 3, 0, 0, time.UTC),
			TimestampColumn: "Timestamp",
		}
		sql, _, err := chsql.Emit(context.Background(), rw)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		onIdx := strings.Index(sql, "ON s.`TraceId` = r.`TraceId`")
		if onIdx < 0 {
			t.Fatalf("expected LEFT JOIN ON clause:\n%s", sql)
		}
		rLeg := sql[:onIdx]
		start := strings.LastIndex(rLeg, "`ParentSpanId` = ?")
		seed := strings.Index(rLeg, "`TraceId` IN (SELECT `TraceId`")
		if start < 0 || seed < 0 || seed <= start {
			t.Fatalf("expected root leg ParentSpanId filter then TraceId-IN seed:\n%s", rLeg)
		}
		return rLeg[start:seed] // the scan filter BEFORE the seed subquery
	}

	// Root-scoped: the direct request-window Timestamp bound is conjoined onto
	// the scan filter, ahead of the (retained) TraceId-IN seed.
	rootPrefix := prefixBeforeSeed(t, true)
	if !strings.Contains(rootPrefix, "`Timestamp` >= fromUnixTimestamp64Nano(?)") ||
		!strings.Contains(rootPrefix, "`Timestamp` <= fromUnixTimestamp64Nano(?)") {
		t.Errorf("root-scoped: direct Timestamp bound must precede the TraceId-IN seed, got prefix:\n%s", rootPrefix)
	}

	// Negative arm (regression discriminator): a non-root selection must NOT gain
	// the direct bound — the prefix before the seed is only the ParentSpanId
	// filter. This is what makes the test fail if the emitter half is reverted.
	nonRootPrefix := prefixBeforeSeed(t, false)
	if strings.Contains(nonRootPrefix, "fromUnixTimestamp64Nano") {
		t.Errorf("non-root: root scan must stay unbounded (no direct Timestamp bound before the seed), got prefix:\n%s", nonRootPrefix)
	}
}

// TestEmitMetricsCompare_BareShape — bare emission groups by
// (cohort, attr, val) with a deterministic ORDER BY and the Float64
// count reducer.
func TestEmitMetricsCompare_BareShape(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), compareNode())
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{
		"arrayJoin(array(tuple(",
		"AS `is_selection`",
		"tupleElement(kv, ?) AS `attr`",
		"tupleElement(kv, ?) AS `val`",
		"toFloat64(count(?)) AS `Value`",
		"GROUP BY `is_selection`, `attr`, `val`",
		"ORDER BY `is_selection`, `attr`, `val`",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("bare SQL missing %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "anchor_ts") {
		t.Errorf("bare SQL must not contain the matrix anchor column:\n%s", sql)
	}
}

// TestEmitRangeWindowCompare_MatrixShape — the RangeWindow wrap adds
// the sample-side anchor fanout, the anchor GROUP BY axis, and the
// (Start - range, End] scan-bound pushdown.
func TestEmitRangeWindowCompare_MatrixShape(t *testing.T) {
	t.Parallel()

	rw := &chplan.RangeWindow{
		Input:           compareNode(),
		Range:           time.Minute,
		Step:            time.Minute,
		Start:           time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
		End:             time.Date(2026, 5, 12, 10, 3, 0, 0, time.UTC),
		TimestampColumn: "Timestamp",
	}
	sql, _, err := chsql.Emit(context.Background(), rw)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{
		"AS `anchor_ts`",
		"GROUP BY `is_selection`, `attr`, `val`, `anchor_ts`",
		"`Timestamp` > toDateTime64('2026-05-12 10:00:00.000000000', 9) - toIntervalNanosecond(60000000000)",
		"`Timestamp` <= toDateTime64('2026-05-12 10:03:00.000000000', 9)",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("matrix SQL missing %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "ORDER BY") {
		t.Errorf("matrix SQL must not pin an ORDER BY (the handler owns series assembly):\n%s", sql)
	}
}

// TestEmitMetricsCompare_ErrorPaths — nil Selection / Pairs / Inner and
// a non-positive matrix Step surface as synchronous emit errors.
func TestEmitMetricsCompare_ErrorPaths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		node chplan.Node
		want string
	}{
		{"nilSelection", func() chplan.Node { m := compareNode(); m.Selection = nil; return m }(), "Selection is nil"},
		{"nilPairs", func() chplan.Node { m := compareNode(); m.Pairs = nil; return m }(), "Pairs is nil"},
		{"nilInner", func() chplan.Node { m := compareNode(); m.Inner = nil; return m }(), "Inner is nil"},
		{"rootLookupWithoutTraceID", func() chplan.Node {
			m := compareNode()
			m.RootLookup = &chplan.Scan{Table: "otel_traces"}
			m.TraceIDColumn = ""
			return m
		}(), "TraceIDColumn empty"},
		{"matrixZeroStep", &chplan.RangeWindow{
			Input:           compareNode(),
			TimestampColumn: "Timestamp",
		}, "requires Step > 0"},
		{"matrixNoTsColumn", &chplan.RangeWindow{
			Input: compareNode(),
			Step:  time.Minute,
		}, "TimestampColumn unset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := chsql.Emit(context.Background(), tc.node)
			if err == nil {
				t.Fatalf("Emit should fail for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q missing %q", err, tc.want)
			}
		})
	}
}
