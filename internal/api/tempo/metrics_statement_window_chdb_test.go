//go:build chdb

// chDB-backed end-to-end pin for the span a TraceQL metrics statement reads.
//
// /api/metrics/query_range wraps the metric in a RangeWindow whose first
// anchor, start, reads (start - step, start], so the statement reads
// (start - step, end]. Every spans read of the statement reads that span: the
// metric's own span rows, both arms of a structural closure, and the
// nested-set numbering. A per-trace lookup (rootName) or a membership
// subquery (traceDuration) reads it widened by the maximum trace duration on
// both sides, and no further. The test drives the real handler, so the
// RangeWindow it checks is the one execMetricsRange builds, not a test-built
// copy, and asserts the decoded samples.
package tempo_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// The request window: start 10:01:00, end 10:02:00, step 60s, so the
// statement reads (10:00:00, 10:02:00] and the per-trace subqueries read
// (09:00:00, 11:02:00] under the default one-hour maximum trace duration.
const (
	statementWindowStart = time.Duration(1778580060) * time.Second // 2026-05-12T10:01:00Z
	statementWindowEnd   = time.Duration(1778580120) * time.Second // 2026-05-12T10:02:00Z
	statementWindowStep  = time.Minute
)

// statementWindowSeed holds five traces around the window. Every span lasts
// 100ms, so a trace read as one span alone lasts 0.1s.
//
//	trace 01  both spans in (start - step, start)
//	  a1 root 'r' 10:00:20
//	  a2 ⤷ a1 'c' 10:00:30
//	trace 02  root in (start - step, start), child after start
//	  b1 root 'r' 10:00:50
//	  b2 ⤷ b1 'c' 10:01:30
//	trace 03  root on the statement's exclusive lower bound
//	  c1 root 'r' 10:00:00   not a span row; read by the per-trace subqueries
//	  c2 ⤷ c1 'c' 10:00:10
//	trace 04  root before the statement's span, inside the per-trace lookback
//	  d1 root 'k' 09:30:00   read by the per-trace subqueries only
//	  d2 ⤷ d1 'k' 10:00:40
//	trace 05  root before the per-trace lookback
//	  e1 root 'z' 08:00:00   read by no subquery
//	  e2 ⤷ e1 'z' 10:00:45
//
// The span rows are a1, a2, b1, c2, d2 and e2 in the first bucket,
// (10:00:00, 10:01:00], and b2 in the second, (10:01:00, 10:02:00].
const statementWindowSeed = `INSERT INTO otel_traces VALUES
    ('f0000000000000000000000000000001', '00000000000000a1', '',                 'r', 'Server',   100000000, toDateTime64('2026-05-12 10:00:20', 9), 'Unset', '', '', '', map(), map('service.name', 'svc')),
    ('f0000000000000000000000000000001', '00000000000000a2', '00000000000000a1', 'c', 'Internal', 100000000, toDateTime64('2026-05-12 10:00:30', 9), 'Unset', '', '', '', map(), map('service.name', 'svc')),
    ('f0000000000000000000000000000002', '00000000000000b1', '',                 'r', 'Server',   100000000, toDateTime64('2026-05-12 10:00:50', 9), 'Unset', '', '', '', map(), map('service.name', 'svc')),
    ('f0000000000000000000000000000002', '00000000000000b2', '00000000000000b1', 'c', 'Internal', 100000000, toDateTime64('2026-05-12 10:01:30', 9), 'Unset', '', '', '', map(), map('service.name', 'svc')),
    ('f0000000000000000000000000000003', '00000000000000c1', '',                 'r', 'Server',   100000000, toDateTime64('2026-05-12 10:00:00', 9), 'Unset', '', '', '', map(), map('service.name', 'svc')),
    ('f0000000000000000000000000000003', '00000000000000c2', '00000000000000c1', 'c', 'Internal', 100000000, toDateTime64('2026-05-12 10:00:10', 9), 'Unset', '', '', '', map(), map('service.name', 'svc')),
    ('f0000000000000000000000000000004', '00000000000000d1', '',                 'k', 'Server',   100000000, toDateTime64('2026-05-12 09:30:00', 9), 'Unset', '', '', '', map(), map('service.name', 'svc')),
    ('f0000000000000000000000000000004', '00000000000000d2', '00000000000000d1', 'k', 'Internal', 100000000, toDateTime64('2026-05-12 10:00:40', 9), 'Unset', '', '', '', map(), map('service.name', 'svc')),
    ('f0000000000000000000000000000005', '00000000000000e1', '',                 'z', 'Server',   100000000, toDateTime64('2026-05-12 08:00:00', 9), 'Unset', '', '', '', map(), map('service.name', 'svc')),
    ('f0000000000000000000000000000005', '00000000000000e2', '00000000000000e1', 'z', 'Internal', 100000000, toDateTime64('2026-05-12 10:00:45', 9), 'Unset', '', '', '', map(), map('service.name', 'svc'));`

// statementWindowCase is one metrics shape and the count_over_time() it
// answers at the two anchors, start and end.
type statementWindowCase struct {
	query string
	want  []float64
	// why names the spans each anchor counts and what a different window
	// would count instead.
	why string
}

var statementWindowCases = []statementWindowCase{
	{
		query: `{ } | count_over_time()`,
		want:  []float64{6, 1},
		why:   "the span rows themselves; a span on 10:00:00 or before is not one",
	},
	{
		query: `{ } > { } | count_over_time()`,
		want:  []float64{1, 1},
		why: "a2 and b2, whose parents lie in (start - step, start); read over [start, end] the closure " +
			"counts neither (0, 0), and a closure that read 10:00:00 or the lookback would count c2 or d2",
	},
	{
		query: `{ } >> { } | count_over_time()`,
		want:  []float64{1, 1},
		why: "a2 and b2, whose ancestors lie in (start - step, start); read over [start, end] the closure " +
			"counts neither (0, 0), and a closure that read 10:00:00 or the lookback would count c2 or d2",
	},
	{
		query: `{ nestedSetLeft > 0 } | count_over_time()`,
		want:  []float64{3, 1},
		why: "a1, a2, b1 and b2, numbered from roots in (start - step, start); numbered over [start, end] " +
			"none is, and a numbering that read 10:00:00 or the lookback would number c2 or d2",
	},
	{
		query: `{ rootName = name } | count_over_time()`,
		want:  []float64{3, 0},
		why: "a1, b1 and d2, whose root d1 lies inside the lookback; read inside the statement's span alone " +
			"d2 has no root (2), and read past the lookback e2 resolves root 'z' (4)",
	},
	{
		query: `{ traceDuration > 1s } | count_over_time()`,
		want:  []float64{5, 1},
		why: "a1, a2, b1, c2 and d2, then b2; measured inside the statement's span alone traces 03 and 04 " +
			"last 0.1s (3), and measured past the lookback trace 05 lasts 2h (6)",
	},
}

// TestMetricsStatementWindow_ChDB answers each metrics shape over
// statementWindowSeed and asserts its samples at start and end.
func TestMetricsStatementWindow_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	c.Seed(t, tracesDDL)
	c.Seed(t, statementWindowSeed)
	s := schema.DefaultOTelTraces()
	if s.MaxTraceDuration != time.Hour {
		t.Fatalf("the seed places traces 04 and 05 around a one-hour maximum trace duration, the default is %s", s.MaxTraceDuration)
	}
	h := tempo.New(c, s, "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	wantAnchorsMs := []int64{statementWindowStart.Milliseconds(), statementWindowEnd.Milliseconds()}
	for _, tc := range statementWindowCases {
		t.Run(tc.query, func(t *testing.T) {
			u := metricsQueryRangeURL(srv.URL, tc.query, map[string]string{
				"start": strconv.FormatInt(int64(statementWindowStart/time.Second), 10),
				"end":   strconv.FormatInt(int64(statementWindowEnd/time.Second), 10),
				"step":  statementWindowStep.String(),
			})
			resp, err := http.Get(u)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			var out tempo.MetricsQueryRangeResponse
			if err := json.Unmarshal([]byte(body), &out); err != nil {
				t.Fatalf("decode: %v\nbody: %s", err, body)
			}
			if len(out.Series) != 1 {
				t.Fatalf("want one series, got %d: %s", len(out.Series), body)
			}
			var gotAnchors []int64
			var got []float64
			for _, sm := range out.Series[0].Samples {
				gotAnchors = append(gotAnchors, sm.TimestampMs)
				got = append(got, sm.Value)
			}
			if !slices.Equal(gotAnchors, wantAnchorsMs) {
				t.Fatalf("anchors = %v, want %v: %s", gotAnchors, wantAnchorsMs, body)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("samples = %v, want %v: %s", got, tc.want, tc.why)
			}
		})
	}
}
