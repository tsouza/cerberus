package loki_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestIndexVolume_CanonicalMapTypeCastCastsBothLevels pins that on a server
// whose resolved optimizations include canonical_map_type_cast, both
// grouping levels of /index/volume — the stored label set and the served
// label set regrouped over it — cast their canonical key, so neither
// GROUP BY fails reading a spilled block back.
func TestIndexVolume_CanonicalMapTypeCastCastsBothLevels(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		volumeRows: []chclient.IndexVolumeRow{{Labels: map[string]string{"job": "api"}, Bytes: 1}},
	}
	h := loki.New(q, schema.DefaultOTelLogs(), nil)
	cell := chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{})
	cell.SetCanonicalMapTypeCast(true)
	h.Strategies = cell
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		`/loki/api/v1/index/volume?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	sql := q.LastSQL()
	if n := strings.Count(sql, "CAST(mapSort("); n != 2 {
		t.Fatalf("want the stored and the served label set cast (2 sites), got %d: %s", n, sql)
	}
	if strings.Count(sql, "mapSort(") != strings.Count(sql, "CAST(mapSort(") {
		t.Fatalf("a canonical key renders without its cast: %s", sql)
	}
}
