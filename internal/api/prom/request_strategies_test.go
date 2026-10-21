package prom_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// recordingQuerier records every statement a request runs and, when next is
// set, stores it into cell after the first — the schema re-probe's Store
// landing while the request is mid-flight.
type recordingQuerier struct {
	cell *chsql.SchemaStrategiesCell
	next *chsql.ResolvedSchemaStrategies

	mu   sync.Mutex
	sqls []string
}

func (q *recordingQuerier) record(sql string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sqls = append(q.sqls, sql)
	if len(q.sqls) == 1 && q.next != nil {
		q.cell.Store(*q.next)
	}
}

func (q *recordingQuerier) Query(_ context.Context, sql string, _ ...any) ([]chclient.Sample, error) {
	q.record(sql)
	return nil, nil
}

func (q *recordingQuerier) QueryCursor(_ context.Context, sql string, _ ...any) (chclient.Cursor, error) {
	q.record(sql)
	return newSliceCursor(nil), nil
}

func (q *recordingQuerier) QueryStrings(_ context.Context, sql string, _ ...any) ([]string, error) {
	q.record(sql)
	return nil, nil
}

func (q *recordingQuerier) QueryLabelSets(_ context.Context, sql string, _ ...any) ([]map[string]string, error) {
	q.record(sql)
	return nil, nil
}

func (q *recordingQuerier) QueryMetricMeta(_ context.Context, sql, _ string, _ ...any) ([]chclient.MetricMetaRow, error) {
	q.record(sql)
	return nil, nil
}

func (q *recordingQuerier) QueryExemplars(_ context.Context, sql string, _ ...any) ([]chclient.ExemplarRow, error) {
	q.record(sql)
	return nil, nil
}

var _ prom.Querier = (*recordingQuerier)(nil)

// requestStatements serves one GET of path?params through a handler over q
// whose Strategies is cell, and returns the statements it ran.
func requestStatements(t *testing.T, q *recordingQuerier, path string, params url.Values) []string {
	t.Helper()
	h := prom.New(q, schema.DefaultOTelMetrics(), nil)
	h.Strategies = q.cell
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + path + "?" + params.Encode())
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", path, resp.StatusCode, body)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.sqls...)
}

// TestRequestRendersUnderOneStrategiesSnapshot pins that every statement one
// metrics request runs renders under the strategies the request took when it
// entered the handler, although the cell is stored with different ones after
// its first statement: the request runs exactly the statements it runs when
// nothing is stored. The request is a /series whose match[] selectors
// outnumber one statement's arms, so it runs a statement per chunk of them;
// its bound lies before 1900, which a nanosecond and a second precision
// render differently.
func TestRequestRendersUnderOneStrategiesSnapshot(t *testing.T) {
	t.Parallel()
	const selectors = 129 // one more than a /series statement's arm cap
	params := url.Values{"start": {"-2500000000"}, "end": {"1778580000"}}
	for i := range selectors {
		params.Add("match[]", "metric_"+strconv.Itoa(i))
	}
	seconds := chsql.ResolvedSchemaStrategies{Metrics: chsql.SchemaStrategies{MetricsTimestamp: schema.MetricsTimestampSeconds}}

	want := requestStatements(t, &recordingQuerier{
		cell: chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{}),
	}, "/api/v1/series", params)
	got := requestStatements(t, &recordingQuerier{
		cell: chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{}),
		next: &seconds,
	}, "/api/v1/series", params)

	const minStatements = 2 // the Store lands between the first and a later one
	if len(want) < minStatements {
		t.Fatalf("request ran %d statements, want at least %d:\n%s", len(want), minStatements, strings.Join(want, "\n---\n"))
	}
	if strings.Join(got, "\n---\n") != strings.Join(want, "\n---\n") {
		t.Errorf("a Store mid-request changed the statements it ran:\ngot:\n%s\nwant:\n%s",
			strings.Join(got, "\n---\n"), strings.Join(want, "\n---\n"))
	}
}
