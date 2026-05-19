package chsql_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestEmitQueryExemplars_GoldenSQL pins the SQL shape for the four
// metrics tables that carry the OTel-CH Exemplars Nested column
// (gauge / sum / histogram / exp_histogram). The summary table has no
// Exemplars column upstream and is rejected by a separate test below.
//
// The matcher predicate is a single `MetricName = ?` equality — the
// minimum the upstream Prom contract requires. PR B's handler wires
// the full label-matcher list via the existing `buildPredicate`
// helper; that bind shape is covered indirectly by the predicate-
// woven test further down.
func TestEmitQueryExemplars_GoldenSQL(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	// One predicate variant reused across the four-table fan-out.
	// Args from the predicate bind first; the time bounds (2 args each
	// — formatted string + precision 9) follow in source order.
	mkPredicate := func() chsql.Frag {
		return chsql.Eq(chsql.Col(s.MetricNameColumn), chsql.Lit("http_request_duration_seconds"))
	}

	// The outer SELECT references the inner SELECT's aliases via
	// BareIdent — `ts[i]`, `val[i]`, etc., without backticks — to
	// match the convention the existing emitter family (absent /
	// range-window) uses for emitter-pinned synthetic alias
	// references. `BareIdent` carries the trust contract that the
	// name is a CH-safe bare identifier; the emitter pins these
	// names.
	const wantSQLTmpl = "SELECT `MetricName`, `Attributes`, `ServiceName`, " +
		"ts[i] AS `Timestamp`, val[i] AS `Value`, " +
		"tid[i] AS `TraceID`, sid[i] AS `SpanID`, " +
		"attrs_arr[i] AS `ExemplarAttributes` " +
		"FROM (SELECT `MetricName`, `Attributes`, `ServiceName`, " +
		"`Exemplars`.`TimeUnix` AS `ts`, `Exemplars`.`Value` AS `val`, " +
		"`Exemplars`.`TraceId` AS `tid`, `Exemplars`.`SpanId` AS `sid`, " +
		"`Exemplars`.`FilteredAttributes` AS `attrs_arr`, " +
		"arrayJoin(arrayEnumerate(`Exemplars`.`TimeUnix`)) AS `i` " +
		"FROM `%TABLE%` " +
		"WHERE (`MetricName` = ?) AND `TimeUnix` >= toDateTime64(?, ?) " +
		"AND `TimeUnix` <= toDateTime64(?, ?) " +
		"AND length(`Exemplars`.`TimeUnix`) > 0)"

	cases := []struct {
		name  string
		table string
	}{
		{name: "gauge", table: s.GaugeTable},
		{name: "sum", table: s.SumTable},
		{name: "histogram", table: s.HistogramTable},
		{name: "exp_histogram", table: s.ExpHistogramTable},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sql, args, err := chsql.EmitQueryExemplars(
				context.Background(),
				tc.table,
				mkPredicate(),
				start,
				end,
				s,
			)
			if err != nil {
				t.Fatalf("EmitQueryExemplars(%q): %v", tc.table, err)
			}

			wantSQL := strings.Replace(wantSQLTmpl, "%TABLE%", tc.table, 1)
			if sql != wantSQL {
				t.Errorf("SQL mismatch\nwant: %s\n got: %s", wantSQL, sql)
			}

			// Arg order: predicate metric-name literal, then start
			// (formatted string + precision 9), then end (same).
			wantArgs := []any{
				"http_request_duration_seconds",
				"2026-01-01 00:00:00.000000000",
				int64(9),
				"2026-01-01 01:00:00.000000000",
				int64(9),
			}
			if len(args) != len(wantArgs) {
				t.Fatalf("Args length = %d; want %d (args=%v)", len(args), len(wantArgs), args)
			}
			for i, want := range wantArgs {
				if args[i] != want {
					t.Errorf("Args[%d] = %v (%T); want %v (%T)", i, args[i], args[i], want, want)
				}
			}
		})
	}
}

// TestEmitQueryExemplars_RejectsSummaryTable — the OTel-CH summary
// table has no `Exemplars` Nested column upstream. The emitter must
// refuse rather than emit SQL that references a non-existent column.
// PR B's handler routing must skip summary metrics before reaching
// the emitter, but this is the defensive guard at the lower layer.
func TestEmitQueryExemplars_RejectsSummaryTable(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	_, _, err := chsql.EmitQueryExemplars(
		context.Background(),
		s.SummaryTable,
		chsql.Eq(chsql.Col(s.MetricNameColumn), chsql.Lit("my_summary_metric")),
		start,
		end,
		s,
	)
	if err == nil {
		t.Fatalf("expected ErrUnsupported for summary table, got nil")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("expected ErrUnsupported, got %v", err)
	}
	if !strings.Contains(err.Error(), "summary") {
		t.Errorf("error %q should mention 'summary' to aid debugging", err.Error())
	}
}

// TestEmitQueryExemplars_RejectsMissingExemplarsColumn — when the
// schema has been configured with an empty `ExemplarsColumn` (e.g. a
// deployment running an exporter version that pre-dates exemplars),
// the emitter must refuse rather than emit SQL that scans a
// non-existent column.
func TestEmitQueryExemplars_RejectsMissingExemplarsColumn(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	s.ExemplarsColumn = ""
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	_, _, err := chsql.EmitQueryExemplars(
		context.Background(),
		s.SumTable,
		chsql.Eq(chsql.Col(s.MetricNameColumn), chsql.Lit("http_requests_total")),
		start,
		end,
		s,
	)
	if err == nil {
		t.Fatalf("expected ErrUnsupported for empty ExemplarsColumn, got nil")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("expected ErrUnsupported, got %v", err)
	}
	if !strings.Contains(err.Error(), "ExemplarsColumn") {
		t.Errorf("error %q should mention ExemplarsColumn to aid debugging", err.Error())
	}
}

// TestEmitQueryExemplars_RejectsEmptyTable — defensive guard against
// a caller passing an empty string for the exemplars table. The
// handler resolves the table from a known set, but we still want a
// crisp error rather than an SQL syntax failure deeper in the stack.
func TestEmitQueryExemplars_RejectsEmptyTable(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	_, _, err := chsql.EmitQueryExemplars(
		context.Background(),
		"",
		chsql.Eq(chsql.Col(s.MetricNameColumn), chsql.Lit("http_requests_total")),
		start,
		end,
		s,
	)
	if err == nil {
		t.Fatalf("expected ErrUnsupported for empty table, got nil")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("expected ErrUnsupported, got %v", err)
	}
}

// TestEmitQueryExemplars_PredicateWoven — confirms a multi-matcher
// predicate (the shape buildPredicate produces from the
// VectorSelector.LabelMatchers) gets parenthesised into the WHERE
// clause as the first conjunct, with the time-bound and
// non-empty-Exemplars guards following in source order. The args
// preserve emission order: predicate args first, then start, then
// end.
func TestEmitQueryExemplars_PredicateWoven(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	// A two-matcher predicate: MetricName = "x" AND Attributes["job"] = "api".
	// The map-subscript shape mirrors what buildPredicate emits for
	// non-`__name__` label matchers.
	predicate := chsql.And(
		chsql.Eq(chsql.Col(s.MetricNameColumn), chsql.Lit("http_request_duration_seconds")),
		chsql.Eq(
			chsql.Subscript(chsql.Col(s.AttributesColumn), chsql.Lit("job")),
			chsql.Lit("api"),
		),
	)

	sql, args, err := chsql.EmitQueryExemplars(
		context.Background(),
		s.SumTable,
		predicate,
		start,
		end,
		s,
	)
	if err != nil {
		t.Fatalf("EmitQueryExemplars: %v", err)
	}

	// The predicate is wrapped in a single Paren and AND-joined with
	// the time-bound and non-empty-Exemplars guards. The substring
	// pins the conjunct ordering (predicate first; non-empty guard
	// last) and the parenthesisation of the predicate as a single
	// WHERE conjunct.
	wantSub := "WHERE (`MetricName` = ? AND `Attributes`[?] = ?) " +
		"AND `TimeUnix` >= toDateTime64(?, ?) " +
		"AND `TimeUnix` <= toDateTime64(?, ?) " +
		"AND length(`Exemplars`.`TimeUnix`) > 0"
	if !strings.Contains(sql, wantSub) {
		t.Errorf("SQL missing expected WHERE shape\nwant substring: %s\n           got: %s", wantSub, sql)
	}

	// Arg order: MetricName literal, Attributes key, Attributes value,
	// start (string + 9), end (string + 9).
	wantArgs := []any{
		"http_request_duration_seconds",
		"job",
		"api",
		"2026-01-01 00:00:00.000000000",
		int64(9),
		"2026-01-01 01:00:00.000000000",
		int64(9),
	}
	if len(args) != len(wantArgs) {
		t.Fatalf("Args length = %d; want %d (args=%v)", len(args), len(wantArgs), args)
	}
	for i, want := range wantArgs {
		if args[i] != want {
			t.Errorf("Args[%d] = %v (%T); want %v (%T)", i, args[i], args[i], want, want)
		}
	}
}

// TestEmitQueryExemplars_NilPredicate — defensive case: when no
// matcher predicate is passed (e.g. a hypothetical "all exemplars"
// query the handler permits in a debug path), the emitter omits the
// matcher conjunct from the WHERE clause. The time bounds and
// non-empty-Exemplars guard still apply. The handler must always
// pass a non-nil predicate per the upstream Prom contract; this test
// pins the emitter's behaviour for the boundary case so a future
// caller misuse surfaces obviously rather than panics deep inside the
// builder.
func TestEmitQueryExemplars_NilPredicate(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	sql, args, err := chsql.EmitQueryExemplars(
		context.Background(),
		s.SumTable,
		nil, // intentionally nil — see test docstring
		start,
		end,
		s,
	)
	if err != nil {
		t.Fatalf("EmitQueryExemplars: %v", err)
	}

	wantSub := "WHERE `TimeUnix` >= toDateTime64(?, ?) " +
		"AND `TimeUnix` <= toDateTime64(?, ?) " +
		"AND length(`Exemplars`.`TimeUnix`) > 0"
	if !strings.Contains(sql, wantSub) {
		t.Errorf("SQL missing expected WHERE shape\nwant substring: %s\n           got: %s", wantSub, sql)
	}
	// No predicate args; just the two time bounds.
	wantArgs := []any{
		"2026-01-01 00:00:00.000000000",
		int64(9),
		"2026-01-01 01:00:00.000000000",
		int64(9),
	}
	if len(args) != len(wantArgs) {
		t.Fatalf("Args length = %d; want %d (args=%v)", len(args), len(wantArgs), args)
	}
	for i, want := range wantArgs {
		if args[i] != want {
			t.Errorf("Args[%d] = %v (%T); want %v (%T)", i, args[i], args[i], want, want)
		}
	}
}
