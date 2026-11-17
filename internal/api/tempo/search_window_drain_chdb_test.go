//go:build chdb

// chDB-backed proof of the windowed-search drain: a search chooses its traces
// from their matching spans inside the request window, then returns every
// matching span of each chosen trace across the window widened by the maximum
// trace duration — the spans outside the window too, as reference Tempo does
// for a trace that overlaps the window.
package tempo_test

import (
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// drainWindow is the request window every drain search sends:
// [2026-05-01 10:00:00, 11:00:00].
const drainWindow = "&start=1777629600&end=1777633200"

// drainSeed holds three traces around drainWindow.
//
//	trace d1 (newest chosen trace)
//	  a1 root  frontend checkout 10:59:00  inside
//	  a2 ⤷ a1  backend  pay      10:59:30  inside
//	  a3 ⤷ a2  db       query    10:59:50  inside
//	  a4 ⤷ a1  backend  ship     11:00:30  outside (after end)
//	  a5 ⤷ a3  db       commit   11:00:40  outside (after end)
//	trace d2 (lies wholly outside the window, never chosen)
//	  b1 root  frontend checkout 11:00:50
//	  b2 ⤷ b1  backend  pay      11:00:55
//	  b3 ⤷ b2  db       query    11:00:58
//	trace d3 (inside the window, older than d1: dropped at limit 1)
//	  c1 root  frontend checkout 10:10:00
//	  c2 ⤷ c1  backend  pay      10:10:10
//	  c3 ⤷ c2  db       query    10:10:20
const drainSeed = `INSERT INTO otel_traces VALUES
    ('d1000000000000000000000000000001', '00000000000000a1', '',                 'checkout', 'Server',   1, toDateTime64('2026-05-01 10:59:00', 9), 'Unset', '', '', '', map(), map('service.name', 'frontend')),
    ('d1000000000000000000000000000001', '00000000000000a2', '00000000000000a1', 'pay',      'Internal', 1, toDateTime64('2026-05-01 10:59:30', 9), 'Unset', '', '', '', map(), map('service.name', 'backend')),
    ('d1000000000000000000000000000001', '00000000000000a3', '00000000000000a2', 'query',    'Client',   1, toDateTime64('2026-05-01 10:59:50', 9), 'Unset', '', '', '', map(), map('service.name', 'db')),
    ('d1000000000000000000000000000001', '00000000000000a4', '00000000000000a1', 'ship',     'Internal', 1, toDateTime64('2026-05-01 11:00:30', 9), 'Unset', '', '', '', map(), map('service.name', 'backend')),
    ('d1000000000000000000000000000001', '00000000000000a5', '00000000000000a3', 'commit',   'Client',   1, toDateTime64('2026-05-01 11:00:40', 9), 'Unset', '', '', '', map(), map('service.name', 'db')),
    ('d2000000000000000000000000000002', '00000000000000b1', '',                 'checkout', 'Server',   1, toDateTime64('2026-05-01 11:00:50', 9), 'Unset', '', '', '', map(), map('service.name', 'frontend')),
    ('d2000000000000000000000000000002', '00000000000000b2', '00000000000000b1', 'pay',      'Internal', 1, toDateTime64('2026-05-01 11:00:55', 9), 'Unset', '', '', '', map(), map('service.name', 'backend')),
    ('d2000000000000000000000000000002', '00000000000000b3', '00000000000000b2', 'query',    'Client',   1, toDateTime64('2026-05-01 11:00:58', 9), 'Unset', '', '', '', map(), map('service.name', 'db')),
    ('d3000000000000000000000000000003', '00000000000000c1', '',                 'checkout', 'Server',   1, toDateTime64('2026-05-01 10:10:00', 9), 'Unset', '', '', '', map(), map('service.name', 'frontend')),
    ('d3000000000000000000000000000003', '00000000000000c2', '00000000000000c1', 'pay',      'Internal', 1, toDateTime64('2026-05-01 10:10:10', 9), 'Unset', '', '', '', map(), map('service.name', 'backend')),
    ('d3000000000000000000000000000003', '00000000000000c3', '00000000000000c2', 'query',    'Client',   1, toDateTime64('2026-05-01 10:10:20', 9), 'Unset', '', '', '', map(), map('service.name', 'db'));`

// newDrainChDBServer serves drainSeed with the maximum trace duration set to
// maxTraceDuration.
func newDrainChDBServer(t *testing.T, maxTraceDuration time.Duration) *httptest.Server {
	t.Helper()
	c := chclienttest.NewChDB(t)
	c.Seed(t, tracesDDL)
	c.Seed(t, drainSeed)
	s := schema.DefaultOTelTraces()
	s.MaxTraceDuration = maxTraceDuration
	h := tempo.New(c, s, "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// drainCase is one search shape: the span ids trace d1 returns when its
// matching spans are read over the request window alone (narrow) and over the
// window widened by the maximum trace duration (drained).
type drainCase struct {
	query   string
	narrow  []string
	drained []string
}

// drainCases covers each shape that drains — plain, a pipeline select, both
// spanset operations, the recursive and direct structural relations and a
// union relation — and the negated relation, which keeps the request window's
// answer.
var drainCases = []drainCase{
	{`{ resource.service.name = "backend" }`, []string{"a2"}, []string{"a2", "a4"}},
	{`{ resource.service.name = "db" } | select(span.x)`, []string{"a3"}, []string{"a3", "a5"}},
	{`{ resource.service.name = "backend" } && { resource.service.name = "db" }`, []string{"a2", "a3"}, []string{"a2", "a3", "a4", "a5"}},
	{`{ name = "ship" } || { name = "pay" }`, []string{"a2"}, []string{"a2", "a4"}},
	{`{ name = "checkout" } >> { resource.service.name = "db" }`, []string{"a3"}, []string{"a3", "a5"}},
	{`{ name = "checkout" } > { resource.service.name = "backend" }`, []string{"a2"}, []string{"a2", "a4"}},
	{`{ name = "pay" } &>> { }`, []string{"a2", "a3"}, []string{"a2", "a3", "a5"}},
	{`{ name = "absent" } !>> { resource.service.name = "db" }`, []string{"a3"}, []string{"a3"}},
}

// d1SpanIDs returns the trimmed span ids of trace d1 in a search response
// (a1 … a5), sorted, and the trace ids the response carries.
func d1SpanIDs(t *testing.T, sr searchResult) (spans, traces []string) {
	t.Helper()
	for _, tr := range sr.Traces {
		traces = append(traces, tr.TraceID)
		if tr.TraceID != "d1000000000000000000000000000001" {
			continue
		}
		for _, set := range tr.SpanSets {
			for _, sp := range set.Spans {
				spans = append(spans, strings.TrimLeft(sp.SpanID, "0"))
			}
		}
	}
	slices.Sort(spans)
	return spans, traces
}

// TestSearch_WindowDrain_ChDB pins the drain on every shape it covers. With
// the default maximum trace duration the newest trace in the window (limit 1)
// returns its matching spans outside the window too; the trace wholly outside
// the window and the older trace in it stay out, so phase B never widens which
// traces are returned. With a zero maximum trace duration the same search
// returns the request window's spans alone.
func TestSearch_WindowDrain_ChDB(t *testing.T) {
	drained := newDrainChDBServer(t, schema.DefaultMaxTraceDuration)
	narrow := newDrainChDBServer(t, 0)
	for _, tc := range drainCases {
		t.Run(tc.query, func(t *testing.T) {
			path := fmt.Sprintf("/api/search?q=%s&limit=1&spss=20%s", url.QueryEscape(tc.query), drainWindow)
			for _, run := range []struct {
				name string
				srv  *httptest.Server
				want []string
			}{
				{"drained", drained, tc.drained},
				{"narrow", narrow, tc.narrow},
			} {
				spans, traces := d1SpanIDs(t, doSearch(t, run.srv, path))
				if !slices.Equal(traces, []string{"d1000000000000000000000000000001"}) {
					t.Fatalf("%s: traces %v, want only d1 (the newest trace matching inside the window)", run.name, traces)
				}
				if !slices.Equal(spans, run.want) {
					t.Errorf("%s: d1 spans %v, want %v", run.name, spans, run.want)
				}
			}
		})
	}
}

// drainManyTraces is how many two-span traces drainManySeed holds: more than
// the largest search limit, so a search at that limit chooses its full
// complement of traces.
const drainManyTraces = 1200

// drainManyLimit is the search limit TestSearch_WindowDrainManyTraces_ChDB
// sends: the largest one the handler accepts.
const drainManyLimit = 1000

// drainManySeed holds drainManyTraces traces inside drainWindow, one second
// apart from 10:00:00: a frontend `checkout` root and a db `query` child.
var drainManySeed = []string{
	fmt.Sprintf(`INSERT INTO otel_traces SELECT lower(leftPad(hex(number + 1), 32, '0')), lower(leftPad(hex(2 * number + 1), 16, '0')), '',
	    'checkout', 'Server', 1, toDateTime64('2026-05-01 10:00:00', 9) + number, 'Unset', '', '', '', map(), map('service.name', 'frontend')
	    FROM numbers(%d)`, drainManyTraces),
	fmt.Sprintf(`INSERT INTO otel_traces SELECT lower(leftPad(hex(number + 1), 32, '0')), lower(leftPad(hex(2 * number + 2), 16, '0')), lower(leftPad(hex(2 * number + 1), 16, '0')),
	    'query', 'Client', 1, toDateTime64('2026-05-01 10:00:00', 9) + number, 'Unset', '', '', '', map(), map('service.name', 'db')
	    FROM numbers(%d)`, drainManyTraces),
}

// TestSearch_WindowDrainManyTraces_ChDB pins that a drain at the largest
// search limit stays within ClickHouse's statement size: phase B splices the
// chosen trace ids as a literal list at every span scan and structural join
// it restricts, so a shape with several of them reads its traces in batches.
// Every trace lies inside the window, so the drained answer equals the
// request window's: the same limit traces, each with the same spans.
func TestSearch_WindowDrainManyTraces_ChDB(t *testing.T) {
	servers := map[string]*httptest.Server{}
	for name, lookback := range map[string]time.Duration{"drained": schema.DefaultMaxTraceDuration, "narrow": 0} {
		c := chclienttest.NewChDB(t)
		c.Seed(t, tracesDDL)
		for _, stmt := range drainManySeed {
			c.Seed(t, stmt)
		}
		s := schema.DefaultOTelTraces()
		s.MaxTraceDuration = lookback
		h := tempo.New(c, s, "v-test", nil)
		mux := http.NewServeMux()
		h.Mount(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		servers[name] = srv
	}
	for _, query := range []string{
		`{ resource.service.name = "frontend" } && { resource.service.name = "db" }`,
		`{ name = "checkout" } || { name = "query" } || { resource.service.name = "db" } || { resource.service.name = "frontend" }`,
		`({ name = "checkout" } >> { resource.service.name = "db" }) || ({ name = "checkout" } > { name = "query" }) || ({ resource.service.name = "frontend" } >> { name = "query" })`,
		`{ name = "checkout" } >> { resource.service.name = "db" }`,
	} {
		t.Run(query, func(t *testing.T) {
			path := fmt.Sprintf("/api/search?q=%s&limit=%d&spss=20%s", url.QueryEscape(query), drainManyLimit, drainWindow)
			want := spanCounts(doSearch(t, servers["narrow"], path))
			got := spanCounts(doSearch(t, servers["drained"], path))
			if len(want) != drainManyLimit {
				t.Fatalf("narrow search returned %d traces, want %d", len(want), drainManyLimit)
			}
			if !maps.Equal(got, want) {
				t.Fatalf("drained search returned %d traces differing from the narrow search's %d", len(got), len(want))
			}
		})
	}
}

// spanCounts maps each trace a search response carries to its span count.
func spanCounts(sr searchResult) map[string]int {
	out := map[string]int{}
	for _, tr := range sr.Traces {
		for _, set := range tr.SpanSets {
			out[tr.TraceID] += len(set.Spans)
		}
	}
	return out
}
