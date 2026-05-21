package promql

import (
	"strings"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// bucketSuffix is the Prometheus convention for classic-histogram bucket
// companion series — `<base>_bucket`. The OTel-CH classic histogram row
// instead stores one row per observation under the BARE base name with
// parallel `BucketCounts` × `ExplicitBounds` arrays, so a bare selector of
// `<X>_bucket{le="0.5"}` must fan out the array into one Sample-shape row
// per bucket boundary with a synthesized `le` label.
const bucketSuffix = "_bucket"

// isClassicBucketSelector reports whether the metric name is a
// `<base>_bucket` companion that should be routed through the histogram
// table with per-bucket fan-out. Returns the bare base name on a hit.
//
// Mirrors the precedent established by `_count` / `_sum` companion
// routing (schema.Metrics.HistogramCompanionColumn) and the existing
// `_bucket` matcher-strip done by `stripBucketSuffix` on the
// histogram_quantile path. The bucket case is structurally different
// from `_count` / `_sum` — those project a single column as `Value`,
// while `_bucket` fans the array into N rows per source row — so the
// helper lives next to the fan-out wrapper rather than as another
// branch in HistogramCompanionColumn.
func isClassicBucketSelector(metricName string, s schema.Metrics) (bare string, ok bool) {
	if metricName == "" {
		return "", false
	}
	if s.HistogramTable == "" {
		return "", false
	}
	if !strings.HasSuffix(metricName, bucketSuffix) {
		return "", false
	}
	return metricName[:len(metricName)-len(bucketSuffix)], true
}

// splitBucketMatchers partitions a matcher list into:
//
//   - scanMatchers: matchers that resolve against scan-row columns
//     (`__name__`, attribute keys present on the histogram row).
//     `__name__` is rewritten to the bare base name so the scan filter
//     resolves against the OTel-CH row's MetricName.
//   - leMatchers: matchers naming the synthetic `le` label, which only
//     exists post-fanout. These are applied as a Filter on the canonical
//     Sample shape AFTER the fan-out Project synthesises `Attributes['le']`.
//
// Copy-on-write semantics mirror stripBucketSuffix / rewriteMetricName:
// the input slice + entries are never mutated.
func splitBucketMatchers(matchers []*labels.Matcher, bareName string) (scanMatchers, leMatchers []*labels.Matcher) {
	scanMatchers = make([]*labels.Matcher, 0, len(matchers))
	for _, m := range matchers {
		if m.Name == "le" {
			leMatchers = append(leMatchers, m)
			continue
		}
		if m.Name == model.MetricNameLabel && m.Type == labels.MatchEqual && m.Value != bareName {
			copied, err := labels.NewMatcher(m.Type, m.Name, bareName)
			if err != nil {
				// labels.NewMatcher only fails on regex compile for the
				// match-regex types; MatchEqual cannot fail. Fall back
				// to the original matcher so the lowering still produces
				// a valid plan even on the impossible path.
				scanMatchers = append(scanMatchers, m)
				continue
			}
			scanMatchers = append(scanMatchers, copied)
			continue
		}
		scanMatchers = append(scanMatchers, m)
	}
	return scanMatchers, leMatchers
}

// wrapHistogramBucketFanout wraps a histogram-table Scan in a two-stage
// Project that fans every histogram row into N+1 Sample-shape rows — one
// per `(le, cumulative_count)` pair — where:
//
//   - `le` carries the explicit bound for buckets 1..N (rendered as a
//     decimal string), and `"+Inf"` for the trailing overflow bucket.
//   - The cumulative count is `arraySum(arraySlice(BucketCounts, 1, i))`
//     — i.e. observations with value ≤ ExplicitBounds[i], matching the
//     Prom `_bucket{le=X}` cumulative-counter convention. OTel-CH stores
//     per-bucket counts (non-cumulative); the slice-sum is what makes
//     the wire shape Prom-compatible.
//   - The synthesised `le` label is mapInsert'd onto the row's Attributes
//     map so downstream `sum by(le) (...)` aggregations see a real
//     grouping key (just like Prom-native classic-histogram series).
//
// The MetricName carries the SUFFIXED name (`<base>_bucket`) so the
// downstream Sample-row contract surfaces the Prom-visible series name —
// /api/v1/series and /api/v1/query both round-trip the user's spelling.
//
// Plan shape produced (outer-most first):
//
//	Project [MetricName='<base>_bucket', Attributes+le, TimeUnix, Value]
//	  Project [MetricName, Attributes, TimeUnix, ExplicitBounds, BucketCounts,
//	           arrayJoin(arrayEnumerate(BucketCounts)) AS le_idx]
//	    Scan(otel_metrics_histogram)
//
// The inner arrayJoin fans the row; the outer Project rewrites the
// canonical Sample columns using the fanned `le_idx`. CH's `arrayJoin`
// in a SELECT position is the lateral fan-out idiom — equivalent to an
// ARRAY JOIN clause but expressed at the expression level so we can
// reuse the existing Project node without introducing a new IR node.
//
// `le_idx` is a CH-safe bare identifier (lowercase ASCII + underscore +
// digits only) so the BareIdent trust contract is satisfied. The name
// is prefixed `le_` to avoid colliding with any plausible user-supplied
// label (Prom forbids labels starting with `__` for the public range and
// reserved-internal range; `le_idx` falls outside both).
func wrapHistogramBucketFanout(scanOrFilter chplan.Node, suffixedName string, s schema.Metrics) chplan.Node {
	// Inner Project — pass the histogram row's identity columns through
	// and add the fanned bucket index via arrayJoin. Every output row
	// carries one (MetricName, Attributes, TimeUnix, ExplicitBounds,
	// BucketCounts, le_idx) tuple — the same row repeats N+1 times,
	// once per BucketCounts entry, with le_idx running 1..length(BucketCounts).
	fanout := &chplan.Project{
		Input: scanOrFilter,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ExplicitBoundsColumn}, Alias: s.ExplicitBoundsColumn},
			{Expr: &chplan.ColumnRef{Name: s.BucketCountsColumn}, Alias: s.BucketCountsColumn},
			{
				Expr: &chplan.FuncCall{
					Name: "arrayJoin",
					Args: []chplan.Expr{
						&chplan.FuncCall{
							Name: "arrayEnumerate",
							Args: []chplan.Expr{&chplan.ColumnRef{Name: s.BucketCountsColumn}},
						},
					},
				},
				Alias: bucketIdxAlias,
			},
		},
	}

	// Outer Project — synthesize the canonical Sample shape with the
	// suffixed metric name + the `le` label baked into Attributes + the
	// cumulative bucket count as Value.
	//
	//   le_str = if(le_idx > length(ExplicitBounds), '+Inf',
	//               toString(ExplicitBounds[le_idx]))
	//
	// BucketCounts has length = length(ExplicitBounds)+1 — the trailing
	// entry is the +Inf overflow bucket. The branch on `le_idx >
	// length(ExplicitBounds)` selects the +Inf label for that last row
	// and reads from ExplicitBounds for every other row.
	//
	//   cum    = arraySum(arraySlice(BucketCounts, 1, le_idx))
	//
	// CH's arraySlice(arr, 1, N) returns the first N elements; arraySum
	// gives the cumulative count for observations with value ≤
	// ExplicitBounds[le_idx]. Wrapped in toFloat64 so the canonical
	// Sample-row Value column stays Float64 (BucketCounts is UInt64).
	leIdx := &chplan.BareIdent{Name: bucketIdxAlias}
	lengthBounds := &chplan.FuncCall{
		Name: "length",
		Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ExplicitBoundsColumn}},
	}
	boundAtIdx := &chplan.Subscript{
		Container: &chplan.ColumnRef{Name: s.ExplicitBoundsColumn},
		Key:       leIdx,
	}
	leStr := &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{
			&chplan.Binary{Op: chplan.OpGt, Left: leIdx, Right: lengthBounds},
			&chplan.LitString{V: "+Inf"},
			&chplan.FuncCall{
				Name: "toString",
				Args: []chplan.Expr{boundAtIdx},
			},
		},
	}
	mergedAttrs := &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{
			&chplan.ColumnRef{Name: s.AttributesColumn},
			&chplan.FuncCall{
				Name: "map",
				Args: []chplan.Expr{
					&chplan.LitString{V: "le"},
					leStr,
				},
			},
		},
	}
	cumCount := &chplan.FuncCall{
		Name: "toFloat64",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "arraySum",
				Args: []chplan.Expr{
					&chplan.FuncCall{
						Name: "arraySlice",
						Args: []chplan.Expr{
							&chplan.ColumnRef{Name: s.BucketCountsColumn},
							&chplan.LitInt{V: 1},
							leIdx,
						},
					},
				},
			},
		},
	}
	return &chplan.Project{
		Input: fanout,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: suffixedName}, Alias: s.MetricNameColumn},
			{Expr: mergedAttrs, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: cumCount, Alias: s.ValueColumn},
		},
	}
}

// bucketIdxAlias is the CH-safe bare identifier used to surface the
// fanned `arrayJoin(arrayEnumerate(BucketCounts))` value into the outer
// Project's expressions. Lowercase ASCII / underscore / digit shape
// satisfies the BareIdent trust contract (see chplan.BareIdent doc).
const bucketIdxAlias = "le_idx"
