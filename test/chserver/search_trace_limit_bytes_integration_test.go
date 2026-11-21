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

// searchTraceLimitImage is the boot floor (versions.yaml
// boot_floor_clickhouse), a server cerberus still starts against. On the
// minimum supported line, 26.3, the limited search reads more bytes than the
// unlimited one; issue #4505 fixes that and moves this probe onto 26.3.
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

// searchTraceLimitWindow is a search's request window.
type searchTraceLimitWindow struct{ start, end time.Time }

// searchTraceLimitWindowAll is the window of the traces seedSearchTraceLimit
// seeds.
var searchTraceLimitWindowAll = searchTraceLimitWindow{searchTraceLimitStart, searchTraceLimitEnd}

// The traces seedSearchTraceLimitScattered seeds: searchTraceLimitScatteredTraces
// traces of searchTraceLimitScatteredSpans spans each, a trace every
// searchTraceLimitScatteredTraceStep and its spans searchTraceLimitSpanStep
// apart, from searchTraceLimitScatteredStart, a day after the other traces,
// and one span in searchTraceLimitErrorEvery with status Error.
const (
	searchTraceLimitScatteredTraces    = 100_000
	searchTraceLimitScatteredSpans     = 8
	searchTraceLimitScatteredTraceStep = 36 * time.Millisecond
	searchTraceLimitScatteredServices  = 8
	searchTraceLimitScatteredNames     = 15
	searchTraceLimitErrorEvery         = 50
)

var (
	searchTraceLimitScatteredStart = searchTraceLimitStart.Add(24 * time.Hour)
	searchTraceLimitScatteredEnd   = searchTraceLimitScatteredStart.Add(searchTraceLimitScatteredTraces * searchTraceLimitScatteredTraceStep)
)

// The windows the status searches run over: every scattered trace, and the
// sixth of them from a sixth of the way in.
var (
	searchTraceLimitWindowScattered = searchTraceLimitWindow{searchTraceLimitScatteredStart, searchTraceLimitScatteredEnd}
	searchTraceLimitWindowSixth     = searchTraceLimitWindow{
		searchTraceLimitScatteredStart.Add(searchTraceLimitScatteredEnd.Sub(searchTraceLimitScatteredStart) / 6),
		searchTraceLimitScatteredStart.Add(searchTraceLimitScatteredEnd.Sub(searchTraceLimitScatteredStart) / 3),
	}
)

// TestSearchTraceLimitReadsNoMoreThanTheUnboundedSearch runs span-filtered
// searches on the boot-floor server with the search trace limit and without it,
// under the strategies auto resolves for that server.
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
	choptwire.SetServerStrategies(h.Strategies, set)
	h.Engine.SetSettings(choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs()))

	for _, query := range []string{
		`{ kind = server } | by(name)`,
		`{ kind = server } | by(kind) | coalesce()`,
		`{ kind = server }`,
	} {
		t.Run(query, func(t *testing.T) {
			limited := searchTraceLimitRun(ctx, t, s, h, query, searchTraceLimitWindowAll, true)
			unlimited := searchTraceLimitRun(ctx, t, s, h, query, searchTraceLimitWindowAll, false)
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

// searchTraceLimitPlacement is a drain placement: the strategies
// chopt.FeatureSearchDrainScalarTraces and chopt.FeatureSearchDrainTraceBounds
// set.
type searchTraceLimitPlacement struct{ scalarTraces, traceBounds bool }

// searchTraceLimitDrainImages are the servers the drain placements are
// compared on, each with the placement auto resolves there: the scalar
// drains on the minimum supported release, 26.3, and the GLOBAL IN set drain,
// no scalar, on 24.8, below the scalar drains' auto lines, and on 26.4 and
// 26.8, above them.
var searchTraceLimitDrainImages = []struct {
	image string
	want  searchTraceLimitPlacement
}{
	{"clickhouse/clickhouse-server:24.8.14.39-alpine", searchTraceLimitPlacement{}},
	{"clickhouse/clickhouse-server:26.3.17.56-alpine", searchTraceLimitPlacement{scalarTraces: true, traceBounds: true}},
	{"clickhouse/clickhouse-server:26.4.5.143-alpine", searchTraceLimitPlacement{}},
	{"clickhouse/clickhouse-server:26.8.36.3-alpine", searchTraceLimitPlacement{}},
}

// searchTraceLimitDrainSearches are the limited searches the drain placements
// are compared on: the span kind filter over seedSearchTraceLimit's traces,
// whose spans sit in few granules each, and the status filter over
// seedSearchTraceLimitScattered's, whose error spans are scattered through
// every granule.
var searchTraceLimitDrainSearches = []struct {
	query  string
	window searchTraceLimitWindow
}{
	{`{ kind = server } | by(name)`, searchTraceLimitWindowAll},
	{`{ kind = server } | by(kind) | coalesce()`, searchTraceLimitWindowAll},
	{`{ kind = server }`, searchTraceLimitWindowAll},
	{`{ status = error } | by(resource.service.name)`, searchTraceLimitWindowScattered},
	{`{ status = error } | by(resource.service.name)`, searchTraceLimitWindowSixth},
	{`{ status = error } | coalesce()`, searchTraceLimitWindowScattered},
	{`{ status = error } | coalesce()`, searchTraceLimitWindowSixth},
	{`{ status = error }`, searchTraceLimitWindowScattered},
}

// TestSearchTraceLimitDrainPlacementAutoReadsNoMore pins that auto resolves
// the drain placement searchTraceLimitDrainImages names for each server, then
// runs searchTraceLimitDrainSearches under that placement and under every
// other one. Every placement that runs a different statement must answer the
// same rows, and where the resolved placement is a scalar drain it must read
// no more bytes than any of them. Each search runs twice and the second
// run's bytes count, so every placement reads with the same warm caches;
// some other placement must run a different statement, or the comparison
// would hold for any placement.
func TestSearchTraceLimitDrainPlacementAutoReadsNoMore(t *testing.T) {
	for _, image := range searchTraceLimitDrainImages {
		t.Run(image.image, func(t *testing.T) {
			ctx := context.Background()
			s := startServer(ctx, t, image.image)
			if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Traces}); err != nil {
				t.Fatalf("apply the traces schema: %v", err)
			}
			seedSearchTraceLimit(ctx, t, s)
			seedSearchTraceLimitScattered(ctx, t, s)

			set, _, err := chopt.Resolve(chopt.Config{Optimizations: chopt.SelectionAuto, Mode: chopt.Permissive}, s.version)
			if err != nil {
				t.Fatalf("resolve the optimizations of %s: %v", s.version, err)
			}
			resolved := searchTraceLimitPlacement{set.Has(chopt.FeatureSearchDrainScalarTraces), set.Has(chopt.FeatureSearchDrainTraceBounds)}
			if resolved != image.want {
				t.Fatalf("auto resolves the drain placement %+v on %s; want %+v", resolved, s.version, image.want)
			}
			compareBytes := resolved != (searchTraceLimitPlacement{})
			handler := func(p searchTraceLimitPlacement) *tempo.Handler {
				h := tempo.New(s.client(t, adminUser, adminPassword, chclient.Config{}), schema.DefaultOTelTraces(), "", nil)
				h.Strategies = chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{})
				choptwire.SetServerStrategies(h.Strategies, set)
				h.Strategies.SetSearchDrainScalarTraces(p.scalarTraces)
				h.Strategies.SetSearchDrainTraceBounds(p.traceBounds)
				h.Engine.SetSettings(choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs()))
				return h
			}
			var others []searchTraceLimitPlacement
			for _, scalarTraces := range []bool{false, true} {
				for _, traceBounds := range []bool{false, true} {
					if p := (searchTraceLimitPlacement{scalarTraces, traceBounds}); p != resolved {
						others = append(others, p)
					}
				}
			}

			for _, search := range searchTraceLimitDrainSearches {
				t.Run(search.query+" "+search.window.start.Format(time.RFC3339), func(t *testing.T) {
					run := func(p searchTraceLimitPlacement) searchTraceLimitResult {
						h := handler(p)
						searchTraceLimitRun(ctx, t, s, h, search.query, search.window, true)
						return searchTraceLimitRun(ctx, t, s, h, search.query, search.window, true)
					}
					got := run(resolved)
					if len(got.traces) != searchTraceLimitLimit {
						t.Fatalf("the resolved placement answers %d traces; want %d", len(got.traces), searchTraceLimitLimit)
					}
					compared := 0
					for _, other := range others {
						alt := run(other)
						if alt.statement == got.statement {
							continue
						}
						compared++
						t.Logf("%s: resolved placement %+v %d bytes, placement %+v %d", s.version, resolved, got.readBytes, other, alt.readBytes)
						if compareBytes && got.readBytes > alt.readBytes {
							t.Errorf("the resolved drain placement %+v read %d bytes, %d more than placement %+v\nresolved: %s\nother:    %s",
								resolved, got.readBytes, got.readBytes-alt.readBytes, other, got.statement, alt.statement)
						}
						if len(got.traces) != len(alt.traces) {
							t.Fatalf("the resolved placement answers %d traces, placement %+v %d", len(got.traces), other, len(alt.traces))
						}
						for id, trace := range alt.traces {
							if !slices.Equal(got.traces[id].rows, trace.rows) {
								t.Errorf("trace %s: the resolved placement answers %v; placement %+v %v", id, got.traces[id].rows, other, trace.rows)
							}
						}
					}
					if compared == 0 {
						t.Fatalf("every drain placement ran the resolved placement's statement:\n%s", got.statement)
					}
				})
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

// seedSearchTraceLimitScattered seeds the traces the status searches read.
// Each trace's root is a Server span of the frontend service and its other
// spans Client, Internal or Producer ones of a service and a name a hash
// picks, so a trace's spans sit in many granules; one span in
// searchTraceLimitErrorEvery has status Error, scattered by a hash, the
// roots and every third span below them Ok, and the rest Unset.
func seedSearchTraceLimitScattered(ctx context.Context, t *testing.T, s *server) {
	t.Helper()
	s.exec(ctx, t, `INSERT INTO otel_traces (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName, ResourceAttributes, ScopeName, ScopeVersion, SpanAttributes, Duration, StatusCode) `+
		`SELECT toDateTime64(?, 9) + toIntervalNanosecond(tr * ? + sp * ?), `+
		`lower(hex(cityHash64('st', tr))) || lower(hex(cityHash64('su', tr))), `+
		`lower(hex(cityHash64('ss', tr, sp))), `+
		`if(sp = 0, '', lower(hex(cityHash64('ss', tr, intDiv(sp - 1, 2))))), `+
		`if(sp = 0, 'GET /api/' || toString(tr % 10), 'op-' || toString(cityHash64('n', tr, sp) % ?)), `+
		`if(sp = 0, 'Server', ['Client', 'Internal', 'Producer'][1 + cityHash64('k', tr, sp) % 3]), `+
		`if(sp = 0, 'frontend', 'svc-' || toString(cityHash64('v', tr, sp) % ?)) AS svc, `+
		`map('service.name', svc, 'deployment.environment', if(tr % 2 = 0, 'prod', 'staging')), `+
		`'scope', '1.0', `+
		`map('http.method', if(tr % 2 = 0, 'GET', 'POST'), 'http.route', '/r/' || toString(sp)), `+
		`cityHash64('d', tr, sp) % 1000000000, `+
		`if(cityHash64('e', tr, sp) % ? = 0, 'Error', if(sp % 3 = 0, 'Ok', 'Unset')) `+
		`FROM (SELECT intDiv(number, ?) AS tr, number % ? AS sp FROM numbers(?))`,
		searchTraceLimitScatteredStart, searchTraceLimitScatteredTraceStep.Nanoseconds(), searchTraceLimitSpanStep.Nanoseconds(),
		searchTraceLimitScatteredNames, searchTraceLimitScatteredServices, searchTraceLimitErrorEvery,
		searchTraceLimitScatteredSpans, searchTraceLimitScatteredSpans, searchTraceLimitScatteredTraces*searchTraceLimitScatteredSpans)
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

// searchTraceLimitRun searches query over window, with the search trace
// limit when limited.
func searchTraceLimitRun(ctx context.Context, t *testing.T, s *server, h *tempo.Handler, query string, window searchTraceLimitWindow, limited bool) searchTraceLimitResult {
	t.Helper()
	qid := "search-trace-limit-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	rctx := chclient.WithQueryID(ctx, qid)
	if limited {
		rctx = traceql_lower.WithSearchTraceLimit(rctx, searchTraceLimitLimit)
	}
	rctx = traceql_lower.WithSearchWindow(rctx, window.start, window.end)
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
