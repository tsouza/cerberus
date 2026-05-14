package promql

import (
	"fmt"
	"math"
	"sort"
	"strconv"
)

// histogramQuantile implements `histogram_quantile(phi, buckets)`
// over a classic-histogram instant vector.
//
// The input is the inner expression already evaluated to a vector of
// per-bucket samples (each carrying a numeric `le` label). The
// function:
//
//  1. Groups buckets by their non-`le` labels (the "series" each
//     bucket belongs to).
//  2. For each group, sorts the buckets by upper bound (`le`) and
//     interpolates the phi-th quantile.
//
// The classic-histogram quantile interpolation rule is:
//
//   - Let buckets be sorted by ascending `le`, with cumulative
//     counts (the values).
//   - Total = count of the +Inf bucket (last bucket).
//   - Find the bucket b such that cumulative(b) >= phi*Total.
//   - Linearly interpolate within b: the bucket's lower bound is the
//     previous bucket's `le` (or 0 for the first), and the count
//     within b is cumulative(b) - cumulative(b-1).
//
// The output's __name__ is dropped and `le` is dropped (Prom
// convention).
func histogramQuantile(phi float64, buckets []VectorRow, evalTsMs int64) ([]VectorRow, error) {
	if phi < 0 {
		// Prom returns -Inf for phi < 0.
		return emitConstQuantile(buckets, math.Inf(-1), evalTsMs), nil
	}
	if phi > 1 {
		return emitConstQuantile(buckets, math.Inf(1), evalTsMs), nil
	}

	// Group by non-`le` labels.
	groups := make(map[string]*histGroup)
	keys := []string{}
	for _, r := range buckets {
		le, ok := r.Labels["le"]
		if !ok {
			return nil, fmt.Errorf("oracle: histogram_quantile: bucket row missing `le` label")
		}
		leF, err := strconv.ParseFloat(le, 64)
		if err != nil {
			return nil, fmt.Errorf("oracle: histogram_quantile: parse `le`=%q: %w", le, err)
		}
		// Strip both __name__ and le for grouping.
		stripped := DropLabel(DropLabel(r.Labels, MetricNameLabel), "le")
		k := labelKey(stripped)
		g, ok := groups[k]
		if !ok {
			g = &histGroup{labels: stripped}
			groups[k] = g
			keys = append(keys, k)
		}
		g.buckets = append(g.buckets, bucketPoint{le: leF, count: r.V})
	}

	sort.Strings(keys)
	out := make([]VectorRow, 0, len(keys))
	for _, k := range keys {
		g := groups[k]
		v, ok := bucketQuantile(phi, g.buckets)
		if !ok {
			continue
		}
		out = append(out, VectorRow{
			Labels: g.labels,
			T:      evalTsMs,
			V:      v,
		})
	}
	sortVectorRows(out)
	return out, nil
}

type histGroup struct {
	labels  map[string]string
	buckets []bucketPoint
}

type bucketPoint struct {
	le    float64
	count float64
}

// bucketQuantile is the textbook histogram_quantile interpolation,
// matching Prom's promql/quantile.go::bucketQuantile.
//
// Returns (NaN, true) for degenerate cases (< 2 buckets, no +Inf
// bucket); (0, false) only when there are no buckets at all.
func bucketQuantile(phi float64, buckets []bucketPoint) (float64, bool) {
	if len(buckets) == 0 {
		return 0, false
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].le < buckets[j].le })

	// Ensure cumulative — input is supposed to be cumulative, but
	// merge duplicate `le` and trust the data otherwise.
	deduped := buckets[:0]
	for _, b := range buckets {
		if len(deduped) > 0 && deduped[len(deduped)-1].le == b.le {
			deduped[len(deduped)-1].count = b.count
			continue
		}
		deduped = append(deduped, b)
	}
	buckets = deduped

	// Require an +Inf bucket — without it, the total is ambiguous.
	if !math.IsInf(buckets[len(buckets)-1].le, 1) {
		return math.NaN(), true
	}
	if len(buckets) < 2 {
		return math.NaN(), true
	}
	total := buckets[len(buckets)-1].count
	if total == 0 {
		return math.NaN(), true
	}
	rank := phi * total

	// Find the bucket b such that count[b] >= rank.
	b := sort.Search(len(buckets)-1, func(i int) bool {
		return buckets[i].count >= rank
	})
	if b == len(buckets)-1 {
		// rank is in the +Inf bucket — return the lower bound of the
		// last finite bucket as Prom does.
		return buckets[len(buckets)-2].le, true
	}
	if b == 0 && buckets[0].le <= 0 {
		// First bucket starts at 0 or below, return its `le`.
		return buckets[0].le, true
	}

	bucketStart := 0.0
	bucketEnd := buckets[b].le
	count := buckets[b].count
	if b > 0 {
		bucketStart = buckets[b-1].le
		count -= buckets[b-1].count
		rank -= buckets[b-1].count
	}
	if count <= 0 {
		return bucketEnd, true
	}
	return bucketStart + (bucketEnd-bucketStart)*(rank/count), true
}

func emitConstQuantile(buckets []VectorRow, v float64, evalTsMs int64) []VectorRow {
	// Match Prom: emit one row per histogram series (group by non-le
	// labels), all with the same constant value.
	groups := make(map[string]map[string]string)
	keys := []string{}
	for _, r := range buckets {
		stripped := DropLabel(DropLabel(r.Labels, MetricNameLabel), "le")
		k := labelKey(stripped)
		if _, ok := groups[k]; !ok {
			groups[k] = stripped
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]VectorRow, 0, len(keys))
	for _, k := range keys {
		out = append(out, VectorRow{Labels: groups[k], T: evalTsMs, V: v})
	}
	return out
}
