package prom_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// This file pins symptom 2 of #1949: the Prom metadata surface must
// degrade gracefully (answer a real, possibly-empty result) when one of
// its configured metric tables is absent from ClickHouse — because this
// deployment does not ingest that signal (e.g. no classic histograms, so
// otel_metrics_histogram was never provisioned), or because a table went
// missing after boot — instead of 502ing the whole request.

// unknownTableErr builds the typed ClickHouse exception (code 60,
// UNKNOWN_TABLE) the driver raises when a query names a table that does
// not exist.
func unknownTableErr(table string) error {
	return &clickhouse.Exception{
		Code:    60,
		Name:    "UNKNOWN_TABLE",
		Message: "Table default." + table + " doesn't exist",
	}
}

// absentTableQuerier simulates a deployment where absentTable is
// configured (the schema resolver defaulted it) but was never provisioned
// in ClickHouse. Any QueryStrings call whose SQL names absentTable in a
// FROM clause fails with UNKNOWN_TABLE; the system.tables existence probe
// (queryStringsDegradingUnknownTable's recovery step) reports every OTHER
// configured metric table as present; any other query succeeds and
// returns the stubbed rows.
type absentTableQuerier struct {
	absentTable string
	rows        []string

	systemTablesCalls int
	queryCalls        []string
}

func (q *absentTableQuerier) QueryStrings(_ context.Context, sql string, _ ...any) ([]string, error) {
	q.queryCalls = append(q.queryCalls, sql)
	if strings.Contains(sql, "`tables`") {
		q.systemTablesCalls++
		var existing []string
		for _, t := range schema.DefaultOTelMetrics().ConfiguredMetricTables() {
			if t != q.absentTable {
				existing = append(existing, t)
			}
		}
		return existing, nil
	}
	if strings.Contains(sql, q.absentTable) {
		return nil, unknownTableErr(q.absentTable)
	}
	return q.rows, nil
}

func (q *absentTableQuerier) Query(context.Context, string, ...any) ([]chclient.Sample, error) {
	return nil, nil
}

func (q *absentTableQuerier) QueryCursor(context.Context, string, ...any) (chclient.Cursor, error) {
	return newSliceCursor(nil), nil
}

func (q *absentTableQuerier) QueryLabelSets(context.Context, string, ...any) ([]map[string]string, error) {
	return nil, nil
}

func (q *absentTableQuerier) QueryMetricMeta(context.Context, string, string, ...any) ([]chclient.MetricMetaRow, error) {
	return nil, nil
}

func (q *absentTableQuerier) QueryExemplars(context.Context, string, ...any) ([]chclient.ExemplarRow, error) {
	return nil, nil
}

var _ prom.Querier = (*absentTableQuerier)(nil)

// allTablesAbsentQuerier simulates the extreme case: every configured
// metric table is absent (e.g. a deployment that ingests only logs and
// traces but never disabled the prom head, so it still defaults
// otel_metrics_gauge/_sum/_histogram — none of which will ever be
// provisioned). Every non-probe QueryStrings call fails UNKNOWN_TABLE; the
// existence probe finds nothing.
type allTablesAbsentQuerier struct {
	systemTablesCalls int
}

func (q *allTablesAbsentQuerier) QueryStrings(_ context.Context, sql string, _ ...any) ([]string, error) {
	if strings.Contains(sql, "`tables`") {
		q.systemTablesCalls++
		return nil, nil
	}
	return nil, unknownTableErr("otel_metrics_gauge")
}

func (q *allTablesAbsentQuerier) Query(context.Context, string, ...any) ([]chclient.Sample, error) {
	return nil, nil
}

func (q *allTablesAbsentQuerier) QueryCursor(context.Context, string, ...any) (chclient.Cursor, error) {
	return newSliceCursor(nil), nil
}

func (q *allTablesAbsentQuerier) QueryLabelSets(context.Context, string, ...any) ([]map[string]string, error) {
	return nil, nil
}

func (q *allTablesAbsentQuerier) QueryMetricMeta(context.Context, string, string, ...any) ([]chclient.MetricMetaRow, error) {
	return nil, nil
}

func (q *allTablesAbsentQuerier) QueryExemplars(context.Context, string, ...any) ([]chclient.ExemplarRow, error) {
	return nil, nil
}

var _ prom.Querier = (*allTablesAbsentQuerier)(nil)

func absentTableServer(q prom.Querier) *httptest.Server {
	h := prom.New(q, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	return httptest.NewServer(mux)
}

// probeFailsQuerier simulates the union query hitting UNKNOWN_TABLE, and
// then the RECOVERY existence probe itself failing (e.g. a transport blip
// hitting system.tables right after the rejection). Belt-and-suspenders
// (#1949): this must still degrade to an empty result, never a 502 — the
// probe failing is not a reason to surface the original rejection either.
type probeFailsQuerier struct{}

func (q *probeFailsQuerier) QueryStrings(_ context.Context, sql string, _ ...any) ([]string, error) {
	if strings.Contains(sql, "`tables`") {
		return nil, errors.New("dial tcp: connection refused")
	}
	return nil, unknownTableErr("otel_metrics_gauge")
}

func (q *probeFailsQuerier) Query(context.Context, string, ...any) ([]chclient.Sample, error) {
	return nil, nil
}

func (q *probeFailsQuerier) QueryCursor(context.Context, string, ...any) (chclient.Cursor, error) {
	return newSliceCursor(nil), nil
}

func (q *probeFailsQuerier) QueryLabelSets(context.Context, string, ...any) ([]map[string]string, error) {
	return nil, nil
}

func (q *probeFailsQuerier) QueryMetricMeta(context.Context, string, string, ...any) ([]chclient.MetricMetaRow, error) {
	return nil, nil
}

func (q *probeFailsQuerier) QueryExemplars(context.Context, string, ...any) ([]chclient.ExemplarRow, error) {
	return nil, nil
}

var _ prom.Querier = (*probeFailsQuerier)(nil)

// doubleVanishQuerier simulates a SECOND table vanishing between the
// existence probe and the narrowed retry: the probe reports only
// otel_metrics_sum as present (both gauge and histogram absent), but the
// retried query against just otel_metrics_sum ALSO hits UNKNOWN_TABLE.
// Belt-and-suspenders (#1949): this must still degrade to empty, never a
// 502, even though the recovery attempt itself failed.
type doubleVanishQuerier struct {
	systemTablesCalls int
}

func (q *doubleVanishQuerier) QueryStrings(_ context.Context, sql string, _ ...any) ([]string, error) {
	if strings.Contains(sql, "`tables`") {
		q.systemTablesCalls++
		return []string{schema.DefaultOTelMetrics().SumTable}, nil
	}
	return nil, unknownTableErr("otel_metrics_gauge")
}

func (q *doubleVanishQuerier) Query(context.Context, string, ...any) ([]chclient.Sample, error) {
	return nil, nil
}

func (q *doubleVanishQuerier) QueryCursor(context.Context, string, ...any) (chclient.Cursor, error) {
	return newSliceCursor(nil), nil
}

func (q *doubleVanishQuerier) QueryLabelSets(context.Context, string, ...any) ([]map[string]string, error) {
	return nil, nil
}

func (q *doubleVanishQuerier) QueryMetricMeta(context.Context, string, string, ...any) ([]chclient.MetricMetaRow, error) {
	return nil, nil
}

func (q *doubleVanishQuerier) QueryExemplars(context.Context, string, ...any) ([]chclient.ExemplarRow, error) {
	return nil, nil
}

var _ prom.Querier = (*doubleVanishQuerier)(nil)

// TestLabels_ExistenceProbeFailureDegradesToEmpty pins the first
// belt-and-suspenders branch: the recovery probe itself failing must not
// surface the original UNKNOWN_TABLE as a 502.
func TestLabels_ExistenceProbeFailureDegradesToEmpty(t *testing.T) {
	t.Parallel()
	srv := absentTableServer(&probeFailsQuerier{})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/labels")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (an existence-probe failure must not surface the original UNKNOWN_TABLE as a 502); body=%s", resp.StatusCode, body)
	}
}

// TestLabels_SecondUnknownTableOnRetryDegradesToEmpty pins the second
// belt-and-suspenders branch: a table vanishing a second time, between the
// existence probe and the narrowed retry, must also degrade to empty
// rather than surface as a 502.
func TestLabels_SecondUnknownTableOnRetryDegradesToEmpty(t *testing.T) {
	t.Parallel()
	q := &doubleVanishQuerier{}
	srv := absentTableServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/labels")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a second UNKNOWN_TABLE on the narrowed retry must not surface as a 502); body=%s", resp.StatusCode, body)
	}
	if q.systemTablesCalls == 0 {
		t.Error("existence probe never called")
	}
}

// TestLabels_DegradesOnAbsentMetricTable is the direct regression test for
// symptom 2: before the fix, unionLabelNamesSQL unconditionally unioned
// all three metric tables with no existence guard, so ClickHouse's
// UNKNOWN_TABLE on the histogram table failed the whole combined query and
// the handler wrapped it as a 502. It must now answer 200 with the names
// the surviving tables produce.
func TestLabels_DegradesOnAbsentMetricTable(t *testing.T) {
	t.Parallel()
	m := schema.DefaultOTelMetrics()
	q := &absentTableQuerier{absentTable: m.HistogramTable, rows: []string{"instance", "job"}}
	srv := absentTableServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/labels")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (must degrade, not 502); body=%s", resp.StatusCode, body)
	}
	var parsed metadataResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("body not JSON: %v; body=%s", err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("status = %q, want success; body=%s", parsed.Status, body)
	}
	if q.systemTablesCalls == 0 {
		t.Error("existence probe (system.tables) never called; want it consulted after the UNKNOWN_TABLE rejection")
	}
	if strings.Contains(body, `"error"`) {
		t.Errorf("body carries an error field on a degraded (still-successful) response: %s", body)
	}
}

// TestLabels_AllMetricTablesAbsent covers the deployment-wide case named in
// the issue: a prom head left enabled by default over a logs+traces-only
// ingestion pipeline, where NONE of the three metric tables ever get
// provisioned. The endpoint must still answer 200 with the synthetic
// __name__ label alone, never a 502 — and it must not issue a zero-arm
// UNION (queryStringsDegradingUnknownTable short-circuits once the
// existence probe narrows the candidate set to nothing).
func TestLabels_AllMetricTablesAbsent(t *testing.T) {
	t.Parallel()
	q := &allTablesAbsentQuerier{}
	srv := absentTableServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/labels")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (an all-absent metrics surface must answer empty, like reference Prometheus, not 502); body=%s", resp.StatusCode, body)
	}
	var parsed metadataResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("body not JSON: %v; body=%s", err, body)
	}
	var names []string
	if err := json.Unmarshal(parsed.Data, &names); err != nil {
		t.Fatalf("data not a string array: %v; body=%s", err, body)
	}
	if len(names) != 1 || names[0] != "__name__" {
		t.Errorf("names = %v, want exactly [\"__name__\"] (no metric table contributes any real label)", names)
	}
	if q.systemTablesCalls == 0 {
		t.Error("existence probe never called")
	}
}

// TestLabelValues_DegradesOnAbsentMetricTable covers unionLabelValuesSQL —
// the /api/v1/label/<name>/values arm named explicitly in #1949 — with the
// same absent-histogram-table scenario as TestLabels_DegradesOnAbsentMetricTable.
func TestLabelValues_DegradesOnAbsentMetricTable(t *testing.T) {
	t.Parallel()
	m := schema.DefaultOTelMetrics()
	q := &absentTableQuerier{absentTable: m.HistogramTable, rows: []string{"prod", "staging"}}
	srv := absentTableServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/label/environment/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (must degrade, not 502); body=%s", resp.StatusCode, body)
	}
	var parsed metadataResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("body not JSON: %v; body=%s", err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("status = %q, want success; body=%s", parsed.Status, body)
	}
	if q.systemTablesCalls == 0 {
		t.Error("existence probe (system.tables) never called; want it consulted after the UNKNOWN_TABLE rejection")
	}
}
