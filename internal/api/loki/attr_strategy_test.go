package loki_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestQuery_JSONAttrStrategyRendersDynamicSubcolumnPath is the end-to-end
// integration proof for cerberus issue #2777's threading: a Handler whose
// AttrStrategies marks the logs ResourceAttributes column as
// AttrStrategyJSON (mirroring cmd/cerberus's runRequirementsCheck ->
// newLokiHandler wiring) must emit chsql's JSON dynamic-subcolumn read for
// a `{job="api"}` stream-selector query, not the Map bracket-subscript
// shape TestQuery_Streams (the byte-identical-Map-path sibling in this
// same package) pins for the default (nil AttrStrategies) Handler.
//
// This closes the loop the chsql-level tests (attr_strategy_test.go,
// attr_strategy_json_chdb_test.go in internal/chsql) don't reach on their
// own: those prove the RENDERING primitive is correct in isolation; this
// proves a real HTTP request, through Handler -> logql.Lang ->
// engine.Engine -> chsql.Emit, actually resolves and applies the
// AttrStrategies a caller wires onto the Handler.
func TestQuery_JSONAttrStrategyRendersDynamicSubcolumnPath(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	q := &stubQuerier{}
	h := loki.New(q, s, nil)
	// Mirrors cmd/cerberus's newLokiHandler: both the Handler's own field
	// (for a future per-request langForRequest copy) AND the long-lived
	// h.Lang (the handler's own field is not read directly by /query — see
	// Handler's own doc — but wiring it too matches production and would
	// catch a future refactor that starts reading it).
	h.AttrStrategies = chsql.AttrStrategies{s.ResourceAttributesColumn: chsql.AttrStrategyJSON}
	h.Lang.AttrStrategies = h.AttrStrategies

	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	sql := q.LastSQL()
	if sql == "" {
		t.Fatal("stubQuerier saw no SQL — request never reached the engine")
	}
	if !strings.Contains(sql, s.ResourceAttributesColumn+"`.`job`.:String") {
		t.Errorf("SQL does not carry the JSON dynamic-subcolumn read for %q:\n%s", s.ResourceAttributesColumn, sql)
	}
	if strings.Contains(sql, "`"+s.ResourceAttributesColumn+"`[") {
		t.Errorf("SQL still carries a Map bracket-subscript against %q, want the JSON path instead:\n%s",
			s.ResourceAttributesColumn, sql)
	}
}

// TestMetadataBuilders_JSONAttrStrategyReachesEveryBuilder covers the
// three builders that were left out of cerberus issue #3063's threading:
// /index/stats, /index/volume and /patterns each construct their SQL
// directly against chsql.NewQuery() and so never reach chsql.Emit's
// ctx-based AttrStrategies plumbing, yet none of them called
// .WithAttrStrategies. Every one of them shares applySelectorAndWindow,
// whose stream-selector matchers are per-key attribute-map accesses — so
// against a JSON-typed attributes column all three emitted a
// Map bracket-subscript that the column cannot answer.
//
// /patterns was explicitly (and wrongly) documented as exempt on the
// grounds that it projects only Timestamp/Body/SeverityText: a builder's
// exposure is wherever it touches an attribute map in the STATEMENT, and
// the WHERE clause is such a place.
func TestMetadataBuilders_JSONAttrStrategyReachesEveryBuilder(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
	}{
		{"index-stats", `/loki/api/v1/index/stats?query=%7Bjob%3D%22api%22%7D`},
		{"index-volume", `/loki/api/v1/index/volume?query=%7Bjob%3D%22api%22%7D`},
		{"patterns", `/loki/api/v1/patterns?query=%7Bjob%3D%22api%22%7D`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := schema.DefaultOTelLogs()
			q := &stubQuerier{}
			h := loki.New(q, s, nil)
			h.AttrStrategies = chsql.AttrStrategies{s.ResourceAttributesColumn: chsql.AttrStrategyJSON}
			h.Lang.AttrStrategies = h.AttrStrategies

			mux := http.NewServeMux()
			h.Mount(mux)
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", resp.StatusCode)
			}

			sql := q.LastSQL()
			if sql == "" {
				t.Fatal("stubQuerier saw no SQL — request never reached a builder")
			}
			if strings.Contains(sql, "`"+s.ResourceAttributesColumn+"`[") {
				t.Errorf("SQL carries a Map bracket-subscript against the JSON-typed %q column:\n%s",
					s.ResourceAttributesColumn, sql)
			}
			if !strings.Contains(sql, s.ResourceAttributesColumn+"`.`job`.:String") {
				t.Errorf("SQL does not carry the JSON dynamic-subcolumn read for %q:\n%s",
					s.ResourceAttributesColumn, sql)
			}
		})
	}
}
