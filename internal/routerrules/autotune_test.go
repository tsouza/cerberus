package routerrules

import (
	"context"
	"testing"
)

// fakeFloorSource is a fixed OOMFloorSource for fit tests.
type fakeFloorSource struct{ f OOMFloor }

func (s fakeFloorSource) OOMFloor(_ context.Context) (OOMFloor, error) { return s.f, nil }

// TestAutotuneFit_PassesOutcomeCounts asserts Fit forwards the rolling-window
// outcome counts on every result — including the no-signal branch, where they
// are still meaningful (route-B volume with zero remaining route-A OOMs).
func TestAutotuneFit_PassesOutcomeCounts(t *testing.T) {
	src := fakeFloorSource{f: OOMFloor{HasSignal: false, RouteBExecutions: 200, RouteBOomCount: 1}}
	res, err := NewAutotuner(src).Fit(context.Background(), Thresholds{MinFanout: 16, MinAnchorPairs: 4000})
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if res.Reason != ReasonAutotuneNoSignal {
		t.Errorf("Reason = %q, want no-signal", res.Reason)
	}
	if res.RouteBExecutions != 200 || res.RouteBOomCount != 1 {
		t.Errorf("outcome counts not passed through: %+v", res)
	}
}

func TestAutotuneFit(t *testing.T) {
	cur := Thresholds{MinFanout: 16, MinAnchorPairs: 4000}

	cases := []struct {
		name        string
		floor       OOMFloor
		wantChanged bool
		wantReason  string
		wantCand    Thresholds
	}{
		{
			name:        "no OOM signal holds thresholds (cold start)",
			floor:       OOMFloor{HasSignal: false},
			wantChanged: false,
			wantReason:  ReasonAutotuneNoSignal,
			wantCand:    cur,
		},
		{
			name:        "A OOMs below default fan-out -> lower both gates",
			floor:       OOMFloor{HasSignal: true, MinFanout: 9, MinAnchors: 241},
			wantChanged: true,
			wantReason:  ReasonAutotuneApplied,
			// candFanout=clamp(9,1,16)=9; candPairs=clamp(241*9=2169,1,4000)=2169.
			wantCand: Thresholds{MinFanout: 9, MinAnchorPairs: 2169},
		},
		{
			name:        "OOM line above current gate -> never raise, no change",
			floor:       OOMFloor{HasSignal: true, MinFanout: 40, MinAnchors: 300},
			wantChanged: false,
			wantReason:  ReasonAutotuneNoChange,
			// candFanout=clamp(40,1,16)=16; candPairs=clamp(12000,1,4000)=4000.
			wantCand: cur,
		},
		{
			name:        "one-step drop is applied (no hysteresis suppression)",
			floor:       OOMFloor{HasSignal: true, MinFanout: 15, MinAnchors: 250},
			wantChanged: true,
			wantReason:  ReasonAutotuneApplied,
			// candFanout=15 (<16 -> changed); candPairs=clamp(3750,1,4000)=3750.
			// The applied gate equals the candidate, so the OOM-floor guarantee
			// holds for the LIVE gate, not just a notional candidate.
			wantCand: Thresholds{MinFanout: 15, MinAnchorPairs: 3750},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAutotuner(fakeFloorSource{f: tc.floor})
			got, err := a.Fit(context.Background(), cur)
			if err != nil {
				t.Fatalf("Fit: %v", err)
			}
			if got.Changed != tc.wantChanged {
				t.Errorf("Changed = %v, want %v", got.Changed, tc.wantChanged)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if got.Candidate != tc.wantCand {
				t.Errorf("Candidate = %+v, want %+v", got.Candidate, tc.wantCand)
			}

			// Structural certification: never raise a gate; and with an OOM signal
			// every observed OOM shape provably clears the APPLIED candidate
			// (fan-out gate <= observed OOM fan-out; pairs gate <= min(N)*min(F)).
			if got.Candidate.MinFanout > cur.MinFanout || got.Candidate.MinAnchorPairs > cur.MinAnchorPairs {
				t.Errorf("candidate raised a gate: %+v vs current %+v", got.Candidate, cur)
			}
			if got.HasOOMSignal {
				if got.Candidate.MinFanout > got.OOMMinFanout {
					t.Errorf("OOM floor broken: MinFanout %d > observed OOM fan-out %d",
						got.Candidate.MinFanout, got.OOMMinFanout)
				}
				if got.Candidate.MinAnchorPairs > got.OOMMinAnchors*got.OOMMinFanout {
					t.Errorf("OOM floor broken: MinAnchorPairs %d > min(N)*min(F) %d",
						got.Candidate.MinAnchorPairs, got.OOMMinAnchors*got.OOMMinFanout)
				}
			}
		})
	}
}
