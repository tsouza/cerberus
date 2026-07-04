package routerrules

import (
	"context"
	"testing"
)

// scriptedSource is a fake CorpusSource that answers the exact aggregates the
// Autotuner asks for, driven by a small scenario. EvalRule is unused by the fit.
type scriptedSource struct {
	oomMinFanout  float64
	oomMinAnchors float64
	hasOOM        bool
}

func (s scriptedSource) Aggregate(_ context.Context, spec AggSpec) (Value, error) {
	oomScope := spec.Scope["route"] == "A" && spec.Scope["exit_status"] == "oom"
	if oomScope && spec.Agg == AggMin && spec.Column == "fanout" {
		if !s.hasOOM {
			return Value{NoSignal: true}, nil
		}
		return Value{Scalar: s.oomMinFanout}, nil
	}
	if oomScope && spec.Agg == AggMin && spec.Column == "n_anchors" {
		if !s.hasOOM {
			return Value{NoSignal: true}, nil
		}
		return Value{Scalar: s.oomMinAnchors}, nil
	}
	return Value{NoSignal: true}, nil
}

func (s scriptedSource) EvalRule(_ context.Context, _ RuleQuery) ([]GroupResult, error) {
	return nil, nil
}

func TestAutotuneFit(t *testing.T) {
	cur := Thresholds{MinFanout: 16, MinAnchorPairs: 4000}

	cases := []struct {
		name        string
		src         scriptedSource
		wantChanged bool
		wantReason  string
		wantCand    Thresholds
	}{
		{
			name:        "no OOM signal holds thresholds (cold start)",
			src:         scriptedSource{hasOOM: false},
			wantChanged: false,
			wantReason:  ReasonAutotuneNoSignal,
			wantCand:    cur,
		},
		{
			name:        "A OOMs below default fan-out -> lower both gates",
			src:         scriptedSource{hasOOM: true, oomMinFanout: 9, oomMinAnchors: 241},
			wantChanged: true,
			wantReason:  ReasonAutotuneApplied,
			// candFanout=clamp(9,1,16)=9; candPairs=clamp(241*9=2169,1,4000)=2169.
			wantCand: Thresholds{MinFanout: 9, MinAnchorPairs: 2169},
		},
		{
			name:        "OOM line above current gate -> never raise, no change",
			src:         scriptedSource{hasOOM: true, oomMinFanout: 40, oomMinAnchors: 300},
			wantChanged: false,
			wantReason:  ReasonAutotuneNoChange,
			// candFanout=clamp(40,1,16)=16; candPairs=clamp(12000,1,4000)=4000.
			wantCand: cur,
		},
		{
			name:        "sub-hysteresis drop is damped",
			src:         scriptedSource{hasOOM: true, oomMinFanout: 15, oomMinAnchors: 250},
			wantChanged: false,
			wantReason:  ReasonAutotuneNoChange,
			// fanout drop 16-15=1 < 2; pairs 4000-3750=250 < 500 -> no change,
			// but the candidate still reflects the clamp.
			wantCand: Thresholds{MinFanout: 15, MinAnchorPairs: 3750},
		},
		{
			name:        "pairs gate crosses hysteresis alone",
			src:         scriptedSource{hasOOM: true, oomMinFanout: 15, oomMinAnchors: 100},
			wantChanged: true,
			wantReason:  ReasonAutotuneApplied,
			// fanout drop 1 (<2) but pairs 4000-1500=2500 (>=500) -> changed.
			wantCand: Thresholds{MinFanout: 15, MinAnchorPairs: 1500},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAutotuner(tc.src, DefaultAutotuneOptions())
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

			// Structural certification: the candidate never RAISES a gate, and
			// when there is an OOM signal every observed OOM shape provably clears
			// the candidate (fan-out gate <= observed OOM fan-out; pairs gate <=
			// the min(N)*min(F) lower bound on OOM shape products).
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
