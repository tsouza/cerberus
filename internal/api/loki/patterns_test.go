package loki_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
)

type patternsResponse struct {
	Status string            `json:"status"`
	Data   loki.PatternsData `json:"data"`
}

// TestPatterns_StubResult — until the pattern-discovery subsystem
// lands, /patterns returns success with an empty pattern list. Grafana
// renders this gracefully (the panel just shows "no data").
func TestPatterns_StubResult(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		`/loki/api/v1/patterns?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var out patternsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "success" {
		t.Fatalf("status=%q", out.Status)
	}
	if out.Data.Patterns == nil || len(out.Data.Patterns) != 0 {
		t.Fatalf("patterns=%+v want []", out.Data.Patterns)
	}
}

// TestPatterns_BadInput — missing/broken parameters still return 400
// so a misconfigured client gets a useful error rather than a silent
// "no data".
func TestPatterns_BadInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
	}{
		{"missing query", `/loki/api/v1/patterns?start=1&end=2`},
		{"bad query", `/loki/api/v1/patterns?query=%7Bnot+a+selector`},
		{"bad start", `/loki/api/v1/patterns?query=%7Bjob%3D%22api%22%7D&start=banana&end=2`},
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
