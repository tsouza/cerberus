package promql

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/promql/promparse"
	"github.com/tsouza/cerberus/internal/schema"
)

// domainEvalTime is the request instant the cases evaluate at unless
// they say otherwise.
var domainEvalTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestLeavesEvaluationDomain(t *testing.T) {
	t.Parallel()
	// minPlusHour / maxMinusHour are an hour inside each domain edge.
	minPlusHour := evaluationDomainMin.Add(time.Hour)
	maxMinusHour := evaluationDomainMax.Add(-time.Hour)
	cases := []struct {
		name       string
		query      string
		start, end time.Time
		want       bool
	}{
		{name: "ordinary selector", query: `up`, want: false},
		{name: "ordinary pin", query: `up @ 1767225600`, want: false},
		{name: "pin near the epoch", query: `up @ 100`, want: false},
		{name: "ordinary subquery", query: `max_over_time(rate(up[5m])[1h:1m])`, want: false},
		{name: "year-one pin", query: `up @ -62135596800`, want: true},
		{name: "pin below 1900", query: `up @ -2209075200`, want: true},
		{name: "pin past 2262", query: `up @ 9300000000`, want: true},
		{name: "pin overflowing int64 milliseconds", query: `up @ 1e17`, want: true},
		{name: "offset reaching below 1900", query: `up offset 130y`, want: true},
		{name: "negative offset reaching past 2262", query: `up offset -240y`, want: true},
		{name: "matrix range reaching below 1900", query: `rate(up[130y])`, want: true},
		{name: "subquery range reaching below 1900", query: `max_over_time(vector(1)[130y:1d])`, want: true},
		{name: "selector lookback under a subquery", query: `max_over_time(up[5m:1m] @ -2208902400)`, want: true},
		{name: "subquery window reaching below the int64-nanosecond floor", query: `count_over_time(vector(1)[5m:1m] @ -9223372036.854)`, want: true},
		{name: "pin under an offset subquery", query: `count_over_time(vector(1)[5m:1m] @ -9223372036 offset 1h)`, want: true},
		{name: "@ start() of a request near 1900", query: `up @ start()`, start: minPlusHour, end: minPlusHour.Add(24 * time.Hour), want: false},
		{name: "@ end() of a request near 2262", query: `up @ end()`, start: maxMinusHour.Add(-24 * time.Hour), end: maxMinusHour, want: false},
		{
			// Reference Prometheus measures the pin from the request's
			// start, so this one would not wrap there; the walk measures
			// from every eval instant, and over-approximates.
			name:  "@ start() of a request spanning more than 292 years",
			query: `up @ start()`, start: minPlusHour, end: maxMinusHour, want: true,
		},
		{
			// 1910 and 2250 are both inside the domain, but more than
			// 292 years apart: reference Prometheus's offset wraps.
			name:  "pin more than 292 years from the eval instant",
			query: `vector(1) + on() count_over_time(vector(1)[5m:1m] @ -1893456000)`,
			start: time.Date(2250, 1, 1, 0, 0, 0, 0, time.UTC), end: time.Date(2250, 1, 1, 0, 0, 0, 0, time.UTC),
			want: true,
		},
		{name: "request start below the domain", query: `vector(1)`, start: evaluationDomainMin.Add(-time.Millisecond), end: domainEvalTime, want: true},
		{name: "request end past the domain", query: `vector(1)`, start: domainEvalTime, end: evaluationDomainMax.Add(time.Millisecond), want: true},
		{name: "request at the domain edges", query: `vector(1)`, start: evaluationDomainMin, end: evaluationDomainMax, want: false},
		{name: "lookback crossing the domain floor", query: `up`, start: evaluationDomainMin, end: evaluationDomainMin, want: true},
		{
			// An explicit subquery step is part of the inner window; only
			// an omitted one falls back to DefaultSubqueryStep.
			name: "explicit subquery step reaching below 1900", query: `max_over_time(vector(1)[1h:130y])`, want: true,
		},
		{name: "aggregation without a parameter over a pin past 2262", query: `sum(up @ 9300000000)`, want: true},
		{
			// An unpinned selector evaluates at every request instant, so
			// the request start's lookback crosses the floor.
			name:  "unpinned selector over a request starting at the floor",
			query: `up`, start: evaluationDomainMin, end: domainEvalTime, want: true,
		},
		{
			// `@ end()` evaluates only at the request end, so the request
			// start's lookback is never read.
			name:  "@ end() of a request starting at the floor",
			query: `up @ end()`, start: evaluationDomainMin, end: domainEvalTime, want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := promparse.New().ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.query, err)
			}
			start, end := tc.start, tc.end
			if start.IsZero() {
				start, end = domainEvalTime, domainEvalTime
			}
			if got := LeavesEvaluationDomain(expr, start, end); got != tc.want {
				t.Errorf("LeavesEvaluationDomain(%q, %s, %s) = %v, want %v", tc.query, start, end, got, tc.want)
			}
			_, err = LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(), start, end)
			if refused := errors.Is(err, ErrOutsideEvaluationDomain); refused != tc.want {
				t.Errorf("LowerAt(%q) refused=%v (err %v), want refused=%v", tc.query, refused, err, tc.want)
			}
		})
	}
}

// TestLeavesEvaluationDomain_NoRangeMeansNow pins the two readings of a
// zero time.Time. To [Lower]'s context-free lowering, which anchors to
// now, it means now, and that is what is checked. To
// LeavesEvaluationDomain it is the instant 0001-01-01, which a request
// can name, and which lies outside the domain.
func TestLeavesEvaluationDomain_NoRangeMeansNow(t *testing.T) {
	t.Parallel()
	for query, want := range map[string]bool{
		`up`:                    false,
		`up @ -62135596800`:     true,
		`rate(up[5m] @ 9.3e9)`:  true,
		`rate(up[5m] @ 1.77e9)`: false,
	} {
		expr, err := promparse.New().ParseExpr(query)
		if err != nil {
			t.Fatalf("parse %q: %v", query, err)
		}
		_, err = Lower(context.Background(), expr, schema.DefaultOTelMetrics())
		if refused := errors.Is(err, ErrOutsideEvaluationDomain); refused != want {
			t.Errorf("Lower(%q) refused=%v (err %v), want refused=%v", query, refused, err, want)
		}
		if !LeavesEvaluationDomain(expr, time.Time{}, time.Time{}) {
			t.Errorf("LeavesEvaluationDomain(%q) at 0001-01-01 = false, want true", query)
		}
	}
}

func TestSaturatingArithmetic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		got, want int64
	}{
		{"add overflow", satAdd(math.MaxInt64, 1), math.MaxInt64},
		{"add underflow", satAdd(math.MinInt64, -1), math.MinInt64},
		{"add in range", satAdd(-5, 3), -2},
		{"sub overflow", satSub(math.MaxInt64, -1), math.MaxInt64},
		{"sub underflow", satSub(math.MinInt64, 1), math.MinInt64},
		{"sub of MinInt64 from non-negative", satSub(0, math.MinInt64), math.MaxInt64},
		{"sub of MinInt64 from negative", satSub(-1, math.MinInt64), math.MaxInt64},
		{"sub in range", satSub(3, 5), -2},
		{"abs of MinInt64", satAbs(math.MinInt64), math.MaxInt64},
		{"abs of negative", satAbs(-7), 7},
		{"abs of positive", satAbs(7), 7},
		{"millis of year one", timeMillis(time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)), -62135596800000},
		{"millis past int64", timeMillis(time.Unix(math.MaxInt64/1000+1, 0)), math.MaxInt64},
		{"millis before int64", timeMillis(time.Unix(math.MinInt64/1000-1, 0)), math.MinInt64},
		{"millis truncate sub-millisecond", timeMillis(time.Unix(1, 1_999_999)), 1001},
		{"add of zero", satAdd(0, 0), 0},
		{"add of zero right operand", satAdd(7, 0), 7},
		{"add of two MinInt64", satAdd(math.MinInt64, math.MinInt64), math.MinInt64},
		{"add of two MaxInt64", satAdd(math.MaxInt64, math.MaxInt64), math.MaxInt64},
		{"sub of equals", satSub(5, 5), 0},
		{"sub of MaxInt64 from MinInt64", satSub(math.MinInt64, math.MaxInt64), math.MinInt64},
		{"abs of zero", satAbs(0), 0},
		{"abs of MaxInt64", satAbs(math.MaxInt64), math.MaxInt64},
		{"abs of -MaxInt64", satAbs(-math.MaxInt64), math.MaxInt64},
		// The last whole second whose milliseconds fit is not saturated.
		{"millis of the last whole second", timeMillis(time.Unix(math.MaxInt64/1000, 0)), math.MaxInt64 / 1000 * 1000},
		{"millis of the first whole second", timeMillis(time.Unix(math.MinInt64/1000, 0)), math.MinInt64 / 1000 * 1000},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// TestPinnedDistanceBoundary pins where reference Prometheus's pin
// offset starts to wrap: a pin exactly maxPinDistanceMillis from an eval
// instant still converts, one millisecond further does not, measured
// from either end of the eval span.
func TestPinnedDistanceBoundary(t *testing.T) {
	t.Parallel()
	d := domainWalk{minMs: math.MinInt64, maxMs: math.MaxInt64}
	pin := int64(-1_000_000)
	cases := []struct {
		name   string
		lo, hi int64
		want   bool
	}{
		{"low end exactly at the limit", pin + maxPinDistanceMillis, pin + maxPinDistanceMillis, false},
		{"low end past the limit", pin + maxPinDistanceMillis + 1, pin + maxPinDistanceMillis + 1, true},
		{"high end exactly at the limit", pin, pin + maxPinDistanceMillis, false},
		{"high end past the limit", pin, pin + maxPinDistanceMillis + 1, true},
		{"below the pin exactly at the limit", pin - maxPinDistanceMillis, pin, false},
		{"below the pin past the limit", pin - maxPinDistanceMillis - 1, pin, true},
	}
	for _, tc := range cases {
		gotLo, gotHi, far := d.pinned(&pin, 0, tc.lo, tc.hi)
		if far != tc.want || gotLo != pin || gotHi != pin {
			t.Errorf("%s: pinned = (%d, %d, %v), want (%d, %d, %v)", tc.name, gotLo, gotHi, far, pin, pin, tc.want)
		}
	}
}

func TestSampleReads(t *testing.T) {
	t.Parallel()
	matchers := []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "__name__", "up")}
	first, last := timeMillis(StorageDomainMin), timeMillis(StorageDomainMax)

	t.Run("one piece", func(t *testing.T) {
		t.Parallel()
		reads := SampleReads(matchers, 1000, 61_000)
		if len(reads) != 1 {
			t.Fatalf("got %d pieces, want 1", len(reads))
		}
		if got, want := reads[0].String(), `up[1m1ms] @ 61.000`; got != want {
			t.Errorf("selector %s, want %s", got, want)
		}
	})

	t.Run("a one-millisecond window", func(t *testing.T) {
		t.Parallel()
		reads := SampleReads(matchers, 5000, 5000)
		if len(reads) != 1 || sampleFrom(reads[0]) != 5000 || samplePin(reads[0]) != 5000 {
			t.Fatalf("got %+v, want one piece reading exactly 5000", reads)
		}
	})

	t.Run("clipped to the storage domain", func(t *testing.T) {
		t.Parallel()
		reads := SampleReads(matchers, math.MinInt64, first+10)
		if len(reads) != 1 || sampleFrom(reads[0]) != first || samplePin(reads[0]) != first+10 {
			t.Fatalf("got %+v", reads)
		}
	})

	t.Run("outside the storage domain is empty", func(t *testing.T) {
		t.Parallel()
		if reads := SampleReads(matchers, math.MinInt64, first-1); len(reads) != 0 {
			t.Errorf("below: got %d pieces", len(reads))
		}
		if reads := SampleReads(matchers, last+1, math.MaxInt64); len(reads) != 0 {
			t.Errorf("above: got %d pieces", len(reads))
		}
	})

	t.Run("whole domain splits into consecutive pieces", func(t *testing.T) {
		t.Parallel()
		reads := SampleReads(matchers, math.MinInt64, math.MaxInt64)
		if len(reads) < 2 {
			t.Fatalf("got %d pieces, want the domain split", len(reads))
		}
		next := first
		for i, r := range reads {
			if sampleFrom(r) != next {
				t.Fatalf("piece %d starts at %d, want %d", i, sampleFrom(r), next)
			}
			end := samplePin(r)
			if end-sampleFrom(r) > maxSampleReadSpanMillis {
				t.Fatalf("piece %d spans %d ms, wider than %d", i, end-sampleFrom(r), maxSampleReadSpanMillis)
			}
			next = end + 1
		}
		if next != last+1 {
			t.Fatalf("pieces end at %d, want %d", next-1, last)
		}
	})
}

// samplePin is the `@` instant of a sample-read selector: the last
// millisecond its window reads.
func samplePin(sel *parser.MatrixSelector) int64 {
	return *sel.VectorSelector.(*parser.VectorSelector).Timestamp
}

// sampleFrom is the first millisecond a sample-read selector's window
// reads.
func sampleFrom(sel *parser.MatrixSelector) int64 {
	return samplePin(sel) - sel.Range.Milliseconds() + 1
}
