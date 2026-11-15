package loki_test

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

// TestHandlerDispatches_CarryEngineSpillThresholds drives every Loki
// metadata route, and the data-plane routes beside them, and requires each
// statement to reach the client with the engine's spill thresholds: without
// them a high-cardinality metadata DISTINCT, GROUP BY or sort aborts at the
// memory cap where an engine query of the same size spills. No Loki
// statement is one whose DISTINCT a spill harms, so every one carries the
// DISTINCT threshold: the statements through the handler's own client carry
// all four, and an instant and a range metric query through the engine carry
// all but the join one, which the engine stamps only on a plan that joins. A
// route marked distinct must dispatch a DISTINCT — the label-name lookup, and
// the set operation — so its keeping the threshold is checked rather than
// vacuously absent.
func TestHandlerDispatches_CarryEngineSpillThresholds(t *testing.T) {
	rec := &spillSettingsRecorder{}
	h := loki.New(rec, schema.DefaultOTelLogs(), nil)
	h.Engine.SetSettings(engine.SettingsRules{JoinSpill: true, DistinctSpill: true})
	mux := http.NewServeMux()
	h.Mount(mux)

	selector := url.QueryEscape(`{service_name="api"}`)
	setOp := url.QueryEscape(`sum by (level) (count_over_time({service_name="api"}[1m])) unless on(level) ` +
		`sum by (level) (count_over_time({service_name="web"}[1m]))`)
	window := "&start=1700000000&end=1700003600"
	for _, tc := range []struct {
		method string
		path   string
		// viaEngine: the statements dispatch through the engine, which
		// stamps the join threshold only on a plan that joins.
		viaEngine bool
		// distinct: the route dispatches a DISTINCT the test must see.
		distinct bool
	}{
		{method: http.MethodGet, path: "/loki/api/v1/labels", distinct: true},
		{method: http.MethodPost, path: "/loki/api/v1/labels?query=" + selector, distinct: true},
		{method: http.MethodGet, path: "/loki/api/v1/label/service_name/values"},
		{method: http.MethodPost, path: "/loki/api/v1/label/service_name/values?query=" + selector},
		{method: http.MethodGet, path: "/loki/api/v1/series?match[]=" + selector},
		{method: http.MethodPost, path: "/loki/api/v1/series?match[]=" + selector},
		{method: http.MethodGet, path: "/loki/api/v1/detected_fields?query=" + selector},
		{method: http.MethodGet, path: "/loki/api/v1/detected_field/level/values?query=" + selector},
		{method: http.MethodGet, path: "/loki/api/v1/detected_labels?query=" + selector},
		{method: http.MethodGet, path: "/loki/api/v1/patterns?query=" + selector},
		{method: http.MethodGet, path: "/loki/api/v1/index/stats?query=" + selector},
		{method: http.MethodPost, path: "/loki/api/v1/index/volume?query=" + selector},
		{method: http.MethodGet, path: "/loki/api/v1/query_range?step=60&query=" + setOp + window, viaEngine: true, distinct: true},
		{method: http.MethodPost, path: "/loki/api/v1/query?time=1700003600&query=" + setOp, viaEngine: true, distinct: true},
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
		for name, value := range wantSpillThresholds() {
			if v, ok := call.settings[name].(int64); !ok || strconv.FormatInt(v, 10) != value {
				t.Errorf("tail poll %d: %s = %v; want %s", i, name, call.settings[name], value)
			}
		}
	}
}

// tailPollWait is the interval at which TestTailDispatch_CarriesEngineSpillThresholds
// checks the recorder for the tail loop's first poll.
const tailPollWait = 50 * time.Millisecond
