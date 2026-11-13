package tempo

import (
	"context"
	"strconv"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/engine"
)

// rootLookupSpillCap is the per-statement memory cap the settings-capturing
// client reports; every spill threshold is half of it.
const rootLookupSpillCap int64 = 3 << 30

// settingsCapturingQuerier records the per-query settings of the last
// statement it receives and answers it with no rows.
type settingsCapturingQuerier struct {
	settings map[string]any
}

func (q *settingsCapturingQuerier) EffectiveMaxQueryMemoryBytes() int64 { return rootLookupSpillCap }

func (q *settingsCapturingQuerier) Query(ctx context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	q.settings = chclient.QuerySettingsFromContext(ctx)
	return nil, nil
}

func (q *settingsCapturingQuerier) QueryStrings(ctx context.Context, _ string, _ ...any) ([]string, error) {
	q.settings = chclient.QuerySettingsFromContext(ctx)
	return nil, nil
}

// TestRootLookup_CarriesEngineSpillThresholds drives the search root lookup,
// which emits and dispatches through the handler's own client after the
// engine's search statement, and requires it to reach the client with all
// four of the engine's spill thresholds.
func TestRootLookup_CarriesEngineSpillThresholds(t *testing.T) {
	q := &settingsCapturingQuerier{}
	h := New(q, tracesSchema(), "v-test", nil)
	h.Engine.SetSettings(engine.SettingsRules{JoinSpill: true, DistinctSpill: true})

	if _, err := h.resolveTraceRoots(context.Background(), []string{"0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("resolveTraceRoots: %v", err)
	}
	if q.settings == nil {
		t.Fatal("the root lookup dispatched no statement")
	}
	half := strconv.FormatInt(rootLookupSpillCap/2, 10)
	for _, name := range []string{
		"max_bytes_before_external_group_by",
		"max_bytes_before_external_sort",
		chopt.SettingMaxBytesBeforeExternalJoin,
		chopt.SettingMaxBytesBeforeExternalDistinct,
	} {
		if v, ok := q.settings[name].(int64); !ok || strconv.FormatInt(v, 10) != half {
			t.Errorf("root lookup: %s = %v; want %s", name, q.settings[name], half)
		}
	}
}
