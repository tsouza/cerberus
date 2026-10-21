//go:build chdb

// chDB-backed coverage of Loki statements over a JSON-typed
// ResourceAttributes column that reach ClickHouse outside the metadata
// builders attr_strategy_fullmap_chdb_test.go covers: the /tail poll
// (buildTailSQL) and the /query_range Lang (langForRangeRequest). A Map
// subscript over a JSON column is rejected by ClickHouse
// (ILLEGAL_TYPE_OF_ARGUMENT); the tail logs that failure and closes the
// socket having sent nothing, so its delivered lines and labels are
// asserted, not only that it connected.
//
// Lives in package loki (not loki_test) to reuse tail_overflow_chdb_test.go's
// dialTailFrom and the unexported tailPollInterval, which it mutates — so,
// like that file, it must NOT t.Parallel().

package loki

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// jsonStrategiesLogsTable is the JSON-typed logs table this file seeds.
const jsonStrategiesLogsTable = "strategies_json_logs"

// newJSONStrategiesLogsServer seeds jsonStrategiesLogsTable with two api
// lines and one web line at base, base+1ms, base+2ms, and serves a handler
// whose cell resolves ResourceAttributes to JSON.
func newJSONStrategiesLogsServer(t *testing.T, base time.Time) *httptest.Server {
	t.Helper()
	const dt64Layout = "2006-01-02 15:04:05.000000000"
	ts := func(i int) string { return base.Add(time.Duration(i) * time.Millisecond).Format(dt64Layout) }
	c := chclienttest.NewChDB(t)
	c.Seed(t, fmt.Sprintf(`CREATE TABLE %[1]s (
    Timestamp DateTime64(9),
    Body String,
    SeverityText String,
    SeverityNumber UInt8,
    LogAttributes Map(String, String),
    ResourceAttributes JSON
) ENGINE = Memory;
INSERT INTO %[1]s (Timestamp, Body, LogAttributes, ResourceAttributes) VALUES
    (toDateTime64('%[2]s', 9), 'api-1', map(), '{"job":"api","service.name":"svc-a"}'),
    (toDateTime64('%[3]s', 9), 'web-1', map(), '{"job":"web","service.name":"svc-b"}'),
    (toDateTime64('%[4]s', 9), 'api-2', map(), '{"job":"api","service.name":"svc-a"}');`,
		jsonStrategiesLogsTable, ts(0), ts(1), ts(2)))
	s := schema.DefaultOTelLogs()
	s.LogsTable = jsonStrategiesLogsTable
	h := New(c, s, nil)
	h.Strategies = chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{Logs: chsql.SchemaStrategies{
		Attrs: chsql.AttrStrategies{s.ResourceAttributesColumn: chsql.AttrStrategyJSON},
	}})
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestTail_ChDB_JSONResourceAttributes pins that /tail matches its stream
// selector against a JSON-typed ResourceAttributes and delivers each
// matching line under the stream labels read from it.
func TestTail_ChDB_JSONResourceAttributes(t *testing.T) {
	prev := tailPollInterval
	tailPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { tailPollInterval = prev })

	base := time.Now().UTC().Add(-time.Second)
	srv := newJSONStrategiesLogsServer(t, base)
	conn := dialTailFrom(t, srv, `{job="api"}`, 10, base)

	want := []string{"api-1", "api-2"}
	var got []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(got) < len(want) {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, payload, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var chunk struct {
			Streams []struct {
				Stream map[string]string `json:"stream"`
				Values [][]string        `json:"values"`
			} `json:"streams"`
		}
		if err := json.Unmarshal(payload, &chunk); err != nil {
			t.Fatalf("unmarshal: %v (payload=%s)", err, payload)
		}
		for _, st := range chunk.Streams {
			if st.Stream["job"] != "api" || st.Stream["service_name"] != "svc-a" {
				t.Errorf("stream labels = %v, want job=api service_name=svc-a", st.Stream)
			}
			for _, v := range st.Values {
				got = append(got, v[1])
			}
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("tail delivered %v, want %v", got, want)
	}
}

// TestQueryRange_ChDB_JSONResourceAttributes pins that /query_range — the
// Lang langForRangeRequest builds — matches a stream selector against a
// JSON-typed ResourceAttributes and labels each line from it.
func TestQueryRange_ChDB_JSONResourceAttributes(t *testing.T) {
	base := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	srv := newJSONStrategiesLogsServer(t, base)
	resp, err := http.Get(srv.URL + "/loki/api/v1/query_range?" + url.Values{
		"query":     {`{job="api"}`},
		"start":     {strconv.FormatInt(base.Add(-time.Minute).UnixNano(), 10)},
		"end":       {strconv.FormatInt(base.Add(time.Minute).UnixNano(), 10)},
		"direction": {"forward"},
	}.Encode())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			Result []struct {
				Stream map[string]string `json:"stream"`
				Values [][]string        `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var got []string
	for _, r := range out.Data.Result {
		if r.Stream["job"] != "api" || r.Stream["service_name"] != "svc-a" {
			t.Errorf("stream labels = %v, want job=api service_name=svc-a", r.Stream)
		}
		for _, v := range r.Values {
			got = append(got, v[1])
		}
	}
	if want := []string{"api-1", "api-2"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("query_range lines = %v, want %v", got, want)
	}
}
