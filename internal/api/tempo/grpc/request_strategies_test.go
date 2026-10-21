package grpc_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/tempo/pkg/tempopb"

	"github.com/tsouza/cerberus/internal/api/tempo"
	tempogrpc "github.com/tsouza/cerberus/internal/api/tempo/grpc"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// mapSpanAttrRead is how a span-attribute read renders over a Map-typed
// SpanAttributes column; a JSON-typed one renders a JSON path instead.
const mapSpanAttrRead = "`SpanAttributes`[?]"

// storingQuerier records every statement and, after the first, stores next
// into cell — the schema re-probe's Store landing mid-RPC.
type storingQuerier struct {
	cell *chsql.SchemaStrategiesCell
	next chsql.ResolvedSchemaStrategies

	mu   sync.Mutex
	sqls []string
}

func (q *storingQuerier) Query(_ context.Context, sql string, _ ...any) ([]chclient.Sample, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sqls = append(q.sqls, sql)
	if len(q.sqls) == 1 {
		q.cell.Store(q.next)
	}
	return nil, nil
}

func (q *storingQuerier) QueryStrings(context.Context, string, ...any) ([]string, error) {
	return nil, nil
}

// TestMetricsQueryRange_RendersUnderOneStrategiesSnapshot pins that every
// statement one RPC emits — the matrix, then the exemplars — renders under
// the traces strategies the RPC took when it started, although the cell is
// stored with different ones between them.
func TestMetricsQueryRange_RendersUnderOneStrategiesSnapshot(t *testing.T) {
	t.Parallel()
	cell := chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{})
	q := &storingQuerier{cell: cell, next: chsql.ResolvedSchemaStrategies{Traces: chsql.SchemaStrategies{
		Attrs: chsql.AttrStrategies{"SpanAttributes": chsql.AttrStrategyJSON},
	}}}
	handler := tempo.New(q, schema.DefaultOTelTraces(), "test-grpc", nil)
	handler.Strategies = cell
	client, cleanup := newServiceTestServer(t, tempogrpc.NewService(handler, nil, nil))
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.MetricsQueryRange(ctx, &tempopb.QueryRangeRequest{
		Query: `{ span.custom_tag = "checkout" } | count_over_time()`,
		Start: metricsFixtureStartNs,
		End:   metricsFixtureEndNs,
		Step:  metricsFixtureStepNs,
	})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	drainRangeFrames(t, stream)

	const wantStatements = 2 // the matrix, then the exemplars
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.sqls) != wantStatements {
		t.Fatalf("RPC ran %d statements, want %d:\n%s", len(q.sqls), wantStatements, strings.Join(q.sqls, "\n---\n"))
	}
	for i, sql := range q.sqls {
		if !strings.Contains(sql, mapSpanAttrRead) {
			t.Errorf("statement %d does not read %s — it rendered under strategies loaded after the RPC "+
				"started:\n%s", i, mapSpanAttrRead, sql)
		}
	}
}
