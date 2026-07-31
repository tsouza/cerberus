package promql

import (
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// hasUnpinnedMetricNameMatcher identifies selectors whose visible metric name
// is a predicate rather than a literal. Such selectors need the histogram
// arms below because classic histogram rows store the bare base name while
// Prometheus exposes only synthetic `_bucket`, `_count`, and `_sum` names.
func hasUnpinnedMetricNameMatcher(matchers []*labels.Matcher) bool {
	for _, m := range matchers {
		if m.Name == model.MetricNameLabel && m.Type != labels.MatchEqual {
			return true
		}
	}
	return false
}

func splitRegexHistogramMatchers(matchers []*labels.Matcher) (names, scan, le []*labels.Matcher) {
	for _, m := range matchers {
		switch m.Name {
		case model.MetricNameLabel:
			names = append(names, m)
		case "le":
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

func regexHistogramScan(s schema.Metrics, matchers []*labels.Matcher) chplan.Node {
	scan := &chplan.Scan{Table: s.HistogramTable}
	if pred := buildPredicate(matchers, s); pred != nil {
		return &chplan.Filter{Input: scan, Predicate: pred}
	}
	return scan
}

func buildRegexHistogramCompanionArm(
	s schema.Metrics, cat *metadataCatalog, names, scanMatchers []*labels.Matcher, suffix, sourceColumn string,
) chplan.Node {
	input := regexHistogramScan(s, scanMatchers)
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
	if ctx.step > 0 && !ctx.start.IsZero() && !ctx.end.IsZero() {
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
