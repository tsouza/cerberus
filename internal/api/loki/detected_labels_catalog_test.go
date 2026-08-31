package loki_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// newCatalogServer builds a /detected_labels server with LabelCatalogEnabled
// set as requested — newServer (handler_test.go) has no knob for it, so this
// mirrors its body with the one extra field set (cerberus issue #2770).
func newCatalogServer(q loki.Querier, catalogEnabled bool) *httptest.Server {
	h := loki.New(q, schema.DefaultOTelLogs(), nil)
	h.LabelCatalogEnabled = catalogEnabled
	mux := http.NewServeMux()
	h.Mount(mux)
	return httptest.NewServer(mux)
}

// TestDetectedLabels_CatalogHit: LabelCatalogEnabled=true, no selector (the
// eligible shape) and a populated catalog response — the handler must serve
// straight from the catalog (uniqMerge query) and never touch the
// per-request GROUP BY path (labelSets stays unread; the recorded SQL is
// the catalog's, not buildDetectedLabelsSQL's).
func TestDetectedLabels_CatalogHit(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		labelCardinalities: []chclient.LabelCardinalityRow{
			{LabelKey: "job", Cardinality: 2},
			{LabelKey: "env", Cardinality: 1},
		},
		// A populated labelSets fallback that must NOT be what answers this
		// request — if it were, cardinalities would differ from the canned
		// catalog rows above.
		labelSets: []map[string]string{{"job": "api", "instance": "host-1", "env": "prod"}},
	}
	srv := newCatalogServer(q, true)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/detected_labels`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var out loki.DetectedLabelsData
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]uint64{"env": 1, "job": 2}
	if len(out.DetectedLabels) != len(want) {
		t.Fatalf("got %d labels, want %d: %+v", len(out.DetectedLabels), len(want), out.DetectedLabels)
	}
	for _, dl := range out.DetectedLabels {
		if got, ok := want[dl.Label]; !ok || dl.Cardinality != got {
			t.Errorf("label %+v not in expected catalog set %+v", dl, want)
		}
	}
	for i := 1; i < len(out.DetectedLabels); i++ {
		if out.DetectedLabels[i-1].Label > out.DetectedLabels[i].Label {
			t.Errorf("not sorted: %+v", out.DetectedLabels)
		}
	}

	last := q.LastSQL()
	if !strings.Contains(last, "uniqMerge(") {
		t.Errorf("expected the catalog uniqMerge query to run, got SQL: %q", last)
	}
	if strings.Contains(last, "mapSort(") {
		t.Errorf("catalog hit must not fall through to the per-request GROUP BY path, got SQL: %q", last)
	}
}

// TestDetectedLabels_CatalogIneligible_SelectorPresent: a request carrying a
// real stream selector is NEVER catalog-eligible, even with
// LabelCatalogEnabled=true and a populated catalog response — the catalog
// is unkeyed by stream (cerberus issue #2770's "keep it trivially
// conservative" rule), so it must fall straight through to the existing
// per-request path.
func TestDetectedLabels_CatalogIneligible_SelectorPresent(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		labelCardinalities: []chclient.LabelCardinalityRow{{LabelKey: "job", Cardinality: 99}},
		labelSets:          []map[string]string{{"job": "api"}},
	}
	srv := newCatalogServer(q, true)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/detected_labels?query=%7Bjob%3D%22api%22%7D`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var out loki.DetectedLabelsData
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.DetectedLabels) != 1 || out.DetectedLabels[0].Cardinality != 1 {
		t.Fatalf("expected the per-request fallback's cardinality=1 (from labelSets), got %+v — catalog's cardinality=99 must not have been served", out.DetectedLabels)
	}
	if strings.Contains(q.LastSQL(), "uniqMerge(") {
		t.Errorf("a selector-bearing request must never run the catalog query, got SQL: %q", q.LastSQL())
	}
}

// TestDetectedLabels_CatalogDisabled: LabelCatalogEnabled=false must never
// attempt the catalog query even when the request is otherwise eligible
// (no selector) and the stub has catalog rows ready — the feature being
// off must be byte-identical to before it existed.
func TestDetectedLabels_CatalogDisabled(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		labelCardinalities: []chclient.LabelCardinalityRow{{LabelKey: "job", Cardinality: 99}},
		labelSets:          []map[string]string{{"job": "api"}},
	}
	srv := newCatalogServer(q, false)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/detected_labels`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var out loki.DetectedLabelsData
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.DetectedLabels) != 1 || out.DetectedLabels[0].Cardinality != 1 {
		t.Fatalf("expected the per-request fallback, got %+v", out.DetectedLabels)
	}
	if strings.Contains(q.LastSQL(), "uniqMerge(") {
		t.Errorf("LabelCatalogEnabled=false must never run the catalog query, got SQL: %q", q.LastSQL())
	}
}

// TestDetectedLabels_CatalogMiss_FallsThrough covers the two ways a
// catalog-eligible attempt can come back empty-handed — a query error
// (table not provisioned yet / UNKNOWN_TABLE) and a genuinely empty result
// (created but never successfully refreshed) — asserting BOTH degrade to
// the per-request fallback rather than a 500 or an empty response.
func TestDetectedLabels_CatalogMiss_FallsThrough(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		rows    []chclient.LabelCardinalityRow
		rowsErr error
	}{
		{"query_error", nil, errCatalogUnavailable},
		{"empty_result", []chclient.LabelCardinalityRow{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{
				labelCardinalities:    tc.rows,
				labelCardinalitiesErr: tc.rowsErr,
				labelSets:             []map[string]string{{"job": "api"}},
			}
			srv := newCatalogServer(q, true)
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + `/loki/api/v1/detected_labels`)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", resp.StatusCode)
			}

			var out loki.DetectedLabelsData
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(out.DetectedLabels) != 1 || out.DetectedLabels[0].Label != "job" {
				t.Fatalf("expected the per-request fallback to have answered, got %+v", out.DetectedLabels)
			}
		})
	}
}

// errCatalogUnavailable is a canned error for TestDetectedLabels_CatalogMiss_FallsThrough's
// query_error case — any error works since detectedLabelsFromCatalog treats
// every catalog query failure the same way (fall through), not just
// UNKNOWN_TABLE.
var errCatalogUnavailable = &catalogTestError{"catalog table not provisioned"}

type catalogTestError struct{ msg string }

func (e *catalogTestError) Error() string { return e.msg }
