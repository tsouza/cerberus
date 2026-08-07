package promql

import (
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// hasUnpinnedMetricNameMatcher identifies selectors whose visible metric
// name is not pinned to a literal — a regex matcher, a negated matcher, or
// no `__name__` matcher at all. All three shapes need the histogram arms
// below because classic histogram rows store the bare base name while
// Prometheus exposes only synthetic `_bucket`, `_count`, and `_sum` names;
// "no `__name__` matcher at all" is strictly less pinned than a regex one
// and must take the same fan-out, not the narrower TablesForUnknownName
// path a literal absence would otherwise fall through to. Delegates to
// wireArms's WireNamePinned split (#1756) rather than re-deriving the
// pinned/unpinned line: a selector is unpinned exactly when it carries no
// WireArmWireNamePinned matcher.
func hasUnpinnedMetricNameMatcher(matchers []*labels.Matcher) bool {
	return len(wireArms(matchers).WireNamePinned()) == 0
}

// Classification is delegated to wireArms (#1756): the union of
// WireArmWireNamePinned + WireArmWireNameUnpinned (via WireName()) is the
// `names` split — both pin-nesses land here unchanged, since this
// consumer resolves the wire name via a post-projection Filter over an
// already-aliased synthetic-name column (see buildRegexHistogramCompanionArm
// / buildRegexHistogramBucketArm) rather than a rewritten predicate, so it
// has no need for wireArms's ResolveName decision.
func splitRegexHistogramMatchers(matchers []*labels.Matcher) (names, scan, le []*labels.Matcher) {
	w := wireArms(matchers)
	for i, m := range matchers {
		switch w.Arms[i] {
		case WireArmWireNamePinned, WireArmWireNameUnpinned:
			names = append(names, m)
		case WireArmWireBound:
			le = append(le, m)
		default:
			scan = append(scan, m)
		}
	}
	return names, scan, le
}

func syntheticMetricNameExpr(s schema.Metrics, suffix string) chplan.Expr {
	return &chplan.FuncCall{
		Name: "concat",
		Args: []chplan.Expr{
			&chplan.ColumnRef{Name: s.MetricNameColumn},
			&chplan.InlineString{V: suffix},
		},
	}
}

func regexHistogramNamePredicate(names []*labels.Matcher, s schema.Metrics) chplan.Expr {
	if len(names) == 0 {
		return nil
	}
	return buildPredicate(names, s)
}

func regexHistogramScanTable(table string, s schema.Metrics, matchers []*labels.Matcher) chplan.Node {
	scan := &chplan.Scan{Table: table}
	if pred := buildPredicate(matchers, s); pred != nil {
		return &chplan.Filter{Input: scan, Predicate: pred}
	}
	return scan
}

func regexHistogramScan(s schema.Metrics, matchers []*labels.Matcher) chplan.Node {
	return regexHistogramScanTable(s.HistogramTable, s, matchers)
}

// buildRegexHistogramCompanionArmTable builds one companion arm (a `Count`
// or `Sum` column projected as the synthesized `<base><suffix>` wire name)
// scanning `table`. Shared by the classic-histogram companion arms
// (table = s.HistogramTable) and the exp-histogram companion arms
// (table = s.ExpHistogramTable, see buildRegexExpHistogramCompanionArm) —
// both physical layouts store Count/Sum as columns on a single row under
// the same synthetic-name convention, so only the source table differs.
func buildRegexHistogramCompanionArmTable(
	table string, s schema.Metrics, cat *metadataCatalog, names, scanMatchers []*labels.Matcher, suffix, sourceColumn string,
) chplan.Node {
	input := regexHistogramScanTable(table, s, scanMatchers)
	project := &chplan.Project{
		Input: input,
		Projections: append([]chplan.Projection{
			{Expr: syntheticMetricNameExpr(s, suffix), Alias: s.MetricNameColumn},
		}, append(
			catalogAttributesProjections(cat, s, selectorAttributesSource(cat, s)),
			chplan.Projection{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			chplan.Projection{
				Expr: &chplan.FuncCall{
					Name: "toFloat64",
					Args: []chplan.Expr{&chplan.ColumnRef{Name: sourceColumn}},
				},
				Alias: s.ValueColumn,
			},
		)...),
	}
	if pred := regexHistogramNamePredicate(names, s); pred != nil {
		return &chplan.Filter{Input: project, Predicate: pred}
	}
	return project
}

// buildRegexHistogramCompanionArm builds a classic-histogram companion arm
// (table = s.HistogramTable) — the thin wrapper the pre-#1549r1 call sites
// use; see buildRegexHistogramCompanionArmTable for the shared body.
func buildRegexHistogramCompanionArm(
	s schema.Metrics, cat *metadataCatalog, names, scanMatchers []*labels.Matcher, suffix, sourceColumn string,
) chplan.Node {
	return buildRegexHistogramCompanionArmTable(s.HistogramTable, s, cat, names, scanMatchers, suffix, sourceColumn)
}

// buildRegexExpHistogramCompanionArm builds an exp-histogram companion arm
// (table = s.ExpHistogramTable) — the residue-1 fix for #1549. Consumes the
// SAME suffix/column policy expHistogramSelectorRouting (lower.go) applies
// to the pinned path — `_count`/`_sum` only, via
// s.HistogramCompanionColumn — rather than re-deriving it: both physical
// layouts (classic and exp-histogram) store Count/Sum as columns on a
// single row, keyed by the bare metric name, so the only thing that
// differs between the two arms is the source table. Unlike the classic
// path there is no bucket-fanout arm here: an exp-histogram row carries no
// ExplicitBounds ladder (Scale/PositiveBucketCounts/NegativeBucketCounts
// instead), so `<base>_exp_hist_bucket` is not a wire-reachable series and
// gets no arm — matching the pinned path, which never builds one either.
//
// A bare exp-histogram selector (`<base>_exp_hist`, no companion suffix)
// also gets no arm here, for the same reason the pinned path (lower.go's
// expHistogramSelectorRouting) rejects it outside histogram_quantile() /
// histogram_count() / histogram_sum(): that row shape has no scalar Value
// column. Unlike the pinned path this is not a hard error — a regex
// `__name__` matcher fans across every metric it matches, most of which
// are ordinary Gauge/Sum/classic-histogram series the query is perfectly
// well-formed for, so rejecting the whole selector because ONE possible
// match is a bare exp-histogram name would be wrong; that portion of the
// union simply contributes zero rows, the same way a regex matching a
// bare CLASSIC-histogram name already does (that name isn't Prom-wire
// visible either, and gets no arm in buildRegexMetricArm/
// buildRegexHistogramCompanionArm).
func buildRegexExpHistogramCompanionArm(
	s schema.Metrics, cat *metadataCatalog, names, scanMatchers []*labels.Matcher, suffix, sourceColumn string,
) chplan.Node {
	return buildRegexHistogramCompanionArmTable(s.ExpHistogramTable, s, cat, names, scanMatchers, suffix, sourceColumn)
}

func buildRegexHistogramBucketArm(
	s schema.Metrics, cat *metadataCatalog, names, scanMatchers, leMatchers []*labels.Matcher,
) chplan.Node {
	input := regexHistogramScan(s, scanMatchers)
	input = wrapHistogramBucketFanout(input, "", s, cat)
	if pred := regexHistogramNamePredicate(names, s); pred != nil {
		input = &chplan.Filter{Input: input, Predicate: pred}
	}
	if pred := buildPredicate(leMatchers, func() schema.Metrics {
		leSchema := s
		leSchema.ResourceAttributesColumn = ""
		return leSchema
	}()); pred != nil {
		input = &chplan.Filter{Input: input, Predicate: pred}
	}
	return input
}

func buildRegexMetricArm(s schema.Metrics, cat *metadataCatalog, matchers []*labels.Matcher) chplan.Node {
	scan := scanFromTables(s.TablesForUnknownName())
	var input chplan.Node = scan
	if pred := buildPredicate(matchers, s); pred != nil {
		input = &chplan.Filter{Input: scan, Predicate: pred}
	}
	projections := []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
	}
	projections = append(projections, catalogAttributesProjections(cat, s, selectorAttributesSource(cat, s))...)
	projections = append(
		projections,
		chplan.Projection{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
		chplan.Projection{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
	)
	return &chplan.Project{Input: input, Projections: projections}
}

func lowerRegexHistogramSelector(v *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	names, scanMatchers, leMatchers := splitRegexHistogramMatchers(v.LabelMatchers)
	inputs := []chplan.Node{buildRegexMetricArm(s, ctx.catalog, v.LabelMatchers)}

	for _, suffix := range s.HistogramCompanionSuffixes() {
		_, sourceColumn, ok := s.HistogramCompanionColumn("base" + suffix)
		if !ok {
			continue
		}
		inputs = append(inputs, buildRegexHistogramCompanionArm(s, ctx.catalog, names, scanMatchers, suffix, sourceColumn))
	}
	inputs = append(inputs, buildRegexHistogramBucketArm(s, ctx.catalog, names, scanMatchers, leMatchers))

	// Exp-histogram companion arms (#1549 residue 1): same `_count`/`_sum`
	// suffix set as the classic arms above, read from ExpHistogramTable
	// instead — see buildRegexExpHistogramCompanionArm. No bucket arm: an
	// exp-histogram row has no ExplicitBounds ladder to fan.
	if s.ExpHistogramTable != "" {
		for _, suffix := range s.HistogramCompanionSuffixes() {
			_, sourceColumn, ok := s.HistogramCompanionColumn("base" + suffix)
			if !ok {
				continue
			}
			inputs = append(inputs, buildRegexExpHistogramCompanionArm(s, ctx.catalog, names, scanMatchers, suffix, sourceColumn))
		}
	}

	selectorInput := chplan.Node(&chplan.UnionAll{Inputs: inputs})
	anchor, err := selectorAnchor(v, ctx)
	if err != nil {
		return nil, err
	}
	if ctx.catalog == nil {
		selectorInput = augmentSelectorAttributes(selectorInput, ctx.withAttributesPreMerged(), s)
	}
	if ctx.inRangeVector {
		if hasModifier(v) {
			return &chplan.Filter{Input: selectorInput, Predicate: timeBoundExpr(s.TimestampColumn, anchor)}, nil
		}
		return selectorInput, nil
	}
	if ctx.rangeMode() {
		if hasAbsoluteAt(v) {
			return wrapRangeAbsoluteAtBroadcast(selectorInput, nil, anchor, ctx, s), nil
		}
		return wrapRangeLatestPerSeries(selectorInput, nil, anchor, ctx, s), nil
	}
	if ctx.metadataFullRange {
		if ctx.catalog != nil {
			return wrapMetadataCatalog(selectorInput, nil, ctx.start, ctx.end, s, ctx.catalog), nil
		}
		return wrapMetadataFullRange(selectorInput, nil, ctx.start, ctx.end, s), nil
	}
	return wrapInstantLatestPerSeries(selectorInput, nil, anchor, s), nil
}
