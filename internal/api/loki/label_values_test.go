package loki_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestLabelValues_HappyPath drives /label/{name}/values returning the
// expected (sorted, deduped) value list. The bound label name must
// appear in the args slice (parameterised), not in the SQL string.
func TestLabelValues_HappyPath(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{stringRows: []string{"api", "billing", "api", ""}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		`/loki/api/v1/label/job/values?start=1717995600&end=1717999200`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var out struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "success" {
		t.Fatalf("status=%q", out.Status)
	}
	want := []string{"api", "billing"}
	if len(out.Data) != len(want) || out.Data[0] != "api" || out.Data[1] != "billing" {
		t.Fatalf("data=%v want=%v", out.Data, want)
	}

	// SQL sanity: the map-access form is present and uses positional
	// `?` for the label name (never spliced into the SQL).
	lastSQL := q.LastSQL()
	lastArgs := q.LastArgs()
	if !strings.Contains(lastSQL, "`ResourceAttributes`[?]") {
		t.Errorf("missing map-access in SQL: %q", lastSQL)
	}
	if strings.Contains(lastSQL, "'job'") {
		t.Errorf("label name leaked into SQL: %q", lastSQL)
	}
	// And it must show up as an arg.
	foundJob := false
	for _, a := range lastArgs {
		if s, ok := a.(string); ok && s == "job" {
			foundJob = true
		}
	}
	if !foundJob {
		t.Errorf("label name not in args: %v", lastArgs)
	}
}

// TestLabelValues_Selector applies an additional stream-selector filter.
// The SQL must contain the matcher predicate alongside the map-access
// projection.
func TestLabelValues_Selector(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{stringRows: []string{"v"}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		`/loki/api/v1/label/service.name/values?query=%7Bjob%3D%22api%22%7D`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	// Selector binds `job` as a key + `api` as a value.
	lastArgs := q.LastArgs()
	hasJob, hasAPI := false, false
	for _, a := range lastArgs {
		if s, ok := a.(string); ok {
			if s == "job" {
				hasJob = true
			}
			if s == "api" {
				hasAPI = true
			}
		}
	}
	if !hasJob || !hasAPI {
		t.Errorf("missing selector args (job=%v, api=%v): %v", hasJob, hasAPI, lastArgs)
	}
}

// TestLabelValues_BadInput — broken selector or empty label name → 400.
func TestLabelValues_BadInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
		want int
	}{
		{"bad query", `/loki/api/v1/label/job/values?query=%7Bnot+a+selector`, http.StatusBadRequest},
		// Empty {name} segment never matches a Go-mux pattern -> 404.
		{"empty name", `/loki/api/v1/label//values`, http.StatusNotFound},
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
			if resp.StatusCode != tc.want {
				t.Fatalf("expected %d, got %d", tc.want, resp.StatusCode)
			}
		})
	}
}
