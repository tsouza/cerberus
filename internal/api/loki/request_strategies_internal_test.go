package loki

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestWithRequestStrategies_OneSnapshotPerRequest pins that every read of
// the logs strategies inside one request answers the snapshot the request
// took on entry, although the cell is stored with different ones between
// two reads, and that the next request takes the stored ones.
func TestWithRequestStrategies_OneSnapshotPerRequest(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()
	jsonAttrs := chsql.ResolvedSchemaStrategies{Logs: chsql.SchemaStrategies{
		Attrs: chsql.AttrStrategies{s.ResourceAttributesColumn: chsql.AttrStrategyJSON},
	}}
	h := New(nil, s, nil)
	h.Strategies = chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{})

	var first, second chsql.SchemaStrategies
	serve := h.withRequestStrategies(func(_ http.ResponseWriter, r *http.Request) {
		first = h.logsStrategies(r.Context())
		h.Strategies.Store(jsonAttrs)
		second = h.logsStrategies(r.Context())
	})
	serve(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if len(first.Attrs) != 0 || len(second.Attrs) != 0 {
		t.Errorf("reads inside one request = %v then %v, want the entry snapshot (no attribute strategies) for both",
			first.Attrs, second.Attrs)
	}

	serve(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if first.Attrs[s.ResourceAttributesColumn] != chsql.AttrStrategyJSON {
		t.Errorf("the next request read %v, want the stored JSON strategy", first.Attrs)
	}
}
