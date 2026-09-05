package promql

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// acceptedStalenessInput is a stalenessLowerInput [NativeStalenessLowerer]
// accepts — mirrors acceptedGridWindow/acceptedLastOverTimeWindow in
// gremlins_kill_native_grid_guard_test.go, but for the bare-selector
// staleness shape (the exact `cerb:project;rwr;union` shape reported in
// issue #3068, which reaches RangeWindowStaleResample via LowerStaleness
// rather than via last_over_time).
func acceptedStalenessInput() stalenessLowerInput {
	s := schema.DefaultOTelMetrics()
	return stalenessLowerInput{
		input:         nativeGuardInput(s),
		start:         nativeGuardStart(),
		end:           nativeGuardEnd(),
		step:          nativeGuardStep,
		lookback:      5 * time.Minute,
		offset:        0,
		metricNameCol: s.MetricNameColumn,
		attributesCol: s.AttributesColumn,
		timestampCol:  s.TimestampColumn,
		valueCol:      s.ValueColumn,
	}
}

// TestNativeStalenessLowerer_WholeSecondsConjunction pins the sub-second
// carve-out NativeStalenessLowerer.LowerStaleness applies on top of its
// pre-existing sampleTimestamp carve-out: a sub-second step, lookback, or
// offset must each independently route to the Fallback (fan-out RangeLWR),
// never to the native RangeWindowStaleResample node.
//
// Before this carve-out existed, a sub-second step reached
// chsql.emitRangeWindowStaleResample's `int64(r.Step.Seconds())` truncation,
// which collapses e.g. 500ms to stepSeconds == 0 — silently producing the
// exact ClickHouse rejection issue #3068 reported ("Step should be greater
// than zero", code 36; verified directly against chDB: passing a Float64
// step_s to timeSeriesRange/timeSeriesResampleToGridWithStaleness is
// ALSO rejected outright with ILLEGAL_TYPE_OF_ARGUMENT, so the family
// genuinely has no faithful sub-second representation at all — the fix must
// avoid the native path, not widen its argument type).
func TestNativeStalenessLowerer_WholeSecondsConjunction(t *testing.T) {
	t.Parallel()
	l := NativeStalenessLowerer{Fallback: FanoutStalenessLowerer{}}

	baseline := l.LowerStaleness(acceptedStalenessInput())
	if _, ok := baseline.(*chplan.RangeWindowStaleResample); !ok {
		t.Fatalf("baseline whole-second staleness input did not reach the native node (%T); the rejection cases below would be vacuous", baseline)
	}

	for _, tc := range []struct {
		name string
		mut  func(*stalenessLowerInput)
	}{
		{"step_sub_second", func(in *stalenessLowerInput) { in.step = 30*time.Second + 500*time.Millisecond }},
		{"lookback_sub_second", func(in *stalenessLowerInput) { in.lookback = 90*time.Second + 500*time.Millisecond }},
		{"offset_sub_second", func(in *stalenessLowerInput) { in.offset = 500 * time.Millisecond }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := acceptedStalenessInput()
			tc.mut(&in)
			got := l.LowerStaleness(in)
			if _, ok := got.(*chplan.RangeWindowStaleResample); ok {
				t.Errorf("LowerStaleness(%s) returned the native node; want the fan-out RangeLWR so a sub-second value is answered instead of truncated to an invalid step_s=0", tc.name)
			}
			if _, ok := got.(*chplan.RangeLWR); !ok {
				t.Errorf("LowerStaleness(%s) = %T, want *chplan.RangeLWR (the Fallback)", tc.name, got)
			}
		})
	}
}

// TestNativeStalenessLowerer_WholeSecondStepRendersNoInvalidZero is the
// end-to-end regression guard: it exercises the SAME emit path issue #3068
// pinned (emitRangeWindowStaleResample via a RangeWindowStaleResample node),
// proving a sub-second step never reaches it (case a) while a normal
// >=1s step still does, unaffected (case b).
func TestNativeStalenessLowerer_WholeSecondStepRendersNoInvalidZero(t *testing.T) {
	t.Parallel()
	l := NativeStalenessLowerer{Fallback: FanoutStalenessLowerer{}}

	subSecond := acceptedStalenessInput()
	subSecond.step = 500 * time.Millisecond
	if got := l.LowerStaleness(subSecond); isRangeWindowStaleResample(got) {
		t.Fatalf("a sub-second Step reached RangeWindowStaleResample: %#v — it would render stepSeconds=int64(0.5)=0, the exact issue #3068 ClickHouse rejection", got)
	}

	wholeSecond := acceptedStalenessInput()
	wholeSecond.step = 30 * time.Second
	if got := l.LowerStaleness(wholeSecond); !isRangeWindowStaleResample(got) {
		t.Fatalf("a normal >=1s Step no longer reaches RangeWindowStaleResample: %T; the whole-seconds carve-out must not regress the native fast path", got)
	}
}

func isRangeWindowStaleResample(n chplan.Node) bool {
	_, ok := n.(*chplan.RangeWindowStaleResample)
	return ok
}
