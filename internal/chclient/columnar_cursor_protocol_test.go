package chclient

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// TestColumnarCursor_IterationProtocol pins the Cursor contract the columnar
// decoder must satisfy identically to the row path (cerberus issue #2991).
// The columnar cursor is an optimisation that replaces rowsCursor for matrix
// queries, so any divergence in the Next/Sample/Inspected protocol is a
// silent, shape-dependent wrong answer rather than an error — and only on
// the optimised path, where the row path stays correct and hides it.
//
// Four properties are asserted together because they constrain one another:
// Next reports whether a row is available, Sample returns the row the LAST
// successful Next landed on, Inspected counts exactly the Next calls that
// returned true, and a drained cursor stays drained.
func TestColumnarCursor_IterationProtocol(t *testing.T) {
	samples := []Sample{
		{MetricName: "a", Timestamp: time.Unix(1, 0).UTC(), Value: 1},
		{MetricName: "b", Timestamp: time.Unix(2, 0).UTC(), Value: 2},
		{MetricName: "c", Timestamp: time.Unix(3, 0).UTC(), Value: 3},
	}
	d := &columnarCursor{samples: samples}

	// Before the first Next, Sample must not hand out a row: a consumer that
	// reads before advancing gets the zero value, never samples[0].
	if got := d.Sample(); !reflect.DeepEqual(got, Sample{}) {
		t.Errorf("Sample() before Next = %+v; want the zero Sample", got)
	}
	if got := d.Inspected(); got != 0 {
		t.Errorf("Inspected() before Next = %d; want 0", got)
	}

	for i, want := range samples {
		if !d.Next() {
			t.Fatalf("Next() at row %d = false; want true (%d rows buffered)", i, len(samples))
		}
		if got := d.Sample(); !reflect.DeepEqual(got, want) {
			t.Errorf("Sample() at row %d = %+v; want %+v", i, got, want)
		}
		if got := d.Inspected(); got != int64(i+1) {
			t.Errorf("Inspected() after %d rows = %d; want %d", i+1, got, i+1)
		}
	}

	if d.Next() {
		t.Errorf("Next() past the last row = true; want false")
	}
	// A drained cursor stays drained, and the final Sample is still the last
	// row rather than the zero value — a consumer that calls Next once too
	// often must not see the stream restart or the last row vanish.
	if d.Next() {
		t.Errorf("second Next() past the last row = true; want false")
	}
	if got := d.Sample(); !reflect.DeepEqual(got, samples[len(samples)-1]) {
		t.Errorf("Sample() after the stream drained = %+v; want the last row %+v", got, samples[len(samples)-1])
	}
	if got := d.Inspected(); got != int64(len(samples)) {
		t.Errorf("Inspected() after draining = %d; want %d — a Next that returned false must not be counted", got, len(samples))
	}
	if err := d.Err(); err != nil {
		t.Errorf("Err() after a clean drain = %v; want nil", err)
	}
}

// TestColumnarCursor_LatchedErrorStopsIteration pins the failure half of the
// same protocol: once the decoder latches an error, Next reports false
// immediately and Err surfaces that error. A cursor that kept handing out
// buffered rows after an over-budget latch would serve a TRUNCATED result as
// though it were complete, which is the exact shape the sample budget exists
// to turn into a 422.
func TestColumnarCursor_LatchedErrorStopsIteration(t *testing.T) {
	sentinel := errors.New("columnar: budget exceeded")
	d := &columnarCursor{
		samples: []Sample{{MetricName: "a"}, {MetricName: "b"}},
		err:     sentinel,
	}
	if d.Next() {
		t.Errorf("Next() with a latched error = true; want false even though rows are buffered")
	}
	if got := d.Inspected(); got != 0 {
		t.Errorf("Inspected() = %d; want 0 — a refused Next is not a consumed row", got)
	}
	if !errors.Is(d.Err(), sentinel) {
		t.Errorf("Err() = %v; want the latched %v", d.Err(), sentinel)
	}
}

// TestColumnarCursor_BudgetLimitPrecedence pins the precedence budgetLimit
// documents: a per-request shared *SampleBudget wins over the per-cursor
// maxSamples, matching rowsCursor. The number this returns is the limit the
// over-budget error REPORTS to the client, so getting the precedence
// backwards tells an operator to raise a knob that is not the binding one.
func TestColumnarCursor_BudgetLimitPrecedence(t *testing.T) {
	// The two limits are deliberately different, so "returned the shared
	// budget's limit" is distinguishable from "returned the per-cursor max".
	const perCursorMax = 111
	const sharedLimit = 222

	onlyCursor := &columnarCursor{maxSamples: perCursorMax}
	if got := onlyCursor.budgetLimit(); got != perCursorMax {
		t.Errorf("budgetLimit() with no shared budget = %d; want the per-cursor max %d", got, perCursorMax)
	}

	withShared := &columnarCursor{maxSamples: perCursorMax, budget: NewSampleBudget(sharedLimit)}
	if got := withShared.budgetLimit(); got != sharedLimit {
		t.Errorf("budgetLimit() with a shared budget = %d; want the shared limit %d (it takes precedence over the per-cursor max %d)",
			got, sharedLimit, perCursorMax)
	}
}

// TestColumnarCursor_ChargePrecedence pins the same precedence on the
// consuming side, and the boundary itself. charge() must exhaust at exactly
// the limit — an off-by-one here either rejects a result that fits or admits
// one row past the cap on every request that reaches the columnar path.
func TestColumnarCursor_ChargePrecedence(t *testing.T) {
	t.Run("per-cursor max", func(t *testing.T) {
		d := &columnarCursor{maxSamples: 2}
		if !d.charge() || !d.charge() {
			t.Fatalf("charge() refused within the limit of 2")
		}
		if d.charge() {
			t.Errorf("charge() past a maxSamples of 2 = true; want false")
		}
	})
	t.Run("shared budget wins", func(t *testing.T) {
		// A generous per-cursor max with a tight shared budget: only if the
		// shared budget takes precedence does the third charge fail.
		d := &columnarCursor{maxSamples: 1000, budget: NewSampleBudget(2)}
		if !d.charge() || !d.charge() {
			t.Fatalf("charge() refused within the shared budget of 2")
		}
		if d.charge() {
			t.Errorf("charge() past a shared budget of 2 = true; want false despite maxSamples=1000")
		}
	})
	t.Run("unbounded when neither is set", func(t *testing.T) {
		d := &columnarCursor{}
		for i := range 5 {
			if !d.charge() {
				t.Fatalf("charge() #%d refused with no budget and no maxSamples; want unbounded", i+1)
			}
		}
	})
}

// TestColumnarCursor_CloseIsIdempotent pins the once-only span end Close
// documents. Close is called by both the handler's defer and the drain's
// error path, so a second End() on an already-ended span is a real
// possibility; the OpenTelemetry SDK treats it as a no-op but the closeErr
// must stay stable across calls either way.
func TestColumnarCursor_CloseIsIdempotent(t *testing.T) {
	d := &columnarCursor{}
	first := d.Close()
	second := d.Close()
	if first != nil {
		t.Errorf("first Close() = %v; want nil", first)
	}
	if second != first {
		t.Errorf("second Close() = %v; want the same result as the first (%v)", second, first)
	}
}

// TestBreakerScopeString pins the scope vocabulary. breakerScope.String's own
// doc calls the vocabulary stable because dashboards and runbooks key on
// these exact strings: they reach operators as the trip metric's cause
// attribute. Renaming one silently blanks a panel rather than failing
// anything, so the strings are asserted literally, and the default arm is
// pinned too — an unmapped scope must read as "unknown" rather than as a
// number or an empty label.
func TestBreakerScopeString(t *testing.T) {
	cases := []struct {
		scope breakerScope
		want  string
	}{
		{breakerScopeServerHealth, "server-health"},
		{breakerScopeStatement, "statement"},
		{breakerScopeClient, "client"},
		{breakerScope(99), "unknown"},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		if got := tc.scope.String(); got != tc.want {
			t.Errorf("breakerScope(%d).String() = %q; want %q", int(tc.scope), got, tc.want)
		}
		// Two scopes sharing a label would make the trip metric's cause
		// attribute unable to tell a statement rejection from a server-health
		// failure — the exact distinction breaker_classify.go exists to draw.
		if seen[tc.want] {
			t.Errorf("scope label %q is used by more than one breakerScope", tc.want)
		}
		seen[tc.want] = true
	}
}
