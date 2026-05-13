package loki_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestLabels_HappyPath drives /labels with a few canned strings and
// asserts the envelope shape Grafana expects (status=success, sorted
// deduplicated string list).
func TestLabels_HappyPath(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{stringRows: []string{"job", "instance", "job", "service.name"}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		`/loki/api/v1/labels?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200`)
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
	want := []string{"instance", "job", "service.name"}
	if len(out.Data) != len(want) {
		t.Fatalf("data=%v want=%v", out.Data, want)
	}
	for i := range want {
		if out.Data[i] != want[i] {
			t.Fatalf("data[%d]=%q want %q", i, out.Data[i], want[i])
		}
	}

	// SQL sanity: arrayJoin(mapKeys(...)) must be present.
	if !strings.Contains(q.lastSQL, "arrayJoin(mapKeys(`ResourceAttributes`))") {
		t.Errorf("missing arrayJoin(mapKeys()) in SQL: %q", q.lastSQL)
	}
	if !strings.Contains(q.lastSQL, "toDateTime64(") {
		t.Errorf("missing time bounds in SQL: %q", q.lastSQL)
	}
}

// TestLabels_NoQuery — /labels with no selector should still succeed —
// the contract is "label keys in the time range". The handler must not
// produce a WHERE clause referring to a nil predicate.
func TestLabels_NoQuery(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{stringRows: []string{"job"}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/labels?start=1717995600&end=1717999200`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

// TestLabels_Empty — empty CH result returns an empty (non-nil) list.
func TestLabels_Empty(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{stringRows: nil}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/labels`)
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
	if len(out.Data) != 0 {
		t.Fatalf("data=%v want []", out.Data)
	}
}

// TestLabels_BadInput — broken query / bad time → 400.
func TestLabels_BadInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
	}{
		{"bad query", `/loki/api/v1/labels?query=%7Bnot+a+selector&start=1&end=2`},
		{"bad start", `/loki/api/v1/labels?query=%7Bjob%3D%22api%22%7D&start=banana&end=2`},
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
