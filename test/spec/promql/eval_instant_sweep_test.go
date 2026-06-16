//go:build chdb

package promql_spec_test

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	spec "github.com/tsouza/cerberus/test/spec"
)

// TestEvalInstantWindowSweep is the eval-instant sweep that pins the rc.8
// instant range-vector window bug (commit d8be88b3): an instant
// `/api/v1/query` with a range selector
// (`sum_over_time(m[5m])`/`rate`/`increase`/…) silently anchored its
// `(T-range, T]` window to ClickHouse `now64(9)` wall-clock instead of
// the requested `time=T`, so it intermittently returned EMPTY at eval
// instants ~60-90s into continuous data.
//
// The harness seeds ONE continuous 60s-spaced DELTA series (mirroring the
// rc.8 GCP LB metric `loadbalancing_googleapis_com_https_request_count`)
// and, for each expr in
// {sum_over_time(m[5m]), rate(m[5m]), increase(m[5m]), sum_over_time(m[5m])/300},
// sweeps the eval instant T across offsets {0,-15,-30,-60,-90,-120,
// -180,-300}s, lowering each query at `time=T` through the real
// pipeline. It asserts:
//
//	(a) NON-EMPTY whenever (T-5m, T] overlaps a seeded sample — true for
//	    every swept offset by construction, since the series is dense and
//	    spans the whole sweep. WITHOUT the fix the window anchors to
//	    chDB wall-clock (years past the seeded 2026-01-01 data), so the
//	    window is empty and this assertion FAILS — that fail-without-fix
//	    toggle is the proof the test catches the bug.
//
//	(b) the instant value at a STEP-ALIGNED T equals the query_range
//	    value at that same step (step>0). The range path pins its window
//	    to the per-step grid anchor and was never affected by the bug;
//	    requiring instant==range is the strongest cross-check that the
//	    instant window anchored where the range window did.
//
//	(c) for sum_over_time, the instant value equals the from-scratch
//	    in-window sum oracle (deterministic over the DELTA seed) — so a
//	    silently-shifted window that happened to stay non-empty but
//	    covered the WRONG samples would still be caught.
func TestEvalInstantWindowSweep(t *testing.T) {
	// Continuous 60s-spaced DELTA series spanning 00:00:00..00:10:00
	// (11 samples). Values are per-window deltas (the GCP LB metric is a
	// DELTA-temporality sum), distinct per sample so a mis-anchored
	// window covering the wrong samples produces a different sum.
	const (
		seriesStart = "2026-01-01 00:00:00"
		sampleStep  = 60 * time.Second
		sampleCount = 11
		rangeWindow = 5 * time.Minute
	)
	base := mustParseUTC(t, seriesStart)
	samples := make([]sample, 0, sampleCount)
	for i := 0; i < sampleCount; i++ {
		samples = append(samples, sample{
			ts:  base.Add(time.Duration(i) * sampleStep),
			val: float64(10 * (i + 1)), // 10,20,...,110 — all distinct
		})
	}

	metric := "loadbalancing_googleapis_com_https_request_count"
	job := map[string]string{"job": "lb"}
	seed := buildDeltaSumSeed(metric, job, samples)

	// Eval instant base: 00:08:00, comfortably inside the dense series so
	// every swept offset's (T-5m, T] window overlaps samples.
	evalBase := mustParseUTC(t, "2026-01-01 00:08:00")
	offsets := []time.Duration{
		0,
		-15 * time.Second,
		-30 * time.Second,
		-60 * time.Second,
		-90 * time.Second,
		-120 * time.Second,
		-180 * time.Second,
		-300 * time.Second,
	}

	exprs := []struct {
		name string
		ql   string
		// oracle, when non-nil, returns the exact expected single-series
		// instant value at time=T for assertion (c). nil means rely on
		// (a) non-empty + (b) instant==range only.
		oracle func(T time.Time) (float64, bool)
	}{
		{
			name:   "sum_over_time",
			ql:     fmt.Sprintf("sum_over_time(%s[5m])", metric),
			oracle: func(T time.Time) (float64, bool) { return windowSum(samples, T, rangeWindow) },
		},
		{
			name: "rate",
			ql:   fmt.Sprintf("rate(%s[5m])", metric),
		},
		{
			name: "increase",
			ql:   fmt.Sprintf("increase(%s[5m])", metric),
		},
		{
			name: "sum_over_time_div_300",
			ql:   fmt.Sprintf("sum_over_time(%s[5m]) / 300", metric),
			oracle: func(T time.Time) (float64, bool) {
				s, ok := windowSum(samples, T, rangeWindow)
				if !ok {
					return 0, false
				}
				return s / 300, true
			},
		},
	}

	db := spec.OpenChDBForSweep(t)
	spec.ApplySeedForSweep(t, db, seed)

	p := parser.NewParser(parser.Options{})

	for _, ex := range exprs {
		ex := ex
		t.Run(ex.name, func(t *testing.T) {
			instantExpr, err := p.ParseExpr(ex.ql)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", ex.ql, err)
			}
			// A fresh parse for the range-step lowering: the lowering
			// mutates nothing, but parsing twice keeps the two paths
			// independent of any AST-sharing assumptions.
			rangeExpr, err := p.ParseExpr(ex.ql)
			if err != nil {
				t.Fatalf("ParseExpr(%q) [range]: %v", ex.ql, err)
			}

			for _, off := range offsets {
				T := evalBase.Add(off)

				// (a) NON-EMPTY: the dense series guarantees (T-5m, T]
				// overlaps samples at every offset.
				inst := spec.RunInstantSweep(t, db, instantExpr, T)
				if !inst.NonEmpty() {
					t.Fatalf("offset %s (T=%s): instant %s returned EMPTY, but (T-5m, T] overlaps %d seeded samples — the rc.8 now64-anchored window bug",
						off, T.Format(time.RFC3339), ex.ql, countInWindow(samples, T, rangeWindow))
				}

				// (c) exact in-window oracle for sum_over_time family.
				if ex.oracle != nil {
					want, ok := ex.oracle(T)
					if !ok {
						t.Fatalf("offset %s: oracle reports empty window though data spans it", off)
					}
					got, ok := inst.Scalar()
					if !ok {
						t.Fatalf("offset %s: instant %s produced %d rows, want exactly 1", off, ex.ql, inst.RowCount())
					}
					if !floatClose(got, want) {
						t.Fatalf("offset %s (T=%s): instant %s = %v, want in-window oracle %v",
							off, T.Format(time.RFC3339), ex.ql, got, want)
					}
				}

				// (b) instant==range at the step-aligned eval instant.
				// The single-step range query pins its window to the
				// grid anchor = T, so instant and range MUST agree.
				rng := spec.RunRangeStepSweep(t, db, rangeExpr, T, sampleStep)
				gotI, okI := inst.Scalar()
				gotR, okR := rng.Scalar()
				if okI != okR {
					t.Fatalf("offset %s (T=%s): instant single-series=%v but range single-series=%v for %s",
						off, T.Format(time.RFC3339), okI, okR, ex.ql)
				}
				if okI && okR && !floatClose(gotI, gotR) {
					t.Fatalf("offset %s (T=%s): instant %s = %v but query_range at same step = %v — instant window did not anchor to time=T",
						off, T.Format(time.RFC3339), ex.ql, gotI, gotR)
				}
			}
		})
	}
}

// sample is one seeded DELTA point.
type sample struct {
	ts  time.Time
	val float64
}

// windowSum is the from-scratch oracle for sum_over_time over the
// left-open / right-closed PromQL range-selector window (T-range, T]: the
// sum of sample values whose ts is in that half-open interval. Returns
// ok=false when no sample falls in the window.
func windowSum(samples []sample, T time.Time, rng time.Duration) (float64, bool) {
	lo := T.Add(-rng)
	var sum float64
	hit := false
	for _, s := range samples {
		if s.ts.After(lo) && !s.ts.After(T) {
			sum += s.val
			hit = true
		}
	}
	return sum, hit
}

// countInWindow counts samples in (T-range, T] — used only for richer
// failure messages.
func countInWindow(samples []sample, T time.Time, rng time.Duration) int {
	lo := T.Add(-rng)
	n := 0
	for _, s := range samples {
		if s.ts.After(lo) && !s.ts.After(T) {
			n++
		}
	}
	return n
}

// buildDeltaSumSeed renders the otel_metrics_{gauge,sum,histogram} DDL
// plus a DELTA-temporality INSERT into otel_metrics_sum for `metric`.
// The gauge + histogram tables are created empty so the three-arm
// metric-name UnionAll the lowering emits has real tables to scan; only
// the sum arm carries rows. The harness backfills the ResourceAttributes
// column the read path projects.
func buildDeltaSumSeed(metric string, labels map[string]string, samples []sample) string {
	var b strings.Builder
	b.WriteString(`CREATE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
CREATE TABLE otel_metrics_sum (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
CREATE TABLE otel_metrics_histogram (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64)
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
`)
	b.WriteString("INSERT INTO otel_metrics_sum VALUES\n")
	for i, s := range samples {
		if i > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, "    ('%s', %s, toDateTime64('%s', 9), %g)",
			metric, chMapLiteral(labels), s.ts.UTC().Format("2006-01-02 15:04:05.000000000"), s.val)
	}
	b.WriteString(";\n")
	return b.String()
}

// chMapLiteral renders a Go map as a CH `map('k','v',...)` literal with
// deterministic key order.
func chMapLiteral(m map[string]string) string {
	if len(m) == 0 {
		return "map()"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// small + fixed in this test; stable sort for determinism
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	var b strings.Builder
	b.WriteString("map(")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "'%s', '%s'", k, m[k])
	}
	b.WriteString(")")
	return b.String()
}

func mustParseUTC(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return ts
}

// floatClose compares two floats with a small relative+absolute epsilon
// (chDB float arithmetic + the rate extrapolation introduce tiny noise).
func floatClose(a, b float64) bool {
	const eps = 1e-9
	if a == b {
		return true
	}
	diff := math.Abs(a - b)
	if diff <= eps {
		return true
	}
	scale := math.Max(math.Abs(a), math.Abs(b))
	return diff <= eps*scale*1e3
}
