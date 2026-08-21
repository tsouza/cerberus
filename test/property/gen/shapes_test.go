package gen

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/test/property"
)

func TestShapeRostersAreExact(t *testing.T) {
	families := []struct {
		name string
		got  []ShapeID
		want []ShapeID
	}{
		{
			name: "promql-instant",
			got:  PromQLShapeIDs(),
			want: []ShapeID{
				"promql.instant.selector",
				"promql.instant.sum",
				"promql.instant.sum-by",
				"promql.instant.rate",
				"promql.instant.sum-rate",
				"promql.instant.label-replace",
			},
		},
		{
			name: "promql-range",
			got:  PromQLRangeShapeIDs(),
			want: []ShapeID{
				"promql.range.selector",
				"promql.range.sum-by",
				"promql.range.rate",
				"promql.range.label-replace",
			},
		},
		{
			name: "promql-native-histogram",
			got:  ExpHistogramShapeIDs(),
			want: []ShapeID{
				"promql.native-histogram.function.count",
				"promql.native-histogram.function.sum",
				"promql.native-histogram.function.avg",
				"promql.native-histogram.function.stddev",
				"promql.native-histogram.function.stdvar",
				"promql.native-histogram.fraction",
				"promql.native-histogram.selector",
				"promql.native-histogram.sum",
				"promql.native-histogram.sum-by",
				"promql.native-histogram.rate",
				"promql.native-histogram.increase",
				"promql.native-histogram.quantile-selector",
				"promql.native-histogram.quantile-sum",
				"promql.native-histogram.quantile-sum-by",
				"promql.native-histogram.quantile-rate",
				"promql.native-histogram.quantile-increase",
				"promql.native-histogram.quantile-sum-rate",
				"promql.native-histogram.quantile-sum-by-rate",
				"promql.native-histogram.quantile-sum-increase",
				"promql.native-histogram.quantile-sum-by-increase",
			},
		},
		{
			name: "logql",
			got:  LogQLShapeIDs(),
			want: []ShapeID{
				"logql.stream.selector",
				"logql.stream.line-contains",
				"logql.stream.line-excludes",
				"logql.stream.label-format",
				"logql.stream.ip-contains",
				"logql.stream.ip-excludes",
				"logql.stream.pattern-contains",
				"logql.stream.pattern-contains-prefix",
				"logql.stream.pattern-contains-suffix",
				"logql.stream.pattern-excludes",
				"logql.stream.pattern-excludes-prefix",
				"logql.stream.pattern-excludes-suffix",
			},
		},
		{
			name: "traceql",
			got:  TraceQLShapeIDs(),
			want: []ShapeID{
				"traceql.selector.service",
				"traceql.pipeline.count",
				"traceql.selector.resource-attribute",
				"traceql.selector.span-attribute",
				"traceql.intrinsic.duration",
				"traceql.intrinsic.status",
				"traceql.intrinsic.name",
				"traceql.selector.regex",
				"traceql.selector.negated-attribute",
				"traceql.selector.conjunction",
				"traceql.structural.child",
				"traceql.structural.descendant",
				"traceql.pipeline.duration-aggregate",
				"traceql.pipeline.duration-aggregate-min",
				"traceql.pipeline.duration-aggregate-max",
				"traceql.pipeline.duration-aggregate-sum",
				"traceql.pipeline.select",
			},
		},
		{
			name: "promql-instant-window",
			got:  InstantWindowShapeIDs(),
			want: instantWindowExpectedRoster(),
		},
	}

	seen := map[ShapeID]string{}
	for _, family := range families {
		t.Run(family.name, func(t *testing.T) {
			if !slices.Equal(family.got, family.want) {
				t.Fatalf("shape roster = %v, want %v", family.got, family.want)
			}
			for _, id := range family.got {
				if id == "" {
					t.Fatal("shape roster contains an empty ID")
				}
				if prior, ok := seen[id]; ok {
					t.Fatalf("shape ID %q is duplicated in %s and %s", id, prior, family.name)
				}
				seen[id] = family.name
			}
		})
	}
}

func TestRandomShapeFamiliesPartitionCanonicalRosters(t *testing.T) {
	tests := []struct {
		name         string
		roster       []ShapeID
		families     [][]ShapeID
		wantFamilies [][]ShapeID
	}{
		{
			name:     "native-histogram",
			roster:   ExpHistogramShapeIDs(),
			families: expHistogramRandomShapeFamilies,
			wantFamilies: [][]ShapeID{
				{
					expHistogramCountFunctionShape,
					expHistogramSumFunctionShape,
					expHistogramAverageFunctionShape,
					expHistogramStddevFunctionShape,
					expHistogramStdvarFunctionShape,
				},
				{expHistogramFractionShape},
				{expHistogramSelectorShape},
				{expHistogramSumShape},
				{expHistogramSumByShape},
				{expHistogramRateShape},
				{expHistogramIncreaseShape},
				{expHistogramQuantileSelectorShape},
				{expHistogramQuantileSumShape},
				{expHistogramQuantileSumByShape},
				{expHistogramQuantileRateShape},
				{expHistogramQuantileIncreaseShape},
				{expHistogramQuantileSumRateShape},
				{expHistogramQuantileSumByRateShape},
				{expHistogramQuantileSumIncreaseShape},
				{expHistogramQuantileSumByIncreaseShape},
			},
		},
		{
			name:     "logql",
			roster:   LogQLShapeIDs(),
			families: logQLRandomShapeFamilies,
			wantFamilies: [][]ShapeID{
				{logQLSelectorShape},
				{logQLLineContainsShape},
				{logQLLineExcludesShape},
				{logQLLabelFormatShape},
				{logQLIPContainsShape},
				{logQLIPExcludesShape},
				{logQLPatternContainsShape, logQLPatternContainsPrefixShape, logQLPatternContainsSuffixShape},
				{logQLPatternExcludesShape, logQLPatternExcludesPrefixShape, logQLPatternExcludesSuffixShape},
			},
		},
		{
			name:     "traceql",
			roster:   TraceQLShapeIDs(),
			families: traceQLRandomShapeFamilies,
			wantFamilies: [][]ShapeID{
				{traceQLServiceShape},
				{traceQLCountShape},
				{traceQLResourceAttributeShape},
				{traceQLSpanAttributeShape},
				{traceQLDurationShape},
				{traceQLStatusShape},
				{traceQLNameShape},
				{traceQLRegexShape},
				{traceQLNegatedAttributeShape},
				{traceQLConjunctionShape},
				{traceQLStructuralChildShape},
				{traceQLStructuralDescendantShape},
				{
					traceQLDurationAggregateShape,
					traceQLDurationAggregateMinShape,
					traceQLDurationAggregateMaxShape,
					traceQLDurationAggregateSumShape,
				},
				{traceQLSelectShape},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !slices.EqualFunc(tc.families, tc.wantFamilies, func(got, want []ShapeID) bool {
				return slices.Equal(got, want)
			}) {
				t.Fatalf("random shape families = %v, want exact weighted layout %v", tc.families, tc.wantFamilies)
			}
			expected := make(map[ShapeID]struct{}, len(tc.roster))
			for _, shapeID := range tc.roster {
				expected[shapeID] = struct{}{}
			}
			seen := make(map[ShapeID]int, len(tc.roster))
			for familyIndex, family := range tc.families {
				if len(family) == 0 {
					t.Errorf("random shape family %d is empty", familyIndex)
				}
				for _, shapeID := range family {
					if _, ok := expected[shapeID]; !ok {
						t.Errorf("random shape families contain non-roster ID %q", shapeID)
					}
					seen[shapeID]++
				}
			}
			for _, shapeID := range tc.roster {
				if seen[shapeID] != 1 {
					t.Errorf("roster ID %q appears in %d random families, want exactly once", shapeID, seen[shapeID])
				}
			}
		})
	}
}

func instantWindowExpectedRoster() []ShapeID {
	profiles := []string{"wave", "positive-increments", "monotonic-running-total"}
	functions := []string{
		"sum-over-time",
		"count-over-time",
		"avg-over-time",
		"max-over-time",
		"min-over-time",
		"rate",
		"increase",
		"delta",
	}
	out := make([]ShapeID, 0, len(profiles)*len(functions))
	for _, profile := range profiles {
		for _, fn := range functions {
			out = append(out, ShapeID("promql.instant-window."+profile+"."+fn))
		}
	}
	return out
}

func TestByShapeGeneratorsDoNotCollapse(t *testing.T) {
	for _, shapeID := range PromQLShapeIDs() {
		t.Run(string(shapeID), func(t *testing.T) {
			seed := shapeTestSeed(shapeID)
			dataset := MetricsDataset().Example(seed)
			query := PromQLQueryForShape(dataset, shapeID).Example(seed)
			assertStampedShape(t, ShapeID(query.ShapeID), query.String, shapeID)
			got, err := classifyPromQLInstant(query.String)
			if err != nil || got != shapeID {
				t.Fatalf("query %q classified as %q (err=%v), want %q", query.String, got, err, shapeID)
			}
		})
	}

	for _, shapeID := range PromQLRangeShapeIDs() {
		t.Run(string(shapeID), func(t *testing.T) {
			seed := shapeTestSeed(shapeID)
			dataset := MetricsDataset().Example(seed)
			query := PromQLRangeQueryForShape(dataset, shapeID).Example(seed)
			if query.ShapeID != shapeID || query.String == "" {
				t.Fatalf("range generator stamped shape=%q query=%q, want shape=%q", query.ShapeID, query.String, shapeID)
			}
			got, err := classifyPromQLRange(query.String)
			if err != nil || got != shapeID {
				t.Fatalf("query %q classified as %q (err=%v), want %q", query.String, got, err, shapeID)
			}
		})
	}

	for _, shapeID := range ExpHistogramShapeIDs() {
		t.Run(string(shapeID), func(t *testing.T) {
			seed := shapeTestSeed(shapeID)
			dataset := ExpHistogramDataset().Example(seed)
			query := ExpHistogramQueryForShape(dataset, shapeID).Example(seed)
			assertStampedShape(t, ShapeID(query.ShapeID), query.String, shapeID)
			got, err := classifyExpHistogram(query.String)
			if err != nil || got != shapeID {
				t.Fatalf("query %q classified as %q (err=%v), want %q", query.String, got, err, shapeID)
			}
		})
	}

	for _, shapeID := range LogQLShapeIDs() {
		t.Run(string(shapeID), func(t *testing.T) {
			seed := shapeTestSeed(shapeID)
			dataset := LogsDataset().Example(seed)
			query := LogQLQueryForShape(dataset, shapeID).Example(seed)
			assertStampedShape(t, ShapeID(query.ShapeID), query.String, shapeID)
			if got := classifyLogQL(query.String); got != shapeID {
				t.Fatalf("query %q classified as %q, want %q", query.String, got, shapeID)
			}
		})
	}

	for _, shapeID := range TraceQLShapeIDs() {
		t.Run(string(shapeID), func(t *testing.T) {
			seed := shapeTestSeed(shapeID)
			dataset := TraceQLDataset().Example(seed)
			query := TraceQLQueryForShape(dataset, shapeID).Example(seed)
			assertStampedShape(t, ShapeID(query.ShapeID), query.String, shapeID)
			if got := classifyTraceQL(query.String); got != shapeID {
				t.Fatalf("query %q classified as %q, want %q", query.String, got, shapeID)
			}
		})
	}

	for _, shapeID := range InstantWindowShapeIDs() {
		t.Run(string(shapeID), func(t *testing.T) {
			seed := shapeTestSeed(shapeID)
			generated := InstantWindowSweepForShape(shapeID).Example(seed)
			assertStampedShape(t, ShapeID(generated.Query.ShapeID), generated.Query.String, shapeID)
			spec, ok := instantWindowShapeByID(shapeID)
			if !ok {
				t.Fatalf("shape %q is absent from the instant-window roster", shapeID)
			}
			if generated.Fn != spec.fn || generated.ValueProfile != spec.profile {
				t.Fatalf("shape %q generated fn=%q value-profile=%d, want fn=%q value-profile=%d",
					shapeID, generated.Fn, generated.ValueProfile, spec.fn, spec.profile)
			}
		})
	}
}

// TestByShapeGeneratorsPanicOnUnknownShape pins the invariant every
// *ForShape generator relies on: the shape ID it is called with always
// comes from that generator's own roster (PromQLShapeIDs and friends), so a
// shape ID absent from the roster is a caller bug, not runtime input to
// tolerate. Each generator panics loudly instead of silently drawing a
// mismatched case.
func TestByShapeGeneratorsPanicOnUnknownShape(t *testing.T) {
	const bogus ShapeID = "unknown.shape"

	wantPanic := func(t *testing.T, label string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s: want panic for unknown shape, got none", label)
			}
		}()
		fn()
	}

	wantPanic(t, "PromQLQueryForShape", func() { PromQLQueryForShape(property.Dataset{}, bogus) })
	wantPanic(t, "PromQLRangeQueryForShape", func() { PromQLRangeQueryForShape(property.Dataset{}, bogus) })
	wantPanic(t, "ExpHistogramQueryForShape", func() { ExpHistogramQueryForShape(property.Dataset{}, bogus) })
	wantPanic(t, "LogQLQueryForShape", func() { LogQLQueryForShape(property.Dataset{}, bogus) })
	wantPanic(t, "TraceQLQueryForShape", func() { TraceQLQueryForShape(property.Dataset{}, bogus) })
	wantPanic(t, "InstantWindowSweepForShape", func() { InstantWindowSweepForShape(bogus) })
}

func assertStampedShape(t *testing.T, got ShapeID, query string, want ShapeID) {
	t.Helper()
	if got != want || query == "" {
		t.Fatalf("generator stamped shape=%q query=%q, want non-empty shape=%q", got, query, want)
	}
}

func shapeTestSeed(shapeID ShapeID) int {
	seed := 0
	for index, b := range []byte(shapeID) {
		seed += (index + 1) * int(b)
	}
	return seed
}

func classifyPromQLInstant(query string) (ShapeID, error) {
	p := parser.NewParser(parser.Options{})
	expr, err := p.ParseExpr(query)
	if err != nil {
		return "", err
	}
	switch node := expr.(type) {
	case *parser.VectorSelector:
		return promQLSelectorShape, nil
	case *parser.Call:
		switch node.Func.Name {
		case "rate":
			return promQLRateShape, nil
		case "label_replace":
			return promQLLabelReplaceShape, nil
		}
	case *parser.AggregateExpr:
		if len(node.Grouping) > 0 {
			return promQLSumByShape, nil
		}
		if call, ok := node.Expr.(*parser.Call); ok && call.Func.Name == "rate" {
			return promQLSumRateShape, nil
		}
		return promQLSumShape, nil
	}
	return "", fmt.Errorf("unrecognized PromQL instant AST %T", expr)
}

func classifyPromQLRange(query string) (ShapeID, error) {
	p := parser.NewParser(parser.Options{})
	expr, err := p.ParseExpr(query)
	if err != nil {
		return "", err
	}
	switch node := expr.(type) {
	case *parser.VectorSelector:
		return promQLRangeSelectorShape, nil
	case *parser.AggregateExpr:
		if len(node.Grouping) == 0 {
			return "", fmt.Errorf("range sum-by collapsed to an empty grouping")
		}
		return promQLRangeSumByShape, nil
	case *parser.Call:
		switch node.Func.Name {
		case "rate":
			return promQLRangeRateShape, nil
		case "label_replace":
			return promQLRangeLabelReplaceShape, nil
		}
	}
	return "", fmt.Errorf("unrecognized PromQL range AST %T", expr)
}

func classifyExpHistogram(query string) (ShapeID, error) {
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		return "", err
	}
	return classifyExpHistogramExpr(expr, false)
}

func classifyExpHistogramExpr(expr parser.Expr, insideQuantile bool) (ShapeID, error) {
	switch node := expr.(type) {
	case *parser.VectorSelector:
		if insideQuantile {
			return expHistogramQuantileSelectorShape, nil
		}
		return expHistogramSelectorShape, nil
	case *parser.AggregateExpr:
		grouped := len(node.Grouping) > 0
		if call, ok := node.Expr.(*parser.Call); ok {
			switch call.Func.Name {
			case "rate":
				if grouped {
					return expHistogramQuantileSumByRateShape, nil
				}
				return expHistogramQuantileSumRateShape, nil
			case "increase":
				if grouped {
					return expHistogramQuantileSumByIncreaseShape, nil
				}
				return expHistogramQuantileSumIncreaseShape, nil
			}
		}
		if insideQuantile {
			if grouped {
				return expHistogramQuantileSumByShape, nil
			}
			return expHistogramQuantileSumShape, nil
		}
		if grouped {
			return expHistogramSumByShape, nil
		}
		return expHistogramSumShape, nil
	case *parser.Call:
		switch node.Func.Name {
		case "histogram_count":
			return expHistogramCountFunctionShape, nil
		case "histogram_sum":
			return expHistogramSumFunctionShape, nil
		case "histogram_avg":
			return expHistogramAverageFunctionShape, nil
		case "histogram_stddev":
			return expHistogramStddevFunctionShape, nil
		case "histogram_stdvar":
			return expHistogramStdvarFunctionShape, nil
		case "histogram_fraction":
			return expHistogramFractionShape, nil
		case "rate":
			if insideQuantile {
				return expHistogramQuantileRateShape, nil
			}
			return expHistogramRateShape, nil
		case "increase":
			if insideQuantile {
				return expHistogramQuantileIncreaseShape, nil
			}
			return expHistogramIncreaseShape, nil
		case "histogram_quantile":
			if len(node.Args) != 2 {
				return "", fmt.Errorf("histogram_quantile has %d args", len(node.Args))
			}
			return classifyExpHistogramExpr(node.Args[1], true)
		}
	}
	return "", fmt.Errorf("unrecognized native-histogram AST %T", expr)
}

func classifyLogQL(query string) ShapeID {
	switch {
	case strings.Contains(query, "| label_format "):
		return logQLLabelFormatShape
	case strings.Contains(query, "|= ip("):
		return logQLIPContainsShape
	case strings.Contains(query, "!= ip("):
		return logQLIPExcludesShape
	case strings.Contains(query, " |>"):
		return classifyLogQLPatternShape(query, false)
	case strings.Contains(query, " !>"):
		return classifyLogQLPatternShape(query, true)
	case strings.Contains(query, " |="):
		return logQLLineContainsShape
	case strings.Contains(query, " !="):
		return logQLLineExcludesShape
	default:
		return logQLSelectorShape
	}
}

func classifyLogQLPatternShape(query string, excludes bool) ShapeID {
	startsWithWildcard := strings.Contains(query, ` "<_>`)
	endsWithWildcard := strings.HasSuffix(query, `<_>"`)
	switch {
	case startsWithWildcard && endsWithWildcard:
		if excludes {
			return logQLPatternExcludesShape
		}
		return logQLPatternContainsShape
	case endsWithWildcard:
		if excludes {
			return logQLPatternExcludesPrefixShape
		}
		return logQLPatternContainsPrefixShape
	case startsWithWildcard:
		if excludes {
			return logQLPatternExcludesSuffixShape
		}
		return logQLPatternContainsSuffixShape
	default:
		return ""
	}
}

func classifyTraceQL(query string) ShapeID {
	switch {
	case strings.Contains(query, " >> "):
		return traceQLStructuralDescendantShape
	case strings.Contains(query, " > {"):
		return traceQLStructuralChildShape
	case strings.Contains(query, " | select("):
		return traceQLSelectShape
	case strings.Contains(query, " | count() "):
		return traceQLCountShape
	case strings.Contains(query, " | min("):
		return traceQLDurationAggregateMinShape
	case strings.Contains(query, " | max("):
		return traceQLDurationAggregateMaxShape
	case strings.Contains(query, " | sum("):
		return traceQLDurationAggregateSumShape
	case strings.Contains(query, " | avg("):
		return traceQLDurationAggregateShape
	case strings.Contains(query, " && "):
		return traceQLConjunctionShape
	case strings.Contains(query, "resource.service.name =~"):
		return traceQLRegexShape
	case strings.Contains(query, "resource.service.name !="):
		return traceQLNegatedAttributeShape
	case strings.HasPrefix(query, "{ duration "):
		return traceQLDurationShape
	case strings.HasPrefix(query, "{ status "):
		return traceQLStatusShape
	case strings.HasPrefix(query, "{ name "):
		return traceQLNameShape
	case strings.Contains(query, "resource.cluster"):
		return traceQLResourceAttributeShape
	case strings.Contains(query, "span.http.method"):
		return traceQLSpanAttributeShape
	default:
		return traceQLServiceShape
	}
}
