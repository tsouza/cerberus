package loki_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
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

func (r *spillSettingsRecorder) QueryStrings(ctx context.Context, _ string, _ ...any) ([]string, error) {
	r.record(ctx)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryDetectedFieldRows(ctx context.Context, _ string, _ ...any) ([]chclient.DetectedFieldRow, error) {
	r.record(ctx)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryTimestampedLines(ctx context.Context, _ string, _ ...any) ([]chclient.TimestampedLine, error) {
	r.record(ctx)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryIndexStats(ctx context.Context, _ string, _ ...any) (chclient.IndexStatsRow, error) {
	r.record(ctx)
	return chclient.IndexStatsRow{}, nil
}

func (r *spillSettingsRecorder) QueryIndexVolume(ctx context.Context, _ string, _ ...any) ([]chclient.IndexVolumeRow, error) {
	r.record(ctx)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryLabelSets(ctx context.Context, _ string, _ ...any) ([]map[string]string, error) {
	r.record(ctx)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryLabelCardinalities(ctx context.Context, _ string, _ ...any) ([]chclient.LabelCardinalityRow, error) {
	r.record(ctx)
	return nil, nil
}

// TestHandlerDispatches_CarryEngineSpillThresholds drives every Loki endpoint
// that dispatches through the handler's own client rather than the engine,
// and requires each statement to reach the client with the engine's spill
// thresholds: without them a high-cardinality metadata DISTINCT aborts at the
// memory cap where an engine query of the same size spills.
func TestHandlerDispatches_CarryEngineSpillThresholds(t *testing.T) {
	rec := &spillSettingsRecorder{}
	h := loki.New(rec, schema.DefaultOTelLogs(), nil)
	h.Engine.SetSettings(engine.SettingsRules{JoinSpill: true, DistinctSpill: true})
	mux := http.NewServeMux()
	h.Mount(mux)

	half := strconv.FormatInt(spillCapBytes/2, 10)
	want := map[string]string{
		"max_bytes_before_external_group_by":        half,
		"max_bytes_before_external_sort":            half,
		chopt.SettingMaxBytesBeforeExternalJoin:     half,
		chopt.SettingMaxBytesBeforeExternalDistinct: half,
	}
	selector := url.QueryEscape(`{service_name="api"}`)
	for _, path := range []string{
		"/loki/api/v1/labels",
		"/loki/api/v1/label/service_name/values",
		"/loki/api/v1/series?match[]=" + selector,
		"/loki/api/v1/index/stats?query=" + selector,
		"/loki/api/v1/index/volume?query=" + selector,
		"/loki/api/v1/detected_fields?query=" + selector,
		"/loki/api/v1/detected_field/level/values?query=" + selector,
		"/loki/api/v1/detected_labels?query=" + selector,
		"/loki/api/v1/patterns?query=" + selector,
	} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			calls := rec.taken()
			if len(calls) == 0 {
				t.Fatalf("GET %s dispatched no statement (HTTP %d: %s)", path, w.Code, w.Body.String())
			}
			for i, got := range calls {
				for name, value := range want {
					if v, ok := got[name].(int64); !ok || strconv.FormatInt(v, 10) != value {
						t.Errorf("statement %d of GET %s: %s = %v; want %s", i, path, name, got[name], value)
					}
				}
			}
		})
	}
}
