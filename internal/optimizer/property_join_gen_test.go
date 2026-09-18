//go:build chdb

// Join generation for TestPropertyOptimizerSemanticEquivalence.
//
// VectorJoin and StructuralJoin are the two shapes the optimizer's
// pattern vocabulary names (KindVectorJoin, KindStructuralJoin) that the
// leaf grammar's gauge seed cannot host: a vector join needs two operands
// whose match is well-defined under the cardinality it draws, and a
// structural join needs spans with parent/child identity. Each generator
// here draws over the seed built for it (propertyJoinTable,
// propertySpansTable) and only ever produces a plan whose answer is a
// function of the row set alone, so a pre/post divergence is a real
// optimizer finding and never a tie ClickHouse was free to break.
//
// Neither node is rewritten by a rule today; what the round-trip pins is
// that the rules which DO fire inside an arm — ConstantFold on a
// `true AND P` predicate, FilterFusion on a Filter(Filter) — leave the
// join's answer alone, and that the fixpoint walk through a binary node
// (two children, neither named Input) reaches both arms.
package optimizer_test

import (
	"math/rand"

	"github.com/tsouza/cerberus/internal/chplan"
)

// The series groups of propertyJoinTable, by the role they play in a
// match: the "many" metrics carry four (job, host) series each, the
// "one" metrics carry one series per job.
var (
	propertyJoinManyMetrics = []string{"requests", "errors"}
	propertyJoinOneMetrics  = []string{"capacity", "quota"}
)

// propertyJoinValueBounds are the `Value <= b` thresholds the second
// filter of a fused arm draws from. Each keeps a prefix of a metric's
// series; dropping rows can never make a match ambiguous, so the arm
// stays well-defined under every cardinality.
var propertyJoinValueBounds = []float64{4.0, 8.0, 12.0}

// The binary ops a generated vector join evaluates, split by whether the
// `bool` modifier applies. Division is included because no Value in the
// seed is zero, so no draw can produce a NaN that reflect.DeepEqual would
// refuse to equate with itself.
var (
	propertyJoinArithmeticOps = []chplan.BinaryOp{chplan.OpAdd, chplan.OpSub, chplan.OpMul, chplan.OpDiv}
	propertyJoinComparisonOps = []chplan.BinaryOp{
		chplan.OpGt, chplan.OpLt, chplan.OpGe, chplan.OpLe, chplan.OpEq, chplan.OpNe,
	}
)

// generateVectorJoin draws one vector-vector binary op whose operands
// and matching are picked together, so the match is one-to-one or
// many-to-one by the seed's construction:
//
//	default match, one-to-one:      many×many or one×one (same series set)
//	on(job) / ignoring(host, tier):
//	  many-to-one (group_left)     : many on the left, one on the right
//	  one-to-many (group_right)    : one on the left, many on the right
//	  one-to-one                   : one×one (both unique per job)
//
// The comparison ops are drawn with and without `bool`, since the two
// render differently (a WHERE filter versus a toFloat64 projection).
func generateVectorJoin(rng *rand.Rand) chplan.Node {
	j := &chplan.VectorJoin{
		MetricNameColumn: propertyGroupColumn,
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	}
	if rng.Intn(2) == 0 {
		j.Op = propertyJoinArithmeticOps[rng.Intn(len(propertyJoinArithmeticOps))]
	} else {
		j.Op = propertyJoinComparisonOps[rng.Intn(len(propertyJoinComparisonOps))]
		j.ReturnBool = rng.Intn(2) == 0
	}

	pickTwo := func(group []string) (string, string) {
		i := rng.Intn(len(group))
		return group[i], group[1-i]
	}
	many := propertyJoinManyMetrics[rng.Intn(len(propertyJoinManyMetrics))]
	one := propertyJoinOneMetrics[rng.Intn(len(propertyJoinOneMetrics))]

	switch rng.Intn(4) {
	case 0:
		// Default matching on the full Attributes map: both sides keep
		// per-series granularity, so the operands must carry the same
		// series set to match at all.
		group := propertyJoinManyMetrics
		if rng.Intn(2) == 0 {
			group = propertyJoinOneMetrics
		}
		left, right := pickTwo(group)
		j.Left, j.Right = joinArm(rng, left), joinArm(rng, right)
	case 1:
		j.Match = generateJoinMatch(rng)
		j.Card = chplan.CardManyToOne
		j.Include = generateJoinInclude(rng)
		j.Left, j.Right = joinArm(rng, many), joinArm(rng, one)
	case 2:
		j.Match = generateJoinMatch(rng)
		j.Card = chplan.CardOneToMany
		j.Include = generateJoinInclude(rng)
		j.Left, j.Right = joinArm(rng, one), joinArm(rng, many)
	default:
		j.Match = generateJoinMatch(rng)
		left, right := pickTwo(propertyJoinOneMetrics)
		j.Left, j.Right = joinArm(rng, left), joinArm(rng, right)
	}
	return j
}

// generateJoinMatch draws the subset matching under which the "one"
// metrics are unique: `on(job)`, or its complement `ignoring(host,
// tier)`, which reduces every series in the seed to the same {job} key.
func generateJoinMatch(rng *rand.Rand) chplan.VectorMatch {
	if rng.Intn(2) == 0 {
		return chplan.VectorMatch{On: true, Labels: []string{"job"}}
	}
	return chplan.VectorMatch{On: false, Labels: []string{"host", "tier"}}
}

// generateJoinInclude draws the group_left/right label list: nothing, or
// the one label the "one" side carries that the "many" side does not.
func generateJoinInclude(rng *rand.Rand) []string {
	if rng.Intn(2) == 0 {
		return nil
	}
	return []string{"tier"}
}

// joinArm returns one operand: the join seed filtered to a single
// metric, sometimes dressed so a rule fires inside the arm.
func joinArm(rng *rand.Rand, metric string) chplan.Node {
	scan := &chplan.Scan{Table: propertyJoinTable}
	pred := chplan.Expr(&chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: propertyGroupColumn},
		Right: &chplan.LitString{V: metric},
	})
	switch rng.Intn(3) {
	case 0:
		return &chplan.Filter{Input: scan, Predicate: pred}
	case 1:
		// `true AND P` — ConstantFold fodder inside the arm.
		return &chplan.Filter{
			Input:     scan,
			Predicate: &chplan.Binary{Op: chplan.OpAnd, Left: &chplan.LitBool{V: true}, Right: pred},
		}
	default:
		// Filter(Filter(Scan)) — FilterFusion fodder inside the arm.
		return &chplan.Filter{
			Input: &chplan.Filter{Input: scan, Predicate: pred},
			Predicate: &chplan.Binary{
				Op:    chplan.OpLe,
				Left:  &chplan.ColumnRef{Name: "Value"},
				Right: &chplan.LitFloat{V: propertyJoinValueBounds[rng.Intn(len(propertyJoinValueBounds))]},
			},
		}
	}
}

// The spans seed's vocabulary. `nope` is a service no span carries, so
// an arm can come out empty and a negated relation can keep every row.
var (
	propertySpanServices = []string{"frontend", "api", "db", "cache", "nope"}
	propertySpanKinds    = []string{"Server", "Client"}
	propertySpanTraceIDs = []string{"t1", "t2", "t3"}
)

// propertyStructuralOps is every structural relation the emitter renders:
// the five positive relations, their negations and their unions.
var propertyStructuralOps = []chplan.StructuralOp{
	chplan.StructuralChild, chplan.StructuralParent, chplan.StructuralDescendant,
	chplan.StructuralAncestor, chplan.StructuralSibling,
	chplan.StructuralNotChild, chplan.StructuralNotParent, chplan.StructuralNotDescendant,
	chplan.StructuralNotAncestor, chplan.StructuralNotSibling,
	chplan.StructuralUnionChild, chplan.StructuralUnionParent, chplan.StructuralUnionDescendant,
	chplan.StructuralUnionAncestor, chplan.StructuralUnionSibling,
}

// propertySpanProjection is the non-key column list every generated
// structural join projects, so a nested join's outer consumer finds the
// same columns whichever arm it reads.
var propertySpanProjection = []string{"SpanName", "SpanKind", "Duration", "Timestamp", "ResourceAttributes"}

// propertyStructuralMaxDepth bounds the recursion-cap draw: 0 leaves the
// cap to the emitter's default, and the small positive values cut the
// closure of the four-span trace short, so the cap is observable.
const propertyStructuralMaxDepth = 2

// propertyStructuralNesting is how deep a chain of structural joins the
// generator builds: one nested join is enough to put a closure inside
// another closure's seed, which is the shape whose column resolution
// differs from a flat join's.
const propertyStructuralNesting = 1

// propertySpansSeedDays is the number of days the spans seed spans, one
// trace per day from propertyWindowStart.
const propertySpansSeedDays = 3

// spansWindow is the request window a generated structural tree is
// stamped with. Zero is the unwindowed harness path.
type spansWindow struct {
	startNano, endNano int64
}

// generateSpansWindow draws the window for one structural tree: none,
// one covering every seeded span, or one covering the first day only,
// which excludes t2 and t3 from the closure's anchor and step scans.
//
// It is drawn ONCE per tree and stamped onto every join in it, the way
// the search lowering stamps a request onto every node it lowers: the
// emit chokepoint (chsql.GuardEmittedSQL) rejects a statement that
// windows one recursive closure but not a nested one, since the
// unwindowed arm would read full retention behind the windowed seed.
func generateSpansWindow(rng *rand.Rand) spansWindow {
	switch rng.Intn(3) {
	case 0:
		return spansWindow{}
	case 1:
		return spansWindow{
			startNano: propertyWindowStart.UnixNano(),
			endNano:   propertyWindowStart.AddDate(0, 0, propertySpansSeedDays).UnixNano(),
		}
	default:
		return spansWindow{
			startNano: propertyWindowStart.UnixNano(),
			endNano:   propertyWindowStart.AddDate(0, 0, 1).UnixNano() - 1,
		}
	}
}

// generateStructuralJoin draws one structural relation over the spans
// seed. Every dimension the emitter branches on is drawn — the relation
// (direct, recursive, negated, union), the recursion cap, the candidate
// prefilter, a literal trace-id restriction, a request window, and
// whether an arm is itself a nested join — except TraceIDExternalTable:
// its id rows travel on the query context through chclient, not in the
// SQL, so the harness's bare chDB session has no table for it to name.
func generateStructuralJoin(rng *rand.Rand, nesting int) chplan.Node {
	return generateWindowedStructuralJoin(rng, generateSpansWindow(rng), nesting)
}

func generateWindowedStructuralJoin(rng *rand.Rand, window spansWindow, nesting int) chplan.Node {
	j := &chplan.StructuralJoin{
		Op:                     propertyStructuralOps[rng.Intn(len(propertyStructuralOps))],
		TraceIDColumn:          "TraceId",
		SpanIDColumn:           "SpanId",
		ParentSpanIDColumn:     "ParentSpanId",
		ExtraProjectionColumns: propertySpanProjection,
		MaxDepth:               rng.Intn(propertyStructuralMaxDepth + 1),
		CandidatePrefilter:     rng.Intn(2) == 0,
		Left:                   spansArm(rng),
		Right:                  spansArm(rng),
	}
	if nesting < propertyStructuralNesting && rng.Intn(3) == 0 {
		if rng.Intn(2) == 0 {
			j.Left = generateWindowedStructuralJoin(rng, window, nesting+1)
		} else {
			j.Right = generateWindowedStructuralJoin(rng, window, nesting+1)
		}
	}
	if rng.Intn(3) == 0 {
		// Phase-B restriction to a literal trace set: t1 and t3 keep a
		// rooted tree and the orphan chain, dropping t2 entirely.
		j.TraceIDRestriction = []string{propertySpanTraceIDs[0], propertySpanTraceIDs[2]}
	}
	if window != (spansWindow{}) {
		j.TimestampColumn = "Timestamp"
		j.WindowStartNano = window.startNano
		j.WindowEndNano = window.endNano
	}
	return j
}

// spansArm returns one side of a structural relation: the bare spans
// table (`{ }`), or a filter on the service or the kind, sometimes with
// ConstantFold fodder.
func spansArm(rng *rand.Rand) chplan.Node {
	scan := &chplan.Scan{Table: propertySpansTable}
	switch rng.Intn(4) {
	case 0:
		return scan
	case 1:
		return &chplan.Filter{Input: scan, Predicate: spanKindPredicate(rng)}
	case 2:
		return &chplan.Filter{Input: scan, Predicate: spanServicePredicate(rng)}
	default:
		return &chplan.Filter{
			Input: scan,
			Predicate: &chplan.Binary{
				Op:    chplan.OpAnd,
				Left:  &chplan.LitBool{V: true},
				Right: spanServicePredicate(rng),
			},
		}
	}
}

// spanServicePredicate is `ResourceAttributes['service.name'] = <svc>`,
// the shape a TraceQL resource matcher lowers to.
func spanServicePredicate(rng *rand.Rand) chplan.Expr {
	return &chplan.Binary{
		Op: chplan.OpEq,
		Left: &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: "ResourceAttributes"},
			Key: &chplan.LitString{V: "service.name"},
		},
		Right: &chplan.LitString{V: propertySpanServices[rng.Intn(len(propertySpanServices))]},
	}
}

// spanKindPredicate is `SpanKind = <kind>`, an intrinsic matcher.
func spanKindPredicate(rng *rand.Rand) chplan.Expr {
	return &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "SpanKind"},
		Right: &chplan.LitString{V: propertySpanKinds[rng.Intn(len(propertySpanKinds))]},
	}
}
