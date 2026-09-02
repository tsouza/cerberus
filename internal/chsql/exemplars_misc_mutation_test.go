package chsql_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestExemplarsMaxPerSeriesZeroNoLimit pins the uncapped contract of
// EmitMetricsExemplars' maxPerSeries parameter: a value of 0 means "every
// span in every bucket window flows through", so the statement must carry
// no `LIMIT N BY` cap at all, while a positive value must carry exactly
// that cap.
//
// It does NOT kill the CONDITIONALS_BOUNDARY mutant on the guard it
// exercises (exemplars.go:`if maxPerSeries > 0` -> `>= 0`); see the
// NOT KILLABLE note at the foot of this file for why that mutant is
// equivalent. The contract is worth pinning regardless — it is the
// difference between "uncapped" and "every bucket capped to zero rows".
func TestExemplarsMaxPerSeriesZeroNoLimit(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)
	m := &chplan.MetricsAggregate{
		Op:             chplan.MetricsOpRate,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "resource.service.name"}},
		GroupByAliases: []string{"resource.service.name"},
		ValueAlias:     "Value",
		Inner:          &chplan.Scan{Table: "otel_traces"},
	}
	rw := &chplan.RangeWindow{
		Input:           m,
		Step:            time.Minute,
		Range:           time.Minute,
		Start:           start,
		End:             end,
		TimestampColumn: "Timestamp",
	}

	// maxPerSeries == 0 -> uncapped -> no LIMIT clause.
	sql, _, err := chsql.EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 0, "")
	if err != nil {
		t.Fatalf("EmitMetricsExemplars: %v", err)
	}
	if strings.Contains(sql, "LIMIT") {
		t.Errorf("maxPerSeries==0 means uncapped, but the SQL carries a LIMIT:\n%s", sql)
	}

	// Sanity counter-case: a positive cap DOES emit the LIMIT BY, proving
	// the assertion above is discriminating (not vacuously true).
	sqlCapped, _, err := chsql.EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 3, "")
	if err != nil {
		t.Fatalf("EmitMetricsExemplars capped: %v", err)
	}
	if !strings.Contains(sqlCapped, "LIMIT 3 BY") {
		t.Errorf("maxPerSeries==3 must emit `LIMIT 3 BY`:\n%s", sqlCapped)
	}

	// The UNGROUPED shape takes the same branch with an empty alias list, so
	// the LIMIT BY key list collapses to the anchor alone. The emitter appends
	// it unconditionally —
	// exemplars.go:`limitByFrags = append(limitByFrags, Col("anchor_ts"))` —
	// which is what keeps `LIMIT n BY` from rendering an empty key list, which
	// ClickHouse rejects. The grouped case above cannot show that, because its
	// aliases would carry the clause on their own.
	mUngrouped := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpRate,
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	}
	rwUngrouped := &chplan.RangeWindow{
		Input:           mUngrouped,
		Step:            time.Minute,
		Range:           time.Minute,
		Start:           start,
		End:             end,
		TimestampColumn: "Timestamp",
	}
	sqlUngrouped, _, err := chsql.EmitMetricsExemplars(
		context.Background(), rwUngrouped, mUngrouped, "TraceId", "SpanId", 2, "",
	)
	if err != nil {
		t.Fatalf("EmitMetricsExemplars ungrouped: %v", err)
	}
	if !strings.Contains(sqlUngrouped, "LIMIT 2 BY `anchor_ts`") {
		t.Errorf("an ungrouped cap must emit `LIMIT 2 BY `anchor_ts``:\n%s", sqlUngrouped)
	}
}

// TestRangeBucketFanoutEmptyGroupBy kills the ARITHMETIC_BASE mutant at
// range_bucket_fanout.go:`make([]Frag, 0, len(r.GroupBy)+1)` (->
// `... len(r.GroupBy)-1`).
//
// The collapse GROUP BY slot is pre-sized to len(GroupBy)+1 (the user
// keys plus the implicit anchor). With an empty GroupBy the original
// capacity is 1; flip `+` to `-` and the capacity becomes
// `len(GroupBy)-1 == -1`, which makes `make` panic with "cap out of
// range" at emit time. Exercising the emitter with an empty GroupBy
// therefore turns the mutation into a panic (test failure) while the
// original emits clean SQL grouping by the anchor alone.
func TestRangeBucketFanoutEmptyGroupBy(t *testing.T) {
	t.Parallel()

	plan := &chplan.RangeBucketFanout{
		Input:        &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
		Start:        time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
		End:          time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC),
		Step:         30 * time.Second,
		Lookback:     5 * time.Minute,
		GroupBy:      nil, // empty -> original cap 1, mutant cap -1 (panics)
		AnchorAlias:  "anchor_ts",
		TimestampCol: "TimeUnix",
		AggFuncs: []chplan.AggFunc{
			{
				Fn:    chplan.FnArgMax,
				Alias: "BucketCounts",
				Args: []chplan.Expr{
					&chplan.ColumnRef{Name: "BucketCounts"},
					&chplan.ColumnRef{Name: "TimeUnix"},
				},
			},
		},
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit (empty GroupBy): %v", err)
	}
	// The collapse must still GROUP BY the lone anchor alias (emitted
	// verbatim, since it is the fan-out SELECT's output column).
	if !strings.Contains(sql, "GROUP BY anchor_ts") {
		t.Errorf("empty-GroupBy fan-out must GROUP BY the anchor alone:\n%s", sql)
	}
}

// TestMetricsCompareScanBoundRequiresBothEnds pins the fail-closed
// spans-scan resource-bound contract on the matrix-compare inner scan.
//
// A compare over the spans table whose request window is half-open (only
// Start or only End set) cannot partition-prune the inner MergeTree legs:
// the (Start-range, End] pushdown needs both endpoints. requireInnerSpansScanBound
// rejects that shape with ErrUnboundedSpansScan rather than silently
// emitting a full-retention scan. This kills the INVERT_LOGICAL mutant on
// the guard's `rw.Start.IsZero() || rw.End.IsZero()` (flip to `&&` and a
// half-open window would slip through). The both-ends case proves the
// guard is non-vacuous: it passes and DOES push the toDateTime64 bound.
func TestMetricsCompareScanBoundRequiresBothEnds(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)

	// The spans-scoped emit context (as the engine threads for the Tempo head)
	// is what arms the resource-bound invariant; an isolated emit is a no-op.
	ctx := chsql.WithSpansTable(context.Background(), "otel_traces")

	// Start set, End zero over a spans inner -> fail closed.
	rwOneEnd := &chplan.RangeWindow{
		Input:           compareNode(),
		Range:           time.Minute,
		Step:            time.Minute,
		Start:           start,
		End:             time.Time{}, // zero
		TimestampColumn: "Timestamp",
	}
	_, _, err := chsql.Emit(ctx, rwOneEnd)
	if !errors.Is(err, chsql.ErrUnboundedSpansScan) {
		t.Fatalf("Start-only window over spans inner must fail closed with ErrUnboundedSpansScan, got %v", err)
	}

	// Both ends set -> bound IS pushed (toDateTime64 present). Proves the
	// assertion above is non-vacuous.
	rwBoth := &chplan.RangeWindow{
		Input:           compareNode(),
		Range:           time.Minute,
		Step:            time.Minute,
		Start:           start,
		End:             start.Add(3 * time.Minute),
		TimestampColumn: "Timestamp",
	}
	sqlBoth, _, err := chsql.Emit(ctx, rwBoth)
	if err != nil {
		t.Fatalf("Emit (both ends): %v", err)
	}
	if !strings.Contains(sqlBoth, "toDateTime64") {
		t.Errorf("scan bound MUST be pushed with both Start and End set:\n%s", sqlBoth)
	}
}

// TestExemplarsRateAndCountIgnoreAttr pins the two-site agreement that
// makes the exemplar Value column well-formed for the counting ops:
// EmitMetricsExemplars's inner SELECT projects the metric operand as
// `metric_arg` ONLY when the op is neither rate nor count_over_time, so
// its Value expression must reach for `argMax(1, ts)` — never
// `argMax(metric_arg, ts)` — for exactly those two ops. The two guards
// are a pair: whichever one moves, the other has to move with it.
// Whether the caller left Attr set is irrelevant:
// rate and count_over_time count rows, and the operand a caller may have
// attached is deliberately ignored.
//
// Inverting the `||` on the Value-expression guard makes it
// unsatisfiable. A rate/count_over_time node carrying an Attr then falls into the
// `else if m.Attr != nil` arm and emits `argMax(metric_arg, ts)` against
// a column the inner SELECT never projected — SQL ClickHouse rejects at
// parse time. Nothing in the suite asked, because every test built the
// counting ops with a nil Attr, where both forms collapse onto the same
// `argMax(1, ts)` else-arm (cerberus issue #2943; the mutant reached a
// verdict for the first time once #2940 stopped `go vet`'s bools
// analyzer rejecting it as "suspect and").
//
// EmitMetricsExemplars is an exported emitter entrypoint whose input is
// a *chplan.MetricsAggregate, so "no lowering builds that combination
// today" is not a property of this package and cannot be what keeps the
// emitted SQL well-formed. The guard is.
func TestExemplarsRateAndCountIgnoreAttr(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC)

	emit := func(t *testing.T, op chplan.MetricsOp, attr chplan.Expr) string {
		t.Helper()
		m := &chplan.MetricsAggregate{
			Op:             op,
			Attr:           attr,
			GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "resource.service.name"}},
			GroupByAliases: []string{"resource.service.name"},
			ValueAlias:     "Value",
			Inner:          &chplan.Scan{Table: "otel_traces"},
		}
		rw := &chplan.RangeWindow{
			Input:           m,
			Step:            time.Minute,
			Range:           time.Minute,
			Start:           start,
			End:             end,
			TimestampColumn: "Timestamp",
		}
		sql, _, err := chsql.EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 0, "")
		if err != nil {
			t.Fatalf("EmitMetricsExemplars(op=%v): %v", op, err)
		}
		return sql
	}

	for _, op := range []chplan.MetricsOp{chplan.MetricsOpRate, chplan.MetricsOpCountOverTime} {
		op := op
		t.Run(op.String(), func(t *testing.T) {
			t.Parallel()
			sql := emit(t, op, &chplan.ColumnRef{Name: "Duration"})
			if !strings.Contains(sql, "toFloat64(argMax(1, `ts`))") {
				t.Errorf("%v with an Attr must still count rows via argMax(1, ts):\n%s", op, sql)
			}
			if strings.Contains(sql, "metric_arg") {
				t.Errorf("%v must never reference metric_arg — the inner SELECT does not project it:\n%s", op, sql)
			}
		})
	}

	// Counter-case: an op that DOES read the operand projects metric_arg
	// and reduces over it, so the two assertions above are discriminating
	// rather than true of every emitted exemplar statement.
	t.Run("sum_over_time reads the operand", func(t *testing.T) {
		t.Parallel()
		sql := emit(t, chplan.MetricsOpSumOverTime, &chplan.ColumnRef{Name: "Duration"})
		if !strings.Contains(sql, "toFloat64(argMax(`metric_arg`, `ts`))") {
			t.Errorf("sum_over_time must reduce over the projected metric_arg:\n%s", sql)
		}
	})
}

// TestExemplarsAttributesMapKeyShape pins the key layout of the
// exemplar Attributes map that attachExemplars depends on: one
// `toString(<alias>)` value per group key, keyed by the Tempo-canonical
// display name, plus the `trace:id` / `span:id` pair. Those four keys
// are what internal/api/tempo/metrics_query_range.go reads back off
// chclient.Sample.Attributes, so the emitter is the only place the
// contract can be pinned against real emitted SQL and real bound args.
//
// It does NOT kill the ARITHMETIC_BASE mutant on
// exemplars.go:`len(groupAliases)*2+6`, and does not try to: that
// operand is a capacity hint, and the map this test inspects is
// byte-identical under every capacity. See the NOT KILLABLE note at the
// foot of this file. The contract is worth pinning on its own merits —
// a dropped display-name key silently unmatches every exemplar from its
// parent series.
func TestExemplarsAttributesMapKeyShape(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 12, 1, 0, 0, time.UTC)
	m := &chplan.MetricsAggregate{
		Op: chplan.MetricsOpRate,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: "ServiceName"},
			&chplan.ColumnRef{Name: "SpanName"},
		},
		GroupByAliases:      []string{"svc", "span"},
		GroupByDisplayNames: []string{"service", "span"},
		ValueAlias:          "Value",
		Inner:               &chplan.Scan{Table: "otel_traces"},
	}
	rw := &chplan.RangeWindow{
		Input: m, Step: time.Minute, Range: time.Minute,
		Start: start, End: end, TimestampColumn: "Timestamp",
	}
	sql, args, err := chsql.EmitMetricsExemplars(context.Background(), rw, m, "TraceId", "SpanId", 1, "")
	if err != nil {
		t.Fatalf("EmitMetricsExemplars: %v", err)
	}
	// Every map KEY rides as a bound arg (the keys render as `?`
	// placeholders), so both display names and both id keys must appear.
	for _, want := range []string{"service", "span", "trace:id", "span:id"} {
		found := false
		for _, a := range args {
			if s, ok := a.(string); ok && s == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected map key %q in bound args; got %v", want, args)
		}
	}
	// And the VALUE side renders toString(<alias>) for each group key.
	if !strings.Contains(sql, "toString(`svc`)") {
		t.Errorf("expected toString(svc) as the `service` map value:\n%s", sql)
	}
	if !strings.Contains(sql, "toString(`span`)") {
		t.Errorf("expected toString(span) as the `span` map value:\n%s", sql)
	}
}

// NOT KILLABLE — documented, not defended by a test.
//
// exemplars.go:`if maxPerSeries > 0` (CONDITIONALS_BOUNDARY, `>` -> `>=`).
// The two forms differ only at maxPerSeries == 0, where the mutant
// enters the block and calls `outerSb.Limit(0)` followed by
// `outerSb.LimitBy(...)`. QueryBuilder.Limit records `hasLimit = n > 0`, so
// Limit(0) leaves hasLimit false, and the renderer emits the whole LIMIT /
// LIMIT BY tail only under `if s.hasLimit` — the LimitBy Frags are never
// invoked, so they bind no args either. The mutant therefore produces a
// byte-identical statement and an identical args slice; only two dead
// allocations differ. (A negative maxPerSeries fails both forms alike.)
//
// exemplars.go:`len(groupAliases)*2+6` (ARITHMETIC_BASE,
// `make([]Frag, 0, len(groupAliases)*2+6)` -> `len(groupAliases)/2+6`). The mutated operand
// is the Attributes-map slice's capacity HINT, which append grows on
// demand; `len/2 + 6` is non-negative for every input, so it cannot even
// panic the way the sibling `+6` -> `-6` mutant does. Capacity is
// unobservable from the emitted SQL, the args, or any exported surface.
//
// Two independent confirmations, because a bare "capacity is
// unobservable" reads like an assumption (cerberus issue #2958):
//
//   - EMPIRICAL. With `*2` flipped to `/2` by hand, the whole
//     internal/chsql package passes. With the sibling `+6` flipped to
//     `-6`, TestEmitMetricsExemplars_PlainWindowedInnerAccepted panics
//     with "makeslice: cap out of range". The two mutants on one
//     expression land on opposite verdicts, which is exactly what the
//     capacity/length distinction predicts.
//
//   - STRUCTURAL, and the stronger of the two. `cap` IS readable from
//     inside this package, so "equivalent" would not follow from
//     "invisible in the SQL" alone if the slice escaped. It does not:
//     attrMapFrags leaves emitMetricsExemplars only through the
//     `Call("map", attrMapFrags...)` at the end of the Attributes
//     projection — named in prose, not cited, because a footer that
//     cites a construct it is NOT adjudicating registers a verdict on
//     it. Call closes over its variadic slice in a returned
//     `func(*Builder)`. Go exposes
//     no way to read a captured variable out of a func value, so no
//     test in any package — internal or external — can observe this
//     capacity. The mutant is unkillable, not merely unkilled.
//
// TestExemplarsAttributesMapKeyShape above pins the map's key layout and
// is deliberately NOT presented as a kill for this mutant.
