//go:build chdb

// chDB-backed regression coverage for range-mode `offset` output-timestamp
// labeling through the FULL handler path (query_range -> lower -> optimize ->
// wrapWithSampleProjection -> emit -> decode).
//
// PromQL `offset` shifts only WHICH samples a reducing range window reads; the
// result is reported at the unshifted eval time t, never t-offset. The matrix
// emitter's gridAnchorFrag adds the offset back on the emitted timestamp, but
// wrapWithSampleProjection was re-reading the raw, still-offset-shifted
// `anchor_ts` column and re-aliasing it to TimeUnix — silently re-shifting
// every range-offset matrix query's output by the offset. Neither the spec SQL
// goldens (which emit the lowered plan, without this handler projection) nor
// the A/B parity lane (route A and B are shifted the SAME way) caught it; only
// the compat differential vs real Prometheus did. These tests reproduce the
// shift end-to-end and pin the unshifted grid.
package prom_test

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// rangeOffsetSumDDL is a minimal otel_metrics_sum counter table mirroring the
// OTel-CH default schema (ResourceAttributes DEFAULT map() so the read path's
// sanitize projection resolves).
const rangeOffsetSumDDL = `CREATE TABLE otel_metrics_sum (
  MetricName String,
  Attributes Map(String, String),
  ResourceAttributes Map(String, String) DEFAULT map(),
  ServiceName LowCardinality(String),
  TimeUnix DateTime64(9),
  Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);`

// TestQueryRange_RateOffset_UnshiftedGrid_ChDB pins that a range-mode
// rate()/*_over_time() with an `offset` reports its samples on the UNSHIFTED
// request grid [start, end] — the same timestamps the un-offset query reports,
// never shifted back by the offset.
func TestQueryRange_RateOffset_UnshiftedGrid_ChDB(t *testing.T) {
	start := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	step := 30 * time.Second
	const offset = 2 * time.Minute

	// Dense monotonic counter from 20m before start through end so every
	// offset-shifted window is populated with >= 2 samples.
	seedStart := start.Add(-20 * time.Minute)
	nSamples := int(end.Sub(seedStart)/step) + 1
	rows := make([]string, 0, nSamples)
	for i := 0; i < nSamples; i++ {
		ts := seedStart.Add(time.Duration(i) * step).Format("2006-01-02 15:04:05.000000000")
		rows = append(rows, fmt.Sprintf(
			`('http_requests_total', map('job', 'api'), 'svc', toDateTime64('%s', 9), %d.0)`,
			ts, i,
		))
	}
	seed := rangeOffsetSumDDL + "\nINSERT INTO otel_metrics_sum (MetricName, Attributes, ServiceName, TimeUnix, Value) VALUES\n  " +
		strings.Join(rows, ",\n  ") + ";"

	srv, _ := newChDBServer(t, seed)

	// The expected output grid is [start, end] stepped by step — identical for
	// the offset and non-offset query. gridTimestamps returns Unix seconds.
	wantGrid := gridTimestamps(start, end, step)

	for _, q := range []string{
		"rate(http_requests_total[5m])",
		"rate(http_requests_total[5m] offset 2m)",
		"increase(http_requests_total[5m] offset 2m)",
		"sum_over_time(http_requests_total[5m] offset 2m)",
		// Aggregate path: the wrapping sum groups by the emitter's
		// gridAnchorFrag-relabeled TimeUnix (not the handler branch), so this
		// exercises the OTHER half of the offset-labeling fix.
		"sum(rate(http_requests_total[5m] offset 2m))",
		"sum by (job) (rate(http_requests_total[5m] offset 2m))",
		// __name__-preserving reducers (last/first_over_time) route through a
		// separate lowering wrapper that also had to be un-shifted.
		"last_over_time(http_requests_total[5m] offset 2m)",
		"first_over_time(http_requests_total[5m] offset 2m)",
	} {
		t.Run(q, func(t *testing.T) {
			matrix := runRangeModeQueryRange(t, srv.URL, q, start, end, step)
			if len(matrix) != 1 {
				t.Fatalf("want 1 series, got %d", len(matrix))
			}
			got := make(map[int64]bool)
			var maxTs int64 = math.MinInt64
			for _, v := range matrix[0].Values {
				tsFloat, ok := v[0].(float64)
				if !ok {
					t.Fatalf("timestamp not a float: %T %v", v[0], v[0])
				}
				ts := int64(math.Round(tsFloat))
				got[ts] = true
				if ts > maxTs {
					maxTs = ts
				}
			}
			if len(got) == 0 {
				t.Fatalf("query %q returned zero samples — seed does not exercise it", q)
			}
			// Every returned timestamp must sit on the unshifted request grid.
			for ts := range got {
				if !wantGrid[ts] {
					t.Fatalf("query %q returned off-grid timestamp %d (Unix); the offset "+
						"must NOT shift the reported grid. grid=[%d..%d step %ds]",
						q, ts, start.Unix(), end.Unix(), int(step.Seconds()))
				}
			}
			// The last reported anchor must be `end` itself: its offset-shifted
			// window is fully populated by the seed, so a correct grid emits a
			// sample at `end`. A re-shifted output tops out at end-offset — the
			// robust catch, since a step-multiple offset keeps every shifted
			// timestamp grid-aligned and mostly in-range (the on-grid check
			// above passes even when shifted).
			if maxTs != end.Unix() {
				t.Fatalf("query %q: last reported timestamp %d != end %d — the output is "+
					"shifted back by the offset (%s); the offset must shift only the sample "+
					"window, not the reported grid", q, maxTs, end.Unix(), offset)
			}
		})
	}
}

func gridTimestamps(start, end time.Time, step time.Duration) map[int64]bool {
	g := make(map[int64]bool)
	for ts := start; !ts.After(end); ts = ts.Add(step) {
		g[ts.Unix()] = true
	}
	return g
}
