package loki_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
	calls []recordedStatement
}

// recordedStatement is one statement a spillSettingsRecorder saw: its SQL
// and its per-query settings.
type recordedStatement struct {
	sql      string
	settings map[string]any
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

func (r *spillSettingsRecorder) QueryStrings(ctx context.Context, sql string, _ ...any) ([]string, error) {
	r.record(ctx, sql)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryDetectedFieldRows(ctx context.Context, sql string, _ ...any) ([]chclient.DetectedFieldRow, error) {
	r.record(ctx, sql)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryTimestampedLines(ctx context.Context, sql string, _ ...any) ([]chclient.TimestampedLine, error) {
	r.record(ctx, sql)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryIndexStats(ctx context.Context, sql string, _ ...any) (chclient.IndexStatsRow, error) {
	r.record(ctx, sql)
	return chclient.IndexStatsRow{}, nil
}

func (r *spillSettingsRecorder) QueryIndexVolume(ctx context.Context, sql string, _ ...any) ([]chclient.IndexVolumeRow, error) {
	r.record(ctx, sql)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryLabelSets(ctx context.Context, sql string, _ ...any) ([]map[string]string, error) {
	r.record(ctx, sql)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryLabelCardinalities(ctx context.Context, sql string, _ ...any) ([]chclient.LabelCardinalityRow, error) {
	r.record(ctx, sql)
	return nil, nil
}

// TestHandlerDispatches_CarryEngineSpillThresholds drives every Loki endpoint
// that dispatches through the handler's own client rather than the engine,
// and requires each statement to reach the client with the engine's spill
// thresholds: without them a high-cardinality metadata DISTINCT aborts at the
// memory cap where an engine query of the same size spills. A statement
// whose DISTINCT runs over attribute-map keys carries every threshold but the
// DISTINCT one, and the label-name endpoint must dispatch at least one such
// statement, so the withholding is exercised rather than vacuously absent.
func TestHandlerDispatches_CarryEngineSpillThresholds(t *testing.T) {
	rec := &spillSettingsRecorder{}
	h := loki.New(rec, schema.DefaultOTelLogs(), nil)
	h.Engine.SetSettings(engine.SettingsRules{JoinSpill: true, DistinctSpill: true})
	mux := http.NewServeMux()
	h.Mount(mux)

	selector := url.QueryEscape(`{service_name="api"}`)
	for _, tc := range []struct {
		path    string
		mapKeys bool
	}{
		{"/loki/api/v1/labels", true},
		{"/loki/api/v1/label/service_name/values", false},
		{"/loki/api/v1/series?match[]=" + selector, false},
		{"/loki/api/v1/index/stats?query=" + selector, false},
		{"/loki/api/v1/index/volume?query=" + selector, false},
		{"/loki/api/v1/detected_fields?query=" + selector, false},
		{"/loki/api/v1/detected_field/level/values?query=" + selector, false},
		{"/loki/api/v1/detected_labels?query=" + selector, false},
		{"/loki/api/v1/patterns?query=" + selector, false},
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
				for name, value := range wantSpillThresholds(mapKeys) {
					if v, ok := call.settings[name].(int64); !ok || strconv.FormatInt(v, 10) != value {
						t.Errorf("statement %d of GET %s: %s = %v; want %s", i, path, name, call.settings[name], value)
					}
				}
				if v, stamped := call.settings[chopt.SettingMaxBytesBeforeExternalDistinct]; mapKeys && stamped {
					t.Errorf("statement %d of GET %s: %s = %v on a DISTINCT over map keys; want it withheld",
						i, path, chopt.SettingMaxBytesBeforeExternalDistinct, v)
				}
			}
			if sawMapKeys != tc.mapKeys {
				t.Errorf("GET %s dispatched a DISTINCT over map keys = %t; want %t", path, sawMapKeys, tc.mapKeys)
			}
		})
	}
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

// isMapKeyDistinct reports whether sql takes its DISTINCT over attribute-map
// keys, the statement shape the handler marks to withhold the DISTINCT spill
// threshold.
func isMapKeyDistinct(sql string) bool {
	return strings.Contains(sql, "DISTINCT") &&
		(strings.Contains(sql, "mapKeys(") || strings.Contains(sql, ".`keys`"))
}

// tailFirstPollBudget bounds the wait for the tail loop's first poll, which
// fires one poll interval after the WebSocket upgrade.
const tailFirstPollBudget = 10 * time.Second

// TestTailDispatch_CarriesEngineSpillThresholds drives the tail WebSocket,
// whose poll loop dispatches through the handler's own client on every tick,
// and requires the first poll to reach the client with all four of the
// engine's spill thresholds.
func TestTailDispatch_CarriesEngineSpillThresholds(t *testing.T) {
	rec := &spillSettingsRecorder{}
	h := loki.New(rec, schema.DefaultOTelLogs(), nil)
	h.Engine.SetSettings(engine.SettingsRules{JoinSpill: true, DistinctSpill: true})
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	conn := dialTail(t, srv, `{service_name="api"}`)
	defer conn.Close()

	var calls []recordedStatement
	for deadline := time.Now().Add(tailFirstPollBudget); len(calls) == 0 && time.Now().Before(deadline); {
		time.Sleep(tailPollWait)
		calls = rec.taken()
	}
	if len(calls) == 0 {
		t.Fatalf("the tail loop dispatched no statement within %s", tailFirstPollBudget)
	}
	for i, call := range calls {
		for name, value := range wantSpillThresholds(false) {
			if v, ok := call.settings[name].(int64); !ok || strconv.FormatInt(v, 10) != value {
				t.Errorf("tail poll %d: %s = %v; want %s", i, name, call.settings[name], value)
			}
		}
	}
}

// tailPollWait is the interval at which TestTailDispatch_CarriesEngineSpillThresholds
// checks the recorder for the tail loop's first poll.
const tailPollWait = 50 * time.Millisecond
