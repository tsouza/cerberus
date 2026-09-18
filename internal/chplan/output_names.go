package chplan

import "strconv"

// Output names a node's RowType() claims and its emitter renders. Each is
// spelled once, here, so the schema claim and the emitted alias cannot drift
// apart. RangeWindowAnchorColumn (range_window.go) is the same kind of name.
const (
	// MatrixTimestampColumn is the public timestamp a matrix-shaped
	// RangeWindow publishes when its TimestampColumn is the anchor itself.
	MatrixTimestampColumn = "TimeUnix"
	// DefaultValueColumn is the value output a reducer publishes when its
	// ValueAlias is unset.
	DefaultValueColumn = "Value"
	// MultiQuantilePhiColumn tags each row of a multi-quantile fan-out with
	// the phi it answers.
	MultiQuantilePhiColumn = "__phi__"
	// HistogramBucketColumn is the bucket key quantile_over_time and
	// histogram_over_time publish when their BucketAlias is unset.
	HistogramBucketColumn = "__bucket"
	// CompareSelectionColumn, CompareAttrColumn and CompareValColumn are
	// MetricsCompare's outputs when the matching alias is unset.
	CompareSelectionColumn = "is_selection"
	CompareAttrColumn      = "attr"
	CompareValColumn       = "val"
)

// OutputDefault returns name, or fallback when name is unset.
func OutputDefault(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

// outerGroupNamePrefix prefixes the synthetic name of an un-aliased group key.
const outerGroupNamePrefix = "g"

// OuterGroupNames returns the output name of every group key: its alias when
// one is set, else the synthetic "g<i>" for its position. Nil when there are
// no keys.
func OuterGroupNames(keys []Expr, aliases []string) []string {
	if len(keys) == 0 {
		return nil
	}
	names := make([]string, len(keys))
	for i := range names {
		if i < len(aliases) {
			names[i] = aliases[i]
		}
		if names[i] == "" {
			names[i] = outerGroupNamePrefix + strconv.Itoa(i)
		}
	}
	return names
}
