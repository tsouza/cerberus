package chplan

import "strconv"

// Default output column names shared by a node's RowType() and the chsql
// emitter that renders it. A node whose alias field is empty publishes
// the column under the constant here, and the emitter aliases the
// rendered expression the same way; keeping both on one constant is what
// lets RowType() describe the emitted statement rather than restate it.

// DefaultSampleTimestampColumn is the wire-projected name of a Sample's
// timestamp — the column every matrix-shape RangeWindow surfaces its
// anchor under when its own TimestampColumn names the anchor alias itself.
const DefaultSampleTimestampColumn = "TimeUnix"

// DefaultSampleValueColumn is the wire-projected name of a Sample's value,
// the fallback for a metrics node whose ValueAlias is empty.
const DefaultSampleValueColumn = "Value"

// MetricsBucketColumn is the per-row bucket column the quantile_over_time
// and histogram_over_time matrix emitters project. It mirrors Tempo's
// internal `__bucket` label (pkg/traceql/engine_metrics.go,
// internalLabelBucket) so the Tempo handler can pick the bucket out of
// the row stream by a stable name.
const MetricsBucketColumn = "__bucket"

// MetricsMultiQuantilePhiColumn is the synthetic per-phi label a
// multi-quantile `quantile_over_time(attr, p1, p2, …)` tags each output
// row with.
const MetricsMultiQuantilePhiColumn = "__phi__"

// MetricsCompareSelectionColumn / MetricsCompareAttrColumn /
// MetricsCompareValColumn are the default output names of a
// MetricsCompare's selection marker, attribute name and attribute value.
const (
	MetricsCompareSelectionColumn = "is_selection"
	MetricsCompareAttrColumn      = "attr"
	MetricsCompareValColumn       = "val"
)

// MetricsGroupKeyName is the output name of the i-th group-by key of a
// Tempo metrics node that carries no alias for it.
func MetricsGroupKeyName(i int) string { return "g" + strconv.Itoa(i) }
