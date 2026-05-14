// Package shadow implements the shadow-mode differential testing
// harness.
//
// The harness runs each query through cerberus (native path) and an in-process
// PromQL oracle, then diffs the two result vectors. See ./README.md for the
// strategy matrix and ./cmd/shadow for the CLI entry point.
package shadow

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Sample is a single (timestamp, value) pair within a series.
type Sample struct {
	TimestampMs int64
	Value       float64
}

// Series is one labelled time-series within a vector result.
type Series struct {
	// Labels is the label set keyed by name. Both sides MUST agree byte-for-byte
	// after normalisation for the series to be considered a match.
	Labels map[string]string
	// Samples is sorted ascending by timestamp.
	Samples []Sample
}

// VectorResult is the shape we diff. It mirrors a Prometheus
// matrix/range-vector response after JSON decoding but kept independent of the
// upstream types so the harness does not pull in `prometheus/common/model`.
type VectorResult struct {
	Series []Series
}

// DiffOptions controls the comparison.
type DiffOptions struct {
	// AbsEpsilon is the absolute tolerance for values near zero.
	AbsEpsilon float64
	// RelEpsilon is the relative tolerance for non-zero values.
	RelEpsilon float64
}

// DefaultDiffOptions returns the standard tolerances.
func DefaultDiffOptions() DiffOptions {
	return DiffOptions{
		AbsEpsilon: 1e-9,
		RelEpsilon: 1e-9,
	}
}

// Diff is the structured outcome of comparing two VectorResults.
type Diff struct {
	// Equal is true iff the two results match within tolerances.
	Equal bool
	// Reasons lists every mismatch in deterministic order.
	Reasons []string
	// ExtraInA  / ExtraInB list series present on one side but not the other.
	ExtraInA []string
	ExtraInB []string
}

// Compare diffs two VectorResults under the given options. Pure function;
// safe for parallel use. Named Compare (not Diff) so it does not collide with
// the Diff result type.
func Compare(a, b VectorResult, opts DiffOptions) Diff {
	out := Diff{Equal: true}

	if opts.AbsEpsilon == 0 && opts.RelEpsilon == 0 {
		opts = DefaultDiffOptions()
	}

	if len(a.Series) != len(b.Series) {
		out.Equal = false
		out.Reasons = append(out.Reasons,
			fmt.Sprintf("cardinality: a=%d series, b=%d series", len(a.Series), len(b.Series)))
	}

	aByKey := indexByLabels(a.Series)
	bByKey := indexByLabels(b.Series)

	for key := range aByKey {
		if _, ok := bByKey[key]; !ok {
			out.Equal = false
			out.ExtraInA = append(out.ExtraInA, key)
		}
	}
	for key := range bByKey {
		if _, ok := aByKey[key]; !ok {
			out.Equal = false
			out.ExtraInB = append(out.ExtraInB, key)
		}
	}
	sort.Strings(out.ExtraInA)
	sort.Strings(out.ExtraInB)

	// Compare the intersection series by series.
	matched := make([]string, 0, len(aByKey))
	for key := range aByKey {
		if _, ok := bByKey[key]; ok {
			matched = append(matched, key)
		}
	}
	sort.Strings(matched)

	for _, key := range matched {
		as := aByKey[key]
		bs := bByKey[key]
		if reasons := compareSamples(key, as.Samples, bs.Samples, opts); len(reasons) > 0 {
			out.Equal = false
			out.Reasons = append(out.Reasons, reasons...)
		}
	}

	return out
}

func indexByLabels(series []Series) map[string]Series {
	out := make(map[string]Series, len(series))
	for _, s := range series {
		out[labelKey(s.Labels)] = s
	}
	return out
}

// labelKey produces a stable string key from a label set.
func labelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return "{}"
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
		b.WriteString("=\"")
		b.WriteString(labels[k])
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func compareSamples(key string, a, b []Sample, opts DiffOptions) []string {
	var reasons []string
	if len(a) != len(b) {
		reasons = append(reasons,
			fmt.Sprintf("series %s: sample count a=%d b=%d", key, len(a), len(b)))
		return reasons
	}
	for i := range a {
		if a[i].TimestampMs != b[i].TimestampMs {
			reasons = append(reasons,
				fmt.Sprintf("series %s: timestamp[%d] a=%d b=%d",
					key, i, a[i].TimestampMs, b[i].TimestampMs))
			continue
		}
		if !valuesClose(a[i].Value, b[i].Value, opts) {
			reasons = append(reasons,
				fmt.Sprintf("series %s: value[%d] @ ts=%d a=%g b=%g (delta=%g)",
					key, i, a[i].TimestampMs, a[i].Value, b[i].Value,
					math.Abs(a[i].Value-b[i].Value)))
		}
	}
	return reasons
}

func valuesClose(a, b float64, opts DiffOptions) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	if math.IsInf(a, 0) || math.IsInf(b, 0) {
		return a == b
	}
	delta := math.Abs(a - b)
	if delta <= opts.AbsEpsilon {
		return true
	}
	scale := math.Max(math.Abs(a), math.Abs(b))
	return delta <= opts.RelEpsilon*scale
}
