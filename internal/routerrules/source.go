package routerrules

import (
	"context"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// CHConn is the narrow ClickHouse read surface this package needs: a single
// typed Query against the router corpus table. It is byte-identical in shape to
// optcorpus.CHConn and is deliberately re-declared here rather than imported, so
// routerrules depends on neither optcorpus nor chclient (avoiding an import
// cycle, exactly as optcorpus itself re-declares rather than importing
// chclient). A *chclient.Client.Conn() satisfies it from cmd/; a fake satisfies
// it in tests without standing up a server.
type CHConn interface {
	Query(ctx context.Context, query string, args ...any) (driver.Rows, error)
}

// AggFunc is the closed set of scalar aggregates a corpus_agg param may request.
type AggFunc string

const (
	AggMax    AggFunc = "max"
	AggAvg    AggFunc = "avg"
	AggMin    AggFunc = "min"
	AggStdDev AggFunc = "stddevPop"
)

// AggSpec is a resolved request to aggregate one corpus column, used to resolve
// a corpus-derived parameter. Exactly one of Percentile / Agg / CountRatio is
// set, selecting the aggregate shape. Scope (and the count-ratio scopes) are
// enum-equality filters validated to reference only enum columns. PartitionBy,
// when non-empty, yields a partition-keyed Value (one scalar per partition).
type AggSpec struct {
	Column      string
	Percentile  *float64 // corpus_percentile: the resolved fraction in (0,1)
	Agg         AggFunc  // corpus_agg
	CountRatio  bool     // corpus_count_ratio
	Scope       Scope    // percentile/agg population filter
	NumScope    Scope    // count_ratio numerator
	DenScope    Scope    // count_ratio denominator
	PartitionBy []string
}

// GroupResult is one grouped row of a rule evaluation: the group-key values (in
// the rule's group_by order), the support count, and any evidence aggregates
// (in the rule's evidence.report order, count excluded).
type GroupResult struct {
	GroupKey []string
	Support  int64
	Evidence []float64
}

// RuleQuery is a resolved request to evaluate one rule: the matched-row filter
// (already lowered with resolved params), the group-by columns, and the
// evidence aggregate expressions to compute over matched rows.
type RuleQuery struct {
	Condition Condition
	GroupBy   []string
	Evidence  []evidenceExpr
	Env       Env
}

// OOMFloor is the observed route-A OOM cost floor: the minimum fan-out and
// minimum anchor count over the eligible corpus population the cost gate can
// actually protect — route-A queries that OOM'd AFTER being gated below the cost
// thresholds (decision_reason = below-threshold). That predicate excludes the
// OOMs lowering the gate cannot move to route B (instant / not-sliceable / high-D
// were rejected for eligibility, not by the cost gate). The additional
// fanout > 0 AND n_anchors > 0 predicate is a guard against a gridless row
// (e.g. an instant query, recorded with fanout = 0) slipping in and cratering
// the min to 0. HasSignal is false when that population is empty (cold start).
type OOMFloor struct {
	MinFanout  int
	MinAnchors int
	HasSignal  bool
}

// OOMFloorSource is the narrow read surface the autotune fit needs: the observed
// route-A OOM floor over the eligible population. Only the ClickHouse-table
// source implements it (the loop runs against CH in production); it is kept
// separate from CorpusSource so adding it does not force every corpus fake to
// grow a method.
type OOMFloorSource interface {
	OOMFloor(ctx context.Context) (OOMFloor, error)
}

// CorpusSource is the backend seam. Both the ClickHouse-table and the JSONL
// implementations satisfy it; the evaluator never branches on backend.
type CorpusSource interface {
	// Aggregate resolves one corpus-derived parameter to a scalar or
	// partition-keyed Value.
	Aggregate(ctx context.Context, spec AggSpec) (Value, error)
	// EvalRule returns one GroupResult per group_by class whose matched-row
	// count is positive. min_support filtering is applied by the evaluator,
	// not here, so both backends share identical thin-class semantics.
	EvalRule(ctx context.Context, q RuleQuery) ([]GroupResult, error)
}

// corpusRow is one decoded corpus row, used by the in-Go (JSONL) matcher and
// aggregator. Numeric columns are widened to float64 so a single comparison
// path serves every numeric column; enum/group columns stay strings.
type corpusRow struct {
	eventTimeUnix float64
	numeric       map[string]float64
	str           map[string]string
}

func (r corpusRow) numericValue(col string) float64 { return r.numeric[col] }

func (r corpusRow) enumValue(col string) string { return r.str[col] }

func (r corpusRow) groupValue(col string) string {
	if v, ok := r.str[col]; ok {
		return v
	}
	// Group columns that are numeric (normalized_query_hash) render through the
	// numeric map; format as an integer for a stable group key.
	if v, ok := r.numeric[col]; ok {
		return formatNumeric(v)
	}
	return ""
}
