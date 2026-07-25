package promql

import "github.com/tsouza/cerberus/internal/schema"

// HistogramSyntheticNames returns, in stable order, the Prom-wire series names
// the selector lowering resolves back onto the classic-histogram row stored
// under `base`.
//
// The OTel-CH classic-histogram exporter writes ONE row per observation under
// the bare `<base>` MetricName, carrying Count / Sum / BucketCounts /
// ExplicitBounds as columns. PromQL never exposes that row under its stored
// name: the wire surface is the companion series `<base>_bucket` (one series
// per boundary, fanned out of the arrays), `<base>_count` and `<base>_sum`
// (single-column projections). So `<base>` and the names a client may select
// are two different alphabets, and every surface that has to answer "which
// series names exist" — the `__name__` catalog, the metadata matcher fan-out —
// must translate between them.
//
// This function is that translation, and it is derived from the SELECTOR
// LOWERING's own routing rather than from a private suffix list: each
// candidate is offered to the two resolvers lowerVectorSelector consults
// (isClassicBucketSelector for the array fan-out, HistogramCompanionColumn for
// the column projections) and is kept only when they route it back to `base`.
// A name this function returns is therefore queryable by construction — the
// invariant the catalog surface depends on — and a routing change in the
// lowering cannot leave the enumeration behind.
//
// Returns nil when no histogram table is configured (nothing serves these
// names) or for an empty base.
func HistogramSyntheticNames(base string, s schema.Metrics) []string {
	if base == "" || s.HistogramTable == "" {
		return nil
	}
	companionSuffixes := s.HistogramCompanionSuffixes()
	candidates := make([]string, 0, 1+len(companionSuffixes))
	candidates = append(candidates, base+bucketSuffix)
	for _, suf := range companionSuffixes {
		candidates = append(candidates, base+suf)
	}
	out := make([]string, 0, len(candidates))
	for _, name := range candidates {
		if resolvesToHistogramRow(name, base, s) {
			out = append(out, name)
		}
	}
	return out
}

// resolvesToHistogramRow asks the selector lowering's two classic-histogram
// resolvers whether `name` reads the histogram row keyed by `base`. It is the
// single decision [HistogramSyntheticNames] and lowerVectorSelector share, so
// the enumeration cannot advertise a name the lowering would route elsewhere.
func resolvesToHistogramRow(name, base string, s schema.Metrics) bool {
	if bare, ok := isClassicBucketSelector(name, s); ok {
		return bare == base
	}
	if bare, _, ok := s.HistogramCompanionColumn(name); ok {
		return bare == base
	}
	return false
}
