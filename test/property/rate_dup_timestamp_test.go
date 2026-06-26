//go:build chdb

// Cross-path correctness PROOF for rate()/increase() over a counter that
// carries a DUPLICATE (Attributes, TimeUnix) sample — the exact bug class
// fixed by "fix(chsql): dedup duplicate-timestamp samples in row-path
// extrapolated rate-family".
//
// OTel/ClickHouse ingestion can write two rows at the same series +
// timestamp; Prometheus treats a timestamp as carrying one sample. The
// duplicate inflates the row-path sample count, shrinking the
// count-derived average interval that feeds Prometheus's extrapolation cap
// (extend to the window boundary only when the gap < 1.1x the average
// interval). The deflated threshold trips the cap when it should not,
// deflating the result.
//
// This file PROVES both PromQL→ClickHouse lowering paths agree with the
// from-scratch Prometheus oracle on that duplicate data, so the claim
// "rate() is correct on every supported ClickHouse server" reduces to a CI
// assertion rather than prose:
//
//   - ROW path (the generic arrayJoin fan-out, runs on every server the
//     gateway supports, >= 24.8). This is the path the fix corrected; the
//     row-path subtests below are GREEN on this branch and RED on the
//     unfixed emitter (the oracle expects the deduped 50 / 0.16666…, the
//     unfixed row path returns the dup-inflated 37.5 / 0.125).
//   - NATIVE path (timeSeriesRateToGrid, requires ClickHouse >= 25.6),
//     forced on via an explicit NativeRateLowerer wiring. timeSeriesRateToGrid
//     dedups equal-timestamp samples internally, so it was always correct;
//     the subtest PROVES that, completing the matrix.
//
// # Substrate / coverage
//
// The chDB property substrate is ClickHouse 25.8.x (chDB v4.0.2), which
// ships timeSeriesRateToGrid — so BOTH paths EXECUTE against the oracle in
// this one chdb-tagged lane:
//
//	┌─────────────┬──────────────────────────────┬──────────────────────────┐
//	│ lowering    │ executes-vs-oracle on dup     │ covers server range       │
//	├─────────────┼──────────────────────────────┼──────────────────────────┤
//	│ row path    │ chDB 25.8 (this file)         │ >= 24.8 (every supported) │
//	│ native path │ chDB 25.8 (this file)         │ >= 25.6                   │
//	└─────────────┴──────────────────────────────┴──────────────────────────┘
//
// Production validation on ClickHouse 26.5 (recorded in the fix commit:
// row path 30.9578 -> 31.3333 on a dup-affected anchor over 1666 series,
// matching the oracle and native timeSeriesRateToGrid) corroborates the
// upper end. No path is left as "trust me"; nothing is skipped or
// soft-asserted.
package property_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/property"
	"github.com/tsouza/cerberus/test/property/gen"
	oraclepromql "github.com/tsouza/cerberus/test/property/oracle/promql"
)

// nativeRateAggregate is the ClickHouse-native aggregate the ts_grid_range
// path emits. Its presence in (or absence from) the streaming SQL is the
// positive proof that a subtest exercised the native vs the fan-out
// lowering — a comparator pass alone cannot distinguish them, because the
// fixed row path now also returns the deduped-correct value.
const nativeRateAggregate = "timeSeriesRateToGrid"

// sqlSpyClient wraps the chDB-backed test client to record every streaming
// query_range SQL the engine emits, so a subtest can assert WHICH lowering
// actually ran. It embeds *chclienttest.Client so it satisfies the full
// prom.Querier method set by promotion and overrides only QueryCursor (the
// method the query_range streaming path calls).
type sqlSpyClient struct {
	*chclienttest.Client
	sqls []string
}

func (c *sqlSpyClient) QueryCursor(ctx context.Context, sql string, args ...any) (chclient.Cursor, error) {
	c.sqls = append(c.sqls, sql)
	return c.Client.QueryCursor(ctx, sql, args...)
}

// emittedNativeAggregate reports whether any captured streaming SQL used
// the native timeSeriesRateToGrid aggregate.
func (c *sqlSpyClient) emittedNativeAggregate() bool {
	for _, s := range c.sqls {
		if strings.Contains(s, nativeRateAggregate) {
			return true
		}
	}
	return false
}

// The proof drives a multi-anchor query_range grid. The row-path fan-out
// matrix materialises anchors only across a real span (start < end), so a
// degenerate single-point grid yields an empty row-path result; the grid
// below spans three anchors. CounterDupEvalTsSec (300) is the FIRST anchor,
// and it is the dup-affected window (0, 300] — the point at which the
// unfixed row-path emitter diverges (50 -> 37.5). The later anchors carry
// their own correct windows; the oracle is evaluated at every anchor and
// the full matrices are compared, so the proof asserts agreement across
// the whole grid, not just one cell.
const (
	rangeStepSeconds  = 60
	rangeStartSeconds = gen.CounterDupEvalTsSec
	rangeEndSeconds   = gen.CounterDupEvalTsSec + 2*rangeStepSeconds // anchors 300, 360, 420
)

// TestPromQL_RateDupTimestamp_RowAndNative proves the row and native
// lowering paths each equal the from-scratch Prometheus oracle on the
// duplicate-timestamp counter dataset.
//
// Each subtest seeds the same 5-row table (the counter with one duplicate
// (Attributes, TimeUnix) sample), runs the query via the real Prom
// query_range HTTP handler, and diffs the result against the oracle. The
// native subtest forces the timeSeriesRateToGrid lowering by wiring a
// NativeRateLowerer onto the handler — the same boot wiring cmd/cerberus
// performs when CERBERUS_CH_OPTIMIZATIONS lists ts_grid_range — so no
// auto-enable branch is needed.
func TestPromQL_RateDupTimestamp_RowAndNative(t *testing.T) {
	ds := gen.CounterDupTimestampDataset()

	rangeQuery := func(fn string) string {
		return fmt.Sprintf("%s(%s[%s])", fn, gen.CounterDupMetricName, gen.CounterDupRangeSelector)
	}

	cases := []struct {
		name     string
		query    string
		native   bool
		expected float64
	}{
		{
			// Row path, increase: the value the fix corrects most
			// visibly (50 deduped vs 37.5 dup-inflated). RED on the
			// unfixed emitter.
			name:     "row_path/increase",
			query:    rangeQuery("increase"),
			native:   false,
			expected: gen.CounterDupExpectedIncrease,
		},
		{
			// Row path, rate: the same window divided by the 300s
			// range (0.16666… deduped vs 0.125 dup-inflated). RED on
			// the unfixed emitter.
			name:     "row_path/rate",
			query:    rangeQuery("rate"),
			native:   false,
			expected: gen.CounterDupExpectedRate,
		},
		{
			// Native path, rate: timeSeriesRateToGrid dedups equal-ts
			// samples internally. Proves the native aggregate matches
			// the oracle on the same dup data.
			name:     "native_path/rate",
			query:    rangeQuery("rate"),
			native:   true,
			expected: gen.CounterDupExpectedRate,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := &sqlSpyClient{Client: chclienttest.NewChDB(t)}
			h := prom.New(cli, schema.DefaultOTelMetrics(), nil)
			if tc.native {
				// Boot-wire the native rate strategy exactly as
				// cmd/cerberus does when ts_grid_range is enabled. The
				// embedded Fallback keeps shape-ineligible windows on
				// the fan-out path (never reached here — this shape is
				// native-eligible).
				h.Lowerers = promql.RangeLowerers{
					Rate: promql.NativeRateLowerer{Fallback: promql.FanoutRateLowerer{}},
				}
			}
			mux := http.NewServeMux()
			h.Mount(mux)
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			cli.Seed(t, ds.DDL)

			anchors := gridAnchors(rangeStartSeconds, rangeEndSeconds, rangeStepSeconds)

			// The oracle is the from-scratch Prometheus evaluator,
			// evaluated independently at every grid anchor. Its
			// FromDataset collapses the duplicate timestamp per
			// Prometheus's one-sample-per-timestamp invariant, so it
			// holds the 4-sample (correct) series at each window.
			oracleOut := oracleRangeOutcome(ds, tc.query, anchors)

			cerberusOut := runCerberusRange(
				t.Context(), srv.URL, tc.query,
				rangeStartSeconds, rangeEndSeconds, rangeStepSeconds,
			)

			// Degeneracy guard: a comparator pass on two EMPTY outcomes
			// would be vacuous. Both sides must surface exactly one row
			// per anchor for the single series the dataset carries, or
			// the proof proves nothing.
			if len(oracleOut.Rows) != len(anchors) {
				t.Fatalf("oracle produced %d rows, want %d (one per anchor; err=%v)",
					len(oracleOut.Rows), len(anchors), oracleOut.Err)
			}
			if len(cerberusOut.Rows) != len(anchors) {
				t.Fatalf("cerberus produced %d rows, want %d (one per anchor; err=%v)",
					len(cerberusOut.Rows), len(anchors), cerberusOut.Err)
			}

			// Pin the absolute Prometheus-correct value at the
			// dup-affected anchor (300 -> window (0, 300]) so a future
			// oracle regression cannot make both sides agree on a wrong
			// number. This is the cell the unfixed row-path emitter gets
			// wrong (50 -> 37.5 increase / 0.16666… -> 0.125 rate).
			pinTsMs := int64(gen.CounterDupEvalTsSec) * 1000
			if got, ok := valueAt(oracleOut, pinTsMs); !ok || !valuesWithinTol(got, tc.expected) {
				t.Fatalf("oracle value @%ds = %g (found=%v), want Prometheus-correct %g",
					gen.CounterDupEvalTsSec, got, ok, tc.expected)
			}

			if diff := property.CompareOutcomes(oracleOut, cerberusOut); diff != "" {
				t.Fatalf("path %s drift vs oracle across grid %d..%d/%ds\n--- query ---\n%s\n--- diff ---\n%s\n"+
					"(row path: a mismatch at @%ds is the deduped-vs-dup-inflated divergence the fix closes)",
					tc.name, rangeStartSeconds, rangeEndSeconds, rangeStepSeconds, tc.query, diff,
					gen.CounterDupEvalTsSec)
			}

			// Positive path proof: the comparator pass above cannot tell
			// the two lowerings apart (the fixed row path also returns the
			// deduped-correct value), so assert WHICH emitter actually ran.
			// The native subtest must have emitted timeSeriesRateToGrid;
			// the row subtests must NOT (they ran the arrayJoin fan-out the
			// dedup fix lives in).
			if got := cli.emittedNativeAggregate(); got != tc.native {
				t.Fatalf("path %s: emitted-native=%v, want %v (captured SQL:\n%s)",
					tc.name, got, tc.native, strings.Join(cli.sqls, "\n"))
			}
		})
	}
}

// gridAnchors returns the query_range grid anchors (unix seconds) for the
// half-open Prometheus stepping start, start+step, …, <= end.
func gridAnchors(startSec, endSec, stepSec int64) []int64 {
	var out []int64
	for ts := startSec; ts <= endSec; ts += stepSec {
		out = append(out, ts)
	}
	return out
}

// oracleRangeOutcome assembles the expected matrix by evaluating the
// instant oracle once per grid anchor and merging the per-anchor rows
// (each stamped at its own anchor) into a single Outcome — the shape the
// framework comparator pairs against the cerberus query_range matrix.
func oracleRangeOutcome(ds property.Dataset, query string, anchors []int64) property.Outcome {
	out := property.Outcome{}
	for _, ts := range anchors {
		o := oraclepromql.Evaluate(ds, property.Query{String: query, EvalTs: ts}, oraclepromql.Options{})
		if o.Err != nil {
			return property.Outcome{Err: o.Err}
		}
		out.Rows = append(out.Rows, o.Rows...)
	}
	return out
}

// valueAt returns the value of the (single-series) outcome's row stamped
// at tsMs, and whether such a row exists.
func valueAt(o property.Outcome, tsMs int64) (float64, bool) {
	for _, r := range o.Rows {
		if r.TimestampMs == tsMs {
			return r.Value, true
		}
	}
	return 0, false
}

// valuesWithinTol mirrors the framework comparator's absolute/relative
// 1e-9 tolerance for the single absolute-value pin above.
func valuesWithinTol(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	if d < 0 {
		d = -d
	}
	if d <= eps {
		return true
	}
	scale := a
	if scale < 0 {
		scale = -scale
	}
	return d <= eps*scale
}

// runCerberusRange GETs /api/v1/query_range and decodes the matrix
// response into a property.Outcome. The proof drives a single-point grid
// (start == end), so each returned series carries exactly one matrix
// value; that value is reshaped into the one OutcomeRow the comparator
// pairs against the oracle's instant evaluation at the same anchor.
func runCerberusRange(
	ctx context.Context, baseURL, query string, startSec, endSec, stepSec int64,
) property.Outcome {
	u := fmt.Sprintf(
		"%s/api/v1/query_range?query=%s&start=%d&end=%d&step=%d",
		baseURL, urlEscape(query), startSec, endSec, stepSec,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("property: build range request: %w", err)}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("property: range roundtrip: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("property: read range body: %w", err)}
	}

	var parsed struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
		Data      struct {
			ResultType string              `json:"resultType"`
			Result     []prom.MatrixSample `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return property.Outcome{
			Err: fmt.Errorf("property: decode range body: %w; status=%d body=%s",
				err, resp.StatusCode, body),
		}
	}
	if parsed.Status != "success" {
		return property.Outcome{
			Err: fmt.Errorf("cerberus query_range status=%q errorType=%q err=%q",
				parsed.Status, parsed.ErrorType, parsed.Error),
		}
	}
	if parsed.Data.ResultType != "matrix" {
		return property.Outcome{
			Err: fmt.Errorf("cerberus query_range resultType=%q, want matrix", parsed.Data.ResultType),
		}
	}

	out := property.Outcome{Rows: make([]property.OutcomeRow, 0, len(parsed.Data.Result))}
	for _, s := range parsed.Data.Result {
		stripped := make(map[string]string, len(s.Metric))
		for k, v := range s.Metric {
			if k == "__name__" {
				continue
			}
			stripped[k] = v
		}
		for _, sample := range s.Values {
			ts, val, perr := parseSample(sample)
			if perr != nil {
				return property.Outcome{Err: fmt.Errorf("property: parse range sample: %w", perr)}
			}
			out.Rows = append(out.Rows, property.OutcomeRow{
				Labels:      stripped,
				TimestampMs: ts,
				Value:       val,
			})
		}
	}
	return out
}
