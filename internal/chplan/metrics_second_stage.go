package chplan

// SecondStageOp is the TraceQL post-aggregate (a.k.a. "second-stage")
// operator that produced a MetricsSecondStage node. Carries the source
// operator symbolically so the emitter can pick the matching SQL wrap
// (LIMIT BY for topk/bottomk, WHERE Value <op> N for threshold).
//
// The enum mirrors Tempo's `pkg/traceql.SecondStageOp` (OpTopK /
// OpBottomK) plus an OpThreshold value covering the `| > N` / `| < N`
// / `| >= N` / `| <= N` / `| == N` / `| != N` family that Tempo
// represents as a `MetricsFilter`. Keeping the trio under one IR node
// lets a single emitter handle the SQL wrap; the discriminator decides
// which variant-specific fields are read.
type SecondStageOp int

const (
	// SecondStageInvalid is the zero value; surfaces as an emitter error.
	SecondStageInvalid SecondStageOp = iota
	// SecondStageTopK corresponds to TraceQL `| topk(N)` — keep the N
	// series with the highest value at each anchor (matrix) or globally
	// (instant). Tempo's `processTopK` is a per-anchor top-K selection,
	// which lowers cleanly to ClickHouse's `LIMIT N BY <anchor_col>`.
	SecondStageTopK
	// SecondStageBottomK corresponds to TraceQL `| bottomk(N)` — the
	// per-anchor low-K dual of SecondStageTopK.
	SecondStageBottomK
	// SecondStageThreshold corresponds to TraceQL `| > N` / `| < N` /
	// `| >= N` / `| <= N` / `| == N` / `| != N` — filter out data
	// points whose Value does not satisfy the comparison.
	SecondStageThreshold
)

// String returns the TraceQL-source name of the operator (`topk`,
// `bottomk`, `threshold`). Used by chplan_print + error messages.
func (o SecondStageOp) String() string {
	switch o {
	case SecondStageTopK:
		return "topk"
	case SecondStageBottomK:
		return "bottomk"
	case SecondStageThreshold:
		return "threshold"
	}
	return "invalid"
}

// MetricsSecondStage applies a post-aggregate transform — topk(N) /
// bottomk(N) / threshold(value <op> N) — to the row-shape output of a
// metrics aggregation. Sits above a `MetricsAggregate` (instant path)
// or a `RangeWindow` wrapping a MetricsAggregate (matrix path); the
// emitter wraps the inner SQL with the variant-specific clause:
//
//   - SecondStageTopK / SecondStageBottomK: `ORDER BY <ValueAlias>
//     <DESC|ASC> LIMIT K [BY <PartitionBy>...]`. The PartitionBy slot
//     is the anchor column for matrix queries (`anchor_ts`); empty for
//     instant queries. ClickHouse's `LIMIT N BY <col>` keeps N rows
//     per distinct partition value — for matrix that means N series
//     per anchor, matching Tempo's `processTopK` per-anchor selection
//     (see engine_metrics.go: processTopK loops timestamps and picks
//     the top-K series at each).
//   - SecondStageThreshold: `WHERE <ValueAlias> <ThresholdOp>
//     <ThresholdValue>`. Filters individual data points whose Value
//     does not satisfy the comparison; same SQL shape works for both
//     instant and matrix inputs because the predicate is per-row.
//
// Chained second-stage transforms (`| topk(5) | > 10`) nest as separate
// MetricsSecondStage nodes — the outermost is the rightmost in the
// TraceQL source. Tempo's `ChainedSecondStage` walks elements in order,
// so the bottom-up nesting of MetricsSecondStage matches.
//
// Fields:
//
//   - Input: the lowered subtree this transform applies to —
//     `*MetricsAggregate` for instant, `*RangeWindow` (wrapping a
//     `*MetricsAggregate`) for matrix, or another `*MetricsSecondStage`
//     in the chained case.
//   - Op: the source TraceQL operator (TopK / BottomK / Threshold).
//   - K: the limit value for TopK / BottomK. Ignored for Threshold.
//   - ThresholdOp: the comparison operator for Threshold (chplan.OpGt /
//     OpGe / OpLt / OpLe / OpEq / OpNe). Ignored for TopK / BottomK.
//   - ThresholdValue: the literal RHS for Threshold. Ignored for TopK /
//     BottomK.
//   - PartitionBy: the column names that partition the LIMIT N BY
//     clause for TopK / BottomK. For matrix queries this is the anchor
//     column (typically `["anchor_ts"]`) so the limit fires per
//     timestamp bucket — matching Tempo's per-anchor top-K semantics.
//     Empty for instant queries (the limit applies globally to the
//     single row-per-series shape).
//   - ValueAlias: the column name carrying the per-anchor value (the
//     `ValueAlias` slot of the inner MetricsAggregate; "Value" in every
//     current code path but kept configurable so the IR doesn't pin a
//     magic string).
type MetricsSecondStage struct {
	Input          Node
	Op             SecondStageOp
	K              int64
	ThresholdOp    BinaryOp
	ThresholdValue float64
	PartitionBy    []string
	ValueAlias     string
}

func (*MetricsSecondStage) planNode() {}

func (m *MetricsSecondStage) Children() []Node { return []Node{m.Input} }

func (m *MetricsSecondStage) Equal(other Node) bool {
	o, ok := other.(*MetricsSecondStage)
	if !ok {
		return false
	}
	if m.Op != o.Op || m.K != o.K || m.ValueAlias != o.ValueAlias {
		return false
	}
	if m.ThresholdOp != o.ThresholdOp || m.ThresholdValue != o.ThresholdValue {
		return false
	}
	if len(m.PartitionBy) != len(o.PartitionBy) {
		return false
	}
	for i := range m.PartitionBy {
		if m.PartitionBy[i] != o.PartitionBy[i] {
			return false
		}
	}
	if (m.Input == nil) != (o.Input == nil) {
		return false
	}
	if m.Input == nil {
		return true
	}
	return m.Input.Equal(o.Input)
}
