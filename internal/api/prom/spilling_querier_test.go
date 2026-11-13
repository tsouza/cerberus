package prom_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
)

// spillCapBytes is the per-statement memory cap the recording client
// reports; every spill threshold is half of it.
const spillCapBytes int64 = 3 << 30

// spillSettingsRecorder is a Querier that records the per-query settings
// every statement reaches the client with, and answers each with no rows.
type spillSettingsRecorder struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (r *spillSettingsRecorder) record(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, chclient.QuerySettingsFromContext(ctx))
}

func (r *spillSettingsRecorder) taken() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	calls := r.calls
	r.calls = nil
	return calls
}

func (r *spillSettingsRecorder) EffectiveMaxQueryMemoryBytes() int64 { return spillCapBytes }

func (r *spillSettingsRecorder) Query(ctx context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	r.record(ctx)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryCursor(ctx context.Context, _ string, _ ...any) (chclient.Cursor, error) {
	r.record(ctx)
	return newSliceCursor(nil), nil
}

func (r *spillSettingsRecorder) QueryStrings(ctx context.Context, _ string, _ ...any) ([]string, error) {
	r.record(ctx)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryLabelSets(ctx context.Context, _ string, _ ...any) ([]map[string]string, error) {
	r.record(ctx)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryMetricMeta(ctx context.Context, _, _ string, _ ...any) ([]chclient.MetricMetaRow, error) {
	r.record(ctx)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryExemplars(ctx context.Context, _ string, _ ...any) ([]chclient.ExemplarRow, error) {
	r.record(ctx)
	return nil, nil
}

// wantSpillThresholds is every spill threshold the engine stamps with both
// version-gated features resolved in, at half spillCapBytes, as the string
// the settings map renders.
func wantSpillThresholds() map[string]string {
	half := strconv.FormatInt(spillCapBytes/2, 10)
	return map[string]string{
		"max_bytes_before_external_group_by":        half,
		"max_bytes_before_external_sort":            half,
		chopt.SettingMaxBytesBeforeExternalJoin:     half,
		chopt.SettingMaxBytesBeforeExternalDistinct: half,
	}
}

// TestHandlerDispatches_CarryEngineSpillThresholds drives every Prom endpoint
// that dispatches through the handler's own client rather than the engine,
// and requires each statement to reach the client with the engine's spill
// thresholds: without them a high-cardinality metadata DISTINCT aborts at the
// memory cap where an engine query of the same size spills. It also replaces
// the Engine after New, as cmd/cerberus does, and requires the replacement's
// rules to size the thresholds.
func TestHandlerDispatches_CarryEngineSpillThresholds(t *testing.T) {
	rec := &spillSettingsRecorder{}
	h := prom.New(rec, schema.DefaultOTelMetrics(), nil)
	h.Engine = &engine.Engine{Optimizer: h.Optimizer, Client: rec}
	h.Engine.SetSettings(engine.SettingsRules{JoinSpill: true, DistinctSpill: true})
	mux := http.NewServeMux()
	h.Mount(mux)

	for _, path := range []string{
		"/api/v1/labels",
		"/api/v1/labels?match[]=up",
		"/api/v1/label/job/values",
		"/api/v1/label/job/values?match[]=up",
		"/api/v1/series?match[]=up",
		"/api/v1/metadata",
		"/api/v1/query_exemplars?query=up&start=0&end=60",
	} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			calls := rec.taken()
			if len(calls) == 0 {
				t.Fatalf("GET %s dispatched no statement (HTTP %d: %s)", path, w.Code, w.Body.String())
			}
			for i, got := range calls {
				for name, want := range wantSpillThresholds() {
					if v, ok := got[name].(int64); !ok || strconv.FormatInt(v, 10) != want {
						t.Errorf("statement %d of GET %s: %s = %v; want %s", i, path, name, got[name], want)
					}
				}
			}
		})
	}
}
