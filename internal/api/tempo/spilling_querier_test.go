package tempo_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
)

// spillCapBytes is the per-statement memory cap the recording client
// reports; every spill threshold is half of it.
const spillCapBytes int64 = 3 << 30

// recordedStatement is one statement as it reached the client.
type recordedStatement struct {
	method   string
	sql      string
	settings map[string]any
}

// spillSettingsRecorder is a Querier that records the SQL and per-query
// settings of every statement it receives, and answers each with no rows.
type spillSettingsRecorder struct {
	mu    sync.Mutex
	calls []recordedStatement
}

func (r *spillSettingsRecorder) record(ctx context.Context, method, sql string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedStatement{method: method, sql: sql, settings: chclient.QuerySettingsFromContext(ctx)})
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
	r.record(ctx, "Query", sql)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryStrings(ctx context.Context, sql string, _ ...any) ([]string, error) {
	r.record(ctx, "QueryStrings", sql)
	return nil, nil
}

// TestHandlerDispatches_CarryEngineSpillThresholds drives the Tempo endpoints
// whose statements dispatch through the handler's own client rather than the
// engine: tag and tag-value discovery, every statement of which bypasses the
// engine; a metrics range query, whose exemplars statement runs after the
// engine's matrix statement over the same closure; and a structural search,
// whose phase-A ranking statement runs before the engine's phase B. Each
// bypassing statement must reach the client with the engine's spill
// thresholds; without them it aborts at the memory cap where the engine's
// statement over the same data spills. A tag-name statement, whose DISTINCT
// runs over attribute-map keys, carries every threshold but the DISTINCT one.
func TestHandlerDispatches_CarryEngineSpillThresholds(t *testing.T) {
	rec := &spillSettingsRecorder{}
	h := tempo.New(rec, schema.DefaultOTelTraces(), "test", nil)
	h.Engine.SetSettings(engine.SettingsRules{JoinSpill: true, DistinctSpill: true})
	mux := http.NewServeMux()
	h.Mount(mux)

	window := "&start=1700000000&end=1700003600"
	for _, tc := range []struct {
		path string
		// bypassing selects the statements that dispatch outside the engine.
		bypassing func([]recordedStatement) []recordedStatement
		// mapKeys reports that every bypassing statement takes its DISTINCT
		// over attribute-map keys.
		mapKeys bool
	}{
		{"/api/search/tags?scope=span" + window, everyStatement, true},
		{"/api/v2/search/tags?scope=none" + window, everyStatement, true},
		{"/api/search/tag/http.method/values?" + window[1:], everyStatement, false},
		{"/api/v2/search/tag/span.http.method/values?" + window[1:], everyStatement, false},
		{"/api/metrics/query_range?step=60s&q=" + url.QueryEscape(`{ kind = server } > { kind = client } | rate()`) + window, exemplarsStatement, false},
		{"/api/search?limit=20&q=" + url.QueryEscape(`{ kind = server } >> { kind = client }`) + window, phaseAStatement, false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			calls := tc.bypassing(rec.taken())
			if len(calls) == 0 {
				t.Fatalf("GET %s dispatched no bypassing statement (HTTP %d: %s)", tc.path, w.Code, w.Body.String())
			}
			want := wantSpillThresholds(tc.mapKeys)
			for _, got := range calls {
				for name, value := range want {
					if v, ok := got.settings[name].(int64); !ok || strconv.FormatInt(v, 10) != value {
						t.Errorf("GET %s: %s = %v; want %s, on %s", tc.path, name, got.settings[name], value, got.sql)
					}
				}
				if v, stamped := got.settings[chopt.SettingMaxBytesBeforeExternalDistinct]; tc.mapKeys && stamped {
					t.Errorf("GET %s: %s = %v on a DISTINCT over map keys; want it withheld, on %s",
						tc.path, chopt.SettingMaxBytesBeforeExternalDistinct, v, got.sql)
				}
			}
		})
	}
}

// everyStatement selects every statement: the endpoint dispatches only
// outside the engine.
func everyStatement(calls []recordedStatement) []recordedStatement { return calls }

// exemplarsStatement selects the final statement, the exemplars statement a
// metrics range query runs after the engine's matrix statement. With fewer
// than two statements there is no exemplars statement to check, and it
// selects none.
func exemplarsStatement(calls []recordedStatement) []recordedStatement {
	const matrixAndExemplars = 2
	if len(calls) < matrixAndExemplars {
		return nil
	}
	return calls[len(calls)-1:]
}

// phaseAStatement selects the phase-A ranking statement of a structural
// search: the one statement a search dispatches through QueryStrings, ahead
// of the engine's phase B, which a phase A that ranks no trace skips.
func phaseAStatement(calls []recordedStatement) []recordedStatement {
	var out []recordedStatement
	for _, c := range calls {
		if c.method == "QueryStrings" {
			out = append(out, c)
		}
	}
	return out
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
