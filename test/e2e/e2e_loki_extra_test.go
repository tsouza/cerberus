//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestLokiQuery_LineFilterContains — `{service_name="api"} |= "id="`
// returns the streams shape with the line filter applied. The seed
// writes log bodies of the form `... id=<n>` so every matching line
// contains the filter substring.
func TestLokiQuery_LineFilterContains(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	v := url.Values{}
	v.Set("query", `{service_name="api"} |= "id="`)
	resp := getJSON(ctx, t, "/loki/api/v1/query?"+v.Encode())
	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]string `json:"stream"`
				Values [][]string        `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	mustDecode(t, resp, &parsed)

	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success", parsed.Status)
	}
	if parsed.Data.ResultType != "streams" {
		t.Fatalf("resultType: got %q, want streams", parsed.Data.ResultType)
	}
	// Every line should contain "id=" (the line-filter literal).
	for _, s := range parsed.Data.Result {
		for _, v := range s.Values {
			if len(v) < 2 || !strings.Contains(v[1], "id=") {
				t.Errorf("line filter not applied: line %q doesn't contain `id=`", v[1])
			}
		}
	}
}

// TestLokiQuery_JSONParserStage — `{...} | json` parses + executes; the
// endpoint returns a 200 envelope with the documented success shape.
// Pins the regression: before the parser stage shipped, the same query
// was rejected with 422.
func TestLokiQuery_JSONParserStage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	v := url.Values{}
	v.Set("query", `{service_name="api"} | json`)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL()+"/loki/api/v1/query?"+v.Encode(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var env struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Status != "success" {
		t.Errorf("status: got %q, want success", env.Status)
	}
}

// TestLokiQuery_InvalidLogQL — syntactically broken LogQL returns 400.
func TestLokiQuery_InvalidLogQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL()+"/loki/api/v1/query?query=%7B", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
}
