package prom_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

// recordedStatement is one statement a spillSettingsRecorder saw: its SQL,
// where the dispatch path passes it through, and its per-query settings.
type recordedStatement struct {
	sql      string
	settings map[string]any
}

// spillSettingsRecorder is a Querier that records the per-query settings
// every statement reaches the client with, and answers each with no rows.
type spillSettingsRecorder struct {
	mu    sync.Mutex
	calls []recordedStatement
}

func (r *spillSettingsRecorder) record(ctx context.Context) { r.recordSQL(ctx, "") }

func (r *spillSettingsRecorder) recordSQL(ctx context.Context, sql string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedStatement{sql: sql, settings: chclient.QuerySettingsFromContext(ctx)})
}

func (r *spillSettingsRecorder) taken() []recordedStatement {
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

func (r *spillSettingsRecorder) QueryStrings(ctx context.Context, sql string, _ ...any) ([]string, error) {
	r.recordSQL(ctx, sql)
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
// the settings map renders. The DISTINCT one is withheld from a statement
// whose DISTINCT runs over attribute-map keys (mapKeys).
func wantSpillThresholds(mapKeys bool) map[string]string {
	half := strconv.FormatInt(spillCapBytes/2, 10)
	want := map[string]string{
		"max_bytes_before_external_group_by":    half,
		"max_bytes_before_external_sort":        half,
		chopt.SettingMaxBytesBeforeExternalJoin: half,
	}
	if !mapKeys {
		want[chopt.SettingMaxBytesBeforeExternalDistinct] = half
	}
	return want
}

// TestHandlerDispatches_CarryEngineSpillThresholds drives every Prom endpoint
// that dispatches through the handler's own client rather than the engine,
// and requires each statement to reach the client with the engine's spill
// thresholds: without them a high-cardinality metadata DISTINCT aborts at the
// memory cap where an engine query of the same size spills. A label-name
// statement, whose DISTINCT runs over attribute-map keys, carries every
// threshold but the DISTINCT one, and every other statement carries all four;
// a label-name endpoint must dispatch at least one such statement, so the
// withholding is exercised rather than vacuously absent. It also replaces the
// Engine after New, as cmd/cerberus does, and requires the replacement's
// rules to size the thresholds.
func TestHandlerDispatches_CarryEngineSpillThresholds(t *testing.T) {
	rec := &spillSettingsRecorder{}
	h := prom.New(rec, schema.DefaultOTelMetrics(), nil)
	h.Engine = &engine.Engine{Optimizer: h.Optimizer, Client: rec}
	h.Engine.SetSettings(engine.SettingsRules{JoinSpill: true, DistinctSpill: true})
	mux := http.NewServeMux()
	h.Mount(mux)

	for _, tc := range []struct {
		path    string
		mapKeys bool
	}{
		{"/api/v1/labels", true},
		{"/api/v1/labels?match[]=up", true},
		{"/api/v1/label/job/values", false},
		{"/api/v1/label/job/values?match[]=up", false},
		{"/api/v1/series?match[]=up", false},
		{"/api/v1/metadata", false},
		{"/api/v1/query_exemplars?query=up&start=0&end=60", false},
	} {
		path := tc.path
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			calls := rec.taken()
			if len(calls) == 0 {
				t.Fatalf("GET %s dispatched no statement (HTTP %d: %s)", path, w.Code, w.Body.String())
			}
			sawMapKeys := false
			for i, call := range calls {
				mapKeys := isMapKeyDistinct(call.sql)
				sawMapKeys = sawMapKeys || mapKeys
				got := call.settings
				want := wantSpillThresholds(mapKeys)
				for name, value := range want {
					if v, ok := got[name].(int64); !ok || strconv.FormatInt(v, 10) != value {
						t.Errorf("statement %d of GET %s: %s = %v; want %s", i, path, name, got[name], value)
					}
				}
				if _, ok := want[chopt.SettingMaxBytesBeforeExternalDistinct]; !ok {
					if v, stamped := got[chopt.SettingMaxBytesBeforeExternalDistinct]; stamped {
						t.Errorf("statement %d of GET %s: %s = %v on a DISTINCT over map keys; want it withheld",
							i, path, chopt.SettingMaxBytesBeforeExternalDistinct, v)
					}
				}
			}
			if sawMapKeys != tc.mapKeys {
				t.Errorf("GET %s dispatched a DISTINCT over map keys = %t; want %t", path, sawMapKeys, tc.mapKeys)
			}
		})
	}
}

// isMapKeyDistinct reports whether sql takes its DISTINCT over attribute-map
// keys, the statement shape the handler marks to withhold the DISTINCT spill
// threshold.
func isMapKeyDistinct(sql string) bool {
	return strings.Contains(sql, "DISTINCT") &&
		(strings.Contains(sql, "mapKeys(") || strings.Contains(sql, ".`keys`"))
}
