//go:build chdb

// Property test for the instant range-vector WINDOW ANCHOR.
//
// This is Build Family B: a pgregory.net/rapid property that sweeps the
// eval instant T across a continuous series and asserts cerberus's
// instant /api/v1/query result for `<fn>(series[range])` AGREES with a
// from-scratch oracle evaluating the SAME (T-range, T] window.
//
// # Why this test exists (the gap it closes)
//
// The chDB spec harness (test/spec/runner_chdb.go::substituteNow64)
// rewrites every now64(...) in emitted SQL to ONE fixed literal the
// seeds are aligned to, so the eval instant is never varied relative to
// the sample timestamps. The existing property lane
// (TestPromQL_Property_FromScratch) pins EvalTs to AnchorTime+200s — a
// single fixed offset just past the data. NEITHER sweeps T across the
// (T-range, T] window over real continuous data — which is exactly where
// the rc.8 "instant range-vector window anchored to now64(9) wall-clock
// instead of time=T" bug lived: the window silently became
// (serverNow-range, serverNow], ignoring time=T, so the result went
// EMPTY at eval instants ~60-90s old.
//
// # How it catches the bug
//
// Cerberus is driven through its REAL Prom HTTP handler with `time=T`
// (the same runCerberusInstant the existing lane uses). WITH the fix the
// emitted window bound is a literal toDateTime64(T...) so the window
// tracks T; WITHOUT the fix it is now64(9) — chDB wall-clock at execution
// (~weeks after the 2026-05-13 seed anchor), so the (serverNow-range,
// serverNow] window misses every seeded sample and the result is empty.
// The oracle evaluates the (T-range, T] window directly off the in-memory
// series, so its result is non-empty whenever the window contains the
// required samples. The comparator then flags the empty-vs-nonempty
// drift, and rapid shrinks the failing draw to the minimal (interval,
// range, offset).
//
// # CI lane
//
// Build-tagged `chdb`; runs in the same `property` workflow as the other
// property tests (the workflow runs `go test -tags chdb ./test/property/...`).
package property_test

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/property"
	"github.com/tsouza/cerberus/test/property/gen"
)

// minWindowSamplesFor is the minimum sample count the (T-range, T]
// window must hold for the given function to produce a value. rate /
// increase / delta need >=2 in-window points to form a difference;
// every other windowed reducer needs >=1.
func minWindowSamplesFor(fn string) int {
	switch fn {
	case "rate", "increase", "delta":
		return 2
	default:
		return 1
	}
}

// oracleInstantWindow is the from-scratch evaluator: it computes the
// (T-range, T] window result for a single-series range-vector instant
// query DIRECTLY off the in-memory MetricsModel, implementing the
// window semantics off the PromQL spec (no delegation to cerberus or
// Prometheus). It returns ok=false when the window holds fewer than the
// function's minimum sample count (the PromQL "empty result" case).
//
// The third return, valueExact, reports whether `value` is a reference-
// exact result the test can assert byte-close against cerberus. The
// plain windowed reducers (sum / count / avg / min / max) are exact —
// no extrapolation, so their value pins the window CONTENTS too. The
// extrapolated counter functions (rate / increase / delta) depend on
// Prometheus's boundary-extrapolation heuristic (extends the first/last
// sample toward the window edge by up to half the average interval, then
// rescales) which the from-scratch oracle does NOT reimplement; for
// those, valueExact=false and the test asserts only NON-EMPTINESS — the
// bug's exact symptom — not the value. That keeps the oracle honest
// (it never claims a value it can't derive from the spec) while still
// catching the empty-window regression on every function in the pool.
func oracleInstantWindow(c gen.InstantWindowCase) (value float64, ok, valueExact bool) {
	rangeMs := c.RangeSec * 1000
	endMs := c.Query.EvalTs * 1000
	startMs := endMs - rangeMs // window is (start, end]

	pts := c.Dataset.Metrics.Series[0].Points
	var win []property.Point
	for _, p := range pts {
		if p.TimestampMs > startMs && p.TimestampMs <= endMs {
			win = append(win, p)
		}
	}
	sort.Slice(win, func(i, j int) bool { return win[i].TimestampMs < win[j].TimestampMs })

	if len(win) < minWindowSamplesFor(c.Fn) {
		return 0, false, false
	}

	switch c.Fn {
	case "sum_over_time":
		var s float64
		for _, p := range win {
			s += p.Value
		}
		return s, true, true
	case "count_over_time":
		return float64(len(win)), true, true
	case "avg_over_time":
		var s float64
		for _, p := range win {
			s += p.Value
		}
		return s / float64(len(win)), true, true
	case "min_over_time":
		m := win[0].Value
		for _, p := range win[1:] {
			m = math.Min(m, p.Value)
		}
		return m, true, true
	case "max_over_time":
		m := win[0].Value
		for _, p := range win[1:] {
			m = math.Max(m, p.Value)
		}
		return m, true, true
	case "rate", "increase", "delta":
		// Extrapolated counter / gauge-difference functions. The window
		// has >=2 samples (minWindowSamplesFor), so a non-empty result
		// is mandated — but the exact value rides Prometheus's boundary
		// extrapolation, which this oracle deliberately does not mirror.
		// Assert non-emptiness only (valueExact=false).
		return 0, true, false
	}
	return 0, false, false
}

// TestPromQL_InstantWindowSweep_FromScratch sweeps the eval instant T
// across a continuous series and asserts cerberus's instant range-vector
// result agrees with the from-scratch window oracle — in particular that
// cerberus is NON-EMPTY exactly when the (T-range, T] window is
// non-empty.
//
// Without the modifiers.go fix the emitted window bound is now64(9)
// (wall-clock at chDB execution, weeks after the seed anchor), so the
// window misses every sample and cerberus returns empty while the oracle
// returns a value — a drift rapid reports and shrinks to the minimal
// (scrapeIntervalSec, rangeSec, evalOffsetSec).
func TestPromQL_InstantWindowSweep_FromScratch(t *testing.T) {
	cli := chclienttest.NewChDB(t)
	h := prom.New(cli, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rapid.Check(t, func(rt *rapid.T) {
		c := gen.InstantWindowSweep().Draw(rt, "case")
		cli.Seed(t, c.Dataset.DDL)

		wantVal, wantOK, valueExact := oracleInstantWindow(c)

		got := runCerberusInstant(context.Background(), srv.URL, c.Query)
		if got.Err != nil {
			rt.Fatalf("instant-window sweep: cerberus error\nquery=%s evalTs=%d offset=%ds range=%ds scrape=%ds\nerr=%v",
				c.Query.String, c.Query.EvalTs, c.EvalOffset, c.RangeSec, c.ScrapeSec, got.Err)
		}

		gotOK := len(got.Rows) > 0

		// PRIMARY assertion: non-emptiness must agree. This is the bug's
		// exact symptom — without the fix cerberus is empty here while the
		// oracle window is non-empty.
		if wantOK != gotOK {
			rt.Fatalf("instant-window sweep: emptiness drift\n"+
				"query=%s\nevalTs=%d offset=%ds range=%ds scrape=%ds fn=%s temporality=%d latestSample=%d\n"+
				"oracle: nonEmpty=%v (value=%g)\ncerberus: nonEmpty=%v (rows=%d)\n"+
				"=> WITHOUT the modifiers.go window-anchor fix cerberus anchors the\n"+
				"   (T-range,T] window to now64(9) wall-clock, missing every seeded\n"+
				"   sample and returning empty at this offset.",
				c.Query.String, c.Query.EvalTs, c.EvalOffset, c.RangeSec, c.ScrapeSec,
				c.Fn, c.Temporality, c.LatestSample, wantOK, wantVal, gotOK, len(got.Rows))
		}

		if !wantOK {
			return // both empty — agreement, nothing more to compare.
		}

		// Non-empty agreement holds. Every case carries exactly one
		// series (single seeded series), so pin that invariant.
		if len(got.Rows) != 1 {
			rt.Fatalf("instant-window sweep: expected exactly 1 series, got %d\nquery=%s evalTs=%d",
				len(got.Rows), c.Query.String, c.Query.EvalTs)
		}

		// SECONDARY assertion (exact-value functions only): the single-
		// series value must agree within tolerance. A range query at a
		// grid-aligned T returns the same value, so this also pins the
		// window CONTENTS, not just its non-emptiness. The extrapolated
		// rate/increase/delta functions, whose reference value this
		// oracle does not reimplement (valueExact=false), only run the
		// non-emptiness assertion above and return here.
		if !valueExact {
			return
		}
		if !valuesCloseLocal(wantVal, got.Rows[0].Value) {
			rt.Fatalf("instant-window sweep: value drift\nquery=%s evalTs=%d offset=%ds range=%ds scrape=%ds fn=%s\noracle=%g cerberus=%g",
				c.Query.String, c.Query.EvalTs, c.EvalOffset, c.RangeSec, c.ScrapeSec, c.Fn,
				wantVal, got.Rows[0].Value)
		}
	})
}

// valuesCloseLocal mirrors the framework's valuesClose tolerance so this
// file doesn't reach into the unexported helper. abs=1e-6 / rel=1e-6 is
// looser than the framework's 1e-9 because rate/increase extrapolation
// and CH float arithmetic diverge by more than nano-scale; the
// non-emptiness assertion is the bug-catching one, and 1e-6 still pins
// the value to six significant figures.
func valuesCloseLocal(a, b float64) bool {
	const absEpsilon, relEpsilon = 1e-6, 1e-6
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	delta := math.Abs(a - b)
	if delta <= absEpsilon {
		return true
	}
	scale := math.Max(math.Abs(a), math.Abs(b))
	return delta <= relEpsilon*scale
}
