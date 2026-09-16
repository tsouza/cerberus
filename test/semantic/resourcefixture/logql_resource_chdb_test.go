//go:build chdb

// LogQL leg of SIGNAL-RESOURCE-001 (cerberus issue #3457): seeds Fixture
// into a custom-named otel_logs table and proves the stream selector both
// selects on and exposes the simple key (team) and the dotted key
// (k8s.namespace.name) — normalized to the SAME underscored wire label
// PromQL uses — and that the returned log-entry timestamp preserves the
// row's full seeded nanosecond value, LogQL's own native wire precision.
package resourcefixture

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

const logqlFixtureBody = "checkout request handled"

// logqlFixtureDDL declares logqlFixtureLogsTable with exactly the columns
// the LogQL stream-selector read path projects. ResourceAttributes carries
// both fixture keys — the resource/stream-identity scope, as opposed to
// the per-line LogAttributes map this fixture leaves empty, since
// SIGNAL-RESOURCE-001 covers resource-attribute projection specifically
// (LogQL's log-attribute scope is a distinct, already-covered surface).
// SeverityText is unused by this fixture but must exist regardless of
// which columns the query touches: internal/logql/lang.go's
// ProjectSamples wrap always selects it (it feeds the response's
// derived detected_level field).
func logqlFixtureDDL() string {
	return fmt.Sprintf(`CREATE TABLE %s (
    Timestamp DateTime64(9),
    Body String,
    SeverityText LowCardinality(String) DEFAULT '',
    LogAttributes Map(String, String),
    ResourceAttributes Map(String, String)
) ENGINE = Memory;

INSERT INTO %s (Timestamp, Body, LogAttributes, ResourceAttributes) VALUES
    (toDateTime64('%s', 9), '%s', map(), map('%s', '%s', '%s', '%s'));
`, logqlFixtureLogsTable, logqlFixtureLogsTable,
		Fixture.Timestamp.UTC().Format(TSFormat), logqlFixtureBody,
		TeamKey, Fixture.Team, NamespaceKey, Fixture.Namespace)
}

type logqlFixtureStreamsResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string        `json:"resultType"`
		Result     []loki.Stream `json:"result"`
	} `json:"data"`
}

// TestSignalResource_LogQL_ChDB is SIGNAL-RESOURCE-001's LogQL evidence.
func TestSignalResource_LogQL_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	c.Seed(t, logqlFixtureDDL())

	s := schema.DefaultOTelLogs()
	s.LogsTable = logqlFixtureLogsTable
	// This fixture's custom-named table does not carry the upstream OTel
	// Collector exporter's dedicated __otel_materialized_* columns
	// DefaultOTelLogs() assumes (a real renamed/custom deployment would
	// not necessarily have them either) — clearing the map keeps the
	// dotted key on the generic ResourceAttributes-map read path this
	// fixture means to exercise, mirroring schema.Logs.MaterializedResourceColumns's
	// own documented opt-out.
	s.MaterializedResourceColumns = nil

	h := loki.New(c, s, nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	start := Fixture.Timestamp.Add(-time.Minute).UnixNano()
	end := Fixture.Timestamp.Add(time.Minute).UnixNano()
	query := fmt.Sprintf(`{team=%q,%s=%q}`, Fixture.Team, NamespaceWireLabel, Fixture.Namespace)
	u := fmt.Sprintf("%s/loki/api/v1/query_range?query=%s&start=%d&end=%d", srv.URL, url.QueryEscape(query), start, end)

	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	var parsed logqlFixtureStreamsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StatusCode != http.StatusOK || parsed.Status != "success" {
		t.Fatalf("query %q: status=%d api_status=%q err=%s", query, resp.StatusCode, parsed.Status, parsed.Error)
	}
	if len(parsed.Data.Result) != 1 {
		t.Fatalf("stream selector matched %d streams, want 1: %+v", len(parsed.Data.Result), parsed.Data.Result)
	}
	stream := parsed.Data.Result[0]

	if got := stream.Stream["team"]; got != Fixture.Team {
		t.Errorf(`stream label "team" = %q, want %q (simple shared key, no normalization)`, got, Fixture.Team)
	}
	if got := stream.Stream[NamespaceWireLabel]; got != Fixture.Namespace {
		t.Errorf("stream label %q = %q, want %q (dotted key, LogQL-normalized wire label — same normalizer PromQL uses)", NamespaceWireLabel, got, Fixture.Namespace)
	}
	if _, present := stream.Stream[NamespaceKey]; present {
		t.Errorf("stream carries the RAW dotted key %q on the wire; LogQL must expose only the sanitized form %q", NamespaceKey, NamespaceWireLabel)
	}

	if len(stream.Values) != 1 {
		t.Fatalf("stream has %d values, want 1: %+v", len(stream.Values), stream.Values)
	}
	wantNanos := strconv.FormatInt(Fixture.Timestamp.UnixNano(), 10)
	if got := stream.Values[0].Timestamp; got != wantNanos {
		t.Errorf("entry timestamp = %q, want %q (LogQL's native full-nanosecond wire precision, not rounded to milliseconds)", got, wantNanos)
	}
	if got := stream.Values[0].Line; got != logqlFixtureBody {
		t.Errorf("entry line = %q, want %q", got, logqlFixtureBody)
	}
}
