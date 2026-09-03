package tempo

import "time"

// AlignMetricsWindowForTest re-exports alignMetricsWindow for the
// external tempo_test package — the grid-snap formula is pure and
// worth pinning directly, without driving a full handler round-trip.
var AlignMetricsWindowForTest = func(start, end time.Time, step time.Duration) (time.Time, time.Time) {
	return alignMetricsWindow(start, end, step)
}

// PostProcessCompareForTest / CompareAnchorGridForTest re-export the
// compare() BaselineAggregator mirror so its top-N / totals / zero-fill
// semantics can be pinned directly on synthetic row streams without a
// handler round-trip.
var (
	PostProcessCompareForTest = postProcessCompare
	CompareAnchorGridForTest  = compareAnchorGrid
)

// GroupBatchesForTest / GroupBatchesProtoForTest re-export the two
// trace-by-ID assemblers so the determinism tests can drive the pure
// assembly repeatedly (50 iterations, shuffled inputs) and compare
// serialized outputs byte-for-byte without a handler round-trip per
// iteration.
var (
	GroupBatchesForTest      = groupBatches
	GroupBatchesProtoForTest = groupBatchesProto
)

// TagsCatalogEligibleForTest / TagValuesCatalogEligibleForTest /
// BuildTagCatalogKeysSQLForTest / BuildTagCatalogValuesSQLForTest
// re-export the tag-catalog (cerberus issue #2771) eligibility rule and
// SQL builders so they can be pinned directly on synthetic inputs,
// without a handler round-trip per case — mirroring the re-export
// pattern above.
var (
	TagsCatalogEligibleForTest      = tagsCatalogEligible
	TagValuesCatalogEligibleForTest = tagValuesCatalogEligible
	BuildTagCatalogKeysSQLForTest   = buildTagCatalogKeysSQL
	BuildTagCatalogValuesSQLForTest = buildTagCatalogValuesSQL
)

// ResolvedTagNameForTest constructs a resolvedTagName for
// TagValuesCatalogEligibleForTest's tests — the struct's fields are
// unexported, so the external tempo_test package needs a constructor
// rather than a literal.
func ResolvedTagNameForTest(isIntrinsic bool, key string, mapScope AttrMapScopeForTest) resolvedTagName {
	return resolvedTagName{IsIntrinsic: isIntrinsic, Key: key, MapScope: attrMapScope(mapScope)}
}

// AttrMapScopeForTest re-exports the attrMapScope type + its three
// values for ResolvedTagNameForTest callers in the external package.
type AttrMapScopeForTest = attrMapScope

const (
	AttrMapScopeAnyForTest      = attrMapScopeAny
	AttrMapScopeResourceForTest = attrMapScopeResource
	AttrMapScopeSpanForTest     = attrMapScopeSpan
	AttrMapScopeEventForTest    = attrMapScopeEvent
	AttrMapScopeLinkForTest     = attrMapScopeLink
)

// BuildAttributeValuesSQLForTest re-exports buildAttributeValuesSQL
// (cerberus issue #2776's materialized-attribute tag-values routing lives
// inside it) so the external tempo_test package can pin its SQL shape
// directly, mirroring the re-export pattern above.
var BuildAttributeValuesSQLForTest = buildAttributeValuesSQL
