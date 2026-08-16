package gen

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/test/property"
)

// This file holds the generators for the eval-instant-sweep property
// (Build Family B). They differ from metrics.go / promql.go in two
// load-bearing ways that target the instant range-vector window bug
// (the rc.8 "empty hole at ~-60s/-90s"):
//
//   - The dataset is a SINGLE continuous gauge-table series whose samples are
//     spaced exactly `scrapeIntervalSec` apart, drawn from {15,30,60} so the
//     range/scrape relationship is realistic. Its value geometry varies across
//     a wave, resetting positive readings, and a monotonic running total so all
//     enrolled range functions have meaningful inputs.
//   - The eval instant T is the LATEST sample minus a swept
//     `evalOffsetSec` drawn from a pool that deliberately includes the
//     60s / 90s offsets where the bug surfaced. This is the axis the
//     spec-harness now64-substitution and the existing property lane
//     never vary: they pin T next to the data.
//
// The instant query is `<fn>(series[<range>])` where range is a multiple
// of the scrape interval, so the (T-range, T] window slides across real
// continuous data as T sweeps.

// EvalSweepOffsetsSec is the eval-offset sweep, in seconds, subtracted
// from the latest sample's timestamp to form the instant eval time T.
// It deliberately includes 60 and 90 — the offsets the rc.8 GCP
// dashboards hit an empty window at — plus 0 (T == latest sample) and a
// 300 far-edge. Offsets up to (range) keep the window non-empty for the
// oracle; the larger offsets exercise the boundary where the window
// starts to fall off the data and BOTH sides must still agree.
var EvalSweepOffsetsSec = []int64{0, 15, 30, 60, 90, 120, 180, 300}

// InstantWindowValueProfile selects the gauge-table value geometry used by an
// instant-window case. These profiles deliberately do not claim physical OTel
// aggregation temporality: every case is stored in otel_metrics_gauge. They
// exercise a non-monotonic wave, periodically resetting positive readings,
// and a monotonic running total against the same PromQL functions.
type InstantWindowValueProfile int

const (
	// InstantWindowValueProfileWave is an arbitrary non-monotonic series.
	InstantWindowValueProfileWave InstantWindowValueProfile = iota
	// InstantWindowValueProfilePositiveIncrements repeats positive readings
	// and therefore includes periodic drops under counter functions.
	InstantWindowValueProfilePositiveIncrements
	// InstantWindowValueProfileMonotonicRunningTotal is a steadily increasing
	// prefix sum of the positive-increments profile.
	InstantWindowValueProfileMonotonicRunningTotal
)

// InstantWindowCase is the fully-drawn parameter bundle for one
// eval-instant-sweep iteration. Keeping the raw axes on the value (not
// just the rendered Query) lets the test's from-scratch oracle evaluate
// the SAME (T-range, T] window the SQL does, and lets rapid shrink a
// violation down to the minimal (interval, range, offset) triple.
type InstantWindowCase struct {
	Dataset      property.Dataset
	ScrapeSec    int64
	RangeSec     int64
	EvalOffset   int64
	ValueProfile InstantWindowValueProfile
	Fn           string
	MetricName   string
	Labels       map[string]string
	Query        property.Query
	LatestSample int64 // unix seconds of the last seeded point
}

// instantWindowMetric is the fixed metric name for the swept series. It
// avoids the _sum/_count/_total/_bucket suffixes so the schema heuristic
// keeps it in the gauge table the DDL creates.
const instantWindowMetric = "series"

// InstantWindowSweep draws one InstantWindowCase: a continuous single
// series plus a range-vector instant query whose eval time T sweeps an
// offset back from the latest sample. The window (T-range, T] therefore
// slides across the data, exercising the offset band (60s/90s) the bug
// hid in.
func InstantWindowSweep() *rapid.Generator[InstantWindowCase] {
	return rapid.Custom(func(t *rapid.T) InstantWindowCase {
		shapeID := rapid.SampledFrom(InstantWindowShapeIDs()).Draw(t, "shapeID")
		return drawInstantWindowCase(t, shapeID)
	})
}

// InstantWindowSweepForShape fixes the function/value-profile axes to one
// exact roster member while leaving the window geometry available to rapid.
func InstantWindowSweepForShape(shapeID ShapeID) *rapid.Generator[InstantWindowCase] {
	if _, ok := instantWindowShapeByID(shapeID); !ok {
		panic("gen/instant-window: unknown shape " + string(shapeID))
	}
	return rapid.Custom(func(t *rapid.T) InstantWindowCase {
		return drawInstantWindowCase(t, shapeID)
	})
}

func drawInstantWindowCase(t *rapid.T, shapeID ShapeID) InstantWindowCase {
	spec, ok := instantWindowShapeByID(shapeID)
	if !ok {
		panic("gen/instant-window: unhandled shape " + string(shapeID))
	}
	scrapeSec := rapid.SampledFrom([]int64{15, 30, 60}).Draw(t, "scrapeSec")
	// rangeMult in [2,8] so the window spans at least two scrape
	// intervals (rate/increase need >=2 in-window samples) and the
	// range is always a clean multiple of the scrape interval.
	rangeMult := rapid.Int64Range(2, 8).Draw(t, "rangeMult")
	rangeSec := scrapeSec * rangeMult
	evalOffset := rapid.SampledFrom(EvalSweepOffsetsSec).Draw(t, "evalOffsetSec")
	profile := spec.profile
	fn := spec.fn

	// Seed enough samples that the data spans well past the largest
	// swept offset + range, so the oracle window is genuinely
	// non-empty across the sweep (the boundary offsets near the
	// data edge are still agreement checks, just possibly empty).
	const numSamples = 40
	labels := map[string]string{"job": "api"}

	points := make([]property.Point, 0, numSamples)
	var acc float64
	for i := 0; i < numSamples; i++ {
		tsMs := AnchorTime().Add(time.Duration(i) * time.Duration(scrapeSec) * time.Second).UnixMilli()
		var v float64
		switch profile {
		case InstantWindowValueProfileWave:
			// A non-trivial but deterministic gauge wave.
			v = float64((i*7)%50) + 1
		case InstantWindowValueProfilePositiveIncrements:
			v = float64((i % 5) + 1)
		case InstantWindowValueProfileMonotonicRunningTotal:
			acc += float64((i % 5) + 1)
			v = acc
		}
		points = append(points, property.Point{TimestampMs: tsMs, Value: v})
	}

	series := []property.SeriesData{{
		MetricName: instantWindowMetric,
		Labels:     labels,
		Points:     points,
	}}
	ds := property.Dataset{
		DDL:     renderDDL(series),
		Metrics: &property.MetricsModel{Series: series},
	}

	latestMs := points[len(points)-1].TimestampMs
	latestSec := latestMs / 1000
	evalTs := latestSec - evalOffset

	queryStr := fmt.Sprintf("%s(%s%s[%ds])", fn, instantWindowMetric, matcherString(labels), rangeSec)

	return InstantWindowCase{
		Dataset:      ds,
		ScrapeSec:    scrapeSec,
		RangeSec:     rangeSec,
		EvalOffset:   evalOffset,
		ValueProfile: profile,
		Fn:           fn,
		MetricName:   instantWindowMetric,
		Labels:       labels,
		LatestSample: latestSec,
		Query: property.Query{
			ShapeID: property.ShapeID(shapeID),
			String:  queryStr,
			EvalTs:  evalTs,
		},
	}
}

// matcherString renders a label set as a PromQL `{k="v",…}` matcher
// fragment with sorted keys for determinism.
func matcherString(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(labels[k])
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}
