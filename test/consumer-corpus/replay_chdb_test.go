//go:build chdb

// chDB-backed corpus replay: the same consumer-captured requests as
// the stub lane, but executed through the FULL pipeline — parse →
// lower → optimize → emit → ClickHouse (chDB) → response shaping —
// against small deterministic seeds. This lane additionally evaluates
// each entry's `expect.data` predicates (non-empty results, grouping
// labels, value/unit sanity), which canned stub rows cannot pin.
package consumercorpus

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// --- Traces seed -----------------------------------------------------
//
// Anchor 2026-05-01T10:00:00Z. Two traces:
//
//	trace A (a…01): the 4-span tree from
//	internal/api/tempo/search_select_nested_set_chdb_test.go —
//	r1 GET /home (Server, frontend, root)
//	├─ c1 auth     (Internal, auth)
//	└─ c2 checkout (Server, checkout, Error)
//	   └─ g1 db    (Client, db)
//	trace B (b…02): a single Error root (Server, payments) so the
//	Comparison tab's compare({status = error}) selection cohort is
//	non-empty among ROOT spans ({nestedSetParent<0} scopes the outer
//	filter to roots).
const tracesChDBSeed = `CREATE TABLE otel_traces (
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String,
    SpanKind LowCardinality(String),
    ServiceName LowCardinality(String),
    Duration Int64,
    Timestamp DateTime64(9),
    StatusCode LowCardinality(String),
    StatusMessage String,
    ScopeName String,
    ScopeVersion String,
    SpanAttributes Map(String, String),
    ResourceAttributes Map(String, String)
) ENGINE = MergeTree() ORDER BY (Timestamp);
INSERT INTO otel_traces VALUES
    ('a0000000000000000000000000000001', '1000000000000001', '', 'GET /home', 'Server', 'frontend', 1000, toDateTime64('2026-05-01 10:00:00.000000001', 9), 'Unset', '', '', '', map(), map('service.name', 'frontend')),
    ('a0000000000000000000000000000001', '1000000000000002', '1000000000000001', 'auth', 'Internal', 'auth', 500, toDateTime64('2026-05-01 10:00:00.000000002', 9), 'Unset', '', '', '', map(), map('service.name', 'auth')),
    ('a0000000000000000000000000000001', '1000000000000003', '1000000000000001', 'checkout', 'Server', 'checkout', 700, toDateTime64('2026-05-01 10:00:00.000000003', 9), 'Error', '', '', '', map(), map('service.name', 'checkout')),
    ('a0000000000000000000000000000001', '1000000000000004', '1000000000000003', 'db', 'Client', 'db', 300, toDateTime64('2026-05-01 10:00:00.000000004', 9), 'Unset', '', '', '', map(), map('service.name', 'db')),
    ('b0000000000000000000000000000002', '2000000000000001', '', 'POST /pay', 'Server', 'payments', 900, toDateTime64('2026-05-01 10:00:30.000000001', 9), 'Error', 'card declined', '', '', map(), map('service.name', 'payments'));`

// chdbTraceID is the trace the chdb lane resolves ${TRACE_ID} to.
const chdbTraceID = "a0000000000000000000000000000001"

var chdbTracesAnchor = time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

// --- Logs seed -------------------------------------------------------
//
// Anchor 2026-01-01T00:00:00Z, one row per 15 s, all in the
// {service_name="api"} stream. Bodies are logfmt with a duration
// field whose values cover ms / s / µs plus one unparseable value
// ("oops"): real OTel log bodies carry Go's Duration.String() µ-sign
// form AND occasional garbage, and reference Loki NEVER aborts on
// either — an unparseable unwrap source degrades per-row to a
// 0-valued sample in an __error__-stamped series
// (pkg/logql/log/metrics_extraction.go), it does not turn the whole
// query into an error. LogAttributes carries a structured-metadata
// field so detected_fields surfaces both parser classes.
const logsChDBSeed = `CREATE TABLE otel_logs (
    Timestamp DateTime64(9),
    SeverityText LowCardinality(String),
    SeverityNumber UInt8 DEFAULT 0,
    Body String,
    LogAttributes Map(String, String),
    ResourceAttributes Map(String, String),
    ServiceName LowCardinality(String) DEFAULT '',
    ScopeName String DEFAULT '',
    ScopeVersion String DEFAULT '',
    EventName LowCardinality(String) DEFAULT '',
    TraceId String DEFAULT '',
    SpanId String DEFAULT '',
    TraceFlags UInt8 DEFAULT 0
) ENGINE = MergeTree ORDER BY Timestamp;
INSERT INTO otel_logs (Timestamp, SeverityText, Body, LogAttributes, ResourceAttributes) VALUES
    (toDateTime64('2026-01-01 00:00:00', 9), 'INFO',  'level=info duration=100ms msg=ok id=1',   map('detected_level', 'info'),  map('service_name', 'api')),
    (toDateTime64('2026-01-01 00:00:15', 9), 'INFO',  'level=info duration=200ms msg=ok id=2',   map('detected_level', 'info'),  map('service_name', 'api')),
    (toDateTime64('2026-01-01 00:00:30', 9), 'ERROR', 'level=error duration=400ms msg=boom id=3', map('detected_level', 'error'), map('service_name', 'api')),
    (toDateTime64('2026-01-01 00:00:45', 9), 'ERROR', 'level=error duration=812µs msg=boom id=4', map('detected_level', 'error'), map('service_name', 'api')),
    (toDateTime64('2026-01-01 00:01:00', 9), 'INFO',  'level=info duration=1.5s msg=ok id=5',    map('detected_level', 'info'),  map('service_name', 'api')),
    (toDateTime64('2026-01-01 00:01:15', 9), 'ERROR', 'level=error duration=oops msg=boom id=6',  map('detected_level', 'error'), map('service_name', 'api'));`

var chdbLogsAnchor = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// --- Metrics seed ----------------------------------------------------
//
// Mirrors regexNameSeed in
// internal/api/prom/handler_chdb_regex_name_matcher_test.go: a
// sum-stored UpDownCounter (cerberus_query_inflight) plus a gauge
// decoy. NOW-anchored because the labels / series matcher paths anchor
// their lookback window at request time. The rows sit two minutes in
// the past so that BOTH anchoring styles cover them: the explicit
// `time=${END_UNIX}` instant query (whole-second truncation would
// otherwise place a now-anchored row just AFTER the requested instant)
// and the request-time-anchored matcher paths. All three metric tables
// are created — the metadata handler and the bare-histogram matcher
// fan-out read every table regardless of which carry rows.
func metricsChDBSeed(now time.Time) string {
	ts := now.Add(-2 * time.Minute).UTC().Format("2006-01-02 15:04:05.000")
	return `CREATE TABLE otel_metrics_gauge (
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);
CREATE TABLE otel_metrics_sum (
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64,
    IsMonotonic Bool DEFAULT false
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);
CREATE TABLE otel_metrics_histogram (
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64)
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);
INSERT INTO otel_metrics_gauge (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ('up', '', '', map('job', 'api'), toDateTime64('` + ts + `', 9), 1.0);
INSERT INTO otel_metrics_sum (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ('cerberus_query_inflight', 'in-flight queries', '', map('cerberus_ql', 'promql'),  toDateTime64('` + ts + `', 9), 2.0),
    ('cerberus_query_inflight', 'in-flight queries', '', map('cerberus_ql', 'logql'),   toDateTime64('` + ts + `', 9), 1.0),
    ('cerberus_query_inflight', 'in-flight queries', '', map('cerberus_ql', 'traceql'), toDateTime64('` + ts + `', 9), 0.0);`
}

// chdbTokens mirrors StubTokens for the chdb lane's seed anchors.
func chdbTokens(datasource string, now time.Time) map[string]string {
	var start, end time.Time
	traceID := chdbTraceID
	switch datasource {
	case "tempo":
		start, end = chdbTracesAnchor.Add(-2*time.Minute), chdbTracesAnchor.Add(3*time.Minute)
	case "loki":
		start, end = chdbLogsAnchor.Add(-1*time.Minute), chdbLogsAnchor.Add(5*time.Minute)
	case "prom":
		start, end = now.Add(-15*time.Minute), now
	}
	return map[string]string{
		"START_UNIX": fmt.Sprintf("%d", start.Unix()),
		"END_UNIX":   fmt.Sprintf("%d", end.Unix()),
		"START_NS":   fmt.Sprintf("%d", start.UnixNano()),
		"END_NS":     fmt.Sprintf("%d", end.UnixNano()),
		"TRACE_ID":   traceID,
	}
}

// TestConsumerCorpus_Replay_ChDB replays every corpus entry against
// chDB-seeded in-process handlers, evaluating wire AND data
// predicates. Entries run as independent subtests so a single run
// reports every violated contract — no early exit, no tolerated
// failures.
func TestConsumerCorpus_Replay_ChDB(t *testing.T) {
	entries, err := Load(".")
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("corpus is empty")
	}

	now := time.Now()

	// Every entry gets its own chdb session + seed, mirroring the
	// repo-wide chclienttest.NewChDB(t)-per-test convention. Sharing
	// one session across entries is NOT safe here: corpus entries
	// that pin known-broken queries surface real ClickHouse
	// exceptions, and chdb-go v1.11.0 follows an exception-bearing
	// query with corrupted parquet result buffers on subsequent
	// queries (deterministic `reading page index of parquet file`
	// panic inside the driver). Per-entry sessions keep each replay
	// hermetic.
	newServer := func(t *testing.T, datasource string) *httptest.Server {
		t.Helper()
		c := chclienttest.NewChDB(t)
		mux := http.NewServeMux()
		switch datasource {
		case "tempo":
			c.Seed(t, tracesChDBSeed)
			tempo.New(c, schema.DefaultOTelTraces(), "v0.0.0-corpus", nil).Mount(mux)
		case "loki":
			c.Seed(t, logsChDBSeed)
			loki.New(c, schema.DefaultOTelLogs(), nil).Mount(mux)
		case "prom":
			c.Seed(t, metricsChDBSeed(now))
			prom.New(c, schema.DefaultOTelMetrics(), nil).Mount(mux)
		default:
			t.Fatalf("unknown datasource %q", datasource)
		}
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	}

	for _, e := range entries {
		e := e
		t.Run(e.Version+"/"+e.Name, func(t *testing.T) {
			srv := newServer(t, e.Datasource)
			for _, err := range Replay(srv.Client(), srv.URL, e, chdbTokens(e.Datasource, now), true) {
				t.Errorf("%v", err)
			}
		})
	}
}
