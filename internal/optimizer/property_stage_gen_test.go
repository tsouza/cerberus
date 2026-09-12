//go:build chdb

// Stage-node generation for TestPropertyOptimizerSemanticEquivalence.
//
// The property's original grammar drew Scan, Filter and Project only —
// three of the thirty-eight kinds in internal/chplan, and none of the
// stage nodes ProjectionPushdown reshapes. That left its shapes (3)–(9)
// entirely unreached, including the arm its own doc calls "the
// correctness-critical arm":
//
//	dropping any of MetricName-class identity columns the GroupBy walks,
//	or the ts/value pair, 502s at runtime with
//	`Unknown expression identifier`.
//
// #860, #861 and #2127 all lived in exactly that arm, and the property
// that exists to catch semantic divergence could not see it.
//
// Every builder here places its stage node DIRECTLY over Scan or
// Filter(Scan), because that adjacency is what applyStageScan matches —
// a stage node over anything else leaves the pushdown inert and the
// generated plan tests nothing about it. The column sets each builder
// names are the ones the corresponding *Columns function in
// projection_pushdown.go promises to preserve, so an omission there
// surfaces here as a failed round-trip rather than as a narrower Scan
// nobody checked.
package optimizer_test

import (
	"math/rand"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// The window the stage builders grid over. The seed's ten gauge rows sit
// at one-second intervals from propertyWindowStart, so a one-second step
// across the full span produces several populated grid anchors per
// series rather than a single degenerate one.
var propertyWindowStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	propertyWindowSpan = 9 * time.Second
	propertyStep       = time.Second
	propertyRange      = 5 * time.Second
	propertyLookback   = 5 * time.Second
)

// propertyGroupColumn is the series-identity column every stage builder
// groups by. MetricName rather than Attributes: it is the
// MetricName-class identity column ProjectionPushdown's
// correctness-critical arm is documented to preserve, and it is a plain
// String, so the row-set comparison needs no Map normalisation.
const propertyGroupColumn = "MetricName"

// propertyWindowEnd is the last grid anchor.
func propertyWindowEnd() time.Time { return propertyWindowStart.Add(propertyWindowSpan) }

func propertyGroupBy() []chplan.Expr {
	return []chplan.Expr{&chplan.ColumnRef{Name: propertyGroupColumn}}
}

// stageBuilder builds one stage node over the supplied input.
type stageBuilder func(input chplan.Node) chplan.Node

// gaugeStageBuilders are the stage shapes that read the gauge seed.
func gaugeStageBuilders() []stageBuilder {
	return []stageBuilder{
		func(in chplan.Node) chplan.Node {
			return &chplan.Aggregate{
				Input:          in,
				GroupBy:        propertyGroupBy(),
				GroupByAliases: []string{propertyGroupColumn},
				AggFuncs: []chplan.AggFunc{{
					Fn:    "sum",
					Args:  []chplan.Expr{&chplan.ColumnRef{Name: "Value"}},
					Alias: "Value",
				}},
			}
		},
		func(in chplan.Node) chplan.Node {
			return &chplan.RangeWindow{
				Input:           in,
				Func:            "sum_over_time",
				Range:           propertyRange,
				Step:            propertyStep,
				Start:           propertyWindowStart,
				End:             propertyWindowEnd(),
				TimestampColumn: "TimeUnix",
				ValueColumn:     "Value",
				GroupBy:         propertyGroupBy(),
			}
		},
		func(in chplan.Node) chplan.Node {
			// The correctness-critical arm: the ClickHouse-native
			// timeSeriesRateToGrid lowering, whose emit reads exactly
			// the GroupBy identity refs and the (ts, value) pair off the
			// Scan. A pushdown that drops any of them fails here with
			// UNKNOWN_IDENTIFIER instead of silently shipping.
			return &chplan.RangeWindowGridNative{
				Input:           declareNativeMatrixPropertyRoles(in),
				Func:            "rate",
				Range:           propertyRange,
				Step:            propertyStep,
				Start:           propertyWindowStart,
				End:             propertyWindowEnd(),
				TimestampColumn: "TimeUnix",
				ValueColumn:     "Value",
				GroupBy:         propertyGroupBy(),
			}
		},
		func(in chplan.Node) chplan.Node {
			return &chplan.RangeWindowStaleResample{
				Input:         in,
				Step:          propertyStep,
				Lookback:      propertyLookback,
				Start:         propertyWindowStart,
				End:           propertyWindowEnd(),
				TimestampCol:  "TimeUnix",
				ValueCol:      "Value",
				MetricNameCol: propertyGroupColumn,
				AttributesCol: "Attributes",
			}
		},
		func(in chplan.Node) chplan.Node {
			return &chplan.RangeLWR{
				Input:         in,
				Step:          propertyStep,
				Lookback:      propertyLookback,
				Start:         propertyWindowStart,
				End:           propertyWindowEnd(),
				TimestampCol:  "TimeUnix",
				ValueCol:      "Value",
				MetricNameCol: propertyGroupColumn,
				AttributesCol: "Attributes",
			}
		},
		func(in chplan.Node) chplan.Node {
			return &chplan.RangeBucketFanout{
				Input:          in,
				Step:           propertyStep,
				Lookback:       propertyLookback,
				Start:          propertyWindowStart,
				End:            propertyWindowEnd(),
				TimestampCol:   "TimeUnix",
				AnchorAlias:    "anchor_ts",
				GroupBy:        propertyGroupBy(),
				GroupByAliases: []string{propertyGroupColumn},
				AggFuncs: []chplan.AggFunc{{
					Fn:    "sum",
					Args:  []chplan.Expr{&chplan.ColumnRef{Name: "Value"}},
					Alias: "Value",
				}},
			}
		},
		func(in chplan.Node) chplan.Node {
			// MetricsAggregate names its child slot Inner rather than
			// Input, which is the shape applyStageScan's own arm has to
			// special-case.
			return &chplan.MetricsAggregate{
				Inner:          in,
				Op:             chplan.MetricsOpCountOverTime,
				GroupBy:        propertyGroupBy(),
				GroupByAliases: []string{propertyGroupColumn},
				ValueAlias:     "Value",
			}
		},
	}
}

func declareNativeMatrixPropertyRoles(input chplan.Node) chplan.Node {
	roles := []chplan.Column{
		{Name: "TimeUnix", Role: chplan.RoleTimestamp},
		{Name: "Value", Role: chplan.RoleValue},
	}
	switch node := input.(type) {
	case *chplan.Scan:
		resolved := *node
		resolved.Roles = roles
		return &resolved
	case *chplan.Filter:
		resolved := *node
		resolved.Input = declareNativeMatrixPropertyRoles(node.Input)
		return &resolved
	default:
		return input
	}
}

// executablePropertyBaseline closes only the native-grid scan in the
// pre-optimizer comparison plan. The generated plan itself remains open so
// ProjectionPushdown must perform the real narrowing under test.
func executablePropertyBaseline(node chplan.Node) chplan.Node {
	rewritten, _ := chplan.RewriteChildren(node, func(child chplan.Node) (chplan.Node, bool) {
		next := executablePropertyBaseline(child)
		return next, next != child
	})
	grid, ok := rewritten.(*chplan.RangeWindowGridNative)
	if !ok {
		return rewritten
	}
	closed := *grid
	closed.Input = closeNativeMatrixPropertyInput(grid.Input)
	return &closed
}

func closeNativeMatrixPropertyInput(input chplan.Node) chplan.Node {
	switch node := input.(type) {
	case *chplan.Scan:
		closed := *node
		closed.Columns = []string{"MetricName", "TimeUnix", "Value"}
		return &closed
	case *chplan.Filter:
		closed := *node
		closed.Input = closeNativeMatrixPropertyInput(node.Input)
		return &closed
	default:
		return input
	}
}

// histogramStageBuilders are the stage shapes that need the classic
// BucketCounts / ExplicitBounds arrays, which live in the histogram seed
// rather than the gauge one.
func histogramStageBuilders() []stageBuilder {
	return []stageBuilder{
		func(in chplan.Node) chplan.Node {
			return &chplan.HistogramQuantile{
				Input:                in,
				Phi:                  0.5,
				GroupBy:              propertyGroupBy(),
				GroupByAliases:       []string{propertyGroupColumn},
				BucketCountsColumn:   "BucketCounts",
				ExplicitBoundsColumn: "ExplicitBounds",
			}
		},
	}
}

// generateSetOpChain builds a left-deep chain of associative
// VectorSetOps — the shape FlattenVectorSetOp linearises into one N-ary
// node.
//
// That rule is the only one in Default() outside the pushdown batches
// that rewrites a plan's structure, and it was unreachable while the
// generator drew no set op at all. Each arm is a distinct Filter over
// the gauge seed so the arms carry different, overlapping series and the
// `or` / `and` semantics are actually observable in the row set — a
// chain of identical arms would round-trip whatever the rule did to it.
//
// `unless` is excluded for the same reason the rule excludes it: it is
// not associative, so a left-deep chain of it is not the shape the rule
// linearises.
func generateSetOpChain(rng *rand.Rand) chplan.Node {
	op := chplan.VectorSetOr
	if rng.Intn(2) == 0 {
		op = chplan.VectorSetAnd
	}

	arms := 2 + rng.Intn(2)
	node := setOpArm(rng)
	for i := 1; i < arms; i++ {
		node = &chplan.VectorSetOp{
			Left:             node,
			Right:            setOpArm(rng),
			Op:               op,
			MetricNameColumn: propertyGroupColumn,
			AttributesColumn: "Attributes",
			TimestampColumn:  "TimeUnix",
			ValueColumn:      "Value",
		}
	}
	return node
}

// setOpArm returns one operand of a set-op chain: a filtered read of the
// gauge seed, projected to the canonical four-column Sample contract the
// set-op emitter matches its arms on.
func setOpArm(rng *rand.Rand) chplan.Node {
	return &chplan.Project{
		Input: &chplan.Filter{
			Input:     &chplan.Scan{Table: propertyTable},
			Predicate: generateStagePredicate(rng),
		},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: propertyGroupColumn}, Alias: propertyGroupColumn},
			{Expr: &chplan.ColumnRef{Name: "Attributes"}, Alias: "Attributes"},
			{Expr: &chplan.ColumnRef{Name: "TimeUnix"}, Alias: "TimeUnix"},
			{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"},
		},
		Roles: setOpTestColumns(),
	}
}

// generateStagePlan draws one stage shape over Scan or Filter(Scan),
// then optionally wraps it in one of the generic pass-through nodes.
func generateStagePlan(rng *rand.Rand) chplan.Node {
	if rng.Intn(4) == 0 {
		return generateSetOpChain(rng)
	}
	gauge := gaugeStageBuilders()
	histogram := histogramStageBuilders()

	var (
		build stageBuilder
		table string
	)
	if pick := rng.Intn(len(gauge) + len(histogram)); pick < len(gauge) {
		build, table = gauge[pick], propertyTable
	} else {
		build, table = histogram[pick-len(gauge)], propertyHistogramTable
	}

	stage := build(generateStageInput(rng, table))
	return wrapStage(rng, stage)
}

// generateStageInput returns the Scan or Filter(Scan) a stage node sits
// directly over — the two adjacencies applyStageScan recognises.
func generateStageInput(rng *rand.Rand, table string) chplan.Node {
	scan := &chplan.Scan{Table: table}
	if rng.Intn(2) == 0 {
		return scan
	}
	return &chplan.Filter{
		Input:     scan,
		Predicate: generateStagePredicate(rng),
	}
}

// generateStagePredicate draws a predicate over the group column alone.
//
// The leaf grammar's Value / TimeUnix predicates are not reused here:
// applyStageScan narrows the Scan to the stage's required columns UNION
// the Filter predicate's columns, and a predicate over the group column
// exercises the union while staying evaluable for every stage shape,
// including the histogram one whose seed has no gauge Value column.
func generateStagePredicate(rng *rand.Rand) chplan.Expr {
	op := chplan.OpEq
	if rng.Intn(2) == 0 {
		op = chplan.OpNe
	}
	return &chplan.Binary{
		Op:    op,
		Left:  &chplan.ColumnRef{Name: propertyGroupColumn},
		Right: &chplan.LitString{V: propertyMetricNames[rng.Intn(len(propertyMetricNames))]},
	}
}

// wrapStage optionally puts a generic pass-through node above the stage.
//
// These add node kinds to the property's reach without needing a seed of
// their own, and they put the stage node mid-tree rather than always at
// the root, which is where the FixedPoint walk's bottom-up order
// actually matters.
//
// UnionAll duplicates the stage rather than drawing a second one so both
// arms stay the same width — a union of differently shaped arms is not a
// plan any lowering builds, and ClickHouse rejects it before the
// optimizer's behaviour could be observed.
func wrapStage(rng *rand.Rand, stage chplan.Node) chplan.Node {
	switch rng.Intn(5) {
	case 0:
		return &chplan.UnionAll{Inputs: []chplan.Node{stage, stage}}
	case 1:
		// OneRow and StepGrid are the two single-arm right sides a
		// CrossJoin gets in production; StepGrid multiplies the row
		// count by the grid, OneRow leaves it alone.
		if rng.Intn(2) == 0 {
			return &chplan.CrossJoin{Left: stage, Right: &chplan.OneRow{}}
		}
		return &chplan.CrossJoin{Left: stage, Right: &chplan.StepGrid{
			Start: propertyWindowStart,
			End:   propertyWindowEnd(),
			Step:  propertyStep,
		}}
	case 2:
		return &chplan.OrderBy{
			Input: stage,
			Keys:  []chplan.OrderKey{{Expr: &chplan.ColumnRef{Name: propertyGroupColumn}}},
		}
	case 3:
		// Limit sits over OrderBy, never over the bare stage: an
		// unordered LIMIT picks an arbitrary subset, and the optimized
		// plan is a different query, so the two could legitimately
		// return different rows and the property would flake rather
		// than report.
		return &chplan.Limit{
			Input: &chplan.OrderBy{
				Input: stage,
				Keys:  []chplan.OrderKey{{Expr: &chplan.ColumnRef{Name: propertyGroupColumn}}},
			},
			Count: 1 + int64(rng.Intn(3)),
		}
	}
	return stage
}
