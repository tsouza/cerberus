package prom_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
)

// TestLabelValues_MatchSelector exercises the `fetchLabelValuesMatched`
// + `labelValuesForMatcher` path under `/api/v1/label/<name>/values?match[]=`.
// Grafana's label-selector dropdown drives this surface — the audit in
// #375 flagged it at 0% coverage.
//
// The stub returns a fixed slice of strings; the assertions cover the
// shape of the SQL the handler emits (DISTINCT projection + Attributes
// map access + matcher-subquery wrap) and the JSON envelope returned to
// the client.
func TestLabelValues_MatchSelector(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		strings: []string{"api", "db", ""},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/label/job/values?" +
		"match%5B%5D=up%7Binstance%3D%22h1%3A8080%22%7D")
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
		t.Fatalf("status: got %q, want success; err=%s",
			parsed.Status, parsed.Error)
	}

	var values []string
	if err := json.Unmarshal(parsed.Data, &values); err != nil {
		t.Fatalf("decode data: %v", err)
	}

	// fetchLabelValuesMatched filters the empty-string sentinel itself
	// (the stub returns it; the handler must drop it) and sorts the
	// remaining values. So two entries, sorted.
	wantValues := []string{"api", "db"}
	if len(values) != len(wantValues) {
		t.Fatalf("expected %d values, got %d: %v",
			len(wantValues), len(values), values)
	}
	for i, w := range wantValues {
		if values[i] != w {
			t.Errorf("values[%d]: got %q, want %q", i, values[i], w)
		}
	}

	// The matcher path wraps the matched scan in a subquery and
	// projects DISTINCT Attributes[?] over it.
	if !strings.Contains(q.lastSQL, "DISTINCT") {
		t.Errorf("expected DISTINCT projection in SQL; got %q",
			q.lastSQL)
	}
	if !strings.Contains(q.lastSQL, "Attributes`[?]") {
		t.Errorf("expected Attributes map access in SQL; got %q",
			q.lastSQL)
	}
}

// TestLabelValues_MatchSelector_MetricName routes through
// `labelValuesForMatcher`'s `__name__` branch — distinct MetricName
// projection over the matcher subquery rather than Attributes[?].
func TestLabelValues_MatchSelector_MetricName(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		strings: []string{"up", "http_requests_total"},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/label/__name__/values?" +
		"match%5B%5D=%7Bjob%3D%22api%22%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	defer resp.Body.Close()

	// __name__ branch uses the MetricName column, NOT Attributes mapAccess.
	if strings.Contains(q.lastSQL, "Attributes`[") {
		t.Errorf("__name__ matcher branch should not access Attributes "+
			"in the outer projection; got %q", q.lastSQL)
	}
	if !strings.Contains(q.lastSQL, "MetricName") {
		t.Errorf("__name__ matcher branch should reference MetricName; "+
			"got %q", q.lastSQL)
	}
	if !strings.Contains(q.lastSQL, "DISTINCT") {
		t.Errorf("expected DISTINCT projection in SQL; got %q",
			q.lastSQL)
	}
}

// TestLabelValues_MatchSelector_Multiple exercises the AND-across-matchers
// path: each `match[]=` selector runs its own query, the union of values
// is returned. The stub's single response is returned for every call, so
// the dedup logic inside fetchLabelValuesMatched is what produces a
// single sorted slice.
func TestLabelValues_MatchSelector_Multiple(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		// Both selectors return overlapping values; dedup yields {api, db}.
		strings: []string{"api", "db"},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/label/job/values?" +
		"match%5B%5D=up&match%5B%5D=down")
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
	wantValues := []string{"api", "db"}
	if len(values) != len(wantValues) {
		t.Fatalf("expected dedup to yield %d values, got %d: %v",
			len(wantValues), len(values), values)
	}
	for i, w := range wantValues {
		if values[i] != w {
			t.Errorf("values[%d]: got %q, want %q", i, values[i], w)
		}
	}
}

// TestLabelValues_MatchSelector_Regex pins the regex matcher
// `{job=~".+"}` against the labelValuesForMatcher path. The matcher
// must lower without an explicit `__name__=`, which falls through to
// the default gauge table per lowerVectorSelector.
func TestLabelValues_MatchSelector_Regex(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		strings: []string{"api"},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/label/job/values?" +
		"match%5B%5D=%7Bjob%3D~%22.%2B%22%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	defer resp.Body.Close()

	if !strings.Contains(q.lastSQL, "DISTINCT") {
		t.Errorf("expected DISTINCT projection in SQL; got %q",
			q.lastSQL)
	}
}

// TestLabelValues_MatchSelector_Empty pins the empty-result path
// through fetchLabelValuesMatched: when every matcher returns an empty
// slice, the handler emits `data: []` (not null) per the Prom wire-
// format contract.
func TestLabelValues_MatchSelector_Empty(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{strings: nil}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/label/job/values?" +
		"match%5B%5D=up%7Bjob%3D%22nonexistent%22%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)

	// data MUST be `[]`, not `null` — Grafana rejects the latter.
	if !strings.Contains(body, `"data":[]`) {
		t.Errorf("expected `data:[]` in empty response; got %s", body)
	}
}

// TestLabelValues_MatchSelector_DropsEmpty pins the empty-string
// sentinel drop inside fetchLabelValuesMatched. ClickHouse's mapAccess
// returns "" for absent keys; the handler must filter those out before
// emitting the values slice.
func TestLabelValues_MatchSelector_DropsEmpty(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		// Only the empty-string sentinel — fetchLabelValuesMatched
		// drops it and we end up with `data: []`.
		strings: []string{""},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/label/job/values?" +
		"match%5B%5D=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"data":[]`) {
		t.Errorf("expected `data:[]` after empty-string drop; got %s",
			body)
	}
}

// TestLabelValues_MatchSelector_BadMatcher exercises the matcherSQL
// error path: an invalid PromQL matcher should surface as 400 / bad_data
// rather than 500 / internal.
func TestLabelValues_MatchSelector_BadMatcher(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/label/job/values?" +
		"match%5B%5D=*broken")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s",
			resp.StatusCode, readBody(t, resp))
	}
}

// TestLabelValues_MatchSelector_UpstreamError exercises the CH-error
// path: the stub injects an error so labelValuesForMatcher's QueryStrings
// call returns it, which fetchLabelValuesMatched propagates, which
// handleLabelValues surfaces as 502.
func TestLabelValues_MatchSelector_UpstreamError(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: errors.New("ch: connection refused")}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/label/job/values?" +
		"match%5B%5D=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s",
			resp.StatusCode, readBody(t, resp))
	}
}

// TestMetadata_TruncateAtLimit pins `truncateMetadata`'s alphabetic-
// truncation behaviour: when the row count exceeds `limit`, the handler
// keeps the first N keys in sorted order. Five rows seeded, limit=2 →
// the first two keys alphabetically (alpha, beta) survive.
func TestMetadata_TruncateAtLimit(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		metaRows: []chclient.MetricMetaRow{
			{Name: "epsilon", Description: "e", Type: "gauge"},
			{Name: "delta", Description: "d", Type: "gauge"},
			{Name: "alpha", Description: "a", Type: "gauge"},
			{Name: "gamma", Description: "g", Type: "gauge"},
			{Name: "beta", Description: "b", Type: "gauge"},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/metadata?limit=2")
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

	var grouped map[string][]prom.MetricMetaEntry
	if err := json.Unmarshal(parsed.Data, &grouped); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if len(grouped) != 2 {
		t.Fatalf("expected 2 entries after truncate, got %d: %+v",
			len(grouped), grouped)
	}
	// Sorted-keys truncation: alpha, beta survive; delta/epsilon/gamma drop.
	for _, want := range []string{"alpha", "beta"} {
		if _, ok := grouped[want]; !ok {
			t.Errorf("expected %q to survive truncate, got keys: %v",
				want, mapKeys(grouped))
		}
	}
	for _, drop := range []string{"delta", "epsilon", "gamma"} {
		if _, ok := grouped[drop]; ok {
			t.Errorf("expected %q to be truncated out; got keys: %v",
				drop, mapKeys(grouped))
		}
	}
}

// TestMetadata_LimitZero pins limit=0: truncateMetadata returns the
// input unchanged (the early-return guard `limit <= 0 || len(in) <= limit`).
// Prom's convention is that limit=0 means "no limit"; we mirror that.
func TestMetadata_LimitZero(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		metaRows: []chclient.MetricMetaRow{
			{Name: "up", Description: "scrape ok", Type: "gauge"},
			{Name: "temperature", Description: "ambient", Type: "gauge"},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/metadata?limit=0")
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

	var grouped map[string][]prom.MetricMetaEntry
	if err := json.Unmarshal(parsed.Data, &grouped); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if len(grouped) != 2 {
		t.Fatalf("limit=0 should not truncate; got %d entries: %+v",
			len(grouped), grouped)
	}
}

// TestMetadata_LimitAboveCount pins the second early-return guard:
// when `limit` exceeds the number of rows, truncateMetadata returns
// the input untouched.
func TestMetadata_LimitAboveCount(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		metaRows: []chclient.MetricMetaRow{
			{Name: "up", Description: "scrape ok", Type: "gauge"},
			{Name: "temperature", Description: "ambient", Type: "gauge"},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/metadata?limit=100")
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

	var grouped map[string][]prom.MetricMetaEntry
	if err := json.Unmarshal(parsed.Data, &grouped); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if len(grouped) != 2 {
		t.Fatalf("limit > count should leave the map untouched; "+
			"got %d entries: %+v", len(grouped), grouped)
	}
}

// mapKeys returns the keys of a map in arbitrary order; used by the
// truncate test to render diagnostic output when an unexpected key
// survives or drops.
func mapKeys(m map[string][]prom.MetricMetaEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
