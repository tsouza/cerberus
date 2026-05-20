package loki_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
)

// TestDetectedLabels_HappyPath drives /detected_labels with a stub label-
// set result and asserts the Grafana-shaped envelope:
//
//	{"detectedLabels": [{"label": "...", "cardinality": N}, ...]}
//
// — sorted by label, empty values dropped, distinct values counted.
func TestDetectedLabels_HappyPath(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		labelSets: []map[string]string{
			{"job": "api", "instance": "host-1", "env": "prod"},
			{"job": "api", "instance": "host-2", "env": "prod"},
			{"job": "worker", "instance": "host-3", "env": "prod"},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		`/loki/api/v1/detected_labels?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200`)
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

	// Expected per-key cardinalities given the canned rows:
	//   env=prod (1), instance=host-1/host-2/host-3 (3), job=api/worker (2)
	want := map[string]uint64{"env": 1, "instance": 3, "job": 2}
	if len(out.DetectedLabels) != len(want) {
		t.Fatalf("got %d labels, want %d: %+v", len(out.DetectedLabels), len(want), out.DetectedLabels)
	}
	// Sorted by label name.
	for i := 1; i < len(out.DetectedLabels); i++ {
		if out.DetectedLabels[i-1].Label > out.DetectedLabels[i].Label {
			t.Errorf("not sorted: %+v", out.DetectedLabels)
		}
	}
	for _, dl := range out.DetectedLabels {
		gotCard, ok := want[dl.Label]
		if !ok {
			t.Errorf("unexpected label %q", dl.Label)
			continue
		}
		if dl.Cardinality != gotCard {
			t.Errorf("label %q cardinality=%d want=%d", dl.Label, dl.Cardinality, gotCard)
		}
	}

	// SQL sanity: ResourceAttributes column + GROUP BY + time bounds.
	last := q.LastSQL()
	if !strings.Contains(last, "`ResourceAttributes` AS `labels`") {
		t.Errorf("missing labels projection in SQL: %q", last)
	}
	if !strings.Contains(last, "GROUP BY `labels`") {
		t.Errorf("missing GROUP BY in SQL: %q", last)
	}
	if !strings.Contains(last, "toDateTime64(") {
		t.Errorf("missing time bounds in SQL: %q", last)
	}
}

// TestDetectedLabels_NoQuery — empty selector means "all streams in the
// window"; the handler must not 400.
func TestDetectedLabels_NoQuery(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{labelSets: []map[string]string{{"job": "api"}}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/detected_labels?start=1717995600&end=1717999200`)
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
		t.Fatalf("unexpected payload: %+v", out.DetectedLabels)
	}
	if out.DetectedLabels[0].Cardinality != 1 {
		t.Fatalf("cardinality=%d want=1", out.DetectedLabels[0].Cardinality)
	}
}

// TestDetectedLabels_Empty — no rows returned → empty list, not nil. JSON
// encoders distinguish `[]` (the wire format Grafana expects) from `null`.
func TestDetectedLabels_Empty(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{labelSets: nil}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/detected_labels`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(string(raw), `"detectedLabels":[]`) {
		t.Fatalf("expected empty detectedLabels array, got %s", string(raw))
	}
}

// TestDetectedLabels_BadInput — broken query / bad time → 400 with the
// Loki JSON error envelope.
func TestDetectedLabels_BadInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
	}{
		{"bad query", `/loki/api/v1/detected_labels?query=%7Bnot+a+selector&start=1&end=2`},
		{"bad start", `/loki/api/v1/detected_labels?query=%7Bjob%3D%22api%22%7D&start=banana&end=2`},
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

// TestLokiUnknownEndpoint_JSON404 — unmatched /loki/api/* paths should
// return the JSON error envelope rather than Go's default text body.
// This is the second half of the Grafana-11.2 fix in this PR: the
// datasource probes feature endpoints, and on a miss the response body
// must still parse as JSON.
func TestLokiUnknownEndpoint_JSON404(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/this_route_does_not_exist`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want=404", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type=%q want application/json*", ct)
	}
	var env struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode 404 body: %v", err)
	}
	if env.Status != "error" {
		t.Errorf("status=%q want error", env.Status)
	}
	if env.Error == "" {
		t.Errorf("empty error field")
	}
}
