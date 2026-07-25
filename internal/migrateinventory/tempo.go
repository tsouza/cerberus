package migrateinventory

// TempoInventory is always exactly this: an honest statement that Tempo has
// no TSDB-head-block or ranked cardinality-stats analog to probe. Tempo's
// storage is block-based spans with no in-memory series-cardinality surface
// exposed over HTTP; a distinct-span-name or service-name count would not
// measure OOM risk the way head-series cardinality does, so no such proxy is
// computed. Present only when the operator supplied --tempo-source.
type TempoInventory struct {
	Source     string `json:"source"`
	OutOfScope string `json:"out_of_scope"`
}

// TempoInventoryOutOfScopeReason is the fixed, specific reason recorded for
// every TempoInventory — a statement of Tempo's storage model, not a
// placeholder for a missing feature.
const TempoInventoryOutOfScopeReason = "tempo storage is block-based spans with no TSDB-head-block or " +
	"ranked cardinality-stats API analogous to Prometheus's status/tsdb or Loki's index/stats; no " +
	"cardinality proxy is computed because a span/service-name count would not measure OOM risk"

// NewTempoInventory builds the fixed out-of-scope entry for a Tempo source.
// No HTTP round-trip is made: there is nothing to call, so pretending to
// probe would itself be dishonest theater.
func NewTempoInventory(source string) TempoInventory {
	return TempoInventory{Source: source, OutOfScope: TempoInventoryOutOfScopeReason}
}
