package loki_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclient"
)

// TestIndexVolume_HappyPath drives /index/volume with two canned rows
// and asserts the Prometheus-vector envelope shape.
func TestIndexVolume_HappyPath(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		volumeRows: []chclient.IndexVolumeRow{
			{Labels: map[string]string{"job": "api", "env": "prod"}, Bytes: 2048},
			{Labels: map[string]string{"job": "api", "env": "stg"}, Bytes: 512},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		`/loki/api/v1/index/volume?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var parsed struct {
		Status string         `json:"status"`
		Data   loki.QueryData `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed.Status != "success" {
		t.Fatalf("status=%q", parsed.Status)
	}
	if parsed.Data.ResultType != "vector" {
		t.Fatalf("resultType=%q", parsed.Data.ResultType)
	}
	raw, _ := json.Marshal(parsed.Data.Result)
	var samples []loki.VectorSample
	if err := json.Unmarshal(raw, &samples); err != nil {
		t.Fatalf("decode vector: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("expected 2 vector samples, got %d", len(samples))
	}

	// SQL sanity: GROUP BY + ORDER BY bytes DESC + LIMIT must all be
	// present; bytes column aggregates via length(Body); labels grouping
	// uses the full ResourceAttributes map when targetLabels is absent.
	lastSQL := q.LastSQL()
	if !strings.Contains(lastSQL, "GROUP BY") {
		t.Errorf("missing GROUP BY: %q", lastSQL)
	}
	if !strings.Contains(lastSQL, "ORDER BY `bytes` DESC") {
		t.Errorf("missing ORDER BY bytes DESC: %q", lastSQL)
	}
	if !strings.Contains(lastSQL, "LIMIT 100") {
		t.Errorf("default limit absent: %q", lastSQL)
	}
	if !strings.Contains(lastSQL, "mapSort(`ResourceAttributes`) AS `labels`") {
		t.Errorf("default group key should be the canonicalised full RA map: %q", lastSQL)
	}
}

// TestIndexVolume_TargetLabels confirms the projected group key for the
// `targetLabels` query parameter: an explicit map literal, one entry per
// requested label, each value resolved through the SAME rule the
// selector matchers resolve under, with absent labels filtered out.
func TestIndexVolume_TargetLabels(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		`/loki/api/v1/index/volume?query=%7Bjob%3D%22api%22%7D&targetLabels=job,env&aggregateBy=labels`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	lastSQL := q.LastSQL()
	lastArgs := q.LastArgs()
	if !strings.Contains(lastSQL, "mapFilter((k, v) -> v != ?, map(") {
		t.Errorf("missing absent-label filter over the projected map literal: %q", lastSQL)
	}
	// Args carry the target-label keys (sorted): env, job, plus the
	// original "job" matcher value pair (job, api) ahead of those. The
	// exact positions track the SQL stream — we just confirm the keys
	// landed in the slice.
	want := map[string]bool{"job": false, "env": false, "api": false}
	for _, a := range lastArgs {
		if s, ok := a.(string); ok {
			if _, present := want[s]; present {
				want[s] = true
			}
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("expected arg %q not bound; args=%v", k, lastArgs)
		}
	}
}

// TestIndexVolume_Limit covers a custom limit + parse error.
func TestIndexVolume_Limit(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	// happy path: explicit limit
	resp, err := http.Get(srv.URL +
		`/loki/api/v1/index/volume?query=%7Bjob%3D%22api%22%7D&limit=25`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	lastSQL := q.LastSQL()
	if !strings.Contains(lastSQL, "LIMIT 25") {
		t.Errorf("expected LIMIT 25 in SQL: %q", lastSQL)
	}

	// bad input
	resp, err = http.Get(srv.URL +
		`/loki/api/v1/index/volume?query=%7Bjob%3D%22api%22%7D&limit=-3`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad limit, got %d", resp.StatusCode)
	}
}

// TestIndexVolume_BadInput covers the validation contract.
func TestIndexVolume_BadInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
	}{
		{"missing query", `/loki/api/v1/index/volume?start=1&end=2`},
		{"bad query", `/loki/api/v1/index/volume?query=not+a+selector`},
		{"bad start", `/loki/api/v1/index/volume?query=%7Bjob%3D%22api%22%7D&start=banana&end=2`},
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

// TestIndexVolume_TargetLabelsResolveLikeTheSelector is the SQL-level
// statement of the wrong-answer bug the chDB sibling
// (TestIndexVolume_ChDB_TargetLabelsResolvesHoistedColumn) demonstrates
// end to end. `service_name` is the sharp case: the selector resolves it
// through the dedicated `ServiceName` column coalesced with the map
// (logql.matcherLHS), because the OTel-CH exporter hoists `service.name`
// out of ResourceAttributes. A projection that reads the literal
// `service_name` map key instead sees nothing on exactly the rows the
// selector matched.
func TestIndexVolume_TargetLabelsResolveLikeTheSelector(t *testing.T) {
	t.Parallel()

	get := func(t *testing.T, q *stubQuerier, srv, query string) string {
		t.Helper()
		resp, err := http.Get(srv + query)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d", resp.StatusCode)
		}
		return q.LastSQL()
	}

	q := &stubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	// (1) Selector side: what does `service_name` resolve to when the
	// request FILTERS on it? Everything between "WHERE (" and " = ?)" is
	// the matcher's left-hand side.
	selectorSQL := get(t, q, srv.URL,
		`/loki/api/v1/index/volume?query=%7Bservice_name%3D%22api%22%7D`)
	const wherePrefix = "WHERE ("
	i := strings.Index(selectorSQL, wherePrefix)
	if i < 0 {
		t.Fatalf("no WHERE clause to read the matcher LHS from: %q", selectorSQL)
	}
	lhs := selectorSQL[i+len(wherePrefix):]
	j := strings.Index(lhs, " = ?)")
	if j < 0 {
		t.Fatalf("could not delimit the matcher LHS in %q", selectorSQL)
	}
	lhs = lhs[:j]
	if !strings.Contains(lhs, "ServiceName") {
		t.Fatalf("premise broken: the selector no longer resolves service_name through "+
			"the ServiceName column; LHS = %q", lhs)
	}

	// (2) Projection side: the same label, now as a targetLabels
	// projection, must resolve to the SAME expression. Projecting the
	// literal `service_name` map key instead returns the empty map on
	// exactly the rows the selector matched, and the whole tenant's
	// volume collapses into one unlabelled sample.
	projectedSQL := get(t, q, srv.URL,
		`/loki/api/v1/index/volume?query=%7Bjob%3D%22api%22%7D&targetLabels=service_name&aggregateBy=labels`)
	if !strings.Contains(projectedSQL, lhs) {
		t.Errorf("projected `service_name` does not resolve as the selector does.\n"+
			"selector LHS: %s\nprojection:   %s", lhs, projectedSQL)
	}
}
