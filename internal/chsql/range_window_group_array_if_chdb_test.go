//go:build chdb

package chsql

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

// Differential coverage for the split-window native array assembly
// (chopt.FeatureTSGridGroupArray, cerberus issue #2862):
// emitWindowedArrayExtrapolatedMatrix's needsDeltaFirstLevel branch builds
// `window_pairs` and `delta_prefix_pairs` from two complementary
// predicates, and under r.NativeGroupArray both become
// timeSeriesGroupArrayIf — whose own duplicate-timestamp collapse is what
// lets seriesArrayPairIfFrag's callers drop the
// arrayReverse/arrayCompact/arrayReverse triple at BOTH sites
// (matrixWindowPairsAlreadyDeduped for the window arm, deltaPrefixSumFrag's
// alreadyDeduped parameter for the prefix arm).
//
// # Why the assertion lives here and not in the TXTAR corpus
//
// native_group_array_if_rate_delta_temporality_range.txtar pins the SQL
// SHAPE and its answer against the real Prometheus engine, but it cannot
// seed a duplicate timestamp: the parity harness's DELTA-temporality
// adapter (cumulativeDeltaSeries, test/spec/parity_chdb.go) running-sums
// every seeded row into the cumulative series Prometheus accepts, so two
// rows sharing a timestamp are two additive increments there while
// cerberus collapses them to one sample. That is a disagreement about what
// a duplicate OTel DELTA observation MEANS, not about this feature. Here
// BOTH sides of the comparison are cerberus's own two lowerings, so the
// duplicate is exactly the interesting input.
//
// The survivor choice among DIFFERENT values at one timestamp (max-valued
// for finite samples, insertion-order dependent for a NaN — the reason
// chopt.FeatureTSGridGroupArray ships AutoSelect: false) is a property of
// the ClickHouse aggregate itself and is pinned against a real server by
// range_window_group_array_realch_integration_test.go, not here.

// groupArrayIfDupEnd / groupArrayIfDupStep / groupArrayIfDupRange anchor the
// query_range evaluation every scenario below shares: two anchors
// (00:00:00, 00:00:30) over a 1m window, with the seed's earliest sample
// sitting at or before the first anchor's window start so it lands in
// `delta_prefix_pairs` rather than `window_pairs`.
var (
	groupArrayIfDupStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	groupArrayIfDupEnd   = time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)
)

const (
	groupArrayIfDupStep  = 30 * time.Second
	groupArrayIfDupRange = time.Minute
)

// groupArrayIfDupWindow builds the DELTA-temporality rate() window both
// lowerings are emitted from. Only NativeGroupArray differs between the
// two runs, so any divergence in the answer is attributable to the array
// assembly and nothing else.
func groupArrayIfDupWindow(native bool) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Input: shapedDeltaPrefixInput(
			&chplan.Scan{Table: "otel_metrics_sum"},
			chplan.Projection{Expr: &chplan.ColumnRef{Name: "TimeUnix"}, Alias: "TimeUnix"},
			chplan.Projection{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"},
			chplan.Projection{Expr: &chplan.ColumnRef{Name: "AggregationTemporality"}, Alias: "AggregationTemporality"},
		),
		Func:              "rate",
		Range:             groupArrayIfDupRange,
		Start:             groupArrayIfDupStart,
		End:               groupArrayIfDupEnd,
		Step:              groupArrayIfDupStep,
		OuterRange:        groupArrayIfDupEnd.Sub(groupArrayIfDupStart),
		TimestampColumn:   "TimeUnix",
		ValueColumn:       "Value",
		TemporalityColumn: "AggregationTemporality",
		GroupBy:           []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		NativeGroupArray:  native,
	}
}

// runGroupArrayIfDupQuery emits r and returns job -> (anchor unix millis ->
// rate() value). The experimental ts-grid aggregate setting is applied to
// the session by the caller, since chDB's session is process-global.
func runGroupArrayIfDupQuery(t *testing.T, db *sql.DB, r *chplan.RangeWindow) map[string]map[int64]float64 {
	t.Helper()
	sqlText, args, err := Emit(context.Background(), r)
	if err != nil {
		t.Fatalf("Emit(native=%v): %v", r.NativeGroupArray, err)
	}
	inner := strings.TrimSuffix(strings.TrimSpace(sqlText), ";")
	wrapped := "SELECT Attributes['job'] AS job, toUnixTimestamp64Milli(anchor_ts) AS anchor_ms, Value FROM (" + inner + ")"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query(native=%v): %v\n%s", r.NativeGroupArray, err, sqlText)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]map[int64]float64{}
	for rows.Next() {
		var job string
		var anchorMs int64
		var value float64
		if err := rows.Scan(&job, &anchorMs, &value); err != nil {
			t.Fatalf("scan(native=%v): %v", r.NativeGroupArray, err)
		}
		if out[job] == nil {
			out[job] = map[int64]float64{}
		}
		out[job][anchorMs] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows(native=%v): %v", r.NativeGroupArray, err)
	}
	return out
}

// TestGroupArrayIf_NativeSplitWindow_DuplicateTimestampMatchesFanout is the
// assertion cerberus issue #2862 turns on: swapping the needsDeltaFirstLevel
// branch's two groupArrayIf calls for timeSeriesGroupArrayIf — and dropping
// the dedup pass on BOTH resulting arrays — must not change the answer,
// including on a series whose duplicate timestamps land in the prefix arm
// and in the window arm.
//
// Series "dup" duplicates a sample at 23:59:30 (which the 00:00:30 anchor
// reads through `delta_prefix_pairs`) and another at 00:00:00 (which both
// anchors read through `window_pairs`). Series "plain" seeds the SAME three
// distinct (timestamp, value) samples with no duplicates, so the two series
// must answer identically at every anchor: the fan-out's dedup triple and
// the native aggregate's own collapse both reduce "dup" to "plain". Without
// that collapse the DELTA reconstruction would double-count — the window arm
// sums window values minus the first, the prefix arm sums the whole prefix
// array — so "dup" would drift away from "plain" and this test would fail
// rather than pass on a no-op.
//
// The duplicate pairs carry EQUAL values, which is what makes "dup must
// equal plain" a well-defined claim about the collapse rather than about
// which of two competing samples survives it.
func TestGroupArrayIf_NativeSplitWindow_DuplicateTimestampMatchesFanout(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	deltaPrefixAggregateSeedDDL(t, db)
	if _, err := db.Exec("SET " + chclient.SettingExperimentalTSGridAggregate + " = 1"); err != nil {
		t.Fatalf("enable %s: %v", chclient.SettingExperimentalTSGridAggregate, err)
	}

	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(1, map('job', 'dup'), toDateTime64('2025-12-31 23:59:30', 9), 90),
		(1, map('job', 'dup'), toDateTime64('2025-12-31 23:59:30', 9), 90),
		(1, map('job', 'dup'), toDateTime64('2026-01-01 00:00:00', 9), 10),
		(1, map('job', 'dup'), toDateTime64('2026-01-01 00:00:00', 9), 10),
		(1, map('job', 'dup'), toDateTime64('2026-01-01 00:00:30', 9), 40),
		(1, map('job', 'plain'), toDateTime64('2025-12-31 23:59:30', 9), 90),
		(1, map('job', 'plain'), toDateTime64('2026-01-01 00:00:00', 9), 10),
		(1, map('job', 'plain'), toDateTime64('2026-01-01 00:00:30', 9), 40)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	fanout := runGroupArrayIfDupQuery(t, db, groupArrayIfDupWindow(false))
	native := runGroupArrayIfDupQuery(t, db, groupArrayIfDupWindow(true))

	for _, job := range []string{"dup", "plain"} {
		if len(fanout[job]) == 0 {
			t.Fatalf("job=%s: fan-out returned zero anchors — the fixture is not exercising the branch", job)
		}
		if len(native[job]) != len(fanout[job]) {
			t.Fatalf("job=%s: anchor-count divergence: native=%d fanout=%d", job, len(native[job]), len(fanout[job]))
		}
	}

	// The native and fan-out assemblies must agree, per series and per
	// anchor: the emission swap this issue makes is meant to be answer-
	// preserving.
	for job, anchors := range fanout {
		for anchorMs, want := range anchors {
			got, ok := native[job][anchorMs]
			if !ok {
				t.Errorf("job=%s anchor=%d present in fan-out but absent in native", job, anchorMs)
				continue
			}
			if math.Abs(got-want) > 1e-9 {
				t.Errorf("job=%s anchor=%d: native=%v fanout=%v NOT equal", job, anchorMs, got, want)
			}
		}
	}

	// The collapse itself: "dup" and "plain" describe the same deduplicated
	// series, so both lowerings must reduce them to the same answer. This is
	// what would fail if either dedup pass were dropped without the native
	// aggregate replacing it.
	for _, side := range []struct {
		name    string
		answers map[string]map[int64]float64
	}{
		{"fanout", fanout},
		{"native", native},
	} {
		for anchorMs, plain := range side.answers["plain"] {
			dup, ok := side.answers["dup"][anchorMs]
			if !ok {
				t.Errorf("%s: anchor=%d answered for job=plain but not for job=dup", side.name, anchorMs)
				continue
			}
			if math.Abs(dup-plain) > 1e-9 {
				t.Errorf(
					"%s anchor=%d: duplicate-timestamp series answered %v, its deduplicated twin answered %v — "+
						"the duplicate-timestamp collapse did not happen",
					side.name, anchorMs, dup, plain,
				)
			}
		}
	}
}
