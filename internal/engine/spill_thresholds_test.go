package engine

import (
	"context"
	"maps"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

// TestWithSpillThresholds_StampsTheSeamThresholds drives
// Engine.WithSpillThresholds, the entry the engine-bypassing handler
// dispatches call, under every combination of the two feature gates, and
// requires exactly the spill thresholds the dispatch seam stamps on a
// join-bearing plan under the same rules and cap: the group_by/sort pair
// always, the join and DISTINCT thresholds only once their feature resolved
// in, each at half the cap the client runs the statement under.
func TestWithSpillThresholds_StampsTheSeamThresholds(t *testing.T) {
	t.Parallel()

	const capBytes int64 = 6 * gib
	wantThreshold := capBytes / spillCapDenominator
	joinPlan := &chplan.VectorJoin{}

	for _, tc := range []struct {
		name                   string
		joinSpill, distinct    bool
		wantJoin, wantDistinct bool
	}{
		{name: "both gates off", wantJoin: false, wantDistinct: false},
		{name: "join_spill only", joinSpill: true, wantJoin: true},
		{name: "distinct_spill only", distinct: true, wantDistinct: true},
		{name: "both gates on", joinSpill: true, distinct: true, wantJoin: true, wantDistinct: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rules := compositionBaseRules()
			rules.JoinSpill = tc.joinSpill
			rules.DistinctSpill = tc.distinct
			e := &Engine{Client: &effectiveCapQuerier{effective: capBytes}}
			e.SetSettings(rules)

			got := chclient.QuerySettingsFromContext(e.WithSpillThresholds(context.Background()))

			want := map[string]any{
				settingMaxBytesBeforeExternalGroupBy: wantThreshold,
				settingMaxBytesBeforeExternalSort:    wantThreshold,
			}
			if tc.wantJoin {
				want[settingMaxBytesBeforeExternalJoin] = wantThreshold
			}
			if tc.wantDistinct {
				want[settingMaxBytesBeforeExternalDistinct] = wantThreshold
			}
			if !maps.Equal(got, want) {
				t.Fatalf("WithSpillThresholds stamped %v; want %v", got, want)
			}

			seam := chclient.QuerySettingsFromContext(
				applySharedQuerySettings(context.Background(), shapeOf(joinPlan), capBytes, rules),
			)
			for name, value := range got {
				if seam[name] != value {
					t.Errorf("%s = %v on a bypassing statement but %v at the dispatch seam for the same cap and rules",
						name, value, seam[name])
				}
			}
		})
	}
}

// TestWithSpillThresholds_SizedFromTheStatementCap pins the cap the
// thresholds are sized from: the client's effective per-statement cap, the
// max_memory_usage the bypassing statement actually runs under, and the
// absolute fallback when no cap is configured, never 0, which would disable
// the spill.
func TestWithSpillThresholds_SizedFromTheStatementCap(t *testing.T) {
	t.Parallel()

	for _, effective := range []int64{3 * gib, 0} {
		e := &Engine{Client: &effectiveCapQuerier{effective: effective, configured: 4 * effective}}
		got := settingValue(e.WithSpillThresholds(context.Background()), settingMaxBytesBeforeExternalGroupBy)
		if want := spillThreshold(effective); got != want {
			t.Errorf("effective cap %d: max_bytes_before_external_group_by = %v; want %v", effective, got, want)
		}
	}
}

// effectiveCapQuerier is a Querier exposing both memory-cap accessors, so a
// test can tell which one the thresholds are sized from.
type effectiveCapQuerier struct {
	effective, configured int64
}

func (*effectiveCapQuerier) Query(context.Context, string, ...any) ([]chclient.Sample, error) {
	return nil, nil
}
func (q *effectiveCapQuerier) MaxQueryMemoryBytes() int64          { return q.configured }
func (q *effectiveCapQuerier) EffectiveMaxQueryMemoryBytes() int64 { return q.effective }
