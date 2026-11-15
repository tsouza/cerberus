package prom_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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

// recordedStatement is one statement a spillSettingsRecorder saw: its SQL
// and its per-query settings.
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

func (r *spillSettingsRecorder) record(ctx context.Context, sql string) {
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

func (r *spillSettingsRecorder) Query(ctx context.Context, sql string, _ ...any) ([]chclient.Sample, error) {
	r.record(ctx, sql)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryCursor(ctx context.Context, sql string, _ ...any) (chclient.Cursor, error) {
	r.record(ctx, sql)
	return newSliceCursor(nil), nil
}

func (r *spillSettingsRecorder) QueryStrings(ctx context.Context, sql string, _ ...any) ([]string, error) {
	r.record(ctx, sql)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryLabelSets(ctx context.Context, sql string, _ ...any) ([]map[string]string, error) {
	r.record(ctx, sql)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryMetricMeta(ctx context.Context, sql, _ string, _ ...any) ([]chclient.MetricMetaRow, error) {
	r.record(ctx, sql)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryExemplars(ctx context.Context, sql string, _ ...any) ([]chclient.ExemplarRow, error) {
	r.record(ctx, sql)
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

// TestHandlerDispatches_CarryEngineSpillThresholds drives every Prom
// metadata route, and the data-plane routes beside them, and requires each
// statement to reach the client with the engine's spill thresholds: without
// them a high-cardinality metadata DISTINCT, GROUP BY or sort aborts at the
// memory cap where an engine query of the same size spills. No Prom
// statement is one whose DISTINCT a spill harms, so every one carries the
// DISTINCT threshold: the metadata and exemplars statements through the
// handler's own client carry all four, and an instant and a range query
// through the engine carry all but the join one, which the engine stamps only
// on a plan that joins. A route marked distinct must dispatch a DISTINCT —
// the label-name and __name__-value lookups, and the set operation — so its
// keeping the threshold is checked rather than vacuously absent. It also
// replaces the Engine after New, as cmd/cerberus does, and requires the
// replacement's rules to size the thresholds.
func TestHandlerDispatches_CarryEngineSpillThresholds(t *testing.T) {
	rec := &spillSettingsRecorder{}
	h := prom.New(rec, schema.DefaultOTelMetrics(), nil)
	h.Engine = &engine.Engine{Optimizer: h.Optimizer, Client: rec}
	h.Engine.SetSettings(engine.SettingsRules{JoinSpill: true, DistinctSpill: true})
	mux := http.NewServeMux()
	h.Mount(mux)

	setOp := url.QueryEscape(`up unless on(__name__) down`)
	for _, tc := range []struct {
		method string
		path   string
		// viaEngine: the statements dispatch through the engine, which
		// stamps the join threshold only on a plan that joins.
		viaEngine bool
		// distinct: the route dispatches a DISTINCT the test must see.
		distinct bool
	}{
		{method: http.MethodGet, path: "/api/v1/labels", distinct: true},
		{method: http.MethodGet, path: "/api/v1/labels?match[]=up", distinct: true},
		{method: http.MethodPost, path: "/api/v1/labels?match[]=up", distinct: true},
		{method: http.MethodGet, path: "/api/v1/label/job/values"},
		{method: http.MethodGet, path: "/api/v1/label/job/values?match[]=up"},
		{method: http.MethodGet, path: "/api/v1/label/__name__/values", distinct: true},
		{method: http.MethodGet, path: "/api/v1/series?match[]=up"},
		{method: http.MethodPost, path: "/api/v1/series?match[]=up"},
		{method: http.MethodGet, path: "/api/v1/metadata"},
		{method: http.MethodGet, path: "/api/v1/query_exemplars?query=up&start=0&end=60"},
		{method: http.MethodGet, path: "/api/v1/query?time=60&query=" + setOp, viaEngine: true, distinct: true},
		{method: http.MethodPost, path: "/api/v1/query_range?start=0&end=600&step=60&query=" + setOp, viaEngine: true, distinct: true},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			calls := rec.taken()
			if len(calls) == 0 {
				t.Fatalf("%s %s dispatched no statement (HTTP %d: %s)", tc.method, tc.path, w.Code, w.Body.String())
			}
			if tc.distinct && !slices.ContainsFunc(calls, func(c recordedStatement) bool { return strings.Contains(c.sql, "DISTINCT") }) {
				t.Fatalf("%s %s dispatched no DISTINCT, so no DISTINCT keeping its threshold is checked", tc.method, tc.path)
			}
			want := wantSpillThresholds()
			if tc.viaEngine {
				delete(want, chopt.SettingMaxBytesBeforeExternalJoin)
			}
			for i, call := range calls {
				for name, value := range want {
					if v, ok := call.settings[name].(int64); !ok || strconv.FormatInt(v, 10) != value {
						t.Errorf("statement %d of %s %s: %s = %v; want %s", i, tc.method, tc.path, name, call.settings[name], value)
					}
				}
			}
		})
	}
}
