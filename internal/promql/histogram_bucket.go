package promql

import (
	"strings"

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
//     exists once the fan-out synthesises each bucket's bound string.
//     [wrapHistogramBucketFanout] applies them to the per-row
//     `(le, cumulative count)` pairs before arrayJoin expands them.
//
// Copy-on-write semantics mirror stripBucketSuffix / rewriteMetricName:
// the input slice + entries are never mutated.
//
// Classification is delegated to wireArms (#1756): WireArmWireBound picks
// out the `le` split, and WireArmWireNamePinned is the one arm this
// caller ever expects a `__name__` matcher to land in — the caller
// (resolveSelectorRouting) only reaches splitBucketMatchers after
// isClassicBucketSelector has already confirmed the selector's name is a
// pinned, `_bucket`-suffixed literal. An unpinned `__name__` matcher
// classifies WireArmWireNameUnpinned and — matching the pre-migration
// behaviour, which had no branch for it either — passes through into
// scanMatchers unchanged alongside WireArmStorage.
func splitBucketMatchers(matchers []*labels.Matcher, bareName string) (scanMatchers, leMatchers []*labels.Matcher) {
	scanMatchers = make([]*labels.Matcher, 0, len(matchers))
	w := wireArms(matchers)
	for i, m := range matchers {
		switch w.Arms[i] {
		case WireArmWireBound:
			leMatchers = append(leMatchers, m)
		case WireArmWireNamePinned:
			if m.Value == bareName {
				scanMatchers = append(scanMatchers, m)
				continue
			}
			// A pinned matcher is an equality, which labels.NewMatcher
			// builds without compiling a pattern, so building it cannot fail.
			scanMatchers = append(scanMatchers, labels.MustNewMatcher(labels.MatchEqual, m.Name, bareName))
		default: // WireArmStorage, WireArmWireNameUnpinned
			scanMatchers = append(scanMatchers, m)
		}
	}
	return scanMatchers, leMatchers
}

// wrapHistogramBucketFanout wraps a histogram-table Scan in a two-stage
// Project that fans every histogram row into N+1 Sample-shape rows — one
// per `(le, cumulative_count)` pair — where:
//
//   - `le` carries the explicit bound for buckets 1..N (rendered as a
//     decimal string), and `"+Inf"` for the trailing overflow bucket.
//   - The cumulative count is the sum of BucketCounts[1..i] — i.e.
//     observations with value ≤ ExplicitBounds[i], matching the Prom
//     `_bucket{le=X}` cumulative-counter convention. OTel-CH stores
//     per-bucket counts (non-cumulative); the running sum is what makes
//     the wire shape Prom-compatible.
//   - The synthesised `le` label is mapInsert'd onto the row's Attributes
//     map so downstream `sum by(le) (...)` aggregations see a real
//     grouping key (just like Prom-native classic-histogram series).
//
// The MetricName carries the SUFFIXED name (`<base>_bucket`) so the
// downstream Sample-row contract surfaces the Prom-visible series name —
// /api/v1/series and /api/v1/query both round-trip the user's spelling.
//
// Plan shape produced (outer-most first), in one of two forms. For a
// selection a range window reads without the stale-marker layout, the
// index shape ([bucketIndexFanout]):
//
//	Project [MetricName='<base>_bucket', Attributes+le, TimeUnix, Value]
//	  Project [MetricName, Attributes, TimeUnix, ExplicitBounds, BucketCounts,
//	           arrayJoin([arrayFilter(<le matchers>,]
//	                     arrayEnumerate(BucketCounts)[)]) AS le_idx]
//	    Scan(otel_metrics_histogram)
//
// and for every other selection, the pair shape ([bucketPairFanout]):
//
//	Project [MetricName='<base>_bucket', Attributes+le, TimeUnix, Value]
//	  Project [MetricName, Attributes, TimeUnix,
//	           arrayJoin([arrayFilter(<le matchers>,]
//	                     arrayZip(le strings, cumulative counts)[)]) AS le_pair]
//	    <stale-marker layout, or Scan(otel_metrics_histogram)>
//
// The inner arrayJoin fans the row into one row per bucket — per selected
// bucket when the selector carries `le` matchers, which both shapes apply
// to the bucket bounds before arrayJoin expands them; the outer Project
// rewrites the canonical Sample columns from the fanned index or pair. CH's
// `arrayJoin` in a SELECT position is the lateral fan-out idiom —
// equivalent to an ARRAY JOIN clause but expressed at the expression level
// so we can reuse the existing Project node without introducing a new IR
// node.
//
// The two shapes trade differently. arrayJoin replicates every column of
// the row it expands, so the index shape copies both bucket arrays onto
// every fanned row, and the pair shape, whose fanned rows carry two
// scalars, does not. But the pair shape builds every bucket's `le` string
// inside arrayJoin's argument, which ClickHouse cannot drop from a copy of
// the fan-out that reads none of its columns: the row-count probe a range
// window's fan-out guard runs over the same rows then reads ExplicitBounds
// and builds the strings too. The index shape's arrayJoin reads only
// BucketCounts (and ExplicitBounds only to test `le` matchers), so that
// probe reads no more of the histogram rows than it counts. Under the
// layout every copy reads ExplicitBounds anyway, for the per-series bound
// union, and an instant selection's fanned rows reach a latest-sample
// pick that no such probe counts, so the pair shape keeps the fanned rows
// narrow there. A range window without the layout is where the probe
// runs, and the index shape keeps it narrow.
//
// `le_idx` and `le_pair` are CH-safe bare identifiers (lowercase ASCII +
// underscore + digits only) so the BareIdent trust contract is satisfied.
// They are prefixed `le_` to avoid colliding with any plausible
// user-supplied label (Prom forbids labels starting with `__` for the
// public range and reserved-internal range; `le_idx` and `le_pair` fall
// outside both).
//
// Under staleMarkersEncoded a stale-marker row first takes its series'
// bucket layout ([staleMarkerBucketLayout], over the rows layout admits)
// and every row it fans into carries the encoded marker as Value, so the
// downstream latest-sample pick ends each `le` series at the marker.
func wrapHistogramBucketFanout(
	scanOrFilter chplan.Node, suffixedName string, leMatchers []*labels.Matcher, s schema.Metrics,
	cat *metadataCatalog, mode staleMarkerMode, layout staleLayoutWindow,
) chplan.Node {
	// Inner Project — pass the histogram row's identity columns through
	// and add the fanned bucket via arrayJoin. The same row repeats N+1
	// times, once per BucketCounts entry (once per selected entry under
	// `le` matchers), with le_idx or le_pair running over the row's
	// buckets in order.
	//
	// Merge resource attributes here, where the raw ResourceAttributes
	// column is still in scope (this Project reads the histogram Scan
	// directly). The arrayJoin fan-out + the outer `mapConcat(Attributes,
	// map('le', …))` then carry the already-merged map, so the selector
	// seam above treats this path as attributes-pre-merged (see lower.go
	// bucket branch) and does not re-reference ResourceAttributes. Catalog
	// mode passes the raw sources through instead — see
	// [catalogAttributesProjections].
	rowProjections := []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
	}
	rowProjections = append(rowProjections, selectorLabelProjections(cat, s)...)
	rowProjections = append(
		rowProjections,
		chplan.Projection{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
	)
	bounds := chplan.Expr(&chplan.ColumnRef{Name: s.ExplicitBoundsColumn})
	counts := chplan.Expr(&chplan.ColumnRef{Name: s.BucketCountsColumn})
	fanInput := scanOrFilter
	if mode == staleMarkersEncoded {
		// The layout stage reads every row column by the name this stage
		// would have given it, and publishes the marker-aware bucket
		// arrays under their own names ([staleMarkerBucketLayout]).
		flags := chplan.Projection{Expr: &chplan.ColumnRef{Name: s.StaleMarkerFlagsColumn()}, Alias: s.StaleMarkerFlagsColumn()}
		rowProjections = append(rowProjections, flags)
		fanInput = staleMarkerBucketLayout(scanOrFilter, rowProjections, layout.bound(s.TimestampColumn), s)
		passThrough := make([]chplan.Projection, len(rowProjections))
		for i, p := range rowProjections {
			passThrough[i] = chplan.Projection{Expr: &chplan.ColumnRef{Name: p.Alias}, Alias: p.Alias}
		}
		rowProjections = passThrough
		bounds = &chplan.ColumnRef{Name: staleLayoutBoundsAlias}
		counts = &chplan.ColumnRef{Name: staleLayoutCountsAlias}
	}
	// The fan-out's shape follows its consumer (see the plan shapes
	// above). Either way `le` ends up in Attributes and the bucket's
	// cumulative count in Value.
	var fanned []chplan.Projection
	var leStr, cumCount chplan.Expr
	if layout.pending && mode != staleMarkersEncoded {
		fanned, leStr, cumCount = bucketIndexFanout(bounds, counts, leMatchers, s)
	} else {
		fanned, leStr, cumCount = bucketPairFanout(bounds, counts, leMatchers)
	}
	fanout := &chplan.Project{Roles: metricRoles(s), Input: fanInput, Projections: append(rowProjections, fanned...)}

	// Outer Project — synthesize the canonical Sample shape with the
	// suffixed metric name + the `le` label baked into Attributes + the
	// cumulative bucket count as Value.
	//
	// No canonicalisation here: mapConcat appends `le` to whatever the
	// inner projection produced, and that projection already binds
	// Attributes canonically, so every row of one logical series gets the
	// same key order. Sorting again would cost a sort per row and change
	// nothing.
	mergedAttrs := &chplan.FuncCall{
		Fn: chplan.FnMapMerge,
		Args: []chplan.Expr{
			&chplan.ColumnRef{Name: s.AttributesColumn},
			&chplan.FuncCall{
				Fn:   chplan.FnMap,
				Args: []chplan.Expr{&chplan.LitString{V: bucketBoundLabel}, leStr},
			},
		},
	}
	metricNameExpr := chplan.Expr(&chplan.LitString{V: suffixedName})
	if suffixedName == "" {
		metricNameExpr = syntheticMetricNameExpr(s, bucketSuffix)
	}
	outer := []chplan.Projection{{Expr: metricNameExpr, Alias: s.MetricNameColumn}}
	// The synthesised `le` key rides on Attributes in both modes: it is the
	// only place a bucket bound exists as a label once the row is fanned.
	outer = append(outer, catalogAttributesProjections(cat, s, mergedAttrs)...)
	outer = append(
		outer,
		chplan.Projection{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
		chplan.Projection{Expr: staleMarkerValueExpr(cumCount, mode, s), Alias: s.ValueColumn},
	)
	return &chplan.Project{Roles: metricRoles(s), Input: fanout, Projections: outer}
}

// bucketIndexFanout returns the index shape's fan-out projections, which
// carry bounds and counts under the stored array columns' names and expand
// each bucket's index as [bucketIdxAlias], and the `le` string and the
// cumulative count the outer Project reads from them ([wrapHistogramBucketFanout]):
//
//	le_str = if(le_idx > length(ExplicitBounds), '+Inf',
//	            classicBucketLeStringExpr(ExplicitBounds[le_idx]))
//	cum    = toFloat64(arraySum(arraySlice(BucketCounts, 1, le_idx)))
//
// BucketCounts has length = length(ExplicitBounds)+1 — the trailing entry
// is the +Inf overflow bucket, which the branch on `le_idx >
// length(ExplicitBounds)` labels. arraySlice(BucketCounts, 1, le_idx)
// holds the per-bucket counts up to bucket le_idx, so its sum is the count
// of observations with value ≤ ExplicitBounds[le_idx], wrapped in
// toFloat64 so the canonical Sample-row Value column stays Float64
// (BucketCounts is UInt64).
func bucketIndexFanout(bounds, counts chplan.Expr, leMatchers []*labels.Matcher, s schema.Metrics) (fanned []chplan.Projection, le, cum chplan.Expr) {
	indices := chplan.Expr(&chplan.FuncCall{Fn: chplan.FnArrayEnumerate, Args: []chplan.Expr{counts}})
	if len(leMatchers) > 0 {
		keep := leMatchersPredicate(leMatchers, classicBucketLeStringExpr(&chplan.BareIdent{Name: bucketIdxCandidateParam}, bounds))
		indices = &chplan.FuncCall{Fn: chplan.FnArrayFilter, Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{bucketIdxCandidateParam}, Body: keep},
			indices,
		}}
	}
	fanned = []chplan.Projection{
		{Expr: bounds, Alias: s.ExplicitBoundsColumn},
		{Expr: counts, Alias: s.BucketCountsColumn},
		{Expr: &chplan.FuncCall{Fn: chplan.FnArrayJoin, Args: []chplan.Expr{indices}}, Alias: bucketIdxAlias},
	}
	idx := &chplan.BareIdent{Name: bucketIdxAlias}
	le = classicBucketLeStringExpr(idx, &chplan.ColumnRef{Name: s.ExplicitBoundsColumn})
	cum = &chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{
		&chplan.FuncCall{Fn: chplan.FnArraySum, Args: []chplan.Expr{
			&chplan.FuncCall{Fn: chplan.FnArraySlice, Args: []chplan.Expr{
				&chplan.ColumnRef{Name: s.BucketCountsColumn}, &chplan.LitInt{V: 1}, idx,
			}},
		}},
	}}
	return fanned, le, cum
}

// bucketPairFanout returns the pair shape's fan-out projection, which
// expands each selected bucket's `(le, cumulative count)` pair as
// [bucketPairAlias], and the pair's two elements the outer Project reads
// ([wrapHistogramBucketFanout]). The pairs are built per histogram row:
//
//	le strings = arrayMap(le_idx -> if(le_idx > length(bounds), '+Inf',
//	                 classicBucketLeStringExpr(bounds[le_idx])), arrayEnumerate(counts))
//	cum counts = arrayMap(c -> toFloat64(c), arrayCumSum(counts))
//
// arrayCumSum(counts)[i] is the count of observations with value ≤
// bounds[i], wrapped in toFloat64 so the canonical Sample-row Value column
// stays Float64 (BucketCounts is UInt64).
func bucketPairFanout(bounds, counts chplan.Expr, leMatchers []*labels.Matcher) (fanned []chplan.Projection, le, cum chplan.Expr) {
	leStrs := &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{bucketIdxAlias},
			Body:   classicBucketLeStringExpr(&chplan.BareIdent{Name: bucketIdxAlias}, bounds),
		},
		&chplan.FuncCall{Fn: chplan.FnArrayEnumerate, Args: []chplan.Expr{counts}},
	}}
	cumCounts := &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramBucketCount},
			Body:   &chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{&chplan.BareIdent{Name: paramBucketCount}}},
		},
		&chplan.FuncCall{Fn: chplan.FnArrayCumSum, Args: []chplan.Expr{counts}},
	}}
	pairs := keepBucketPairs(&chplan.FuncCall{Fn: chplan.FnArrayZip, Args: []chplan.Expr{leStrs, cumCounts}}, leMatchers)
	fanned = []chplan.Projection{{Expr: &chplan.FuncCall{Fn: chplan.FnArrayJoin, Args: []chplan.Expr{pairs}}, Alias: bucketPairAlias}}
	pairField := func(pos int64) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{
			&chplan.ColumnRef{Name: bucketPairAlias}, &chplan.LitInt{V: pos},
		}}
	}
	return fanned, pairField(bucketPairLe), pairField(bucketPairCount)
}

// bucketIdxAlias is the CH-safe bare identifier naming a bucket's 1-based
// index inside the fan-out's per-row arrays. Lowercase ASCII / underscore
// / digit shape satisfies the BareIdent trust contract (see
// chplan.BareIdent doc).
const bucketIdxAlias = "le_idx"

// bucketPairAlias names the fanned `(le, cumulative count)` tuple the
// fan-out's arrayJoin yields; bucketPairLe / bucketPairCount are the
// tuple's 1-based element positions.
const (
	bucketPairAlias       = "le_pair"
	bucketPairLe    int64 = 1
	bucketPairCount int64 = 2
)

// bucketIdxCandidateParam is the lambda parameter naming one bucket
// index while [bucketIndexFanout]'s `le` filter tests it. It is distinct
// from [bucketIdxAlias] so the lambda never shadows the fanned column the
// same Project defines.
const bucketIdxCandidateParam = "le_candidate_idx"

// bucketPairCandidateParam is the lambda parameter naming one zipped
// `(le, cumulative count)` pair while [keepBucketPairs] tests it. It is
// distinct from [bucketPairAlias] so the lambda never shadows the fanned
// column.
const bucketPairCandidateParam = "le_candidate"

// keepBucketPairs narrows the zipped `(le, cumulative count)` pairs of one
// histogram row to the ones every `le` matcher selects, so arrayJoin
// expands only the selected buckets instead of fanning every bucket and
// discarding most of the rows afterwards. Multiple matchers AND together,
// as PromQL's do for repeated matchers on one label. Regex values pass
// Returns pairs unchanged when there is no `le` matcher.
func keepBucketPairs(pairs chplan.Expr, leMatchers []*labels.Matcher) chplan.Expr {
	keep := leMatchersPredicate(leMatchers, &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{
		&chplan.BareIdent{Name: bucketPairCandidateParam}, &chplan.LitInt{V: bucketPairLe},
	}})
	if keep == nil {
		return pairs
	}
	return &chplan.FuncCall{Fn: chplan.FnArrayFilter, Args: []chplan.Expr{
		&chplan.Lambda{Params: []string{bucketPairCandidateParam}, Body: keep},
		pairs,
	}}
}

// leMatchersPredicate ANDs every `le` matcher against bound, the
// synthesised `le` string of one bucket, as PromQL ANDs repeated matchers
// on one label; nil when there is no matcher. matchOp covers all four
// labels.MatchType values. Regex values pass through as-is: the chsql
// emitter's anchoredRegexPattern wraps every OpMatch / OpNotMatch pattern
// in `^(?:...)$`, so an unanchored "1" only ever matches bucket "1", never
// "0.1" or "21". Both bucket consumers — the fan-out's bucket filter and
// histogram_quantile's ladder restriction — select buckets through it.
func leMatchersPredicate(leMatchers []*labels.Matcher, bound chplan.Expr) chplan.Expr {
	var pred chplan.Expr
	for _, m := range leMatchers {
		pred = andExpr(pred, &chplan.Binary{Op: matchOp(m.Type), Left: bound, Right: &chplan.LitString{V: m.Value}})
	}
	return pred
}

// isClassicBucketFanoutProject recognizes the row expansion produced above,
// in either shape. It changes one stored histogram into independent
// cumulative bucket counters, not evaluation anchors. Native rate grids can
// consume those counters after the enclosing canonical sample projection.
// An `le` matcher narrows the indices or the zipped pairs inside the
// arrayJoin and is looked through.
func isClassicBucketFanoutProject(p *chplan.Project, s schema.Metrics) bool {
	for _, projection := range p.Projections {
		if projection.Alias != bucketIdxAlias && projection.Alias != bucketPairAlias {
			continue
		}
		join, ok := projection.Expr.(*chplan.FuncCall)
		if !ok || join.Fn != chplan.FnArrayJoin || len(join.Args) != 1 {
			return false
		}
		fanned, ok := join.Args[0].(*chplan.FuncCall)
		if ok && fanned.Fn == chplan.FnArrayFilter && len(fanned.Args) == 2 {
			fanned, ok = fanned.Args[1].(*chplan.FuncCall)
		}
		if !ok {
			return false
		}
		if projection.Alias == bucketPairAlias {
			if fanned, ok = zippedBucketIndices(fanned); !ok {
				return false
			}
		}
		if fanned.Fn != chplan.FnArrayEnumerate || len(fanned.Args) != 1 {
			return false
		}
		column, ok := fanned.Args[0].(*chplan.ColumnRef)
		return ok && column.Name == s.BucketCountsColumn
	}
	return false
}

// zippedBucketIndices returns the bucket indices the pair shape's `le`
// strings are mapped over: the second argument of the arrayMap that is the
// first argument of pairs, an arrayZip.
func zippedBucketIndices(pairs *chplan.FuncCall) (*chplan.FuncCall, bool) {
	if pairs.Fn != chplan.FnArrayZip || len(pairs.Args) == 0 {
		return nil, false
	}
	les, ok := pairs.Args[0].(*chplan.FuncCall)
	if !ok || les.Fn != chplan.FnArrayMap || len(les.Args) != 2 {
		return nil, false
	}
	indices, ok := les.Args[1].(*chplan.FuncCall)
	return indices, ok
}
