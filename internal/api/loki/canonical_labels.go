package loki

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// canonicalLabelsFrag wraps a label-set Map expression in ClickHouse's
// canonical key-order function, establishing the invariant every
// series-identity key depends on: the attributes Map is ordered by key.
//
// A CH Map compares, groups and hashes POSITIONALLY over its
// (keys, values) arrays, so map('job','api','dc','eu') and
// map('dc','eu','job','api') are unequal despite carrying the same label
// set. Divergent key order reaches the store straight from ingestion —
// the OTel-CH exporter preserves OTLP wire order and never sorts — so two
// deliveries of one logical stream can land under two physical orders.
// Any metadata endpoint that keys on the whole Map (a GROUP BY label set,
// a uniqExact stream count) then sees two streams where there is one.
//
// The function name is shared with the plan IR's
// [chplan.CanonicalAttributesExpr] via [chplan.CanonicalMapFunc] so the
// Frag layer and the IR layer cannot drift onto different functions. It
// is order-only and idempotent, so it can never merge two genuinely
// different label sets and re-wrapping is a no-op.
//
// Callers MUST keep the wrap inside a SELECT alias whose name differs
// from the underlying column, and keep grouping by that alias. ClickHouse
// resolves GROUP BY identifiers against SELECT aliases BEFORE FROM
// columns, so `SELECT mapSort(RA) AS RA ... GROUP BY mapSort(RA)` groups
// by mapSort(mapSort(RA)) and fails with NOT_AN_AGGREGATE.
func canonicalLabelsFrag(inner chsql.Frag) chsql.Frag {
	return chsql.Call(chplan.CanonicalMapFunc, inner)
}
