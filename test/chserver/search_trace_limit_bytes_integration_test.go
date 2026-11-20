//go:build integration

package chserver

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
	traceql_lower "github.com/tsouza/cerberus/internal/traceql"
)

// searchTraceLimitImage is the supported floor.
const searchTraceLimitImage = "clickhouse/clickhouse-server:24.8.14.39-alpine"

// The seeded traces: searchTraceLimitTraces traces of searchTraceLimitSpans
// spans each, one Server root and Client children, a trace every
// searchTraceLimitTraceStep and its spans searchTraceLimitSpanStep apart.
const (
	searchTraceLimitTraces    = 200_000
	searchTraceLimitSpans     = 5
	searchTraceLimitLimit     = 20
	searchTraceLimitTraceStep = 9 * time.Millisecond
	searchTraceLimitSpanStep  = time.Millisecond
)

// searchTraceLimitTraceIDLabel is the label a search row carries its trace
// ID in.
const searchTraceLimitTraceIDLabel = "__cerberus_traceID"

var (
	searchTraceLimitStart = time.Date(2026, time.May, 11, 10, 0, 0, 0, time.UTC)
	searchTraceLimitEnd   = searchTraceLimitStart.Add(searchTraceLimitTraces * searchTraceLimitTraceStep)
)

// TestSearchTraceLimitReadsNoMoreThanTheUnboundedSearch runs span-filtered
// searches on the floor server with the search trace limit and without it.
// The unlimited search reads every matching span once. The limited one reads
// the matching spans' trace id and timestamp to rank the traces, then the
// rest of the row only for the chosen traces' spans, so over spans whose
// payload outweighs what the ranking reads it reads no more bytes than the
// unlimited search, and it answers what the unlimited search answers for the
// traces it keeps.
func TestSearchTraceLimitReadsNoMoreThanTheUnboundedSearch(t *testing.T) {
	ctx := context.Background()
	s := startServer(ctx, t, searchTraceLimitImage)
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("apply the traces schema: %v", err)
	}
	seedSearchTraceLimit(ctx, t, s)

	set, _, err := chopt.Resolve(chopt.Config{Optimizations: chopt.SelectionAuto, Mode: chopt.Permissive}, s.version)
	if err != nil {
		t.Fatalf("resolve the optimizations of %s: %v", s.version, err)
	}
	h := tempo.New(s.client(t, adminUser, adminPassword, chclient.Config{}), schema.DefaultOTelTraces(), "", nil)
	h.Strategies = chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{})
	h.Engine.SetSettings(choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs()))

	for _, query := range []string{
		`{ kind = server } | by(name)`,
		`{ kind = server } | by(kind) | coalesce()`,
		`{ kind = server }`,
	} {
		t.Run(query, func(t *testing.T) {
			limited := searchTraceLimitRun(ctx, t, s, h, query, true)
			unlimited := searchTraceLimitRun(ctx, t, s, h, query, false)
			t.Logf("%s: limited %d bytes, unlimited %d", s.version, limited.readBytes, unlimited.readBytes)
			if limited.readBytes > unlimited.readBytes {
				t.Errorf("the limited search read %d bytes, %d more than the unlimited one; want no more\nlimited:   %s\nunlimited: %s",
					limited.readBytes, limited.readBytes-unlimited.readBytes, limited.statement, unlimited.statement)
			}
			if len(limited.traces) != searchTraceLimitLimit {
				t.Fatalf("the limited search answers %d traces; want %d", len(limited.traces), searchTraceLimitLimit)
			}
			for _, id := range newestTraces(unlimited.traces, searchTraceLimitLimit) {
				if got, want := limited.traces[id], unlimited.traces[id]; !slices.Equal(got.rows, want.rows) {
					t.Errorf("trace %s: the limited search answers %v; the unlimited one %v", id, got.rows, want.rows)
				}
			}
		})
	}
}

// seedSearchTraceLimit seeds the traces the searches read: each span carries
// two resource attributes and three span attributes.
func seedSearchTraceLimit(ctx context.Context, t *testing.T, s *server) {
	t.Helper()
	s.exec(ctx, t, `INSERT INTO otel_traces (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName, ResourceAttributes, ScopeName, ScopeVersion, SpanAttributes, Duration, StatusCode) `+
		`SELECT toDateTime64(?, 9) + toIntervalNanosecond(tr * ? + sp * ?), `+
		`lower(hex(cityHash64('t', tr))) || lower(hex(cityHash64('u', tr))), `+
		`lower(hex(cityHash64('s', number))), `+
		`if(sp = 0, '', lower(hex(cityHash64('s', number - sp)))), `+
		`if(sp = 0, 'root-' || toString(tr % 10), 'op-' || toString(number % 7)), `+
		`if(sp = 0, 'Server', 'Client'), `+
		`'svc-' || toString(tr % 5), `+
		`map('service.name', 'svc-' || toString(tr % 5), 'k8s.namespace.name', if(tr % 3 = 0, 'prod', 'dev')), `+
		`'scope', '1.0', `+
		`map('http.method', if(number % 2 = 0, 'GET', 'POST'), 'rpc.method', 'm' || toString(number % 4), 'http.status_code', toString(200 + number % 3 * 100)), `+
		`cityHash64('d', number) % 1000000000, 'Unset' `+
		`FROM (SELECT number, intDiv(number, ?) AS tr, number % ? AS sp FROM numbers(?))`,
		searchTraceLimitStart, searchTraceLimitTraceStep.Nanoseconds(), searchTraceLimitSpanStep.Nanoseconds(),
		searchTraceLimitSpans, searchTraceLimitSpans, searchTraceLimitTraces*searchTraceLimitSpans)
	s.exec(ctx, t, "OPTIMIZE TABLE otel_traces FINAL")
}

// searchTraceLimitTrace is one trace's rows in a search answer, and the
// earliest row timestamp the search ranks it by.
type searchTraceLimitTrace struct {
	start time.Time
	rows  []string
}

// searchTraceLimitResult is one search: its rows per trace ID, and the
// statement the server finished with what it read.
type searchTraceLimitResult struct {
	traces    map[string]searchTraceLimitTrace
	statement string
	readBytes uint64
}

// searchTraceLimitRun searches query over the seeded window, with the
// search trace limit when limited.
func searchTraceLimitRun(ctx context.Context, t *testing.T, s *server, h *tempo.Handler, query string, limited bool) searchTraceLimitResult {
	t.Helper()
	qid := "search-trace-limit-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	rctx := chclient.WithQueryID(ctx, qid)
	if limited {
		rctx = traceql_lower.WithSearchTraceLimit(rctx, searchTraceLimitLimit)
	}
	rctx = traceql_lower.WithSearchWindow(rctx, searchTraceLimitStart, searchTraceLimitEnd)
	res, err := h.SearchResult(rctx, query, searchTraceLimitLimit)
	if err != nil {
		t.Fatalf("search %s (limited %v): %v", query, limited, err)
	}
	run := searchTraceLimitResult{traces: map[string]searchTraceLimitTrace{}}
	for _, sample := range res.Samples {
		id := sample.Labels[searchTraceLimitTraceIDLabel]
		if id == "" {
			t.Fatalf("search %s (limited %v): a row without %s: %v", query, limited, searchTraceLimitTraceIDLabel, sample.Labels)
		}
		trace := run.traces[id]
		if trace.rows == nil || sample.Timestamp.Before(trace.start) {
			trace.start = sample.Timestamp
		}
		trace.rows = append(trace.rows, fmt.Sprintf("%s %v %d %g", sample.MetricName, sample.Labels, sample.Timestamp.UnixNano(), sample.Value))
		run.traces[id] = trace
	}
	for id, trace := range run.traces {
		slices.Sort(trace.rows)
		run.traces[id] = trace
	}
	s.flushLogs(ctx, t)
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT sum(read_bytes), arrayStringConcat(groupArray(query), '\n') FROM system.query_log WHERE type = 'QueryFinish' AND query_id = ?", qid).
		Scan(&run.readBytes, &run.statement); err != nil {
		t.Fatalf("read query_log for %s: %v", qid, err)
	}
	return run
}

// newestTraces returns the IDs of the n traces that start last, newest
// first, a tie broken by ascending trace ID: the order a search keeps.
func newestTraces(traces map[string]searchTraceLimitTrace, n int) []string {
	ids := make([]string, 0, len(traces))
	for id := range traces {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int {
		if c := traces[b].start.Compare(traces[a].start); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	return ids[:min(n, len(ids))]
}
