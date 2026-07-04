package tempo

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestPhaseAStaysNarrow pins the invariant the two-phase cost model — and the
// deliberate decision NOT to add a density cost-gate (see the handler decision
// point) — depends on: buildStructuralPhaseAPlan strips the wide attribute-map
// columns from EVERY structural join (the root AND the inner closures of a
// chain) and preserves the request window, so phase A stays a narrow,
// window-pruned rank. If a future change widens phase A's projection or drops
// its window, the always-paid phase-A overhead inflates and the "phase A is
// already cheap, no gate needed" reasoning silently breaks — this pin fails
// first.
func TestPhaseAStaysNarrow(t *testing.T) {
	s := schema.DefaultOTelTraces()
	const wStart, wEnd = int64(1_000), int64(2_000)

	// The wide projection every join starts with — exactly the fat maps the OOM
	// came from; phase A must drop them down to Timestamp only.
	wide := []string{"ResourceAttributes", "SpanAttributes"}
	scan := func() chplan.Node { return &chplan.Scan{Table: s.SpansTable} }
	mkJoin := func(l, r chplan.Node) *chplan.StructuralJoin {
		return &chplan.StructuralJoin{
			Left: l, Right: r, Op: chplan.StructuralDescendant,
			TraceIDColumn:          s.TraceIDColumn,
			SpanIDColumn:           s.SpanIDColumn,
			ParentSpanIDColumn:     s.ParentSpanIDColumn,
			TimestampColumn:        s.TimestampColumn,
			WindowStartNano:        wStart,
			WindowEndNano:          wEnd,
			ExtraProjectionColumns: wide,
		}
	}
	// `A >> B >> C` is left-associative: root.Left is itself a StructuralJoin, so
	// eachStructuralJoin must narrow the inner closure, not only the root.
	inner := mkJoin(scan(), scan())
	root := mkJoin(inner, scan())

	phaseA := buildStructuralPhaseAPlan(root, s, 5)

	joins := 0
	eachStructuralJoin(phaseA, func(j *chplan.StructuralJoin) {
		joins++
		if len(j.ExtraProjectionColumns) != 1 || j.ExtraProjectionColumns[0] != s.TimestampColumn {
			t.Errorf("phase-A join projects %v, want narrow [%q] only — wide attribute maps leaked into the rank",
				j.ExtraProjectionColumns, s.TimestampColumn)
		}
		if j.WindowStartNano != wStart || j.WindowEndNano != wEnd {
			t.Errorf("phase-A join lost its window: got [%d,%d], want [%d,%d] — phase A must stay partition-pruned",
				j.WindowStartNano, j.WindowEndNano, wStart, wEnd)
		}
	})
	if joins != 2 {
		t.Fatalf("expected buildStructuralPhaseAPlan to narrow 2 structural joins (root + inner closure), walked %d", joins)
	}
}
