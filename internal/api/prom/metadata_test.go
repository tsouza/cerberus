package prom_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
)

// metadataResponse decodes the Prom metadata-endpoint shape — `data` is a
// direct slice rather than a {resultType, result} wrapper.
type metadataResponse struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
	Error  string          `json:"error"`
}

func TestLabels_Endpoint(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		strings: []string{"foo", "bar", "instance"},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/labels")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var parsed metadataResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	defer resp.Body.Close()

	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success; err=%s", parsed.Status, parsed.Error)
	}

	var names []string
	if err := json.Unmarshal(parsed.Data, &names); err != nil {
		t.Fatalf("decode data: %v", err)
	}

	// `__name__` is always prepended; result is sorted.
	if len(names) < 1 || names[0] != "__name__" {
		t.Fatalf("expected first name to be __name__, got %v", names)
	}
	wantContains := []string{"bar", "foo", "instance"}
	for _, w := range wantContains {
		found := false
		for _, n := range names {
			if n == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing %q in %v", w, names)
		}
	}

	if !strings.Contains(q.lastSQL, "mapKeys") {
		t.Errorf("expected SQL to use mapKeys; got %q", q.lastSQL)
	}
}

func TestLabels_MatchSelectorRejected(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/labels?" +
		"match%5B%5D=up%7Bjob%3D%22api%22%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for match[] selector, got %d", resp.StatusCode)
	}
}

func TestLabelValues_Endpoint(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		strings: []string{"api", "db", "cache"},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/label/job/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var parsed metadataResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	defer resp.Body.Close()

	var values []string
	if err := json.Unmarshal(parsed.Data, &values); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if len(values) != 3 {
		t.Fatalf("expected 3 values, got %d: %v", len(values), values)
	}
	// Sorted ascending.
	if values[0] != "api" || values[1] != "cache" || values[2] != "db" {
		t.Errorf("expected sorted values, got %v", values)
	}

	// The job label should bind through Attributes['job'].
	if !strings.Contains(q.lastSQL, "Attributes`[?]") {
		t.Errorf("expected SQL to reference Attributes map access; got %q", q.lastSQL)
	}
	// arg[0..N] should all be "job" (one per UNION segment + WHERE bind).
	for i, a := range q.lastArgs {
		if a != "job" {
			t.Errorf("arg[%d] = %v, want %q", i, a, "job")
		}
	}
}

func TestLabelValues_MetricNameLabel(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{strings: []string{"up", "http_requests_total"}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/label/__name__/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	// __name__ uses MetricName column, no Attributes mapAccess.
	if strings.Contains(q.lastSQL, "Attributes`[") {
		t.Errorf("__name__ should NOT use Attributes mapAccess; got %q", q.lastSQL)
	}
	if !strings.Contains(q.lastSQL, "MetricName") {
		t.Errorf("__name__ should query the MetricName column; got %q", q.lastSQL)
	}
}

func TestLabelValues_InvalidName(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	// Invalid char (slash would be eaten by routing, so use a leading digit).
	resp, err := http.Get(srv.URL + "/api/v1/label/1invalid/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid label name, got %d", resp.StatusCode)
	}
}

func TestSeries_Endpoint(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "up", Labels: map[string]string{"job": "api", "instance": "h1:8080"}},
			{MetricName: "up", Labels: map[string]string{"job": "api", "instance": "h2:8080"}},
			// Duplicate of the first row → should dedupe.
			{MetricName: "up", Labels: map[string]string{"job": "api", "instance": "h1:8080"}},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/series?match%5B%5D=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var parsed metadataResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	defer resp.Body.Close()

	var series []map[string]string
	if err := json.Unmarshal(parsed.Data, &series); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 deduped series, got %d: %+v", len(series), series)
	}
	for _, lset := range series {
		if lset["__name__"] != "up" {
			t.Errorf("expected __name__=up, got %+v", lset)
		}
	}
}

func TestSeries_RequiresMatch(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/series")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestMetadata_Endpoint(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		metaRows: []chclient.MetricMetaRow{
			{Name: "up", Description: "scrape ok", Unit: "", Type: "gauge"},
			{Name: "temperature", Description: "ambient temp", Unit: "celsius", Type: "gauge"},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/metadata")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var parsed metadataResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	defer resp.Body.Close()

	if parsed.Status != "success" {
		t.Fatalf("status: got %q", parsed.Status)
	}

	var grouped map[string][]prom.MetricMetaEntry
	if err := json.Unmarshal(parsed.Data, &grouped); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if _, ok := grouped["up"]; !ok {
		t.Errorf("expected 'up' metadata; got %+v", grouped)
	}
	if _, ok := grouped["temperature"]; !ok {
		t.Errorf("expected 'temperature' metadata; got %+v", grouped)
	}
}

func TestMetadata_FilterByName(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		metaRows: []chclient.MetricMetaRow{
			{Name: "up", Description: "scrape ok", Type: "gauge"},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/metadata?metric=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	defer resp.Body.Close()

	if len(q.lastArgs) == 0 || q.lastArgs[0] != "up" {
		t.Errorf("expected last query arg = 'up', got %v", q.lastArgs)
	}
}

func TestMetadata_LimitBadValue(t *testing.T) {
	t.Parallel()

	cases := []string{"-1", "abc"}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + "/api/v1/metadata?limit=" + raw)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("limit=%q: expected 400, got %d", raw, resp.StatusCode)
			}
		})
	}
}
