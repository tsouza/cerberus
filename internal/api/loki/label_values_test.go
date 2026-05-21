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

// TestLabelValues_ServiceName_FallsBackToServiceNameColumn pins the
// task-#217 fix: `/loki/api/v1/label/service_name/values` must surface
// values stored under any of the three OTel-CH shapes — the dedicated
// `ServiceName` column (where the OTel collector → CH exporter routes
// `service.name` for cerberus-self rows), the underscored map key
// (`ResourceAttributes['service_name']`, used by the seed fixtures and
// Grafana panels), and the OTel-semantic-convention dotted key
// (`ResourceAttributes['service.name']`).
//
// Before the fix the endpoint queried `ResourceAttributes['service_name']`
// only, so cerberus's own logs were invisible on `service_name` while
// `/label/service.name/values` returned just `["cerberus"]` — the
// inverse set described in N8 / N9 of the task brief.
func TestLabelValues_ServiceName_FallsBackToServiceNameColumn(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{stringRows: []string{"cerberus", "api", "db", "frontend"}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/label/service_name/values`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	lastSQL := q.LastSQL()
	lastArgs := q.LastArgs()

	// The dedicated `ServiceName` column appears in the UNION ALL — the
	// arm that surfaces cerberus-self rows whose value is promoted out
	// of the map.
	if !strings.Contains(lastSQL, "`ServiceName`") {
		t.Errorf("SQL missing ServiceName column arm: %q", lastSQL)
	}
	// Both candidate map keys are bound as positional args — the
	// underscored form (used by seed fixtures) AND the dotted form
	// (OTel semantic convention). Without both, the inverse N8/N9
	// failure modes return.
	hasUnderscored, hasDotted := false, false
	for _, a := range lastArgs {
		if s, ok := a.(string); ok {
			if s == "service_name" {
				hasUnderscored = true
			}
			if s == "service.name" {
				hasDotted = true
			}
		}
	}
	if !hasUnderscored {
		t.Errorf("missing underscored map-key arg: %v", lastArgs)
	}
	if !hasDotted {
		t.Errorf("missing dotted map-key arg: %v", lastArgs)
	}
	// Label name never leaks as a SQL literal.
	if strings.Contains(lastSQL, "'service_name'") || strings.Contains(lastSQL, "'service.name'") {
		t.Errorf("label name leaked into SQL string: %q", lastSQL)
	}
	// UNION ALL across the storage-shape arms.
	if !strings.Contains(lastSQL, "UNION ALL") {
		t.Errorf("expected UNION ALL across storage shapes: %q", lastSQL)
	}
}

// TestLabelValues_DottedServiceName_MirrorsUnderscored pins the
// inverse direction: `/loki/api/v1/label/service.name/values` (the
// dotted-form endpoint) also surfaces the same UNION across the
// `ServiceName` column + the dotted/underscored map keys. Before the
// fix the dotted endpoint read `ResourceAttributes['service.name']`
// alone and returned `["cerberus"]` — the orthogonal slice to the
// underscored endpoint's `["api","db","frontend"]`. After the fix the
// two endpoints return the same set.
func TestLabelValues_DottedServiceName_MirrorsUnderscored(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{stringRows: []string{"cerberus", "api", "db"}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/label/service.name/values`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	lastSQL := q.LastSQL()
	if !strings.Contains(lastSQL, "`ServiceName`") {
		t.Errorf("dotted-name endpoint missing ServiceName column arm: %q", lastSQL)
	}
	if !strings.Contains(lastSQL, "UNION ALL") {
		t.Errorf("dotted-name endpoint missing UNION ALL: %q", lastSQL)
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
