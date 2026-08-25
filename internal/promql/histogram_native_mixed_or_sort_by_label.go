package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_sort_by_label.go lowers `sort_by_label`/
// `sort_by_label_desc` wrapping a mixed float/histogram `or`
// (histogram_native_mixed_or.go's #2330/#2335 shape) — cerberus issue
// #2611, the sort_by_label-specific sibling `sort`/`sort_desc`
// (histogram_native_mixed_or_sort.go, cerberus issue #2605) and the
// eight value-reading date/time functions (cerberus issue #2609)
// already have.
//
// Reference Prometheus's funcSortByLabel/funcSortByLabelDesc
// (promql/functions.go) never read a sample's `H` or `F` field at all —
// the sort key comes entirely from the Metric label set via
// `slices.SortFunc`, so EVERY row survives regardless of its value
// type, only reordered. This is the OPPOSITE composition from
// sort()/sort_desc()'s own filterFloats rule (#2605), which drops every
// histogram-shaped row before sorting: sort_by_label needs a
// preserve-BOTH-arms mechanism, deliberately left open by cerberus issue
// #2605/PR #2607 pending exactly this issue (see that PR's own
// "Deliberately left open" section).
//
// sort.go's own [lowerSortByLabel] already composes over the NON-mixed
// all-histogram shape ([lowerExpHistogramValuedShape]) by feeding it
// straight into the same [chplan.OrderBy] the plain-vector path builds —
// [chplan.RowShapeOf]'s own `*OrderBy` case forwards whatever row shape
// its Input publishes (cerberus issue #2462), so an OrderBy over a
// HistogramProjection already stays HistogramRowShape end to end without
// [lowerSortByLabel] needing to know that. [chplan.MixedRowShape] earns
// the identical treatment for the SAME structural reason:
// [lowerMixedExpHistogramSetOp] builds the combined Mixed node exactly
// as it does for the query-root leaf case and for
// histogram_native_mixed_or_label.go's `label_replace`/`label_join`
// composition; [naturalSortKeyExpr]/[mergedLabelValueExpr]'s sort-key
// expression reads only Attributes (present, and meaning-identical, on
// both a float-shaped and a histogram-shaped row); and the wrapping
// OrderBy forwards MixedRowShape unchanged. No shadow resolution beyond
// what [lowerMixedExpHistogramSetOp] itself already applies for the
// `or`'s own de-duplication rule, no drop, and no per-payload branching
// is needed at all — unlike sort()/sort_desc() and the float-only
// aggregate family, this composition needs no reduction machinery
// whatsoever, only the SAME Mixed node label_replace's own composition
// already proved safe to hand to a generic-shaped forwarder that never
// reads Value.
func sortByLabelArgOverMixedExpHistogramSetOp(v parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.BinaryExpr, bool) {
	return mixedExpHistogramSetOp(v, s, ctx)
}

// lowerSortByLabelArgOverMixedExpHistogramSetOp lowers the shape
// [sortByLabelArgOverMixedExpHistogramSetOp] recognised: the combined
// Mixed node itself, unchanged — see this file's header for why no
// reduction is needed before [lowerSortByLabel]'s own OrderBy wraps it.
func lowerSortByLabelArgOverMixedExpHistogramSetOp(b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if b.ReturnBool {
		return nil, fmt.Errorf("promql: 'bool' modifier is only allowed on comparison binary ops")
	}
	return lowerMixedExpHistogramSetOp(b, s, ctx)
}
