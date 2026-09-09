package logql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// LabelValueExpr resolves the ClickHouse expression that yields the
// VALUE of stream label `label` for a row, under the same OTel-CH
// storage-shape precedence a stream-selector matcher on that label
// resolves under: the `detected_level` family, an exporter-materialized
// k8s.* column, a dedicated top-level scalar column coalesced with the
// map, or the plain `ResourceAttributes[<label>]` lookup. See
// [matcherLHS], which this delegates to unchanged.
//
// [SelectorPredicate] is the sibling entry point for callers that need
// the whole selector as a predicate. This one exists for callers that
// have to PROJECT a label rather than filter on it — /index/volume's
// `targetLabels`, which groups rows by the requested labels' values.
// Projecting through a different rule than the selector scopes with is
// how one endpoint ends up answering over rows it then cannot describe:
// `targetLabels=service_name` selected every row through the dedicated
// `ServiceName` column while a literal `ResourceAttributes['service_name']`
// projection returned the empty map for all of them, collapsing a whole
// tenant's volume into a single unlabelled row.
func LabelValueExpr(label string, s schema.Logs) chplan.Expr {
	return matcherLHS(label, s)
}
