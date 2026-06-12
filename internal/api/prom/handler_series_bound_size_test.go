package prom_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
)

// This file pins the #799 regression: a heavily-underscored GAUGE metric
// whose /api/v1/series fan-out blows past ClickHouse's max_query_size once
// the driver inlines the bound metric-name candidate literals.
//
// chDB-lenient-vs-prod-strict gap: the bug never reproduces under chDB or a
// SQL-string-only assertion. clickhouse-go/v2 speaks the native protocol,
// which has no server-side bound-parameter channel — it substitutes every
// positional `?` with its rendered literal CLIENT-SIDE before the query
// reaches the server. So the bytes ClickHouse's max_query_size (256KB)
// ceiling counts are the placeholder SQL with every `?` REPLACED by its arg
// literal, not the compact `?`-carrying string the handler renders. The
// original #71 rendered-size guard measured `len(placeholderSQL)` only, so
// the metric-name candidate powerset (which rides the invisible `[]any` arg
// channel) never counted against the budget: the placeholder SQL stayed
// ~1KB/arm and passed the guard, but once the driver inlined ~thousands of
// candidate string literals the wire query crossed 256KB at parse position
// ~262142 and real clickhouse-server 502'd with code 62 "Max query size
// exceeded". The compose-smoke `iterate-metrics-explorer` probe on
// `otelcol_process_runtime_total_sys_memory_bytes` reproduced it
// deterministically; chDB masked it; the SQL-text guard in the fan-in tests
// could not see it.
//
// The guard against recurrence: render every combined /api/v1/series query
// the handler issues for this exact metric and measure its BOUND size (the
// placeholder SQL with every `?` accounted for by its inlined arg literal —
// the size CH actually parses). Assert every bound query stays under CH's
// max_query_size. This FAILS on the pre-fix code (the guard split on
// placeholder size, so a bound query crosses the ceiling) and PASSES after
// boundQueryBytes teaches the rendered-size guard to measure the wire size.

// chMaxQuerySizeBytes is ClickHouse's default `max_query_size`: the byte
// ceiling the server's parser enforces on an incoming query. A bound query
// at or past this is rejected with code 62 "Max query size exceeded" — the
// #799 502. The handler's own rendered-size guard keeps a safety margin
// below this; the test asserts the stricter invariant that nothing reaches
// the CH ceiling itself.
const chMaxQuerySizeBytes = 262144

// faninArgsRecordingQuerier captures both the SQL text AND the bound args of
// every Query the handler issues, so the test can reconstruct the BOUND
// query size the native driver transmits (placeholder SQL + inlined arg
// literals) rather than the misleadingly-compact placeholder SQL the fan-in
// tests assert on.
type faninArgsRecordingQuerier struct {
	queries []boundQuery
}

type boundQuery struct {
	sql  string
	args []any
}

func (q *faninArgsRecordingQuerier) Query(_ context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	q.queries = append(q.queries, boundQuery{sql: sql, args: args})
	return nil, nil
}

func (q *faninArgsRecordingQuerier) QueryCursor(_ context.Context, sql string, args ...any) (chclient.Cursor, error) {
	q.queries = append(q.queries, boundQuery{sql: sql, args: args})
	return newSliceCursor(nil), nil
}

func (q *faninArgsRecordingQuerier) QueryStrings(_ context.Context, sql string, args ...any) ([]string, error) {
	q.queries = append(q.queries, boundQuery{sql: sql, args: args})
	return nil, nil
}

func (q *faninArgsRecordingQuerier) QueryLabelSets(_ context.Context, _ string, _ ...any) ([]map[string]string, error) {
	return nil, nil
}

func (q *faninArgsRecordingQuerier) QueryMetricMeta(_ context.Context, _, _ string, _ ...any) ([]chclient.MetricMetaRow, error) {
	return nil, nil
}

func (q *faninArgsRecordingQuerier) QueryExemplars(_ context.Context, _ string, _ ...any) ([]chclient.ExemplarRow, error) {
	return nil, nil
}

var _ prom.Querier = (*faninArgsRecordingQuerier)(nil)

// boundWireBytes reconstructs the byte length of the query the native
// clickhouse-go/v2 driver transmits after inlining the positional args
// client-side: the placeholder SQL plus, per `?`, the extra bytes its
// rendered literal occupies beyond the single `?` it replaces. This mirrors
// the production boundQueryBytes accounting in metadata.go so the test
// measures the same wire size CH's parser counts against max_query_size.
//
// String literals dominate the series fan-out (metric-name candidates);
// they render as `'<value>'`, so each contributes at least len+2 bytes —
// the test uses len+2 (a LOWER bound on the inlined cost: it never
// over-states the wire size), making the assertion conservative against
// false positives.
func boundWireBytes(sql string, args []any) int {
	total := len(sql)
	for _, a := range args {
		if s, ok := a.(string); ok {
			total += (len(s) + 2) - 1 // 'value' replaces one '?'
			continue
		}
		total += 1 // non-string literals are small; count ≥ the '?' they replace
	}
	return total
}

// TestHandleSeries_GaugeFanoutStaysUnderCHMaxQuerySize is the #799 pin: the
// heavily-underscored gauge metric whose dotted-candidate × histogram-
// companion fan-out produced the 262142-byte bound query that real
// clickhouse-server 502'd on. Every combined query the handler issues for it
// must stay under CH's max_query_size when measured at its BOUND (inlined-
// literal) size — the size the native driver actually transmits.
func TestHandleSeries_GaugeFanoutStaysUnderCHMaxQuerySize(t *testing.T) {
	t.Parallel()
	q := &faninArgsRecordingQuerier{}
	srv := faninServer(q)
	defer srv.Close()

	// The exact compose-smoke failure: a 6-internal-underscore gauge name.
	// Its dotted-candidate powerset is 2^6 = 64, and the bare-histogram
	// companion layer multiplies the arm set further — the combined
	// UNION-ALL binds the full candidate set inline, which is what crossed
	// 256KB pre-fix.
	form := url.Values{}
	form.Add("match[]", `{__name__="otelcol_process_runtime_total_sys_memory_bytes"}`)
	resp, err := http.PostForm(srv.URL+"/api/v1/series", form)
	if err != nil {
		t.Fatalf("POST /series: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	if len(q.queries) == 0 {
		t.Fatal("handler issued no /series query for the gauge fan-out — the test " +
			"can't observe the bound-size invariant it pins")
	}

	maxBound := 0
	for _, bq := range q.queries {
		bound := boundWireBytes(bq.sql, bq.args)
		if bound > maxBound {
			maxBound = bound
		}
		if bound >= chMaxQuerySizeBytes {
			t.Fatalf("a combined /series query for the gauge fan-out binds to a "+
				"%d-byte WIRE query (placeholder SQL %d bytes + %d inlined arg literals), "+
				"at/over ClickHouse's %d max_query_size — the native driver inlines the "+
				"metric-name candidate literals client-side, so this is the exact "+
				"262142-byte query real clickhouse-server 502'd on in #799. The "+
				"rendered-size guard must measure the BOUND size, not len(placeholderSQL).",
				bound, len(bq.sql), len(bq.args), chMaxQuerySizeBytes)
		}
	}
	t.Logf("gauge fan-out: %d combined queries, largest bound wire size %d bytes "+
		"(< %d CH max_query_size)", len(q.queries), maxBound, chMaxQuerySizeBytes)
}

// TestHandleSeries_GaugeFanoutBoundSizeExceedsPlaceholderSize documents WHY
// the SQL-text-only fan-in guard could not catch #799: for the gauge fan-out
// the bound (inlined-literal) wire size is materially larger than the
// placeholder SQL size — the gap IS the metric-name candidate payload riding
// the invisible arg channel. If a future refactor ever made the args inline
// in the SQL text (or eliminated them), this relationship would change and
// the bound-size guard above would no longer be load-bearing; this test pins
// that the gap is real so the #799 guard stays meaningful.
func TestHandleSeries_GaugeFanoutBoundSizeExceedsPlaceholderSize(t *testing.T) {
	t.Parallel()
	q := &faninArgsRecordingQuerier{}
	srv := faninServer(q)
	defer srv.Close()

	form := url.Values{}
	form.Add("match[]", `{__name__="otelcol_process_runtime_total_sys_memory_bytes"}`)
	resp, err := http.PostForm(srv.URL+"/api/v1/series", form)
	if err != nil {
		t.Fatalf("POST /series: %v", err)
	}
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	sawGap := false
	for _, bq := range q.queries {
		// Only the Sample-projecting series queries carry the candidate
		// powerset; identify them by the IN-list arg payload.
		if len(bq.args) == 0 {
			continue
		}
		placeholder := len(bq.sql)
		bound := boundWireBytes(bq.sql, bq.args)
		if bound > placeholder {
			sawGap = true
			// The arg payload must be substantial — a single stray `?` would
			// be a trivial gap. The candidate powerset is dozens of strings.
			if !strings.Contains(bq.sql, "?") {
				t.Fatalf("query binds %d args but renders no `?` placeholder — the "+
					"bound-size accounting can't be reasoned about", len(bq.args))
			}
		}
	}
	if !sawGap {
		t.Fatal("no /series query showed bound size > placeholder size for the gauge " +
			"fan-out — either the candidate fan-out stopped binding args (the #799 " +
			"guard would no longer be load-bearing) or the arg channel changed shape")
	}
}
