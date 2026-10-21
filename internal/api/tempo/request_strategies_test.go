package tempo_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// mapSpanAttrRead is how a span-attribute read renders over a Map-typed
// SpanAttributes column; a JSON-typed one renders a JSON path instead.
const mapSpanAttrRead = "`SpanAttributes`[?]"

// storingQuerier records every statement it is asked to run and, after the
// first, stores next into cell — the schema re-probe's Store landing while
// a request is mid-flight.
type storingQuerier struct {
	cell *chsql.SchemaStrategiesCell
	next chsql.ResolvedSchemaStrategies

	mu   sync.Mutex
	sqls []string
}

func (q *storingQuerier) record(sql string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sqls = append(q.sqls, sql)
	if len(q.sqls) == 1 {
		q.cell.Store(q.next)
	}
}

func (q *storingQuerier) Query(_ context.Context, sql string, _ ...any) ([]chclient.Sample, error) {
	q.record(sql)
	return nil, nil
}

func (q *storingQuerier) QueryStrings(_ context.Context, sql string, _ ...any) ([]string, error) {
	q.record(sql)
	return nil, nil
}

// TestRequestRendersUnderOneStrategiesSnapshot pins that every statement
// one Tempo request emits renders under the traces strategies the request
// took when it entered the handler, although the cell is stored with
// different ones between its statements: a metrics range query emits the
// matrix statement and then the exemplars statement, and both must read
// span.custom_tag as the Map they were rendered for at the start.
func TestRequestRendersUnderOneStrategiesSnapshot(t *testing.T) {
	t.Parallel()
	cell := chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{})
	q := &storingQuerier{cell: cell, next: chsql.ResolvedSchemaStrategies{Traces: chsql.SchemaStrategies{
		Attrs: chsql.AttrStrategies{"SpanAttributes": chsql.AttrStrategyJSON},
	}}}
	h := tempo.New(q, schema.DefaultOTelTraces(), "v-test", nil)
	h.Strategies = cell
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(metricsQueryRangeURL(srv.URL, `{ span.custom_tag = "checkout" } | count_over_time()`,
		map[string]string{"start": fixtureStartUnix, "end": fixtureEndUnix, "step": "60s"}))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	const wantStatements = 2 // the matrix, then the exemplars
	if len(q.sqls) != wantStatements {
		t.Fatalf("request ran %d statements, want %d:\n%s", len(q.sqls), wantStatements, strings.Join(q.sqls, "\n---\n"))
	}
	for i, sql := range q.sqls {
		if !strings.Contains(sql, mapSpanAttrRead) {
			t.Errorf("statement %d does not read %s — it rendered under strategies loaded after the request "+
				"entered the handler:\n%s", i, mapSpanAttrRead, sql)
		}
	}
}
