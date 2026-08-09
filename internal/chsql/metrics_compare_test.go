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

// compareRootLeg asks the emitter for the root ('r') leg of rw's compare join
// as a statement of its own, and pins that what it returns really is the leg
// the join embeds — the emitted matrix statement must contain that exact text.
//
// The seam (chsql.EmitCompareRootLeg) replaces what used to be a balanced-paren
// scan from a `LEFT JOIN (` marker, in this file and again in test/perf: two
// copies of one algorithm keyed on emitter internals nothing pinned — that a
// join side is parenthesised, that this marker opens the leg, and (in the perf
// copy, which has to run the leg) that args are appended in text order so a `?`
// count yields the leg's arg slice. The emitter now answers all three itself,
// and the containment check below is the assertion those assumptions never got.
func compareRootLeg(t *testing.T, rw *chplan.RangeWindow, matrixSQL string) (string, []any) {
	t.Helper()
	legSQL, legArgs, err := chsql.EmitCompareRootLeg(context.Background(), rw)
	if err != nil {
		t.Fatalf("EmitCompareRootLeg: %v", err)
	}
	if !strings.Contains(matrixSQL, legSQL) {
		t.Fatalf("the standalone root leg must be the leg the compare join embeds, verbatim.\nleg:\n%s\nmatrix:\n%s",
			legSQL, matrixSQL)
	}
	return legSQL, legArgs
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
	// compareNodeWithRoot leaves the trace_id_ts lookup fields empty, so the
	// envelope of TestEmitRangeWindowCompare_NonRootTraceIDTsEnrichmentBound is
	// off too: asserting on the Timestamp column rather than on one bound's
	// rendering pins that NEITHER bound shape reaches this scan.
	nonRootPrefix := prefixBeforeSeed(t, false)
	if strings.Contains(nonRootPrefix, "`Timestamp`") {
		t.Errorf("non-root: root scan must stay unbounded (no Timestamp bound before the seed), got prefix:\n%s", nonRootPrefix)
	}
}

// TestEmitRangeWindowCompare_NonRootTraceIDTsEnrichmentBound proves the
// non-root strategy retains Tempo's root enrichment without pretending the
// request window contains the root. The trace_id_ts min/max envelope is built
// from exactly the seeded cohort, then conjoined on the physical root scan.
//
// The envelope rides a single scalar WITH binding, so this also pins the
// deduplication that binding exists for: the cohort seed — a whole subquery
// over the spans table — renders exactly twice per statement (the binding body
// and the root scan's own membership gate), never once per bound, and the
// trace_id_ts table is scanned once. A test that only checked the two bounds
// are present would pass on the shape that re-derives the envelope per bound.
//
// It pins the binding's PLACEMENT too: the declaration and its readers are one
// statement, the root ('r') leg, so that leg stands alone as a runnable query.
func TestEmitRangeWindowCompare_NonRootTraceIDTsEnrichmentBound(t *testing.T) {
	t.Parallel()

	m := compareNodeWithRoot()
	m.RootLookupTraceIDTsTable = "otel_traces_trace_id_ts"
	m.RootLookupTraceIDTsStartColumn = "Start"
	m.RootLookupTraceIDTsEndColumn = "End"
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

	// The `(min(Start), max(End))` tuple both bounds read is declared on the
	// root ('r') leg — the innermost statement containing every reader, since
	// the bounds sit in that leg's own scan filter several subqueries down.
	//
	// Counting alias occurrences cannot tell a resolvable binding from an
	// unresolvable one: a declaration hoisted onto some enclosing SELECT
	// satisfies the count while leaving the leg a fragment that names an
	// identifier it does not declare. So assert the placement instead — the
	// declaration opens the leg the emitter hands back, and no reference to the
	// alias escapes it.
	rootLeg, legArgs := compareRootLeg(t, rw, sql)
	const wantBinding = "WITH (SELECT tuple(min(`Start`), max(`End`)) FROM `otel_traces_trace_id_ts` WHERE "
	if !strings.HasPrefix(rootLeg, wantBinding) {
		t.Fatalf("expected the envelope binding to open the root ('r') leg, got:\n%s", rootLeg)
	}
	if got := strings.Count(sql, "AS "+chplan.TraceIDTsEnvelopeAlias); got != 1 {
		t.Errorf("envelope binding declared %d times, want exactly 1:\n%s", got, sql)
	}
	if inLeg, total := strings.Count(rootLeg, chplan.TraceIDTsEnvelopeAlias),
		strings.Count(sql, chplan.TraceIDTsEnvelopeAlias); inLeg != total {
		t.Errorf("%s is referenced %d times but only %d live in the leg that declares it:\n%s",
			chplan.TraceIDTsEnvelopeAlias, total, inLeg, sql)
	}
	// One binding body means one trace_id_ts scan and one aggregate per bound
	// column. The pre-binding shape re-derived the envelope per bound, which
	// rendered both of these twice.
	for _, once := range []string{"`otel_traces_trace_id_ts`", "min(`Start`)", "max(`End`)"} {
		if got := strings.Count(sql, once); got != 1 {
			t.Errorf("%s rendered %d times, want exactly 1:\n%s", once, got, sql)
		}
	}
	// The cohort is a whole subquery over the spans table, so every extra
	// render of it is another scan. It renders exactly twice: inside the
	// binding body, and as the root scan's own membership gate.
	const seedOpener = "`TraceId` IN (SELECT `TraceId`"
	if got := strings.Count(sql, seedOpener); got != 2 {
		t.Errorf("cohort seed rendered %d times, want exactly 2:\n%s", got, sql)
	}

	onIdx := strings.Index(sql, "ON s.`TraceId` = r.`TraceId`")
	if onIdx < 0 {
		t.Fatalf("expected LEFT JOIN ON clause:\n%s", sql)
	}
	rLeg := sql[:onIdx]
	start := strings.LastIndex(rLeg, "`ParentSpanId` = ?")
	// The final seed occurrence in the root leg is the root scan's exact
	// membership predicate; the bounds established before it are the envelope's.
	seed := strings.LastIndex(rLeg, seedOpener)
	if start < 0 || seed < 0 || seed <= start {
		t.Fatalf("expected root scan filter then TraceId seed:\n%s", rLeg)
	}
	prefix := rLeg[start:seed]
	// Both bounds resolve to the binding rather than carrying a subquery of
	// their own.
	for _, want := range []string{
		"`Timestamp` >= tupleElement(" + chplan.TraceIDTsEnvelopeAlias + ", ?)",
		"`Timestamp` <= addSeconds(tupleElement(" + chplan.TraceIDTsEnvelopeAlias + ", ?), ?)",
	} {
		if !strings.Contains(prefix, want) {
			t.Errorf("non-root root scan must carry %q before its exact seed:\n%s", want, prefix)
		}
	}
	// Only the root scan's direct predicate is forbidden from using the request
	// window; the seeds nested below it are request-windowed, as they must be.
	if strings.Contains(prefix, "fromUnixTimestamp64Nano") {
		t.Errorf("non-root root scan must not use the request window directly:\n%s", prefix)
	}

	// addSeconds() alone does not prove the upper bound is actually widened —
	// a zero pad renders identically. trace_id_ts stores End as a DateTime
	// floored from a DateTime64(9) max(Timestamp), so the pad must be a full
	// second for the envelope to stay a superset of the trace's last span.
	// Nor does tupleElement() alone prove each bound reads the element it
	// means: reading Start for both would bound the scan to the cohort's
	// earliest instant. These are read off the leg's OWN args — the pairing the
	// emitter returns with the leg's SQL, not a slice of the whole statement's
	// args recovered by counting placeholders. The exact TraceId-IN seed renders
	// last within the leg and contributes exactly the two request-window params,
	// so counting back from the end the pad is third, the End element fourth and
	// the Start element fifth.
	const wantEndPadSeconds = int64(chplan.TraceIDTsEndPadSeconds)
	const (
		padArgsFromEnd        = 3
		endElemArgsFromEnd    = 4
		startElemArgsFromEnd  = 5
		minTrailingBoundsArgs = startElemArgsFromEnd
	)
	if len(legArgs) < minTrailingBoundsArgs {
		t.Fatalf("expected at least %d root-leg args, got %#v", minTrailingBoundsArgs, legArgs)
	}
	for _, tc := range []struct {
		name    string
		fromEnd int
		want    int64
	}{
		{"addSeconds pad", padArgsFromEnd, wantEndPadSeconds},
		{"End tuple element", endElemArgsFromEnd, chplan.TraceIDTsEnvelopeEndElement},
		{"Start tuple element", startElemArgsFromEnd, chplan.TraceIDTsEnvelopeStartElement},
	} {
		if got := legArgs[len(legArgs)-tc.fromEnd]; got != any(tc.want) {
			t.Errorf("%s = %#v, want %d", tc.name, got, tc.want)
		}
	}
}

// TestEmitCompareRootLeg pins the seam itself — the boundary that lets a
// caller ask for the compare join's root ('r') leg instead of parsing the
// emitted statement for it.
//
// Two properties make it a boundary rather than a convenience. The leg is the
// one the matrix statement embeds, verbatim; and the args it comes with are
// exactly the args its own text binds, which is the guarantee the previous
// `?`-counting extraction assumed (args appended in text order) and could
// never check.
func TestEmitCompareRootLeg(t *testing.T) {
	t.Parallel()

	window := func(m *chplan.MetricsCompare) *chplan.RangeWindow {
		return &chplan.RangeWindow{
			Input:           m,
			Range:           time.Minute,
			Step:            time.Minute,
			Start:           time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
			End:             time.Date(2026, 5, 12, 10, 3, 0, 0, time.UTC),
			TimestampColumn: "Timestamp",
		}
	}
	withEnvelope := func() *chplan.MetricsCompare {
		m := compareNodeWithRoot()
		m.RootLookupTraceIDTsTable = "otel_traces_trace_id_ts"
		m.RootLookupTraceIDTsStartColumn = "Start"
		m.RootLookupTraceIDTsEndColumn = "End"
		return m
	}
	rootScoped := func() *chplan.MetricsCompare {
		m := compareNodeWithRoot()
		m.InnerRootScoped = true
		return m
	}

	t.Run("isTheLegTheJoinEmbeds", func(t *testing.T) {
		t.Parallel()

		// All three root-leg shapes the matrix path can produce: #1214's
		// unbounded-but-lossless leg, the root-scoped leg with its direct
		// request-window bound, and the non-root leg under the trace_id_ts
		// envelope. Each must round-trip through the seam identically.
		for _, tc := range []struct {
			name string
			node *chplan.MetricsCompare
		}{
			{"unboundedLossless", compareNodeWithRoot()},
			{"rootScoped", rootScoped()},
			{"traceIDTsEnvelope", withEnvelope()},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				rw := window(tc.node)
				matrixSQL, _, err := chsql.Emit(context.Background(), rw)
				if err != nil {
					t.Fatalf("Emit: %v", err)
				}
				legSQL, legArgs := compareRootLeg(t, rw, matrixSQL)

				// A statement of its own, not a parenthesised fragment: the
				// caller runs this text directly.
				if !strings.HasPrefix(legSQL, "SELECT ") && !strings.HasPrefix(legSQL, "WITH ") {
					t.Errorf("root leg must be a standalone statement, got:\n%s", legSQL)
				}
				// The pairing the seam exists to own: one arg per placeholder
				// in the leg's own text. This counts placeholders to CHECK
				// that pairing; the extraction this seam replaced counted them
				// to RECONSTRUCT it — to guess which slice of the whole
				// statement's args belonged to the leg — which is the
				// assumption no test makes any more.
				if placeholders := strings.Count(legSQL, "?"); placeholders != len(legArgs) {
					t.Errorf("root leg binds %d placeholders but came with %d args (%#v):\n%s",
						placeholders, len(legArgs), legArgs, legSQL)
				}
			})
		}
	})

	t.Run("rejectsWhatHasNoRootLeg", func(t *testing.T) {
		t.Parallel()

		noRoot := compareNode() // RootLookup nil
		for _, tc := range []struct {
			name string
			rw   *chplan.RangeWindow
			want string
		}{
			{"nilRangeWindow", nil, "RangeWindow is nil"},
			{"noTimestampColumn", &chplan.RangeWindow{Input: compareNodeWithRoot(), Step: time.Minute}, "TimestampColumn unset"},
			{"zeroStep", &chplan.RangeWindow{Input: compareNodeWithRoot(), TimestampColumn: "Timestamp"}, "requires Step > 0"},
			{"notACompare", &chplan.RangeWindow{
				Input:           &chplan.Scan{Table: "otel_traces"},
				Step:            time.Minute,
				TimestampColumn: "Timestamp",
			}, "want *chplan.MetricsCompare"},
			{"noRootLookup", window(noRoot), "RootLookup is nil"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				_, _, err := chsql.EmitCompareRootLeg(context.Background(), tc.rw)
				if err == nil {
					t.Fatalf("EmitCompareRootLeg should fail for %s", tc.name)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("error %q missing %q", err, tc.want)
				}
			})
		}
	})
}

// TestEmitRangeWindowCompare_NonRootTraceIDTsPartialConfig pins that the
// trace_id_ts readiness triple is all-or-nothing. A schema override can name
// the table but leave a column blank; emitting `min()` over an empty
// identifier would be a broken query, not a looser envelope, so each
// half-configured triple must fall back to the unbounded-but-lossless shape.
// Each row also discriminates one clause of the readiness guard.
func TestEmitRangeWindowCompare_NonRootTraceIDTsPartialConfig(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name              string
		table, start, end string
	}{
		{"noTable", "", "Start", "End"},
		{"noStartColumn", "otel_traces_trace_id_ts", "", "End"},
		{"noEndColumn", "otel_traces_trace_id_ts", "Start", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := compareNodeWithRoot()
			m.RootLookupTraceIDTsTable = tc.table
			m.RootLookupTraceIDTsStartColumn = tc.start
			m.RootLookupTraceIDTsEndColumn = tc.end
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
			// With no envelope the FIRST TraceId-IN is the exact cohort seed, so
			// the prefix is the whole pre-seed scan filter.
			seed := strings.Index(rLeg, "`TraceId` IN (SELECT `TraceId`")
			if start < 0 || seed < 0 || seed <= start {
				t.Fatalf("expected root scan filter then TraceId seed:\n%s", rLeg)
			}
			if prefix := rLeg[start:seed]; strings.Contains(prefix, "`Timestamp`") {
				t.Errorf("half-configured trace_id_ts must not bound the root scan, got prefix:\n%s", prefix)
			}
		})
	}
}

// TestEmitRangeWindowCompare_TraceIDTsEnvelopeUnreferenced pins that the
// envelope binding is declared only when a scan actually reads it. A root
// lookup whose aggregate sits directly on its Scan has no Filter to push the
// bounds into, so the emitter keeps #1214's unbounded-but-lossless root leg —
// and must then drop the binding too. A scalar WITH that nothing references is
// not free: ClickHouse still evaluates it, which is a full trace_id_ts scan
// plus a render of the cohort seed bought for nothing.
func TestEmitRangeWindowCompare_TraceIDTsEnvelopeUnreferenced(t *testing.T) {
	t.Parallel()

	m := compareNodeWithRoot()
	m.RootLookupTraceIDTsTable = "otel_traces_trace_id_ts"
	m.RootLookupTraceIDTsStartColumn = "Start"
	m.RootLookupTraceIDTsEndColumn = "End"
	// Aggregate straight over Scan: no Filter for the bounds to land in.
	m.RootLookup = &chplan.Aggregate{
		Input:   &chplan.Scan{Table: "otel_traces"},
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "TraceId"}},
		AggFuncs: []chplan.AggFunc{
			{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: "SpanName"}}, Alias: "__root_name"},
		},
	}
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
	if strings.Contains(sql, chplan.TraceIDTsEnvelopeAlias) {
		t.Errorf("envelope binding must not be declared when no scan reads it:\n%s", sql)
	}
	if strings.Contains(sql, "`otel_traces_trace_id_ts`") {
		t.Errorf("unreferenced envelope must not scan trace_id_ts:\n%s", sql)
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
