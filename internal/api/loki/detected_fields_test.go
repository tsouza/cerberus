package loki_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
)

type detectedFieldsResponse struct {
	Status string                  `json:"status"`
	Data   loki.DetectedFieldsData `json:"data"`
	Error  string                  `json:"error"`
}

// TestDetectedFields_JSON exercises the JSON detection branch. A row
// with `{"status":200,"path":"/x"}` should yield two fields with
// typed inference.
func TestDetectedFields_JSON(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{stringRows: []string{
		`{"status":200,"path":"/api","ok":true}`,
		`{"status":404,"path":"/api","ok":false}`,
	}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		`/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var out detectedFieldsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "success" {
		t.Fatalf("status=%q", out.Status)
	}
	byLabel := map[string]loki.DetectedField{}
	for _, f := range out.Data.Fields {
		byLabel[f.Label] = f
	}
	if got := byLabel["status"].Type; got != "int" {
		t.Errorf("status.type=%q want int", got)
	}
	if got := byLabel["ok"].Type; got != "boolean" {
		t.Errorf("ok.type=%q want boolean", got)
	}
	if got := byLabel["path"].Type; got != "string" {
		t.Errorf("path.type=%q want string", got)
	}
	if got := byLabel["status"].Cardinality; got != 2 {
		t.Errorf("status.cardinality=%d want 2", got)
	}

	// SQL sanity: ordering by Timestamp DESC + LIMIT.
	if !strings.Contains(q.lastSQL, "ORDER BY `Timestamp` DESC") {
		t.Errorf("missing ORDER BY DESC: %q", q.lastSQL)
	}
	if !strings.Contains(q.lastSQL, "LIMIT 1000") {
		t.Errorf("missing default LIMIT 1000: %q", q.lastSQL)
	}
}

// TestDetectedFields_Logfmt covers the logfmt branch: free-form
// key=value lines.
func TestDetectedFields_Logfmt(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{stringRows: []string{
		`level=info method=GET status=200 duration=12ms`,
		`level=error method=POST status=500 duration=1s`,
	}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		`/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out detectedFieldsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byLabel := map[string]loki.DetectedField{}
	for _, f := range out.Data.Fields {
		byLabel[f.Label] = f
	}
	if got := byLabel["duration"].Type; got != "duration" {
		t.Errorf("duration.type=%q want duration", got)
	}
	if got := byLabel["status"].Type; got != "int" {
		t.Errorf("status.type=%q want int", got)
	}
	if got := byLabel["level"].Type; got != "string" {
		t.Errorf("level.type=%q want string", got)
	}
}

// TestDetectedFields_Empty — no rows returned → empty fields list (not
// nil — Grafana renders an empty array gracefully but errors on null).
func TestDetectedFields_Empty(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{stringRows: nil}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		`/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out detectedFieldsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data.Fields) != 0 {
		t.Fatalf("fields=%+v want []", out.Data.Fields)
	}
}

// TestDetectedFields_BadInput — missing or broken parameters → 400.
func TestDetectedFields_BadInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
	}{
		{"missing query", `/loki/api/v1/detected_fields?start=1&end=2`},
		{"bad query", `/loki/api/v1/detected_fields?query=%7Bnot+a+selector`},
		{"bad line_limit", `/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D&line_limit=-1`},
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
