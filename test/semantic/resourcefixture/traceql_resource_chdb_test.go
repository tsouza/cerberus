//go:build chdb

// TraceQL leg of SIGNAL-RESOURCE-001 (cerberus issue #3457): seeds Fixture
// into a custom-named otel_traces table and proves the resource-scoped
// attribute filter both selects on and returns the simple key (team) and
// the dotted key (k8s.namespace.name) verbatim — dotted, scope-qualified,
// NEVER underscored, unlike PromQL/LogQL's wire label — and that the
// returned span start time preserves the row's full seeded nanosecond
// value, TraceQL's own native wire precision.
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

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

const (
	traceqlFixtureTraceID = "a0000000000000000000000000000001"
	traceqlFixtureSpanID  = "b000000000000001"
	traceqlFixtureSpan    = "POST /checkout"
)

// traceqlFixtureDDL declares traceqlFixtureTracesTable with exactly the
// columns the TraceQL structural + attribute read path projects.
// ResourceAttributes carries both fixture keys — SpanAttributes stays
// empty, since SIGNAL-RESOURCE-001 covers resource-attribute projection
// specifically (TraceQL's span-attribute scope is TRACEQL-ATTRIBUTE-SCOPE-
// RESOLUTION's already-covered surface).
func traceqlFixtureDDL() string {
	return fmt.Sprintf(`CREATE TABLE %s (
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String,
    SpanKind LowCardinality(String),
    Duration Int64,
    Timestamp DateTime64(9),
    StatusCode LowCardinality(String),
    StatusMessage String,
    ScopeName String,
    ScopeVersion String,
    SpanAttributes Map(String, String),
    ResourceAttributes Map(String, String)
) ENGINE = MergeTree() ORDER BY (Timestamp);

INSERT INTO %s VALUES
    ('%s', '%s', '', '%s', 'Server', 1500000000, toDateTime64('%s', 9), 'Ok', '', '', '', map(), map('%s', '%s', '%s', '%s'));
`, traceqlFixtureTracesTable, traceqlFixtureTracesTable,
		traceqlFixtureTraceID, traceqlFixtureSpanID, traceqlFixtureSpan,
		Fixture.Timestamp.UTC().Format(TSFormat),
		TeamKey, Fixture.Team, NamespaceKey, Fixture.Namespace)
}

// TestSignalResource_TraceQL_ChDB is SIGNAL-RESOURCE-001's TraceQL
// evidence.
func TestSignalResource_TraceQL_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	c.Seed(t, traceqlFixtureDDL())

	s := schema.DefaultOTelTraces()
	s.SpansTable = traceqlFixtureTracesTable

	h := tempo.New(c, s, "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	query := fmt.Sprintf(`{ resource.%s = %q && resource.%s = %q } | select(resource.%s, resource.%s)`,
		TeamKey, Fixture.Team, NamespaceKey, Fixture.Namespace, TeamKey, NamespaceKey)
	start := Fixture.Timestamp.Add(-time.Minute).Unix()
	end := Fixture.Timestamp.Add(time.Minute).Unix()
	u := fmt.Sprintf("%s/api/search?q=%s&start=%d&end=%d&limit=20&spss=20", srv.URL, url.QueryEscape(query), start, end)

	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	var parsed tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query %q: status=%d", query, resp.StatusCode)
	}
	if len(parsed.Traces) != 1 {
		t.Fatalf("search matched %d traces, want 1: %+v", len(parsed.Traces), parsed.Traces)
	}
	tr := parsed.Traces[0]
	if len(tr.SpanSets) != 1 || len(tr.SpanSets[0].Spans) != 1 {
		t.Fatalf("want 1 spanset with 1 span, got %+v", tr.SpanSets)
	}
	span := tr.SpanSets[0].Spans[0]

	wantAttr := map[string]string{TeamKey: Fixture.Team, NamespaceKey: Fixture.Namespace}
	got := map[string]string{}
	for _, kv := range span.Attributes {
		if kv.Value.StringValue != nil {
			got[kv.Key] = *kv.Value.StringValue
		}
	}
	for key, want := range wantAttr {
		if got[key] != want {
			t.Errorf("selected attribute %q = %q, want %q (dotted, resource-scoped, unlike PromQL/LogQL's underscored wire label)", key, got[key], want)
		}
	}
	if _, present := got[NamespaceWireLabel]; present {
		t.Errorf("selected attributes carry the underscored wire label %q; TraceQL must address the dotted key %q verbatim, never normalized", NamespaceWireLabel, NamespaceKey)
	}

	wantNanos := strconv.FormatInt(Fixture.Timestamp.UnixNano(), 10)
	if span.StartTimeUnixNano != wantNanos {
		t.Errorf("span startTimeUnixNano = %q, want %q (TraceQL's native full-nanosecond wire precision, not rounded to milliseconds)", span.StartTimeUnixNano, wantNanos)
	}
}
