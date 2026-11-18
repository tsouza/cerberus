package traceql

import (
	"maps"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestMaterializedCarriedBy pins the registry narrowing a stage lowers its
// reads with: a closed row type keeps exactly the materialized columns it
// carries, and an open one keeps every registry entry.
func TestMaterializedCarriedBy(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	s.MaterializedSpanAttributeColumns = map[string]string{"rpc.method": "RpcMethod", "http.status_code": "HttpStatusCode"}
	s.MaterializedResourceAttributeColumns = map[string]string{"service.name": "ServiceName"}

	closed := chplan.Schema{Columns: []chplan.Column{{Name: "TraceId"}, {Name: "RpcMethod"}, {Name: "SpanAttributes"}}}
	got := materializedCarriedBy(closed, s)
	if want := map[string]string{"rpc.method": "RpcMethod"}; !maps.Equal(got.MaterializedSpanAttributeColumns, want) {
		t.Errorf("closed row: span registry = %v, want %v", got.MaterializedSpanAttributeColumns, want)
	}
	if len(got.MaterializedResourceAttributeColumns) != 0 {
		t.Errorf("closed row: resource registry = %v, want empty", got.MaterializedResourceAttributeColumns)
	}
	if len(s.MaterializedSpanAttributeColumns) != 2 {
		t.Errorf("narrowing mutated the input registry: %v", s.MaterializedSpanAttributeColumns)
	}

	open := materializedCarriedBy(chplan.Schema{Open: true}, s)
	if !maps.Equal(open.MaterializedSpanAttributeColumns, s.MaterializedSpanAttributeColumns) ||
		!maps.Equal(open.MaterializedResourceAttributeColumns, s.MaterializedResourceAttributeColumns) {
		t.Errorf("open row: registries = %v / %v, want the input's", open.MaterializedSpanAttributeColumns, open.MaterializedResourceAttributeColumns)
	}
}
