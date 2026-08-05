//go:build chdb

// chDB-backed wire-level pins for `__name__` on `last_over_time` /
// `first_over_time` applied to a SUBQUERY.
//
// Prometheus keeps `__name__` on exactly two range functions
// (promql/engine.go:2114, `dropName := (fn != "last_over_time" && fn !=
// "first_over_time")`) and resolves the surviving name PER SERIES from the
// input series' own labels. For a subquery the input series come from
// `evalSubquery`, so the outer reducer reads whatever name the INNER
// produced — which is why nothing here can be folded to a query-text
// literal.
//
// Cerberus lowers a subquery into a stack of RangeWindows that each
// grouped on the Attributes map ALONE. The windowed-array emitter projects
// exactly the grouping keys, so `MetricName` was absent from the window's
// output relation and the HTTP layer's `wrapWithSampleProjection`
// classified the plan as a derived shape and synthesised `'' AS
// MetricName` — dropping `__name__` on the wire (#1602, #1778, #1794).
//
// Every seed below uses TWO metrics with an IDENTICAL attribute set, so a
// dropped name is doubly visible: the label is missing AND the two series
// collapse into one.

package prom_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
)

// subqueryNameSeed seeds `cpu_temp` and `gpu_temp` under one shared
// attribute set, one sample per minute across [start-15m, end], so every
// step anchor of a `[10m:1m]` subquery has data in its window.
//
// metaShapedMetricsDDL creates every metric table: a regex `__name__`
// matcher cannot be resolved to a single table, so the scan unions all of
// them and chDB errors on a missing-table read.
func subqueryNameSeed(t *testing.T, start, end time.Time) string {
	t.Helper()
	const (
		seedLead  = 15 * time.Minute
		seedEvery = time.Minute
	)
	rows := ""
	value := 0
	for ts := start.Add(-seedLead); !ts.After(end); ts = ts.Add(seedEvery) {
		lit := ts.Format("2006-01-02 15:04:05.000000000")
		if rows != "" {
			rows += ",\n    "
		}
		rows += fmt.Sprintf(
			"('cpu_temp', '', '', map('host', 'a'), toDateTime64('%[1]s', 9), %[2]d.0),\n    "+
				"('gpu_temp', '', '', map('host', 'a'), toDateTime64('%[1]s', 9), %[3]d.0)",
			lit, 40+value, 70+value,
		)
		value++
	}
	return metaShapedMetricsDDL + `
INSERT INTO otel_metrics_gauge (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ` + rows + ";"
}

// subqueryNameWindow is the shared request window: a 5-minute range at a
// 1-minute step, anchored on a fixed instant so nothing depends on
// wall-clock time.
func subqueryNameWindow() (start, end time.Time, step time.Duration) {
	start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return start, start.Add(5 * time.Minute), time.Minute
}

// assertNamedTempSeries asserts the matrix holds exactly the two seeded
// metrics, each carrying its own `__name__` plus the shared `host` label.
// Two series with one attribute set is only expressible if `__name__`
// survived, so this single assertion covers both halves of the bug.
func assertNamedTempSeries(t *testing.T, matrix []prom.MatrixSample, query string) {
	t.Helper()
	if len(matrix) != 2 {
		t.Fatalf("%s: got %d series, want 2 (cpu_temp + gpu_temp share one attribute set, so a dropped __name__ merges them): %+v",
			query, len(matrix), matrix)
	}
	seen := map[string]bool{}
	for _, s := range matrix {
		name, ok := s.Metric["__name__"]
		if !ok {
			t.Fatalf("%s: series is missing __name__ (full metric: %+v)", query, s.Metric)
		}
		if seen[name] {
			t.Fatalf("%s: __name__ %q reported twice (full result: %+v)", query, name, matrix)
		}
		seen[name] = true
		if got := s.Metric["host"]; got != "a" {
			t.Errorf("%s: host label: got %q, want %q (full metric: %+v)", query, got, "a", s.Metric)
		}
		if len(s.Values) == 0 {
			t.Errorf("%s: series %q carries no samples", query, name)
		}
	}
	for _, want := range []string{"cpu_temp", "gpu_temp"} {
		if !seen[want] {
			t.Errorf("%s: expected a series named %q, got %v", query, want, seen)
		}
	}
}

// TestQueryRange_Subquery_LastOverTime_PreservesName_ChDB is #1778: a
// regex `__name__` matcher under a subquery. The name differs per series,
// so it can only reach the wire by riding the spine's grouping keys.
func TestQueryRange_Subquery_LastOverTime_PreservesName_ChDB(t *testing.T) {
	start, end, step := subqueryNameWindow()
	srv, _ := newChDBServer(t, subqueryNameSeed(t, start, end))

	const query = `last_over_time({__name__=~"cpu_temp|gpu_temp"}[10m:1m])`
	matrix := runRangeModeQueryRange(t, srv.URL, query, start, end, step)
	assertNamedTempSeries(t, matrix, query)
}

// TestQueryRange_Subquery_FirstOverTime_PreservesName_ChDB pins the other
// half of the preserving pair, so a narrowing of the preserve set cannot
// pass with only `last_over_time` covered.
func TestQueryRange_Subquery_FirstOverTime_PreservesName_ChDB(t *testing.T) {
	start, end, step := subqueryNameWindow()
	srv, _ := newChDBServer(t, subqueryNameSeed(t, start, end))

	const query = `first_over_time({__name__=~"cpu_temp|gpu_temp"}[10m:1m])`
	matrix := runRangeModeQueryRange(t, srv.URL, query, start, end, step)
	assertNamedTempSeries(t, matrix, query)
}

// TestQueryRange_Subquery_AtPinned_PreservesName_ChDB is #1602: an `@`
// modifier freezes the subquery's evaluation instant, so range mode
// evaluates the window once and broadcasts it across the step grid. That
// broadcast arm hardcoded the EMPTY metric-name literal.
func TestQueryRange_Subquery_AtPinned_PreservesName_ChDB(t *testing.T) {
	start, end, step := subqueryNameWindow()
	srv, _ := newChDBServer(t, subqueryNameSeed(t, start, end))

	query := fmt.Sprintf(`last_over_time({__name__=~"cpu_temp|gpu_temp"}[10m:1m] @ %d)`, start.Unix())
	matrix := runRangeModeQueryRange(t, srv.URL, query, start, end, step)
	assertNamedTempSeries(t, matrix, query)
}

// TestQueryRange_Subquery_NestedCall_PreservesName_ChDB is #1794: the
// subquery's body is a range-function CALL rather than a bare selector, so
// the spine is RangeWindow-over-RangeWindow and the name must survive both
// windowed-array GROUP BYs.
func TestQueryRange_Subquery_NestedCall_PreservesName_ChDB(t *testing.T) {
	start, end, step := subqueryNameWindow()
	srv, _ := newChDBServer(t, subqueryNameSeed(t, start, end))

	const query = `last_over_time(last_over_time({__name__=~"cpu_temp|gpu_temp"}[5m])[10m:1m])`
	matrix := runRangeModeQueryRange(t, srv.URL, query, start, end, step)
	assertNamedTempSeries(t, matrix, query)
}

// TestQueryRange_Subquery_RateInner_DropsName_ChDB is the NEGATIVE pin:
// `rate` has already stripped `__name__` (upstream's `inputDropName`), so
// a preserving OUTER function must not resurrect it. Only the label's
// absence is asserted — how many series a nameless duplicate labelset
// yields is a separate question this test deliberately does not answer.
func TestQueryRange_Subquery_RateInner_DropsName_ChDB(t *testing.T) {
	start, end, step := subqueryNameWindow()
	srv, _ := newChDBServer(t, subqueryNameSeed(t, start, end))

	const query = `last_over_time(rate({__name__=~"cpu_temp|gpu_temp"}[5m])[10m:1m])`
	matrix := runRangeModeQueryRange(t, srv.URL, query, start, end, step)
	if len(matrix) == 0 {
		t.Fatalf("%s: got an empty matrix; the fixture cannot discriminate a dropped name", query)
	}
	for _, s := range matrix {
		if name, ok := s.Metric["__name__"]; ok {
			t.Errorf("%s: __name__ %q survived a rate inner (full metric: %+v)", query, name, s.Metric)
		}
	}
}
