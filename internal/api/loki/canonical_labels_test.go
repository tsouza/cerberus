package loki_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// canonicalLabelsCol is the exact token every Loki metadata query that
// keys on the whole label-set Map must emit for its identity key: the
// canonical key-order function applied to the raw attributes column.
// Built from chplan.CanonicalMapFunc so this pin tracks the shared
// primitive rather than hard-coding a second copy of the function name.
var canonicalLabelsCol = chplan.CanonicalMapFunc + "(`ResourceAttributes`)"

// labelsAlias is the SELECT alias the three GROUP BY-on-label-set
// queries project the canonicalised Map under. It MUST differ from the
// source column name: ClickHouse resolves GROUP BY identifiers against
// SELECT aliases BEFORE FROM columns, so aliasing the wrapped expression
// back to `ResourceAttributes` and grouping by it would either group by
// mapSort(mapSort(...)) or fail outright with NOT_AN_AGGREGATE.
const labelsAlias = "labels"

// keyOrderSite is one emitted-SQL shape pin. want is the substring the
// query must carry for its whole-Map identity key to be key-order
// canonical; groupsByAlias marks the queries that additionally GROUP BY
// the alias rather than by the wrapped expression.
type keyOrderSite struct {
	name          string
	path          string
	want          string
	groupsByAlias bool
}

// TestMetadataSQL_CanonicalisesWholeMapIdentityKeys pins that every Loki
// metadata query whose series-identity key is the WHOLE ResourceAttributes
// Map wraps that key in the canonical key-order function.
//
// A CH Map compares, groups and hashes positionally over its
// (keys, values) arrays, and the OTel-CH exporter never sorts, so one
// logical stream can reach the store under two physical key orders. Drop
// the wrap from any site below and that stream splits in two: /index/volume
// reports half-size volumes and a corrupted top-N, /index/stats reports
// double the stream count, /series and /detected_labels emit redundant
// rows that burn the client's row drain budget.
//
// This is the cheap shape-level gate; the behavioural proof that the wrap
// changes the ANSWER lives in map_key_order_chdb_test.go, which executes
// the emitted SQL against real ClickHouse semantics.
func TestMetadataSQL_CanonicalisesWholeMapIdentityKeys(t *testing.T) {
	t.Parallel()

	const window = "start=1717995600&end=1717999200"
	sites := []keyOrderSite{
		{
			name:          "index/volume groups by the canonicalised label set",
			path:          `/loki/api/v1/index/volume?query=%7Bjob%3D%22api%22%7D&` + window,
			want:          canonicalLabelsCol + " AS `" + labelsAlias + "`",
			groupsByAlias: true,
		},
		{
			name: "index/stats counts distinct canonicalised label sets",
			path: `/loki/api/v1/index/stats?query=%7Bjob%3D%22api%22%7D&` + window,
			want: "uniqExact(" + canonicalLabelsCol + ")",
		},
		{
			name:          "series groups by the canonicalised label set",
			path:          `/loki/api/v1/series?match%5B%5D=%7Bjob%3D%22api%22%7D&` + window,
			want:          canonicalLabelsCol + " AS `" + labelsAlias + "`",
			groupsByAlias: true,
		},
		{
			name:          "detected_labels groups by the canonicalised label set",
			path:          `/loki/api/v1/detected_labels?query=%7Bjob%3D%22api%22%7D&` + window,
			want:          canonicalLabelsCol + " AS `" + labelsAlias + "`",
			groupsByAlias: true,
		},
	}

	for _, site := range sites {
		t.Run(site.name, func(t *testing.T) {
			t.Parallel()
			sql := captureMetadataSQL(t, site.path)
			if !strings.Contains(sql, site.want) {
				t.Fatalf("identity key is not key-order canonical: want substring %q in %q",
					site.want, sql)
			}
			if !site.groupsByAlias {
				return
			}
			wantGroup := "GROUP BY `" + labelsAlias + "`"
			if !strings.Contains(sql, wantGroup) {
				t.Errorf("must GROUP BY the alias (CH resolves GROUP BY against SELECT "+
					"aliases first): want %q in %q", wantGroup, sql)
			}
			// The alias-landmine guard: grouping by the wrapped
			// EXPRESSION while aliasing it to the source column name is
			// what produces CH's NOT_AN_AGGREGATE (code 215).
			badGroup := "GROUP BY " + canonicalLabelsCol
			if strings.Contains(sql, badGroup) {
				t.Errorf("must not GROUP BY the wrapped expression: found %q in %q",
					badGroup, sql)
			}
		})
	}
}

// TestMetadataSQL_DoesNotCanonicaliseMapPayloads is the negative half of
// the invariant, and it is load-bearing: the canonical wrap belongs where
// a Map is an identity KEY, never where it is per-row PAYLOAD returned
// under some other key. /detected_fields projects ResourceAttributes as
// `stream_labels` and /tail projects it as `Attributes`; both are read
// per row and folded in Go, so wrapping them would buy nothing and cost a
// sort on every scanned row.
func TestMetadataSQL_DoesNotCanonicaliseMapPayloads(t *testing.T) {
	t.Parallel()

	const window = "start=1717995600&end=1717999200"
	payloads := []struct {
		name  string
		path  string
		alias string
	}{
		{
			name:  "detected_fields projects the raw stream-label payload",
			path:  `/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D&` + window,
			alias: "stream_labels",
		},
	}

	for _, p := range payloads {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			sql := captureMetadataSQL(t, p.path)
			wantRaw := "`ResourceAttributes` AS `" + p.alias + "`"
			if !strings.Contains(sql, wantRaw) {
				t.Errorf("payload projection must stay raw (no identity key here): "+
					"want %q in %q", wantRaw, sql)
			}
			badWrapped := canonicalLabelsCol + " AS `" + p.alias + "`"
			if strings.Contains(sql, badWrapped) {
				t.Errorf("payload projection must not pay for a per-row sort: found %q in %q",
					badWrapped, sql)
			}
		})
	}
}

// captureMetadataSQL drives one metadata endpoint against a stubQuerier
// and returns the SQL the handler emitted. The stub's canned rows are
// irrelevant — only the emitted query is under test.
func captureMetadataSQL(t *testing.T, path string) string {
	t.Helper()
	q := &stubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status=%d", path, resp.StatusCode)
	}

	q.mu.Lock()
	sql := q.lastSQL
	q.mu.Unlock()
	if sql == "" {
		t.Fatalf("handler emitted no SQL for %s", path)
	}
	return sql
}
