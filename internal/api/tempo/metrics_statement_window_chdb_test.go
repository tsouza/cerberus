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

// statementWindowSeed holds eight traces around the window. Every span lasts
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
//	trace 06  child after end, inside the per-trace lookback
//	  f1 root 'u' 10:01:40
//	  f2 ⤷ f1 'u' 10:30:00   read by the per-trace subqueries only
//	trace 07  child after the per-trace lookback
//	  71 root 'v' 10:01:50
//	  72 ⤷ 71 'v' 13:00:00   read by no subquery
//	trace 08  a grandchild reached through a parent outside the statement's span
//	  81 root 'r' 10:00:05
//	  82 ⤷ 81 'c' 09:59:30   read by the per-trace subqueries only
//	  83 ⤷ 82 'c' 10:00:15
//
// The span rows are a1, a2, b1, c2, d2, e2, 81 and 83 in the first bucket,
// (10:00:00, 10:01:00], and b2, f1 and 71 in the second, (10:01:00, 10:02:00].
// Measured over the per-trace window, traces 05 and 07 last 0.1s and every
// other trace lasts more than 1s.
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
    ('f0000000000000000000000000000005', '00000000000000e2', '00000000000000e1', 'z', 'Internal', 100000000, toDateTime64('2026-05-12 10:00:45', 9), 'Unset', '', '', '', map(), map('service.name', 'svc')),
    ('f0000000000000000000000000000006', '00000000000000f1', '',                 'u', 'Server',   100000000, toDateTime64('2026-05-12 10:01:40', 9), 'Unset', '', '', '', map(), map('service.name', 'svc')),
    ('f0000000000000000000000000000006', '00000000000000f2', '00000000000000f1', 'u', 'Internal', 100000000, toDateTime64('2026-05-12 10:30:00', 9), 'Unset', '', '', '', map(), map('service.name', 'svc')),
    ('f0000000000000000000000000000007', '00000000000000g1', '',                 'v', 'Server',   100000000, toDateTime64('2026-05-12 10:01:50', 9), 'Unset', '', '', '', map(), map('service.name', 'svc')),
    ('f0000000000000000000000000000007', '00000000000000g2', '00000000000000g1', 'v', 'Internal', 100000000, toDateTime64('2026-05-12 13:00:00', 9), 'Unset', '', '', '', map(), map('service.name', 'svc')),
    ('f0000000000000000000000000000008', '00000000000000h1', '',                 'r', 'Server',   100000000, toDateTime64('2026-05-12 10:00:05', 9), 'Unset', '', '', '', map(), map('service.name', 'svc')),
    ('f0000000000000000000000000000008', '00000000000000h2', '00000000000000h1', 'c', 'Internal', 100000000, toDateTime64('2026-05-12 09:59:30', 9), 'Unset', '', '', '', map(), map('service.name', 'svc')),
    ('f0000000000000000000000000000008', '00000000000000h3', '00000000000000h2', 'c', 'Internal', 100000000, toDateTime64('2026-05-12 10:00:15', 9), 'Unset', '', '', '', map(), map('service.name', 'svc'));`

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
		want:  []float64{8, 3},
		why:   "the span rows themselves; a span on 10:00:00 or before, or after 10:02:00, is not one",
	},
	{
		query: `{ } > { } | count_over_time()`,
		want:  []float64{1, 1},
		why: "a2 and b2, whose parents lie in (start - step, start); read over [start, end] the closure " +
			"counts neither (0, 0), and a closure that read 10:00:00 or the lookback would count c2, d2 or 83",
	},
	{
		query: `{ } >> { } | count_over_time()`,
		want:  []float64{1, 1},
		why: "a2 and b2, whose ancestors lie in (start - step, start); read over [start, end] the closure " +
			"counts neither (0, 0), and a closure whose step arm read the lookback would reach 83 through 82",
	},
	{
		query: `{ nestedSetLeft > 0 } | count_over_time()`,
		want:  []float64{4, 3},
		why: "a1, a2, b1 and 81, then b2, f1 and 71, numbered from roots in the statement's span; numbered over " +
			"[start, end] only f1 and 71 are, and a numbering that read 10:00:00 or the lookback would number c2, d2 or 83",
	},
	{
		query: `{ rootName = name } | count_over_time()`,
		want:  []float64{4, 2},
		why: "a1, b1, d2 and 81, then f1 and 71, d2's root d1 lying inside the lookback; read inside the " +
			"statement's span alone d2 has no root (3, 2), and read past the lookback e2 resolves root 'z' (5, 2)",
	},
	{
		query: `{ traceDuration > 1s } | count_over_time()`,
		want:  []float64{7, 2},
		why: "every span row but e2, then b2 and f1; measured inside the statement's span alone traces 03, 04 " +
			"and 06 last 0.1s (5, 1), and measured past the lookback traces 05 and 07 last over 2h (8, 3)",
	},
	{
		query: `{ traceDuration > duration } | count_over_time()`,
		want:  []float64{7, 2},
		why: "the spans of every trace longer than its 0.1s spans: every span row but e2, then b2 and f1; " +
			"measured inside the statement's span alone (5, 1), and measured past the lookback (8, 3)",
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
