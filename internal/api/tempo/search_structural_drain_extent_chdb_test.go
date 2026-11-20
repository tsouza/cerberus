//go:build chdb

// chDB-backed proof that a windowed structural search reads its chosen traces
// drained only when one of them has a span outside the request window: over
// traces lying wholly inside the window it issues the statements and returns
// the answer of the request window's read, and over a trace reaching outside
// the window it returns that trace's matching spans outside the window too.
package tempo_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// insideSeed is shortIDSeed without its span outside the window (e4): both
// traces lie wholly inside drainWindow.
const insideSeed = `INSERT INTO otel_traces VALUES
    ('` + shortTraceID + `', '00000000000000e1', '',                 'op-6', 'Server',   1, toDateTime64('2026-05-01 10:59:00', 9), 'Unset', '', '', '', map(), map('service.name', 'frontend')),
    ('` + shortTraceID + `', '00000000000000e2', '00000000000000e1', 'op-1', 'Internal', 1, toDateTime64('2026-05-01 10:59:10', 9), 'Unset', '', '', '', map(), map('service.name', 'backend')),
    ('` + shortTraceID + `', '00000000000000e3', '00000000000000e1', 'op-2', 'Internal', 1, toDateTime64('2026-05-01 10:59:20', 9), 'Unset', '', '', '', map(), map('service.name', 'backend')),
    ('f2000000000000000000000000000002', '00000000000000f1', '',                 'op-6', 'Server',   1, toDateTime64('2026-05-01 10:10:00', 9), 'Unset', '', '', '', map(), map('service.name', 'frontend')),
    ('f2000000000000000000000000000002', '00000000000000f2', '00000000000000f1', 'op-1', 'Internal', 1, toDateTime64('2026-05-01 10:10:10', 9), 'Unset', '', '', '', map(), map('service.name', 'backend')),
    ('f2000000000000000000000000000002', '00000000000000f3', '00000000000000f1', 'op-2', 'Internal', 1, toDateTime64('2026-05-01 10:10:20', 9), 'Unset', '', '', '', map(), map('service.name', 'backend'));`

// spansStatementClient records every statement a handler sends against the
// spans table.
type spansStatementClient struct {
	*chclienttest.Client
	mu    sync.Mutex
	stmts []string
}

func (c *spansStatementClient) record(sql string) {
	if !strings.Contains(sql, "otel_traces") {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stmts = append(c.stmts, sql)
}

func (c *spansStatementClient) Query(ctx context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	c.record(sql)
	return c.Client.Query(ctx, sql, args...)
}

func (c *spansStatementClient) QueryStrings(ctx context.Context, sql string, args ...any) ([]string, error) {
	c.record(sql)
	return c.Client.QueryStrings(ctx, sql, args...)
}

// take returns the statements recorded so far and forgets them.
func (c *spansStatementClient) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.stmts
	c.stmts = nil
	return out
}

// newStatementServer serves c's spans table with the maximum trace duration
// set to maxTraceDuration.
func newStatementServer(t *testing.T, c *spansStatementClient, maxTraceDuration time.Duration) *httptest.Server {
	t.Helper()
	s := schema.DefaultOTelTraces()
	s.MaxTraceDuration = maxTraceDuration
	h := tempo.New(c, s, "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestSearch_StructuralDrainOnlyOutside_ChDB pins each structural relation's
// statements and answer against the same search over the request window alone
// (a zero maximum trace duration).
//
// Over insideSeed no chosen trace has a span outside the window, so the
// drained search returns the request window's answer — every trace summary
// equal, root metadata included — through the request window's statements.
// When some chosen trace lacks its root row, the root lookup carries the
// outside-window question and the statement count is the same; when every
// chosen trace has its root row, the question is one extra statement reading
// the trace-id and timestamp columns alone.
//
// Over shortIDSeed the short trace has span e4 outside the window, so the
// search adds the drained read and the short trace returns e4 too.
//
// chDB's catalog is process-wide, so one client serves every search, and each
// phase reseeds the spans table it reads.
func TestSearch_StructuralDrainOnlyOutside_ChDB(t *testing.T) {
	c := &spansStatementClient{Client: chclienttest.NewChDB(t)}
	drained := newStatementServer(t, c, schema.DefaultMaxTraceDuration)
	narrow := newStatementServer(t, c, 0)
	for _, tc := range []struct {
		query string
		// extentStatement is whether the outside-window question needs a
		// statement of its own: every chosen trace carries its root row.
		extentStatement bool
		// outsideSpans is the short trace's span ids over shortIDSeed.
		outsideSpans []string
	}{
		{`{ name = "op-6" } > { name = "op-2" }`, false, []string{"e3", "e4"}},
		{`{ name = "op-6" } > { name = "op-2" } | select(span.x)`, false, []string{"e3", "e4"}},
		{`{ name = "op-1" } ~ { name = "op-2" }`, false, []string{"e3", "e4"}},
		{`{ name = "op-1" } < { name = "op-6" }`, true, []string{"e1"}},
		{`{ name = "op-6" } >> { name = "op-2" }`, false, []string{"e3", "e4"}},
		{`{ name = "op-6" } &> { name = "op-2" }`, true, []string{"e1", "e3", "e4"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			path := fmt.Sprintf("/api/search?q=%s&limit=2&spss=20%s", url.QueryEscape(tc.query), drainWindow)

			c.Seed(t, tracesDDL)
			c.Seed(t, insideSeed)
			c.take()
			want := doSearch(t, narrow, path).Traces
			narrowStmts := c.take()
			got := doSearch(t, drained, path).Traces
			drainedStmts := c.take()
			if len(want) != 2 {
				t.Fatalf("request-window search returned %d traces, want 2", len(want))
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("drained search over traces inside the window answered\n%+v\nwant the request window's\n%+v", got, want)
			}
			wantStmts := len(narrowStmts)
			if tc.extentStatement {
				wantStmts++
			}
			if len(drainedStmts) != wantStmts {
				t.Fatalf("drained search issued %d statements, want %d:\n%s", len(drainedStmts), wantStmts, strings.Join(drainedStmts, "\n"))
			}
			if tc.extentStatement {
				extent := drainedStmts[len(drainedStmts)-1]
				for _, wide := range []string{"ResourceAttributes", "SpanAttributes", "SpanName"} {
					if strings.Contains(extent, wide) {
						t.Errorf("outside-window statement reads %s: %s", wide, extent)
					}
				}
			}

			c.Seed(t, tracesDDL)
			c.Seed(t, shortIDSeed)
			c.take()
			outside := doSearch(t, drained, path)
			outsideStmts := c.take()
			if extra := len(outsideStmts) - len(drainedStmts); extra < 1 {
				t.Errorf("search over a trace reaching outside the window issued %d statements, want more than the %d of one inside it",
					len(outsideStmts), len(drainedStmts))
			}
			spans, traces := spanIDsOf(outside, shortTraceID)
			if len(traces) != 2 {
				t.Fatalf("search over shortIDSeed returned traces %v, want 2", traces)
			}
			if !reflect.DeepEqual(spans, tc.outsideSpans) {
				t.Errorf("trace %s spans %v, want %v", shortTraceID, spans, tc.outsideSpans)
			}
		})
	}
}

// spanIDsOf returns the sorted trimmed span ids trace id carries in sr, and
// every trace id sr carries.
func spanIDsOf(sr searchResult, id string) (spans, traces []string) {
	for _, tr := range sr.Traces {
		traces = append(traces, tr.TraceID)
		if tr.TraceID != id {
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
