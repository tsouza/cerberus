package prom

import (
	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
)

// labelMemo collapses the per-row label normalisation the matrix / vector
// pivots do into one computation per distinct series.
//
// Why this matters for memory: every pivot loop turned each decoded sample
// into a wire label set via
// `format.NormalizeLabelMap(format.WithMetricName(s.Labels, s.MetricName))`.
// Both calls allocate a FRESH map[string]string (WithMetricName copies +1
// slot; NormalizeLabelMap rebuilds key-by-key), so a range query with K
// samples per series did 2×K map allocations per series even though every
// row of a series shares the SAME wire label set. Only the first survives
// (it lands in the per-series state); the other 2×(K-1) become immediate
// garbage. The rc.5 ResourceAttributes-as-Prom-labels merge inflated the
// per-series label set with the promoted resource keys (k8s_namespace_name,
// k8s_pod_name, …), so each of those throwaway maps got proportionally
// bigger — raising the allocation RATE and the live-heap watermark a
// concurrent query holds mid-pivot, which is the load-dependent heap spike
// that OOMKilled the 1Gi e2e cerberus pod on the rc.5 commit.
//
// The cursor interns decoded Labels maps by series identity and stamps each
// Sample with a stable per-cursor [chclient.Sample.SeriesID] (1-based, 0 =
// not interned). Keying the memo on that ordinal (plus MetricName, which
// the cursor does NOT fold into the interned key) turns the 2×K
// allocations per series back into 2 — the normalisation runs once, every
// later row reuses the cached result. No reflect / map-pointer probe is
// needed: the cursor already computed the canonical key during interning,
// so the ordinal is free. The cached map is the exact instance that lands
// in the per-series state, so downstream behaviour is byte-identical to the
// un-memoised path.
//
// Cross-cursor safety: SeriesID restarts at 1 per cursor, so a consumer
// that merges rows from several cursors (the /series chunk loop) could see
// two different series share an ordinal. That is harmless here because the
// memo only governs which (already-correct) normalised map is returned for
// a given row — the consumer still keys its OWN dedup map on
// CanonicalKey(labels), so a collision at most recomputes a normalisation
// the consumer would have deduped anyway. To stay strictly correct the memo
// folds MetricName into the key, and falls back to a fresh normalisation
// whenever SeriesID is 0 (the non-interned test/slice cursor) so distinct
// series never collapse.
type labelMemo struct {
	cache map[labelMemoKey]map[string]string
}

// labelMemoKey identifies a normalised label set by the interned series
// ordinal plus the MetricName the row carries. Two series that share an
// Attributes map but differ in MetricName (the same attribute set under
// distinct metric names) must NOT collapse — the MetricName component keeps
// them distinct.
type labelMemoKey struct {
	seriesID uint32
	metric   string
}

// newLabelMemo returns a memo sized for sizeHint distinct series.
func newLabelMemo(sizeHint int) labelMemo {
	return labelMemo{cache: make(map[labelMemoKey]map[string]string, sizeHint)}
}

// normalize returns the wire label set for s — the same value
// `format.NormalizeLabelMap(format.WithMetricName(s.Labels, s.MetricName))`
// would produce — computing it at most once per (interned series,
// MetricName) pair. When s.SeriesID is 0 (a non-interned cursor) it
// recomputes every call so distinct series are never aliased.
func (m labelMemo) normalize(s chclient.Sample) map[string]string {
	if s.SeriesID == 0 {
		return format.NormalizeLabelMap(format.WithMetricName(s.Labels, s.MetricName))
	}
	key := labelMemoKey{seriesID: s.SeriesID, metric: s.MetricName}
	if cached, ok := m.cache[key]; ok {
		return cached
	}
	out := format.NormalizeLabelMap(format.WithMetricName(s.Labels, s.MetricName))
	m.cache[key] = out
	return out
}
