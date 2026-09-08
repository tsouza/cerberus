package chsql_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// searchTraceLimitScanTable is the spans table the SearchTraceLimit guard
// fixtures scan. Named so the assertion below can look for it in the
// emitted SQL: a guard that failed OPEN would render the input subtree,
// and the input subtree is the only thing that puts this token in the
// output.
const searchTraceLimitScanTable = "otel_traces"

func searchTraceLimitNode(traceLimit int64) *chplan.SearchTraceLimit {
	return &chplan.SearchTraceLimit{
		Input:           &chplan.Scan{Table: searchTraceLimitScanTable, Columns: []string{"TraceId", "Timestamp"}},
		TraceIDColumn:   "TraceId",
		TimestampColumn: "Timestamp",
		TraceLimit:      traceLimit,
	}
}

// TestEmitSearchTraceLimit_NonPositiveLimitFailsClosed pins that a
// SearchTraceLimit carrying a non-positive TraceLimit is REJECTED rather
// than emitted as its bare input.
//
// The top-N subquery is the only thing bounding /api/search's drain to N
// traces. Emitting the input unchanged (the previous behaviour) silently
// removed that bound and drained every trace matching the window — a
// scan-amplification defect no assertion could observe, because the
// emitter reported success. Failing closed matches every sibling guard in
// the package (emitTopK, emitMetricsSecondStageTopK,
// lagAdjacencyMatrixShapeCheck).
func TestEmitSearchTraceLimit_NonPositiveLimitFailsClosed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		limit int64
	}{
		{name: "zero limit", limit: 0},
		{name: "negative limit", limit: -1},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sql, _, err := chsql.Emit(context.Background(), searchTraceLimitNode(tc.limit))
			if err == nil {
				t.Fatalf("Emit(SearchTraceLimit{TraceLimit: %d}) returned no error; it emitted:\n%s\n"+
					"A non-positive trace limit must be rejected — emitting the input unchanged "+
					"drops the top-N restriction and drains every matching trace.", tc.limit, sql)
			}
			if !errors.Is(err, chsql.ErrUnsupported) {
				t.Errorf("error %v does not wrap chsql.ErrUnsupported; callers classify emit "+
					"rejections by that sentinel", err)
			}
			// The fail-open shape returned the input subtree's SQL, which
			// necessarily scans the spans table. Assert the table name is
			// absent so restoring `return e.emitNode(n.Input)` fails here
			// even if the emitter ever returned SQL alongside an error.
			if strings.Contains(sql, searchTraceLimitScanTable) {
				t.Errorf("rejected emit leaked the unbounded input scan of %q:\n%s",
					searchTraceLimitScanTable, sql)
			}
		})
	}
}

// TestEmitQueryExemplars_ZeroBoundRejected pins that a zero start or end
// is rejected with a wrapped ErrUnsupported instead of being formatted
// into the SQL as `0001-01-01 00:00:00.000000000` — a bound that reads as
// a real window but is either unbounded below or empty by construction.
//
// The HTTP handler validates the range too
// (internal/api/prom/exemplars.go), but the emitter is a public entry
// point and must not depend on its caller for that.
func TestEmitQueryExemplars_ZeroBoundRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	valid := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	predicate := chsql.Eq(chsql.Col(s.MetricNameColumn), chsql.Lit("http_requests_total"))
	// The zero time renders through Go's reference layout as this literal;
	// asserting on it makes the test fail loudly if the guard is removed
	// and the bound is silently formatted instead.
	const zeroTimeLiteral = "0001-01-01 00:00:00.000000000"

	cases := []struct {
		name       string
		start, end time.Time
	}{
		{name: "zero start", start: time.Time{}, end: valid},
		{name: "zero end", start: valid, end: time.Time{}},
		{name: "both zero", start: time.Time{}, end: time.Time{}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sql, args, _, err := chsql.EmitQueryExemplars(
				context.Background(), s.GaugeTable, predicate, tc.start, tc.end, s,
			)
			if err == nil {
				t.Fatalf("EmitQueryExemplars accepted a zero bound; it emitted:\n%s\nargs: %v", sql, args)
			}
			if !errors.Is(err, chsql.ErrUnsupported) {
				t.Errorf("error %v does not wrap chsql.ErrUnsupported", err)
			}
			for _, a := range args {
				if got, ok := a.(string); ok && got == zeroTimeLiteral {
					t.Errorf("zero time was formatted into the bound args as %q", zeroTimeLiteral)
				}
			}
		})
	}

	// The union entry point threads through the same builder, so it must
	// reject identically rather than only on the single-arm path.
	t.Run("union arm", func(t *testing.T) {
		t.Parallel()

		arms := []chsql.ExemplarArm{
			{Table: s.GaugeTable, Predicate: predicate},
			{Table: s.SumTable, Predicate: predicate},
		}
		if _, _, _, err := chsql.EmitQueryExemplarsUnion(
			context.Background(), arms, time.Time{}, valid, s,
		); !errors.Is(err, chsql.ErrUnsupported) {
			t.Errorf("EmitQueryExemplarsUnion with a zero start: err = %v, want ErrUnsupported", err)
		}
	})
}
