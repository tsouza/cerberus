package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// siblingPlan returns a `~`-family structural join over the spans table.
// op selects the flavour (`~` / `!~` / `&~`), all three of which read their
// L side through the same parent-presence gate.
func siblingPlan(op chplan.StructuralOp) *chplan.StructuralJoin {
	return &chplan.StructuralJoin{
		Left:               &chplan.Scan{Table: "otel_traces"},
		Right:              &chplan.Scan{Table: "otel_traces"},
		Op:                 op,
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
	}
}

// TestEmitSibling_GateAdmitsRootsAndRealParentsOnly pins BOTH halves of the
// sibling parent-presence gate, in every flavour of the operator.
//
// Upstream answers `~` from the nested-set numbering: two spans are siblings
// when they share a non-zero nestedSetParent, and a span whose parent row is
// not in the data is never numbered at all. The presence half of the gate is
// what encodes that. The empty-ParentSpanId half encodes the other end of the
// same rule — a root carries the -1 sentinel rather than 0, so two roots of one
// trace ARE siblings — and it cannot be dropped as redundant, because no row
// ever carries an empty SpanId and a bare presence test would therefore
// un-sibling every root pair.
//
// Asserting both halves in one test is the point: each is the other's
// counterexample, so an emitter that keeps only one fails here.
func TestEmitSibling_GateAdmitsRootsAndRealParentsOnly(t *testing.T) {
	t.Parallel()

	for _, op := range []chplan.StructuralOp{
		chplan.StructuralSibling,
		chplan.StructuralNotSibling,
		chplan.StructuralUnionSibling,
	} {
		t.Run(string(op), func(t *testing.T) {
			t.Parallel()

			sql, _, err := chsql.Emit(context.Background(), siblingPlan(op))
			if err != nil {
				t.Fatalf("Emit(%s): %v", op, err)
			}
			// The root half: a span with no parent stays in the L side.
			if !strings.Contains(sql, "`ParentSpanId` = ''") {
				t.Errorf("%s: gate must admit root spans (empty ParentSpanId), so two roots of "+
					"one trace stay siblings.\nSQL: %s", op, sql)
			}
			// The presence half: the shared parent must be a real row of the
			// same trace, which is what the (TraceId, ParentSpanId) tuple
			// membership over the spans table's own (TraceId, SpanId) tests.
			if !strings.Contains(sql, "(`TraceId`, `ParentSpanId`) IN (SELECT `TraceId`, `SpanId` FROM `otel_traces`") {
				t.Errorf("%s: gate must require the shared parent to be a real row of the same "+
					"trace.\nSQL: %s", op, sql)
			}
			// The two are a disjunction, not a conjunction: a root would
			// otherwise have to also have a parent row, which no root has.
			if !strings.Contains(sql, "`ParentSpanId` = '' OR (`TraceId`, `ParentSpanId`) IN") {
				t.Errorf("%s: the gate's two halves must be OR-ed.\nSQL: %s", op, sql)
			}
		})
	}
}

// TestEmitSibling_ParentProbeIsTraceScoped pins the probe's bound. The probe
// reads the spans table directly, so an unscoped one would read the window's
// whole span set to answer a question about the L side's parents; the emitter
// scopes it with the same plan-derived trace superset the nested-set numbering
// walk uses, which is also the resource bound the FROM is admitted under.
func TestEmitSibling_ParentProbeIsTraceScoped(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), siblingPlan(chplan.StructuralSibling))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "FROM `otel_traces` WHERE `TraceId` IN (SELECT `TraceId` FROM") {
		t.Fatalf("parent probe must be scoped to the L side's traces:\n%s", sql)
	}
}

// TestEmitSibling_ParentProbeCarriesWindowAndPhaseB pins that the probe folds
// the two optional bounds the plan can carry. Both are pure pruning: a
// window-stamped node must partition-prune the probe, and a phase-B node must
// granule-prune it to the top-N traces. Either one dropped turns a bounded
// probe into a wide read that no result-set assertion would notice.
func TestEmitSibling_ParentProbeCarriesWindowAndPhaseB(t *testing.T) {
	t.Parallel()

	plan := siblingPlan(chplan.StructuralSibling)
	plan.TimestampColumn = "Timestamp"
	plan.WindowStartNano = 1_700_000_000_000_000_000
	plan.WindowEndNano = 1_700_003_600_000_000_000
	plan.TraceIDRestriction = []string{"aabb", "ccdd"}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{
		"`Timestamp` >= fromUnixTimestamp64Nano(1700000000000000000)",
		"`Timestamp` <= fromUnixTimestamp64Nano(1700003600000000000)",
		"`TraceId` IN ('aabb', 'ccdd')",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("parent probe missing bound %q.\nSQL: %s", want, sql)
		}
	}
}

// TestEmitSibling_UnwindowedProbeStaysBare is the complement: a plan carrying
// neither bound must render the probe with the trace scope alone, so the
// assertions above cannot be satisfied by an emitter that always splices a
// window in.
func TestEmitSibling_UnwindowedProbeStaysBare(t *testing.T) {
	t.Parallel()

	sql, _, err := chsql.Emit(context.Background(), siblingPlan(chplan.StructuralSibling))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(sql, "fromUnixTimestamp64Nano") {
		t.Fatalf("an unwindowed sibling plan must not invent a window bound:\n%s", sql)
	}
}
