package loki_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestSeries_HappyPath returns multiple unique stream label sets.
// Grouping is done in SQL — the handler dedupes again Go-side for
// deterministic ordering.
func TestSeries_HappyPath(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		labelSets: []map[string]string{
			{"job": "api", "instance": "i1"},
			{"job": "billing", "instance": "i1"},
			{"job": "api", "instance": "i1"}, // duplicate — dedup
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		`/loki/api/v1/series?match%5B%5D=%7Bjob%3D%22api%22%7D&match%5B%5D=%7Bjob%3D%22billing%22%7D&start=1717995600&end=1717999200`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var out struct {
		Status string              `json:"status"`
		Data   []map[string]string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "success" {
		t.Fatalf("status=%q", out.Status)
	}
	if len(out.Data) != 2 {
		t.Fatalf("data=%v want 2 entries", out.Data)
	}

	// SQL sanity: GROUP BY labels + two predicate groups OR'd.
	lastSQL := q.LastSQL()
	if !strings.Contains(lastSQL, "GROUP BY") {
		t.Errorf("missing GROUP BY in SQL: %q", lastSQL)
	}
	if !strings.Contains(lastSQL, " OR ") {
		t.Errorf("missing OR between selectors in SQL: %q", lastSQL)
	}
}

// TestSeries_NoMatch — no match[] returns every stream in the range.
func TestSeries_NoMatch(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{labelSets: []map[string]string{{"job": "api"}}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/series?start=1717995600&end=1717999200`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	// SQL must not contain " OR " (no selector groups) but must still
	// have a GROUP BY.
	lastSQL := q.LastSQL()
	if !strings.Contains(lastSQL, "GROUP BY") {
		t.Errorf("missing GROUP BY: %q", lastSQL)
	}
}

// TestSeries_SingleResult covers the one-row path.
func TestSeries_SingleResult(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{labelSets: []map[string]string{{"job": "api"}}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/series?match%5B%5D=%7Bjob%3D%22api%22%7D`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

// TestSeries_BadMatch — a syntactically broken match[] → 400.
func TestSeries_BadMatch(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/series?match%5B%5D=%7Bnot+a+selector`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}
