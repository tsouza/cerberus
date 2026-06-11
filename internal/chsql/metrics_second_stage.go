package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitMetricsSecondStage renders a chplan.MetricsSecondStage as a
// subquery-wrapping SELECT over the lowered metrics aggregate's row
// shape. The wrap depends on m.Op:
//
//   - SecondStageTopK / SecondStageBottomK: `ORDER BY <ValueAlias>
//     <DESC|ASC> LIMIT K [BY <PartitionBy...>]`. ClickHouse's
//     `LIMIT N BY <col>` keeps N rows per distinct partition value —
//     for matrix queries the partition is `anchor_ts` so the limit
//     fires per-timestamp bucket (matching Tempo's `processTopK`
//     per-anchor selection). For instant queries PartitionBy is empty
//     and the limit applies globally (top-K series in the
//     row-per-series shape).
//   - SecondStageThreshold: `WHERE <ValueAlias> <ThresholdOp>
//     <ThresholdValue>`. Same SQL for instant + matrix since the
//     predicate is per-row.
//
// The Input subquery's column shape is preserved verbatim (outer
// `SELECT *`) so downstream consumers (chDB roundtrip, handler
// projection) see the same columns the inner MetricsAggregate emits.
func (e *emitter) emitMetricsSecondStage(m *chplan.MetricsSecondStage) error {
	if m.Input == nil {
		return fmt.Errorf("%w: MetricsSecondStage.Input is nil", ErrUnsupported)
	}
	if m.ValueAlias == "" {
		return fmt.Errorf("%w: MetricsSecondStage.ValueAlias unset", ErrUnsupported)
	}

	switch m.Op {
	case chplan.SecondStageTopK, chplan.SecondStageBottomK:
		return e.emitMetricsSecondStageTopK(m)
	case chplan.SecondStageThreshold:
		return e.emitMetricsSecondStageThreshold(m)
	}
	return fmt.Errorf("%w: MetricsSecondStage op %s", ErrUnsupported, m.Op)
}

// emitMetricsSecondStageTopK renders the topk/bottomk wrap:
//
//	SELECT * FROM (<inner>) ORDER BY <ValueAlias> <DESC|ASC>
//	  LIMIT <K> [BY <PartitionBy_1>, <PartitionBy_2>, ...]
//
// K <= 0 is rejected — TraceQL's parser validates `limit > 0` so the
// only path to a non-positive K is a programmer error in the lowering
// layer; surfacing it loud avoids an unbounded LIMIT that silently
// disables the second-stage filter.
func (e *emitter) emitMetricsSecondStageTopK(m *chplan.MetricsSecondStage) error {
	if m.K <= 0 {
		return fmt.Errorf("%w: MetricsSecondStage %s with non-positive K=%d", ErrUnsupported, m.Op, m.K)
	}

	sub, err := e.subqueryFrag(m.Input)
	if err != nil {
		return err
	}

	desc := m.Op == chplan.SecondStageTopK
	alias := m.ValueAlias
	sb := NewQuery().From(sub).
		OrderBy(func(b *Builder) { b.Ident(alias) }, desc).
		Limit(m.K)
	// An empty PartitionBy appends no `LIMIT … BY` keys (LimitBy is a
	// no-op on an empty slice), so no length guard is needed.
	parts := make([]Frag, 0, len(m.PartitionBy))
	for _, p := range m.PartitionBy {
		col := p
		parts = append(parts, func(b *Builder) { b.Ident(col) })
	}
	sb.LimitBy(parts...)
	e.emitSelect(sb)
	return nil
}

// emitMetricsSecondStageThreshold renders the threshold wrap:
//
//	SELECT * FROM (<inner>) WHERE <ValueAlias> <Op> <Value>
//
// The threshold predicate flows through Builder.Expr as a
// chplan.Binary{ValueAlias <Op> LitFloat(<Value>)} so the SQL shape
// matches the rest of the emitter's comparison rendering (parenthesis
// rules, parameter binding) without duplicating the operator table.
func (e *emitter) emitMetricsSecondStageThreshold(m *chplan.MetricsSecondStage) error {
	if !isThresholdOp(m.ThresholdOp) {
		return fmt.Errorf("%w: MetricsSecondStage threshold op %s not supported", ErrUnsupported, m.ThresholdOp)
	}

	sub, err := e.subqueryFrag(m.Input)
	if err != nil {
		return err
	}

	pred := &chplan.Binary{
		Op:    m.ThresholdOp,
		Left:  &chplan.ColumnRef{Name: m.ValueAlias},
		Right: &chplan.LitFloat{V: m.ThresholdValue},
	}
	if err := (&Builder{}).Expr(pred); err != nil {
		return err
	}
	sb := NewQuery().From(sub).Where(func(b *Builder) { _ = b.Expr(pred) })
	e.emitSelect(sb)
	return nil
}

// isThresholdOp reports whether op is one of the six comparison
// operators TraceQL's `MetricsFilter.validate()` accepts (>, >=, <,
// <=, =, !=). All other chplan.BinaryOp values surface as a clean
// emitter error from emitMetricsSecondStageThreshold.
func isThresholdOp(op chplan.BinaryOp) bool {
	switch op {
	case chplan.OpGt, chplan.OpGe, chplan.OpLt, chplan.OpLe, chplan.OpEq, chplan.OpNe:
		return true
	}
	return false
}
