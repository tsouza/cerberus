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

// TestExemplarsMaxPerSeriesZeroNoLimit kills the CONDITIONALS_BOUNDARY
// mutant at exemplars.go:259 (`if maxPerSeries > 0` -> `>= 0`).
//
// With maxPerSeries == 0 the original guard is false, so the exemplars
// SQL emits NO `LIMIT N BY` cap (value 0 means "uncapped", per the
// EmitMetricsExemplars doc). Flip `>` to `>=` and the boundary value 0
// passes the guard, so the mutant emits `LIMIT 0 BY ...` — which both
// adds a LIMIT clause and (worse) caps every bucket to zero rows. We
// pin the original by asserting the emitted SQL contains no `LIMIT`
// token at all when maxPerSeries == 0.
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
		t.Errorf("maxPerSeries==0 must emit no LIMIT cap, but SQL contains LIMIT (boundary mutant `>=0` would emit `LIMIT 0 BY`):\n%s", sql)
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
}

// TestRangeBucketFanoutEmptyGroupBy kills the ARITHMETIC_BASE mutant at
// range_bucket_fanout.go:160 (`make([]Frag, 0, len(r.GroupBy)+1)` ->
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
				Name:  "argMax",
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

	// Start set, End zero over a spans inner -> fail closed.
	rwOneEnd := &chplan.RangeWindow{
		Input:           compareNode(),
		Range:           time.Minute,
		Step:            time.Minute,
		Start:           start,
		End:             time.Time{}, // zero
		TimestampColumn: "Timestamp",
	}
	_, _, err := chsql.Emit(context.Background(), rwOneEnd)
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
	sqlBoth, _, err := chsql.Emit(context.Background(), rwBoth)
	if err != nil {
		t.Fatalf("Emit (both ends): %v", err)
	}
	if !strings.Contains(sqlBoth, "toDateTime64") {
		t.Errorf("scan bound MUST be pushed with both Start and End set:\n%s", sqlBoth)
	}
}
