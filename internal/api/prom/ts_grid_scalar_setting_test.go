package prom_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// The experimental timeSeries*ToGrid aggregate family only runs when the
// dispatch carries allow_experimental_time_series_aggregate_functions=1.
// ClickHouse enforces that per QUERY, so the invariant this file pins is
// per-dispatch: every statement cerberus sends that names one of those
// aggregates must carry the setting.
//
// The two shapes below are the compatibility-corpus queries that regressed
// when a computed scalar parameter started binding its vector per step: the
// resample node moved INSIDE a chplan.ScalarSubquery, where a Children()-only
// sweep could not see it. They exercise both dispatch seams —
// `vector(scalar(m))` fails on the main statement, `topk(scalar(m) * 2, m)`
// on the ScalarGuard that runs the K parameter's own query — so covering one
// alone leaves half the regression unpinned.

// tsGridAggregatePrefix is the ClickHouse aggregate-name prefix shared by
// every member of the experimental timeSeries*ToGrid family
// (timeSeriesResampleToGridWithStaleness, timeSeriesRateToGrid, ...).
const tsGridAggregatePrefix = "timeSeries"

// tsGridDispatch is one statement the handler sent, with the per-query
// ClickHouse settings that rode with it.
type tsGridDispatch struct {
	sql      string
	settings map[string]any
}

// ctxRecordingQuerier records every dispatch's SQL together with the
// per-request ClickHouse settings attached to its ctx. Everything it does not
// override is answered by the shared stub.
type ctxRecordingQuerier struct {
	*stubQuerier
	dispatches []tsGridDispatch
}

func (q *ctxRecordingQuerier) record(ctx context.Context, sql string) {
	settings := map[string]any{}
	for k, v := range chclient.QuerySettingsFromContext(ctx) {
		settings[k] = v
	}
	q.dispatches = append(q.dispatches, tsGridDispatch{sql: sql, settings: settings})
}

func (q *ctxRecordingQuerier) Query(ctx context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	q.record(ctx, sql)
	return q.stubQuerier.Query(ctx, sql, args...)
}

func (q *ctxRecordingQuerier) QueryCursor(ctx context.Context, sql string, args ...any) (chclient.Cursor, error) {
	q.record(ctx, sql)
	return q.stubQuerier.QueryCursor(ctx, sql, args...)
}

// TestQueryRange_ComputedScalarCarriesTSGridSetting drives the two regressed
// corpus shapes through the HTTP range endpoint with the native staleness
// lowerer installed (the posture the compatibility lane runs), and asserts
// every dispatch that emits a ts-grid aggregate carries the experimental
// setting.
func TestQueryRange_ComputedScalarCarriesTSGridSetting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"vector of a computed scalar", `vector(scalar(demo_num_cpus))`},
		{"topk with a computed K", `topk(scalar(demo_num_cpus) * 2, demo_memory_usage_bytes)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dispatches := runTSGridRangeQuery(t, tc.query)

			native := 0
			for i, d := range dispatches {
				if !strings.Contains(d.sql, tsGridAggregatePrefix) {
					continue
				}
				native++
				if got := d.settings[chclient.SettingExperimentalTSGridAggregate]; got != 1 {
					t.Errorf("dispatch %d emits a %s* aggregate with %s = %v; want 1 — ClickHouse "+
						"rejects it with code 63 otherwise.\nSQL: %s",
						i, tsGridAggregatePrefix, chclient.SettingExperimentalTSGridAggregate, got, d.sql)
				}
			}
			if native == 0 {
				t.Fatalf("no dispatch emitted a %s* aggregate across %d statements; the assertion above "+
					"would pass vacuously — the native staleness lowerer did not take effect",
					tsGridAggregatePrefix, len(dispatches))
			}
		})
	}
}

// runTSGridRangeQuery issues one /api/v1/query_range against a handler wired
// with the native staleness lowerer and returns every dispatch it made. A
// non-200 answer is itself a failure: these queries must succeed.
func runTSGridRangeQuery(t *testing.T, query string) []tsGridDispatch {
	t.Helper()

	q := &ctxRecordingQuerier{stubQuerier: &stubQuerier{}}
	h := prom.New(q, schema.DefaultOTelMetrics(), nil)
	// The posture cmd/cerberus wires when chopt resolves the ts_grid_resample
	// feature in: a bare range-mode selector lowers to the native
	// timeSeriesResampleToGridWithStaleness node instead of the fan-out.
	h.SetLowerers(promql.RangeLowerers{
		Staleness: promql.NativeStalenessLowerer{Fallback: promql.FanoutStalenessLowerer{}},
	})
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	const (
		rangeStart = 1767225600 // 2026-01-01T00:00:00Z
		rangeEnd   = 1767225900 // +5m
		rangeStep  = "30"
	)
	params := url.Values{
		"query": {query},
		"start": {fmt.Sprint(rangeStart)},
		"end":   {fmt.Sprint(rangeEnd)},
		"step":  {rangeStep},
	}
	resp, err := http.Get(srv.URL + "/api/v1/query_range?" + params.Encode())
	if err != nil {
		t.Fatalf("query_range request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for %s", resp.StatusCode, query)
	}
	return q.dispatches
}
