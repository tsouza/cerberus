//go:build chdb

// chDB-backed proof that a series whose bucket ladder is EMPTY — every
// observation fell in the zero bucket, so the OTel SDK / CH exporter wrote
// PositiveOffset 0 with no buckets — contributes nothing to a merge's
// natural width. Such a row still carries an offset, and that offset used
// to enter the width computation as if it were a real bucket position: a
// Scale-20 group with real buckets far from index 0 then measured a
// "natural width" that spanned from 0 to the real buckets and was
// downscaled to Scale 5 for no reason. Every merge path shares the same
// per-row start/end arithmetic, so every path is checked here.
package promql_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/promql"
)

// emptyLadderRealScale is the scale the real rows carry — the OTel SDK's
// maximum resolution, at which a merge has the most to lose from a
// spurious downscale.
const emptyLadderRealScale = 20

// emptyLadderRealOffset is the real rows' bucket position at Scale 20: far
// enough from index 0 (the empty row's offset) that an empty ladder counted
// as a bucket position would force fifteen extra downscale steps.
const emptyLadderRealOffset = -4_500_000

// emptyLadderRows renders the real four-bucket row at `offset`, plus either
// a second real row two positions away (the control) or one all-zero
// observation row with an empty ladder at PositiveOffset 0 (the case).
func emptyLadderRows(metric, route string, at time.Time, withEmpty bool) []string {
	ts := at.Format("2006-01-02 15:04:05")
	rows := []string{fmt.Sprintf(
		"('%s', map('route', '%s', 'series', 'real'), toDateTime64('%s', 9), 10, 40.0, %d, 0, %d, [1,2,3,4], 0, [])",
		metric, route, ts, emptyLadderRealScale, emptyLadderRealOffset,
	)}
	if withEmpty {
		rows = append(rows, fmt.Sprintf(
			"('%s', map('route', '%s', 'series', 'zeros'), toDateTime64('%s', 9), 3, 0.0, %d, 3, 0, [], 0, [])",
			metric, route, ts, emptyLadderRealScale,
		))
	} else {
		rows = append(rows, fmt.Sprintf(
			"('%s', map('route', '%s', 'series', 'real2'), toDateTime64('%s', 9), 3, 12.0, %d, 0, %d, [3], 0, [])",
			metric, route, ts, emptyLadderRealScale, emptyLadderRealOffset-2,
		))
	}
	return rows
}

func emptyLadderFixture(t *testing.T, withEmpty bool) *chdbFixture {
	t.Helper()
	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram " + histogramMergeBoundInsertColumns + " VALUES\n")
	b.WriteString("    " + strings.Join(emptyLadderRows(histogramMergeBoundMetric, "r0", histogramMergeBoundEvalTS.Add(-time.Second), withEmpty), ",\n    ") + ";\n")
	return newChDBFixture(t, b.String())
}

// TestHistogramMerge_ChDB_EmptyLadderDoesNotCoarsenScale runs every merge
// shape over a Scale-20 group once with a second real row (control) and
// once with an empty-ladder row instead, and asserts the merged Scale is
// the same in both: the empty row must not widen the merge.
func TestHistogramMerge_ChDB_EmptyLadderDoesNotCoarsenScale(t *testing.T) {
	sumQuery := fmt.Sprintf("sum(%s)", histogramMergeBoundMetric)
	byQuery := fmt.Sprintf("sum by(route) (%s)", histogramMergeBoundMetric)
	for _, tc := range []struct {
		name  string
		query string
		opts  promql.LowerOpts
	}{
		{"fold", sumQuery, promql.LowerOpts{}},
		{"sumMap single group", sumQuery, promql.LowerOpts{Lowerers: expHistSumMapBoundNativeLowerers}},
		{"sumMap multi group", byQuery, promql.LowerOpts{Lowerers: expHistSumMapBoundNativeLowerers}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control, err := readMergedHistogramShape(t, emptyLadderFixture(t, false), tc.query, tc.opts)
			if err != nil {
				t.Fatalf("control merge: %v", err)
			}
			if control.Scale != emptyLadderRealScale {
				t.Fatalf("control merged Scale = %d, want the rows' own %d (two rows two buckets apart need no downscale)", control.Scale, emptyLadderRealScale)
			}
			got, err := readMergedHistogramShape(t, emptyLadderFixture(t, true), tc.query, tc.opts)
			if err != nil {
				t.Fatalf("merge with an empty-ladder row: %v", err)
			}
			if got.Scale != control.Scale {
				t.Fatalf("an all-zero-observation series coarsened the merge: Scale %d, control %d", got.Scale, control.Scale)
			}
			if got.Width != 4 {
				t.Fatalf("merged width = %d, want the real row's own 4 buckets", got.Width)
			}
			if got.Count != 13 {
				t.Fatalf("merged Count = %v, want 13 (10 real + 3 zero observations)", got.Count)
			}
		})
	}
}

// TestHistogramMerge_ChDB_EmptyLadderDoesNotCoarsenRangeScale is the
// range-mode sibling for the sumMap window chain, which computes its
// per-step natural width from per-row start/end scalars rather than a
// groupArray.
func TestHistogramMerge_ChDB_EmptyLadderDoesNotCoarsenRangeScale(t *testing.T) {
	const steps = 3
	start := histogramMergeBoundEvalTS
	end := start.Add((steps - 1) * expHistSumMapRangeBoundStep)
	seed := func(withEmpty bool) *chdbFixture {
		var b strings.Builder
		b.WriteString(histogramMergeBoundSeedDDL)
		b.WriteString("INSERT INTO otel_metrics_exponential_histogram " + histogramMergeBoundInsertColumns + " VALUES\n")
		var tuples []string
		for step := 0; step < steps; step++ {
			at := start.Add(time.Duration(step) * expHistSumMapRangeBoundStep).Add(-time.Second)
			tuples = append(tuples, emptyLadderRows(histogramMergeBoundMetric, "r0", at, withEmpty)...)
		}
		b.WriteString("    " + strings.Join(tuples, ",\n    ") + ";\n")
		return newChDBFixture(t, b.String())
	}
	query := fmt.Sprintf("sum(%s)", histogramMergeBoundMetric)
	opts := promql.LowerOpts{Lowerers: expHistSumMapBoundNativeLowerers}
	control, err := readMergedHistogramShapes(t, seed(false), query, start, end, expHistSumMapRangeBoundStep, opts)
	if err != nil {
		t.Fatalf("control merge: %v", err)
	}
	got, err := readMergedHistogramShapes(t, seed(true), query, start, end, expHistSumMapRangeBoundStep, opts)
	if err != nil {
		t.Fatalf("merge with an empty-ladder row: %v", err)
	}
	if len(control) != steps || len(got) != steps {
		t.Fatalf("got %d control / %d case rows, want %d each", len(control), len(got), steps)
	}
	for i := range got {
		if control[i].Scale != emptyLadderRealScale {
			t.Fatalf("step %d: control merged Scale = %d, want %d", i, control[i].Scale, emptyLadderRealScale)
		}
		if got[i].Scale != control[i].Scale || got[i].Width != 4 || got[i].Count != 13 {
			t.Fatalf("step %d: an all-zero-observation series changed the merge: got %+v, control %+v", i, got[i], control[i])
		}
	}
}
