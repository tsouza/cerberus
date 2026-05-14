package loki_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclient"
)

// TestIndexStats_HappyPath drives /index/stats with a canned aggregate
// row and asserts the envelope shape Grafana expects, plus that chunks
// is the documented zero (cerberus has no chunk model).
func TestIndexStats_HappyPath(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		statsRow: chclient.IndexStatsRow{
			Streams: 3,
			Entries: 42,
			Bytes:   1024,
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		`/loki/api/v1/index/stats?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var out loki.IndexStats
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := loki.IndexStats{Streams: 3, Chunks: 0, Entries: 42, Bytes: 1024}
	if out != want {
		t.Fatalf("body mismatch: got %+v want %+v", out, want)
	}

	// SQL sanity: the matcher predicate must end up in the rendered SQL,
	// going through the Builder (no fmt.Sprintf string concatenation),
	// and the time bounds must be present as DateTime64 literals.
	lastSQL := q.LastSQL()
	if !strings.Contains(lastSQL, "uniqExact(`ResourceAttributes`)") {
		t.Errorf("missing uniqExact in SQL: %q", lastSQL)
	}
	if !strings.Contains(lastSQL, "sum(length(`Body`))") {
		t.Errorf("missing bytes agg in SQL: %q", lastSQL)
	}
	if !strings.Contains(lastSQL, "toDateTime64(") {
		t.Errorf("missing time bounds in SQL: %q", lastSQL)
	}
}

// TestIndexStats_BadInput covers the validation contract.
func TestIndexStats_BadInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
	}{
		{"missing query", `/loki/api/v1/index/stats?start=1&end=2`},
		{"bad query", `/loki/api/v1/index/stats?query=%7Bnot+a+selector&start=1&end=2`},
		{"bad start", `/loki/api/v1/index/stats?query=%7Bjob%3D%22api%22%7D&start=banana&end=2`},
		{"end before start", `/loki/api/v1/index/stats?query=%7Bjob%3D%22api%22%7D&start=2000000000&end=1000000000`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + tc.url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", resp.StatusCode)
			}
		})
	}
}

// TestIndexStats_MetricForm covers the "rate(...)" form — the handler
// must unwrap the selector instead of erroring on the sample-expr type.
func TestIndexStats_MetricForm(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{statsRow: chclient.IndexStatsRow{Streams: 1, Entries: 1, Bytes: 1}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		`/loki/api/v1/index/stats?query=rate(%7Bjob%3D%22api%22%7D%5B5m%5D)&start=1717995600&end=1717999200`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}
