package tempo_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
)

// metricsQueryRangeURL builds the test URL with proper escaping for
// the TraceQL `q` parameter — TraceQL queries always contain `{` / `}`,
// quotes, and pipes, so url.QueryEscape is mandatory.
func metricsQueryRangeURL(base, q string, params map[string]string) string {
	vals := url.Values{}
	vals.Set("q", q)
	for k, v := range params {
		vals.Set(k, v)
	}
	return base + "/api/metrics/query_range?" + vals.Encode()
}

// fixtureStart / fixtureEnd are the canonical test-time bookends used
// across handler tests in this package; matches handler_test.go's
// 2026-05-12T10:00:00Z anchor so a future seed swap doesn't have to
// touch every test.
const (
	fixtureStartUnix = "1778580000" // 2026-05-12T10:00:00Z
	fixtureEndUnix   = "1778580180" // +3m
)

// TestMetricsQueryRange_SingleSeriesNoGroupBy — a bare `| rate()` over
// the full spans table returns a single series (no labels). Matrix-shape
// zero-fill is the SQL emitter's concern (countIf over the per-anchor
// predicate, see internal/chsql/range_window.go), so the handler returns
// whatever row stream the cursor surfaced — three stub samples here
// pass through unchanged, sorted ascending by timestamp.
func TestMetricsQueryRange_SingleSeriesNoGroupBy(t *testing.T) {
	t.Parallel()

	ts := func(min int) time.Time {
		return time.Date(2026, 5, 12, 10, min, 0, 0, time.UTC)
	}
	// The matrix-shape SQL emits `map('__name__', 'rate')` for the
	// ungrouped Attributes projection (mirroring Tempo's
	// UngroupedAggregator wire shape — see
	// wrapMetricsForSample's doc-comment). The stub mimics that wire
	// projection so the handler's response-shaper sees the same
	// Labels map a real CH cursor would surface.
	q := &stubQuerier{samples: []chclient.Sample{
		// Intentionally out of order — handler must sort within each series.
		{MetricName: "", Labels: map[string]string{"__name__": "rate"}, Timestamp: ts(2), Value: 2.0},
		{MetricName: "", Labels: map[string]string{"__name__": "rate"}, Timestamp: ts(0), Value: 0.5},
		{MetricName: "", Labels: map[string]string{"__name__": "rate"}, Timestamp: ts(1), Value: 1.5},
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL, "{} | rate()", map[string]string{
		"start": fixtureStartUnix,
		"end":   fixtureEndUnix,
		"step":  "60s",
	})

	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	var body tempo.MetricsQueryRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Series) != 1 {
		t.Fatalf("expected 1 series, got %d: %+v", len(body.Series), body)
	}
	s := body.Series[0]
	// Ungrouped queries surface a single `__name__=<op>` label per
	// series (matching Tempo's UngroupedAggregator wire shape) — never
	// an empty label set. Previously cerberus emitted `{}` here and the
	// Tempo differ flagged the divergence as
	// `missing_in_a series key e3b0c44298fc1c14 (sha256("") prefix)`.
	if len(s.Labels) != 1 || s.Labels[0].Key != "__name__" || s.Labels[0].Value != "rate" {
		t.Errorf("expected single {__name__=rate} label for ungrouped rate(), got %+v", s.Labels)
	}
	// The stub returns 3 samples; the handler passes them through to
	// the wire envelope sorted ascending by timestamp. Zero-fill of
	// the matrix step grid lives in the SQL emitter
	// (internal/chsql/range_window.go), so the handler-level test
	// pins the three observed values' ascending order rather than
	// the full step grid (which only manifests against a real CH).
	if len(s.Samples) != 3 {
		t.Fatalf("expected 3 samples passed through from stub, got %d: %+v", len(s.Samples), s.Samples)
	}
	// Sorted ascending: 0.5, 1.5, 2.0 in that order.
	for i, want := range []float64{0.5, 1.5, 2.0} {
		if s.Samples[i].Value != want {
			t.Errorf("sample[%d].Value = %v, want %v", i, s.Samples[i].Value, want)
		}
	}
	for i := 1; i < len(s.Samples); i++ {
		if s.Samples[i-1].TimestampMs >= s.Samples[i].TimestampMs {
			t.Errorf("samples not sorted ascending by timestamp: %+v", s.Samples)
		}
	}

	// The handler should have asked the matrix-shape SQL emitter (an
	// arrayJoin fanout). Probe a few hallmark substrings to confirm
	// we're hitting emitRangeWindowMetrics and not a Sprintf fallback.
	assertSQLContains(t, q.lastSQL, "arrayJoin")
	assertSQLContains(t, q.lastSQL, "anchor_ts")
}

// TestMetricsQueryRange_MultiSeriesGroupBy — `| count_over_time() by
// (resource.service.name)` returns one series per unique service.
// Labels surface as {key,value} pairs in the response, ordered to match
// the by(...) attribute list.
func TestMetricsQueryRange_MultiSeriesGroupBy(t *testing.T) {
	t.Parallel()

	ts := func(min int) time.Time {
		return time.Date(2026, 5, 12, 10, min, 0, 0, time.UTC)
	}
	q := &stubQuerier{samples: []chclient.Sample{
		{Labels: map[string]string{"resource.service.name": "frontend"}, Timestamp: ts(0), Value: 12},
		{Labels: map[string]string{"resource.service.name": "frontend"}, Timestamp: ts(1), Value: 18},
		{Labels: map[string]string{"resource.service.name": "backend"}, Timestamp: ts(0), Value: 3},
		{Labels: map[string]string{"resource.service.name": "backend"}, Timestamp: ts(1), Value: 5},
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL,
		"{} | count_over_time() by (resource.service.name)",
		map[string]string{
			"start": fixtureStartUnix,
			"end":   fixtureEndUnix,
			"step":  "60s",
		})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	var body tempo.MetricsQueryRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Series) != 2 {
		t.Fatalf("expected 2 series, got %d: %+v", len(body.Series), body)
	}

	// Each series should have exactly one label entry whose Key is the
	// by(...) attribute name (TraceQL: resource.service.name).
	byService := map[string]tempo.MetricsSeries{}
	for _, s := range body.Series {
		if len(s.Labels) != 1 {
			t.Errorf("expected 1 label per series, got %+v", s.Labels)
			continue
		}
		if s.Labels[0].Key != "resource.service.name" {
			t.Errorf("expected label key 'resource.service.name', got %q", s.Labels[0].Key)
		}
		byService[s.Labels[0].Value] = s
	}
	if _, ok := byService["frontend"]; !ok {
		t.Errorf("missing 'frontend' series: %+v", body.Series)
	}
	if _, ok := byService["backend"]; !ok {
		t.Errorf("missing 'backend' series: %+v", body.Series)
	}
	// Each series surfaces the stub's per-(group, anchor) rows
	// unchanged (two per service). Matrix-grid zero-fill is the SQL
	// emitter's concern (internal/chsql/range_window.go); the handler
	// only pivots the row stream into the Tempo series envelope.
	if len(byService["frontend"].Samples) != 2 {
		t.Errorf("expected 2 stub samples for frontend, got %d: %+v",
			len(byService["frontend"].Samples), byService["frontend"].Samples)
	}
	if len(byService["backend"].Samples) != 2 {
		t.Errorf("expected 2 stub samples for backend, got %d: %+v",
			len(byService["backend"].Samples), byService["backend"].Samples)
	}
}

// TestMetricsQueryRange_EmptyResult — when CH returns zero rows the
// response is `{series: []}` (not `null`). Grafana's Tempo datasource
// short-circuits on a `null` series array, which would surface as a
// dashboard "no data" badge for a healthy gateway with no spans yet.
func TestMetricsQueryRange_EmptyResult(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: nil}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL, "{} | rate()", map[string]string{
		"start": fixtureStartUnix,
		"end":   fixtureEndUnix,
		"step":  "30s",
	})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var body tempo.MetricsQueryRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Series == nil {
		t.Fatalf("expected non-nil empty Series slice, got nil (JSON null)")
	}
	if len(body.Series) != 0 {
		t.Fatalf("expected 0 series, got %d", len(body.Series))
	}
}

// TestMetricsQueryRange_ZeroFillSQLShapeCountOverTime pins the contract
// that count_over_time / rate matrix queries push the per-anchor window
// predicate into a `countIf(...)` reducer (rather than the outer WHERE
// clause). The inner-fanout subquery materialises one (group, anchor)
// row per Inner row × N anchors, so the outer GROUP BY emits a row at
// every (observed-group, anchor) tuple regardless of whether any
// sample landed in (anchor_ts - range, anchor_ts] — countIf returns 0
// for empty anchors. This is the SQL-level zero-fill that replaced the
// handler-side `zeroFillMatrixGrid` post-pass (see PR removing the
// Go-side fill).
//
// The handler-with-stub harness can't observe the fill end-to-end
// because the stub returns pre-built `[]chclient.Sample` rather than
// executing SQL; the assertion lives on the captured `q.lastSQL`
// instead, with the SQL contract that the runtime executes proven by
// chsql-layer unit tests + the compatibility suite against a real CH.
func TestMetricsQueryRange_ZeroFillSQLShapeCountOverTime(t *testing.T) {
	t.Parallel()

	for _, query := range []string{
		"{} | count_over_time()",
		"{} | rate()",
	} {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			q := &stubQuerier{samples: nil}
			srv := newServer(q, "v1.0.0-test")
			t.Cleanup(srv.Close)

			u := metricsQueryRangeURL(srv.URL, query, map[string]string{
				"start": fixtureStartUnix,
				"end":   fixtureEndUnix,
				"step":  "60s",
			})
			resp, err := http.Get(u)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
			}

			// The handler issues two SQL statements per request:
			// the matrix-shape query (arrayJoin fanout) plus the
			// exemplar lookup. Reach the matrix SQL via the
			// arrayJoin needle so the assertion targets the right
			// statement; q.lastSQL would point at the exemplar
			// query (last one executed).
			matrixSQL := findSQLContaining(t, q.queriedSQLs, "arrayJoin")
			// Reducer shape: countIf(<window pred>) — the per-anchor
			// window predicate lives inside the aggregate, not the
			// outer WHERE clause.
			assertSQLContains(t, matrixSQL,
				"countIf(ts > anchor_ts - toIntervalNanosecond(")
			// The outer WHERE that the legacy path produced is now
			// suppressed for count/rate (the window predicate moved
			// into countIf). Pin the absence so a regression toward
			// the old shape surfaces here.
			if strings.Contains(matrixSQL, "WHERE ts > anchor_ts -") {
				t.Errorf("count/rate matrix SQL must not carry an outer WHERE on the window predicate (it moved into countIf); SQL=%s", matrixSQL)
			}
		})
	}
}

// TestMetricsQueryRange_ZeroFillSkippedForOverTimeAggs pins the
// contract that the SQL emitter does NOT push the window predicate
// into a conditional aggregate for sum / avg / min / max _over_time —
// it stays in the outer WHERE clause. Tempo's OverTimeAggregator
// initialises its value to NaN and the SeriesSet.ToProto loop skips
// NaN samples (`pkg/traceql/engine_metrics.go`'s OverTimeAggregator +
// ToProto), so the observed-only emission is the wire-correct match —
// pushing the predicate into a sumIf / avgIf would emit 0 for empty
// buckets and inject false zeros where Tempo emits nothing.
//
// `quantile_over_time` is the inverse case (HistogramAggregator.Results
// emits 0.0 at empty buckets, not NaN) and is asserted by
// TestMetricsQueryRange_ZeroFillSQLShapeQuantileOverTime below.
func TestMetricsQueryRange_ZeroFillSkippedForOverTimeAggs(t *testing.T) {
	t.Parallel()

	mid := time.Date(2026, 5, 12, 10, 1, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
		op    string
	}{
		{"avg_over_time", "{} | avg_over_time(duration)", "avg_over_time"},
		{"sum_over_time", "{} | sum_over_time(duration)", "sum_over_time"},
		{"min_over_time", "{} | min_over_time(duration)", "min_over_time"},
		{"max_over_time", "{} | max_over_time(duration)", "max_over_time"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			q := &stubQuerier{samples: []chclient.Sample{{
				MetricName: "",
				// Matrix-shape SQL projects `map('__name__',
				// '<op>')` for ungrouped queries (see
				// wrapMetricsForSample); stub mimics the cursor's
				// surfaced Labels map.
				Labels:    map[string]string{"__name__": tc.op},
				Timestamp: mid,
				Value:     42.0,
			}}}
			srv := newServer(q, "v1.0.0-test")
			t.Cleanup(srv.Close)

			u := metricsQueryRangeURL(srv.URL, tc.query, map[string]string{
				"start": fixtureStartUnix,
				"end":   fixtureEndUnix,
				"step":  "60s",
			})
			resp, err := http.Get(u)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
			}
			var body tempo.MetricsQueryRangeResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(body.Series) != 1 {
				t.Fatalf("expected 1 series, got %d", len(body.Series))
			}
			s := body.Series[0]
			if len(s.Samples) != 1 {
				t.Errorf("expected 1 observed sample for %s (NaN-skip path: no SQL-side fill), got %d: %+v",
					tc.op, len(s.Samples), s.Samples)
			}
			if len(s.Samples) > 0 && s.Samples[0].Value != 42.0 {
				t.Errorf("expected observed sample value 42, got %v", s.Samples[0].Value)
			}
			// SQL contract: the window predicate stays in the outer
			// WHERE for NaN-skip operators — pushing it into a sumIf /
			// avgIf would surface 0 at empty buckets and diverge from
			// Tempo's NaN-skip emit. Pin the WHERE shape so a future
			// refactor doesn't silently extend the countIf path. The
			// matrix-shape SQL is the arrayJoin-fanout statement (the
			// handler also fires an exemplar lookup).
			if len(q.queriedSQLs) > 0 {
				matrixSQL := findSQLContaining(t, q.queriedSQLs, "arrayJoin")
				assertSQLContains(t, matrixSQL,
					"WHERE ts > anchor_ts - toIntervalNanosecond(")
				if strings.Contains(matrixSQL, "countIf(") {
					t.Errorf("%s SQL must not use countIf (NaN-skip op), got %s", tc.op, matrixSQL)
				}
			}
		})
	}
}

// TestMetricsQueryRange_ZeroFillSQLShapeQuantileOverTime pins the
// SQL-emit contract that ensures `quantile_over_time` matrix queries
// produce a row at every (observed-group, anchor) tuple — even anchors
// where no spans landed — to match Tempo's reference
// HistogramAggregator.Results emit of `ts.Values[i] = 0.0` for empty
// buckets. The chsql emitter wraps the per-row bucket projection in
// `if(<window pred>, <real bucket>, 0)` and the count in
// `countIf(<window pred>)` — empty anchors fall into a phantom
// `__bucket=0 / count=0` row that the handler's post-processor
// (Log2QuantileWithBucket) resolves to a 0 sample.
//
// This replaced the Go-side `zeroFillMatrixGrid` post-pass that lived
// in the handler before; the assertion lives on the captured SQL
// because the handler-with-stub harness can't observe the SQL fill
// (the stub returns pre-built rows rather than executing SQL). The
// runtime fill is covered by chsql-layer unit tests
// (TestRangeWindowMetricsQuantileBuckets*) plus the tempo
// compatibility suite running against a real ClickHouse.
func TestMetricsQueryRange_ZeroFillSQLShapeQuantileOverTime(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: nil}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL,
		"{} | quantile_over_time(duration, 0.95)",
		map[string]string{
			"start": fixtureStartUnix,
			"end":   fixtureEndUnix,
			"step":  "60s",
		})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	// The handler issues two SQL statements per request: matrix + the
	// exemplar lookup. Reach the matrix SQL via the arrayJoin needle
	// so the assertion targets the right statement.
	matrixSQL := findSQLContaining(t, q.queriedSQLs, "arrayJoin")
	// Bucket projection wraps the real bucket in `if(<pred>, ..., 0)`
	// so non-matching rows fall into the phantom 0-bucket group. The
	// count wraps in `countIf(<pred>)` so observed buckets count
	// matched rows and the phantom buckets count zero.
	assertSQLContains(t, matrixSQL,
		"if(ts > anchor_ts - toIntervalNanosecond(")
	assertSQLContains(t, matrixSQL, ", 0)")
	assertSQLContains(t, matrixSQL,
		"countIf(ts > anchor_ts - toIntervalNanosecond(")
	// The outer WHERE that the legacy quantile path produced is now
	// suppressed (the window predicate + bucketize-min guard moved
	// into the conditional projections). Pin the absence so a
	// regression toward the old shape surfaces here.
	if strings.Contains(matrixSQL, "WHERE ts > anchor_ts -") {
		t.Errorf("quantile matrix SQL must not carry an outer WHERE on the window predicate; SQL=%s", matrixSQL)
	}
}

// findSQLContaining returns the first SQL string in haystack that
// contains needle; fails the test if no match is found. Used to pluck
// the matrix-shape SQL out of stubQuerier.queriedSQLs (which also
// records the follow-up exemplar lookup) so assertions don't
// accidentally bind to the wrong statement.
func findSQLContaining(t *testing.T, haystack []string, needle string) string {
	t.Helper()
	for _, sql := range haystack {
		if strings.Contains(sql, needle) {
			return sql
		}
	}
	t.Fatalf("no SQL containing %q in %d recorded statements: %v", needle, len(haystack), haystack)
	return ""
}

// TestMetricsQueryRange_DurationAggInSeconds pins the contract that
// duration-based *_over_time aggregations emit values in seconds, not
// raw nanoseconds. The OTel-CH Duration column is Int64 ns; Tempo's
// reference engine divides by 1e9 before producing the InstantSeries
// value (see pkg/traceql/engine_metrics.go: sumOverTimeAggregator /
// averageOverTimeAggregator / quantileOverTimeAggregator all
// divide-by-1e9). Cerberus matches by wrapping the lowered Duration
// expression in `<expr> / 1e9` at lowering time
// (internal/traceql/metrics_pipeline.go: metricsAggregateAttr); this
// test asserts the wrap survives end-to-end by probing the emitted SQL.
//
// Without the wrap the Tempo compat differ flagged
// `metrics_avg_over_time_instant` with a ~1e9 ratio between cerberus
// (raw ns) and Tempo (seconds). Pinning the SQL shape here keeps that
// regression from re-emerging.
func TestMetricsQueryRange_DurationAggInSeconds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
		// labels mimics the row Labels the matrix SQL projects for this
		// op. Non-quantile ungrouped ops surface `__name__=<op>` (Tempo
		// UngroupedAggregator parity); quantile_over_time surfaces
		// `__bucket=<float>` (the bucket-shape row stream the
		// post-processor consumes — Tempo HistogramAggregator wire
		// shape is synthesised by `postProcessQuantileBuckets`).
		labels map[string]string
		// stubValue picks the Value the stub returns. Per-bucket counts
		// for quantile_over_time are integer-typed (CH `count(1)`); the
		// other ops carry the metric value directly. Tracked separately
		// so the quantile post-processor sees a count rather than a
		// pre-quantile float.
		stubValue float64
	}{
		{"avg_over_time", "{} | avg_over_time(duration)", map[string]string{"__name__": "avg_over_time"}, 0.5},
		{"sum_over_time", "{} | sum_over_time(duration)", map[string]string{"__name__": "sum_over_time"}, 0.5},
		{"min_over_time", "{} | min_over_time(duration)", map[string]string{"__name__": "min_over_time"}, 0.5},
		{"max_over_time", "{} | max_over_time(duration)", map[string]string{"__name__": "max_over_time"}, 0.5},
		// Bucket at 0.5s → Log2QuantileWithBucket(0.95, [{0.5, 5}])
		// returns 0.5 (the bucket Max, since the per-quantile walk
		// consumes the whole single bucket and reports b.Max). That's
		// what surfaces in the Value column of the response sample.
		{"quantile_over_time", "{} | quantile_over_time(duration, 0.95)", map[string]string{"__bucket": "0.5"}, 5},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mid := time.Date(2026, 5, 12, 10, 1, 0, 0, time.UTC)
			q := &stubQuerier{samples: []chclient.Sample{{
				MetricName: "",
				Labels:     tc.labels,
				Timestamp:  mid,
				// Seed a value mimicking what CH would return after
				// the `/ 1e9` divisor — a sub-second duration. The
				// stub does not actually run SQL, so we're asserting
				// the handler propagates the value verbatim; the
				// real ns→s rebase happens server-side in the
				// emitted SQL and is asserted via assertSQLContains
				// below.
				Value: tc.stubValue,
			}}}
			srv := newServer(q, "v1.0.0-test")
			t.Cleanup(srv.Close)

			u := metricsQueryRangeURL(srv.URL, tc.query, map[string]string{
				"start": fixtureStartUnix,
				"end":   fixtureEndUnix,
				"step":  "60s",
			})
			resp, err := http.Get(u)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
			}

			// The emitted SQL must contain the `/ ?` division that
			// rebases the raw-ns Duration column into fractional
			// seconds before the per-bucket reducer (sum / avg / min
			// / max / quantile) sees it. Without this guard, a
			// future refactor that bypasses metricsAggregateAttr's
			// duration branch would silently regress every Tempo
			// compat case that consumes the `Duration` intrinsic.
			assertSQLContains(t, q.lastSQL, "`Duration` / toFloat64(?)")

			// The 1e9 divisor must appear in the args list — locks in
			// the parameterised form rather than an inline literal
			// (the chplan IR carries data through `?` placeholders).
			foundDivisor := false
			for _, a := range q.lastArgs {
				if f, ok := a.(float64); ok && f == 1e9 {
					foundDivisor = true
					break
				}
			}
			if !foundDivisor {
				t.Errorf("expected 1e9 divisor in args, got %v", q.lastArgs)
			}

			// The stubbed sub-second value must surface verbatim in
			// the response — the handler does not (and must not)
			// further rescale the Value.
			var body tempo.MetricsQueryRangeResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(body.Series) != 1 {
				t.Fatalf("expected 1 series, got %d: %+v", len(body.Series), body)
			}
			// Find the sample that matches the stubbed Timestamp;
			// other anchors get zero-fill or NaN-skip per Op
			// (irrelevant to this test — we care about value
			// fidelity for the observed bucket).
			midMs := mid.UnixMilli()
			var observed *tempo.MetricsSample
			for i := range body.Series[0].Samples {
				if body.Series[0].Samples[i].TimestampMs == midMs {
					observed = &body.Series[0].Samples[i]
					break
				}
			}
			if observed == nil {
				t.Fatalf("no sample matches stubbed timestamp %d: %+v", midMs, body.Series[0].Samples)
			}
			if observed.Value != 0.5 {
				t.Errorf("expected sub-second value 0.5 to pass through unchanged, got %v", observed.Value)
			}
		})
	}
}

// TestMetricsQueryRange_BadInputs covers the 4xx surface: missing or
// malformed `q` / `start` / `end` / `step`, and a non-metric TraceQL
// query (one that lowers to a Scan/Filter rather than a
// MetricsAggregate).
func TestMetricsQueryRange_BadInputs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		params  map[string]string
		wantSub string
	}{
		{
			name:    "missing_q",
			params:  map[string]string{"start": fixtureStartUnix, "end": fixtureEndUnix, "step": "30s"},
			wantSub: "missing 'q'",
		},
		{
			name:    "missing_step",
			params:  map[string]string{"q": "{} | rate()", "start": fixtureStartUnix, "end": fixtureEndUnix},
			wantSub: "missing 'step'",
		},
		{
			name:    "missing_start",
			params:  map[string]string{"q": "{} | rate()", "end": fixtureEndUnix, "step": "30s"},
			wantSub: "required",
		},
		{
			name:    "missing_end",
			params:  map[string]string{"q": "{} | rate()", "start": fixtureStartUnix, "step": "30s"},
			wantSub: "required",
		},
		{
			name: "malformed_step",
			params: map[string]string{
				"q":     "{} | rate()",
				"start": fixtureStartUnix,
				"end":   fixtureEndUnix,
				"step":  "five-minutes",
			},
			wantSub: "step",
		},
		{
			name: "zero_step",
			params: map[string]string{
				"q":     "{} | rate()",
				"start": fixtureStartUnix,
				"end":   fixtureEndUnix,
				"step":  "0s",
			},
			wantSub: "step",
		},
		{
			name: "malformed_time",
			params: map[string]string{
				"q":     "{} | rate()",
				"start": "not-a-time",
				"end":   fixtureEndUnix,
				"step":  "30s",
			},
			wantSub: "time",
		},
		{
			name: "end_before_start",
			params: map[string]string{
				"q":     "{} | rate()",
				"start": fixtureEndUnix,
				"end":   fixtureStartUnix,
				"step":  "30s",
			},
			wantSub: "before",
		},
		{
			name: "parse_error",
			params: map[string]string{
				"q":     "this is not traceql {{{",
				"start": fixtureStartUnix,
				"end":   fixtureEndUnix,
				"step":  "30s",
			},
			wantSub: "",
		},
		{
			name: "non_metric_query",
			params: map[string]string{
				// Bare spanset with no metrics pipeline — lowers to a
				// Filter(Scan), not a MetricsAggregate.
				"q":     `{ resource.service.name = "frontend" }`,
				"start": fixtureStartUnix,
				"end":   fixtureEndUnix,
				"step":  "30s",
			},
			wantSub: "metrics-pipeline",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{}
			srv := newServer(q, "v1.0.0-test")
			t.Cleanup(srv.Close)

			vals := url.Values{}
			for k, v := range tc.params {
				vals.Set(k, v)
			}
			u := srv.URL + "/api/metrics/query_range?" + vals.Encode()

			resp, err := http.Get(u)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode < 400 || resp.StatusCode >= 500 {
				t.Fatalf("expected 4xx, got status=%d body=%s",
					resp.StatusCode, readBody(t, resp))
			}
			var er tempo.ErrorResponse
			body := readBody(t, resp)
			if err := json.Unmarshal([]byte(body), &er); err != nil {
				t.Fatalf("decode: %v body=%s", err, body)
			}
			if !er.Error {
				t.Errorf("expected error=true, got %+v", er)
			}
			if tc.wantSub != "" && !strings.Contains(er.Message, tc.wantSub) {
				t.Errorf("error message missing %q: got %q", tc.wantSub, er.Message)
			}
		})
	}
}

// TestMetricsQueryRange_CHFailure — a CH-side error surfaces as 502 +
// the Tempo error envelope so Grafana renders the right "data source
// error" UI rather than a generic 5xx page.
func TestMetricsQueryRange_CHFailure(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: errors.New("clickhouse: connection refused")}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL, "{} | rate()", map[string]string{
		"start": fixtureStartUnix,
		"end":   fixtureEndUnix,
		"step":  "30s",
	})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 body=%s", resp.StatusCode, readBody(t, resp))
	}
	var er tempo.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !er.Error || !strings.Contains(er.Message, "connection refused") {
		t.Errorf("expected error envelope with upstream message; got %+v", er)
	}
}

// TestMetricsQueryRange_LabelWireShape pins the tempopb KeyValue +
// AnyValue wire shape for `{"labels":[...]}`. Grafana's Tempo datasource
// (and any other consumer parsing through `gogo/protobuf/jsonpb` against
// `pkg/tempopb/common/v1.KeyValue`) requires the typed AnyValue envelope
// `{"key":"k","value":{"stringValue":"v"}}` — the flat string form
// cerberus used to emit (`{"key":"k","value":"v"}`) silently round-trips
// to an empty AnyValue on the consumer side. EF #398 caught this in the
// tempo-compatibility harness; this test pins the fixed wire shape so a
// future refactor can't quietly regress it.
func TestMetricsQueryRange_LabelWireShape(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: []chclient.Sample{{
		Labels:    map[string]string{"resource.service.name": "frontend"},
		Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
		Value:     1.0,
	}}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL,
		"{} | count_over_time() by (resource.service.name)",
		map[string]string{
			"start": fixtureStartUnix,
			"end":   fixtureEndUnix,
			"step":  "60s",
		})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	// Read raw body so we assert wire shape without going through the
	// MarshalJSON we just defined (i.e. the assertion is on the bytes
	// Grafana actually receives).
	raw := readBody(t, resp)
	wantSub := `"value":{"stringValue":"frontend"}`
	if !strings.Contains(raw, wantSub) {
		t.Fatalf("response missing tempopb AnyValue shape %q\nbody=%s", wantSub, raw)
	}
	// And the legacy flat shape MUST NOT appear.
	badSub := `"value":"frontend"`
	if strings.Contains(raw, badSub) {
		t.Errorf("response still emits legacy flat label shape %q\nbody=%s", badSub, raw)
	}

	// Decode through MetricsQueryRangeResponse and verify the in-process
	// struct still surfaces `frontend` so handler callers stay ergonomic.
	var body tempo.MetricsQueryRangeResponse
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode tempopb shape: %v", err)
	}
	if len(body.Series) != 1 || len(body.Series[0].Labels) != 1 {
		t.Fatalf("unexpected shape: %+v", body)
	}
	if body.Series[0].Labels[0].Key != "resource.service.name" ||
		body.Series[0].Labels[0].Value != "frontend" {
		t.Errorf("decoded label = %+v, want {resource.service.name, frontend}",
			body.Series[0].Labels[0])
	}

	// Round-trip: also tolerate the legacy flat shape so an old consumer
	// pushing data into the type (or an older replay fixture) doesn't
	// break the decoder side of the contract.
	legacyJSON := []byte(`{"series":[{"labels":[{"key":"k","value":"v"}],"samples":[]}]}`)
	var legacy tempo.MetricsQueryRangeResponse
	if err := json.Unmarshal(legacyJSON, &legacy); err != nil {
		t.Fatalf("decode legacy flat shape: %v", err)
	}
	if len(legacy.Series) != 1 || len(legacy.Series[0].Labels) != 1 ||
		legacy.Series[0].Labels[0].Key != "k" || legacy.Series[0].Labels[0].Value != "v" {
		t.Errorf("legacy flat decode = %+v, want {k, v}", legacy.Series[0].Labels)
	}
}

// TestMetricsQueryRange_StepDurationForms — accepts integer seconds,
// float seconds, and Go duration strings interchangeably. Matches the
// PromQL handler's tolerance so Grafana's Tempo datasource (which can
// send either shape) interoperates.
func TestMetricsQueryRange_StepDurationForms(t *testing.T) {
	t.Parallel()

	for _, step := range []string{"30s", "0.5m", "30", "1m"} {
		step := step
		t.Run(fmt.Sprintf("step=%s", step), func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{samples: []chclient.Sample{{
				Labels:    map[string]string{},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
				Value:     1.0,
			}}}
			srv := newServer(q, "v1.0.0-test")
			t.Cleanup(srv.Close)
			u := metricsQueryRangeURL(srv.URL, "{} | rate()", map[string]string{
				"start": fixtureStartUnix,
				"end":   fixtureEndUnix,
				"step":  step,
			})
			resp, err := http.Get(u)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
			}
		})
	}
}

// TestMetricsQueryRange_ExemplarsEnvelope pins the wire-shape contract
// that every MetricsSeries emits `"exemplars": []` even before cerberus
// populates them. Grafana's Tempo datasource expects the field, so
// omitting it (or rendering it as null) destabilises the envelope. See
// EF #398 for the broader Tempo metrics shape parity work.
//
// Sub-test guard: feed a stub that returns the matrix-shape sample but
// NO exemplar samples (samplesBySQL with an `exemplar_trace_id` needle
// returns nil). The empty-array envelope contract still holds.
func TestMetricsQueryRange_ExemplarsEnvelope(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		samples: []chclient.Sample{{
			Labels:    map[string]string{},
			Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
			Value:     1.0,
		}},
		samplesBySQL: map[string][]chclient.Sample{
			"exemplar_trace_id": nil,
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL, "{} | rate()", map[string]string{
		"start": fixtureStartUnix,
		"end":   fixtureEndUnix,
		"step":  "60s",
	})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	raw := readBody(t, resp)
	if !strings.Contains(raw, `"exemplars":[]`) {
		t.Fatalf("response missing empty exemplars array\nbody=%s", raw)
	}
}

// TestMetricsQueryRange_ExemplarsPopulated exercises the end-to-end
// data path now that the exemplars query landed: the handler fires
// BOTH a matrix-shape query (anchor-fanout / Sample projection) and an
// exemplars-shape query (argMax over `exemplar_trace_id` /
// `exemplar_span_id`), then merges them so each MetricsSeries surfaces
// trace-anchored Exemplar entries.
//
// Stub layout: the matrix branch returns one sample per anchor; the
// exemplars branch returns a Sample whose Labels map carries the
// `trace:id`, `span:id`, and the by(...) alias values — same shape
// chsql.EmitMetricsExemplars projects via the outer `map(...) AS
// Attributes` column. attachExemplars keys each exemplar against its
// matching series via the by(...) label canonical key.
//
// Note: the stubbed Labels map keys use the Tempo-canonical wire form
// (`resource.service.name`) rather than the SQL-side alias
// (`service.name`). The chsql emitter projects the inner SELECT with
// `ResourceAttributes['service.name'] AS service.name` (the SQL alias
// drops the scope prefix to keep column names compact), but the outer
// matrix and exemplar projections key the `Attributes` map by the
// scope-prefixed display name so the wire shape matches upstream
// Tempo's metrics-query response. attachExemplars matches each
// exemplar to its parent series via canonical label-set hash, so both
// the matrix branch and exemplars branch must agree on the key form.
func TestMetricsQueryRange_ExemplarsPopulated(t *testing.T) {
	t.Parallel()

	ts := func(min int) time.Time {
		return time.Date(2026, 5, 12, 10, min, 0, 0, time.UTC)
	}
	q := &stubQuerier{
		samples: []chclient.Sample{
			// Matrix branch — one series, two anchors. Labels are keyed
			// by the Tempo-canonical scope-prefixed wire name
			// (`resource.service.name`), matching what
			// wrapMetricsForSample projects via the outer `Attributes`
			// map.
			{
				Labels:    map[string]string{"resource.service.name": "frontend"},
				Timestamp: ts(0),
				Value:     12,
			},
			{
				Labels:    map[string]string{"resource.service.name": "frontend"},
				Timestamp: ts(1),
				Value:     18,
			},
		},
		samplesBySQL: map[string][]chclient.Sample{
			// Exemplars branch — one trace-anchored sample per anchor,
			// carrying the trace:id + span:id pair attachExemplars
			// surfaces under Exemplar.TraceID / SpanID. The by(...)
			// display key (`resource.service.name`) lets
			// attachExemplars match the exemplar back to its parent
			// series by canonical label-set hash.
			"exemplar_trace_id": {
				{
					Labels: map[string]string{
						"resource.service.name": "frontend",
						"trace:id":              "0123456789abcdef0123456789abcdef",
						"span:id":               "0011223344556677",
					},
					Timestamp: ts(0),
					Value:     1,
				},
				{
					Labels: map[string]string{
						"resource.service.name": "frontend",
						"trace:id":              "fedcba9876543210fedcba9876543210",
						"span:id":               "aabbccddeeff0011",
					},
					Timestamp: ts(1),
					Value:     1,
				},
			},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL,
		"{} | count_over_time() by (resource.service.name)",
		map[string]string{
			"start": fixtureStartUnix,
			"end":   fixtureEndUnix,
			"step":  "60s",
		})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	// The handler must have issued BOTH the matrix-shape query and the
	// exemplars-shape query (recorded via samplesBySQL needle).
	var matrixFired, exemplarFired bool
	for _, sql := range q.queriedSQLs {
		if strings.Contains(sql, "exemplar_trace_id") {
			exemplarFired = true
		} else if strings.Contains(sql, "arrayJoin") {
			matrixFired = true
		}
	}
	if !matrixFired {
		t.Errorf("expected a matrix-shape (arrayJoin fanout) query to fire; saw %d SQL statements", len(q.queriedSQLs))
	}
	if !exemplarFired {
		t.Errorf("expected an exemplars-shape (argMax over exemplar_trace_id) query to fire; saw %d SQL statements", len(q.queriedSQLs))
	}

	var body tempo.MetricsQueryRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Series) != 1 {
		t.Fatalf("expected 1 series, got %d: %+v", len(body.Series), body)
	}
	got := body.Series[0]
	// Stub returns the two observed-anchor rows; the handler passes
	// them through without modification. Matrix-grid zero-fill lives
	// in the SQL emitter (internal/chsql/range_window.go), so the
	// handler-with-stub harness only sees the rows the stub yields.
	if len(got.Samples) != 2 {
		t.Errorf("expected 2 stub samples to pass through unchanged, got %d: %+v", len(got.Samples), got.Samples)
	}
	if len(got.Exemplars) != 2 {
		t.Fatalf("expected 2 exemplars, got %d: %+v", len(got.Exemplars), got.Exemplars)
	}

	// Exemplars must be sorted ascending by timestamp and carry the
	// stubbed (TraceID, SpanID) pairs verbatim — attachExemplars
	// reads `trace:id` / `span:id` off Sample.Labels and projects them
	// to the Exemplar.{TraceID,SpanID} fields plus the labels slice.
	if got.Exemplars[0].Timestamp >= got.Exemplars[1].Timestamp {
		t.Errorf("exemplars not sorted ascending: %+v", got.Exemplars)
	}
	wantTraceIDs := []string{
		"0123456789abcdef0123456789abcdef",
		"fedcba9876543210fedcba9876543210",
	}
	wantSpanIDs := []string{
		"0011223344556677",
		"aabbccddeeff0011",
	}
	for i, want := range wantTraceIDs {
		if got.Exemplars[i].TraceID != want {
			t.Errorf("exemplar[%d].TraceID = %q, want %q", i, got.Exemplars[i].TraceID, want)
		}
	}
	for i, want := range wantSpanIDs {
		if got.Exemplars[i].SpanID != want {
			t.Errorf("exemplar[%d].SpanID = %q, want %q", i, got.Exemplars[i].SpanID, want)
		}
	}

	// And the Exemplar.Labels slice must carry the trace:id + span:id
	// MetricsLabel entries so consumers binding to the labels array
	// (rather than the typed scalar fields) see the same data.
	for i, ex := range got.Exemplars {
		if len(ex.Labels) != 2 {
			t.Errorf("exemplar[%d].Labels: got %d entries, want 2: %+v", i, len(ex.Labels), ex.Labels)
			continue
		}
		if ex.Labels[0].Key != "trace:id" || ex.Labels[0].Value != wantTraceIDs[i] {
			t.Errorf("exemplar[%d].Labels[0] = %+v, want {trace:id, %q}", i, ex.Labels[0], wantTraceIDs[i])
		}
		if ex.Labels[1].Key != "span:id" || ex.Labels[1].Value != wantSpanIDs[i] {
			t.Errorf("exemplar[%d].Labels[1] = %+v, want {span:id, %q}", i, ex.Labels[1], wantSpanIDs[i])
		}
	}
}

// TestMetricsQueryRange_ResourceLabelWireShape conforms cerberus's
// metrics-query response to upstream Tempo's wire shape for
// resource-scoped group-by labels.
//
// Upstream Tempo emits the full scope-prefixed form
// `resource.service.name` on the response Labels list for a
// `by (resource.service.name)` clause (see grafana/tempo
// `pkg/traceql.Attribute.String` and `engine_metrics.go::labelsFor`;
// the upstream integration test `integration/api/query_range_test.go`
// pins `label.Key == "resource.res_attr"` for the equivalent query).
//
// Cerberus's SQL emitter aliases the inner SELECT column as the bare
// path (`AS service.name`) to keep CH column names short, but the
// matrix-shape outer wrap projects the `Attributes` map with the
// Tempo-canonical scope-prefixed key — so the chclient.Sample.Labels
// the handler decodes carries `resource.service.name`, and the JSON
// envelope mirrors that. This test feeds a stub Sample whose Labels
// map uses the wire-canonical key (matching what the matrix SQL would
// produce post-wrap) and asserts the JSON `labels[].key` reads
// `resource.service.name`.
func TestMetricsQueryRange_ResourceLabelWireShape(t *testing.T) {
	t.Parallel()

	ts := func(min int) time.Time {
		return time.Date(2026, 5, 12, 10, min, 0, 0, time.UTC)
	}
	q := &stubQuerier{samples: []chclient.Sample{
		// The matrix outer SELECT emits `map('resource.service.name', toString(service.name), ...)`
		// — see wrapMetricsForSample. The decoded Sample.Labels map is
		// therefore keyed by the scope-prefixed wire form, not the bare
		// SQL alias. The stub mirrors that contract.
		{Labels: map[string]string{"resource.service.name": "frontend"}, Timestamp: ts(0), Value: 12},
		{Labels: map[string]string{"resource.service.name": "frontend"}, Timestamp: ts(1), Value: 18},
		{Labels: map[string]string{"resource.service.name": "backend"}, Timestamp: ts(0), Value: 3},
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL,
		"{} | count_over_time() by (resource.service.name)",
		map[string]string{
			"start": fixtureStartUnix,
			"end":   fixtureEndUnix,
			"step":  "60s",
		})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	raw := readBody(t, resp)
	// The Tempo-canonical wire key MUST appear verbatim (the assertion
	// is on the bytes Grafana / a tempopb consumer actually reads).
	wantSub := `"key":"resource.service.name"`
	if !strings.Contains(raw, wantSub) {
		t.Fatalf("response missing wire-canonical resource-scope key %q\nbody=%s", wantSub, raw)
	}
	// And the bare-alias form (the SQL-side alias) MUST NOT appear as a
	// response key — that would mean the wrap leaked the alias to the
	// wire instead of the scope-prefixed display name.
	badSub := `"key":"service.name"`
	if strings.Contains(raw, badSub) {
		t.Errorf("response surfaces bare SQL alias %q where the Tempo-canonical form is required\nbody=%s",
			badSub, raw)
	}

	// Decode the response through the typed struct and verify every
	// series's first (and only) label carries the prefixed key.
	var body tempo.MetricsQueryRangeResponse
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Series) != 2 {
		t.Fatalf("expected 2 series, got %d: %+v", len(body.Series), body)
	}
	for i, s := range body.Series {
		if len(s.Labels) != 1 {
			t.Errorf("series[%d]: expected 1 label, got %+v", i, s.Labels)
			continue
		}
		if s.Labels[0].Key != "resource.service.name" {
			t.Errorf("series[%d].Labels[0].Key = %q, want %q",
				i, s.Labels[0].Key, "resource.service.name")
		}
	}
}

// TestMetricsQueryRange_QuantileOverTimeWireShape pins the
// HistogramAggregator parity contract for `quantile_over_time` end to
// end: every per-phi series MUST carry a `p="<phi>"` label and MUST
// NOT carry `__name__="quantile_over_time"`. Tempo routes
// quantile_over_time through HistogramAggregator
// (engine_metrics.go::HistogramAggregator.Results), which appends
// `Label{"p", NewStaticFloat(q)}` to every series — so the
// tempo-compat differ canonicalises each series under that key. Before
// the bucket-shape rewrite cerberus delegated quantile computation to
// CH's `quantile()` aggregate, whose linear interpolation diverged
// from Tempo's power-of-two histogram. Now the matrix SQL returns
// `(group, anchor, bucket, count)` tuples and the handler post-processor
// (`postProcessQuantileBuckets`) calls `Log2QuantileWithBucket` per phi.
//
// Covers both ungrouped (`{p="0.95"}`) and grouped
// (`{resource.service.name="...", p="0.95"}`) wire shapes; the post-
// processor synthesises the `p` label from `MetricsAggregate.Quantiles`,
// strips the synthetic `__bucket` label from each row, and emits one
// series per (group, phi).
func TestMetricsQueryRange_QuantileOverTimeWireShape(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		name           string
		query          string
		stubSamples    []chclient.Sample
		wantPairs      []tempo.MetricsLabel
		wantSampleSize int
	}{
		{
			name:  "ungrouped_single_phi",
			query: "{} | quantile_over_time(duration, 0.95)",
			stubSamples: []chclient.Sample{
				{Labels: map[string]string{"__bucket": "0.125"}, Timestamp: ts, Value: 5},
			},
			wantPairs: []tempo.MetricsLabel{
				{Key: "p", Value: "0.95"},
			},
			wantSampleSize: 1,
		},
		{
			name:  "grouped_single_phi",
			query: "{} | quantile_over_time(duration, 0.95) by (resource.service.name)",
			stubSamples: []chclient.Sample{
				{
					Labels:    map[string]string{"resource.service.name": "frontend", "__bucket": "0.125"},
					Timestamp: ts,
					Value:     5,
				},
			},
			wantPairs: []tempo.MetricsLabel{
				{Key: "resource.service.name", Value: "frontend"},
				{Key: "p", Value: "0.95"},
			},
			wantSampleSize: 1,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			q := &stubQuerier{samples: tc.stubSamples}
			srv := newServer(q, "v1.0.0-test")
			t.Cleanup(srv.Close)

			u := metricsQueryRangeURL(srv.URL, tc.query, map[string]string{
				"start": fixtureStartUnix,
				"end":   fixtureEndUnix,
				"step":  "60s",
			})
			resp, err := http.Get(u)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
			}

			var body tempo.MetricsQueryRangeResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(body.Series) != 1 {
				t.Fatalf("expected 1 series, got %d: %+v", len(body.Series), body)
			}
			gotLabels := body.Series[0].Labels
			if len(gotLabels) != len(tc.wantPairs) {
				t.Fatalf("labels = %d entries, want %d: got=%+v want=%+v",
					len(gotLabels), len(tc.wantPairs), gotLabels, tc.wantPairs)
			}
			for i, want := range tc.wantPairs {
				if gotLabels[i].Key != want.Key || gotLabels[i].Value != want.Value {
					t.Errorf("labels[%d] = %+v, want %+v", i, gotLabels[i], want)
				}
			}

			// `__name__` MUST NOT appear: HistogramAggregator path
			// doesn't go through UngroupedAggregator, so the Tempo
			// reference response has no metric-name synthetic.
			for _, l := range gotLabels {
				if l.Key == "__name__" {
					t.Errorf("quantile_over_time leaked __name__ label: %+v", gotLabels)
				}
				if l.Key == "__bucket" {
					t.Errorf("quantile_over_time leaked __bucket label to wire: %+v", gotLabels)
				}
			}

			// SQL must take the bucket-shape route — no CH-side
			// `quantile()` / `quantiles()` call (those would compute
			// the wrong quantile per Tempo's HistogramAggregator).
			for _, banned := range []string{"quantile(?)", "quantiles(?", "qs_array"} {
				if strings.Contains(q.lastSQL, banned) {
					t.Errorf("matrix quantile SQL must not contain %q: %s", banned, q.lastSQL)
				}
			}
		})
	}
}

// TestMetricsQueryRange_MultiQuantileOverTimeWireShape pins the
// multi-phi quantile_over_time wire shape: the post-processor
// (`postProcessQuantileBuckets`) fans the bucket-shape row stream out
// into one series per phi, each carrying a `p="<phi>"` label whose
// value is `strconv.FormatFloat(phi, 'f', -1, 64)`.
//
// Mirrors Tempo's HistogramAggregator emitting one TimeSeries per phi
// per group (engine_metrics.go::HistogramAggregator.Results) labelled
// `Label{"p", NewStaticFloat(q)}`.
func TestMetricsQueryRange_MultiQuantileOverTimeWireShape(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)

	// One bucket-shape row — three distinct power-of-two buckets fed
	// into the post-processor. The post-processor calls
	// Log2QuantileWithBucket(phi, buckets) per phi, so three phis →
	// three per-phi output series sharing the same (empty) group key.
	q := &stubQuerier{samples: []chclient.Sample{
		{Labels: map[string]string{"__bucket": "0.125"}, Timestamp: ts, Value: 5},
		{Labels: map[string]string{"__bucket": "0.25"}, Timestamp: ts, Value: 3},
		{Labels: map[string]string{"__bucket": "0.5"}, Timestamp: ts, Value: 2},
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL,
		"{} | quantile_over_time(duration, 0.5, 0.9, 0.99)",
		map[string]string{
			"start": fixtureStartUnix,
			"end":   fixtureEndUnix,
			"step":  "60s",
		})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	var body tempo.MetricsQueryRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Series) != 3 {
		t.Fatalf("expected 3 per-phi series, got %d: %+v", len(body.Series), body)
	}
	// Each series must carry exactly one label `{p="<phi>"}`. Series
	// order is deterministic via the canonical-key sort in
	// toMetricsSeries (sorted by "p=<phi>\n" key) — verify each phi
	// shows up exactly once rather than depending on the sort order.
	gotPhis := map[string]bool{}
	for _, s := range body.Series {
		if len(s.Labels) != 1 {
			t.Errorf("expected 1 label per series, got %+v", s.Labels)
			continue
		}
		if s.Labels[0].Key != "p" {
			t.Errorf("expected label key 'p', got %q", s.Labels[0].Key)
		}
		gotPhis[s.Labels[0].Value] = true
	}
	for _, want := range []string{"0.5", "0.9", "0.99"} {
		if !gotPhis[want] {
			t.Errorf("missing per-phi series for p=%q: gotPhis=%v", want, gotPhis)
		}
	}

	// Wire-shape is the contract — three per-phi series with `p=<phi>`
	// label set asserted above. The internal routing (bucket-shape SQL
	// + Tempo's Log2QuantileWithBucket post-processor) is exercised
	// through the wire shape rather than pinned to SQL substrings.
}
