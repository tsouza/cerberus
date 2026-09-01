package tempo

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestUseExternalTraceIDTable_Threshold pins the byte-size crossover
// useExternalTraceIDTable computes: below traceIDLiteralByteBudget (once
// multiplied by traceIDRestrictionSiteCount) the literal splice wins, at and
// above it the external table wins. A 32-hex-char TraceId costs
// len(id)+3 = 35 bytes per estimatedLiteralBytes (the two quotes plus the
// separator), so the crossover
// count is traceIDLiteralByteBudget / (35 * traceIDRestrictionSiteCount).
func TestUseExternalTraceIDTable_Threshold(t *testing.T) {
	const idBytes = 32 + 3 // one 32-hex-char TraceId plus estimatedLiteralBytes' quote/comma overhead
	crossover := traceIDLiteralByteBudget / (idBytes * traceIDRestrictionSiteCount)

	below := make([]string, crossover)
	for i := range below {
		below[i] = strings.Repeat("a", 32)
	}
	if useExternalTraceIDTable(below) {
		t.Fatalf("useExternalTraceIDTable(%d ids) = true, want false (still under budget)", len(below))
	}

	above := make([]string, crossover+1)
	copy(above, below)
	above[len(above)-1] = strings.Repeat("a", 32)
	if !useExternalTraceIDTable(above) {
		t.Fatalf("useExternalTraceIDTable(%d ids) = false, want true (over budget)", len(above))
	}
}

// TestUseExternalTraceIDTable_EmptyIsLiteral pins the vacuous case: zero ids
// never trips the external-table path (runStructuralTwoPhase already returns
// early on an empty phase-A result before restrictStructural is even called,
// but the pure function itself must not be the one relying on that).
func TestUseExternalTraceIDTable_EmptyIsLiteral(t *testing.T) {
	if useExternalTraceIDTable(nil) {
		t.Fatalf("useExternalTraceIDTable(nil) = true, want false")
	}
}

// TestRestrictStructural_LiteralWhenIneligible pins that restrictStructural
// never takes the external-table path when externalEligible is false
// (h.ExternalTraceIDPush unset — the default for every un-wired Handler),
// even for a closure whose literal splice would cross the byte budget. This
// is the byte-identical-by-default guarantee: the feature is inert until
// BOTH the chopt gate and the byte threshold agree.
func TestRestrictStructural_LiteralWhenIneligible(t *testing.T) {
	wide := make([]string, 2000)
	for i := range wide {
		wide[i] = strings.Repeat("a", 32)
	}
	plan := structuralClosurePlanForTest()

	restricted, ids, external := restrictStructural(plan, wide, false)
	if external {
		t.Fatalf("restrictStructural chose external with externalEligible=false")
	}
	if len(ids) != len(wide) {
		t.Fatalf("restrictStructural returned %d ids, want %d", len(ids), len(wide))
	}
	joins := 0
	eachStructuralJoin(restricted, func(sj *chplan.StructuralJoin) {
		joins++
		if sj.TraceIDExternalTable != "" {
			t.Errorf("join carries TraceIDExternalTable %q though externalEligible=false", sj.TraceIDExternalTable)
		}
		if len(sj.TraceIDRestriction) != len(wide) {
			t.Errorf("join TraceIDRestriction has %d ids, want %d", len(sj.TraceIDRestriction), len(wide))
		}
	})
	if joins == 0 {
		t.Fatalf("eachStructuralJoin visited no join")
	}
}

// TestRestrictStructural_ExternalWhenEligibleAndWide pins the positive case:
// externalEligible=true plus a wide closure switches every structural join in
// the plan to the external-table form, and the returned ids are the padded
// values runStructuralTwoPhase threads to chclient.WithExternalTraceIDs.
func TestRestrictStructural_ExternalWhenEligibleAndWide(t *testing.T) {
	wide := make([]string, 2000)
	for i := range wide {
		wide[i] = strings.Repeat("a", 32)
	}
	plan := structuralClosurePlanForTest()

	restricted, ids, external := restrictStructural(plan, wide, true)
	if !external {
		t.Fatalf("restrictStructural chose literal though externalEligible=true and the closure is wide")
	}
	if len(ids) != len(wide) {
		t.Fatalf("restrictStructural returned %d ids, want %d", len(ids), len(wide))
	}
	joins := 0
	eachStructuralJoin(restricted, func(sj *chplan.StructuralJoin) {
		joins++
		if sj.TraceIDExternalTable != externalTraceIDTableName {
			t.Errorf("join TraceIDExternalTable = %q, want %q", sj.TraceIDExternalTable, externalTraceIDTableName)
		}
		if len(sj.TraceIDRestriction) != 0 {
			t.Errorf("join TraceIDRestriction = %v, want empty when external is chosen", sj.TraceIDRestriction)
		}
	})
	if joins == 0 {
		t.Fatalf("eachStructuralJoin visited no join")
	}
}

// TestRestrictStructural_LiteralWhenEligibleButNarrow pins that a narrow
// closure stays on the literal path even with externalEligible=true — the
// external-table round trip is only worth it once the byte budget is
// actually crossed.
func TestRestrictStructural_LiteralWhenEligibleButNarrow(t *testing.T) {
	narrow := []string{strings.Repeat("a", 32), strings.Repeat("b", 32)}
	plan := structuralClosurePlanForTest()

	restricted, _, external := restrictStructural(plan, narrow, true)
	if external {
		t.Fatalf("restrictStructural chose external for a narrow (%d-id) closure", len(narrow))
	}
	eachStructuralJoin(restricted, func(sj *chplan.StructuralJoin) {
		if sj.TraceIDExternalTable != "" {
			t.Errorf("join carries TraceIDExternalTable %q for a narrow closure", sj.TraceIDExternalTable)
		}
	})
}

// structuralClosurePlanForTest returns a minimal chained `A >> B` closure
// (root over an inner StructuralJoin) so restrictStructural's
// eachStructuralJoin walk has more than one node to visit — mirroring
// phase_a_narrow_test.go's own TestPhaseAStaysNarrow fixture shape.
func structuralClosurePlanForTest() chplan.Node {
	scan := func() chplan.Node { return &chplan.Scan{Table: "otel_traces"} }
	mkJoin := func(l, r chplan.Node) *chplan.StructuralJoin {
		return &chplan.StructuralJoin{
			Left: l, Right: r, Op: chplan.StructuralDescendant,
			TraceIDColumn:      "TraceId",
			SpanIDColumn:       "SpanId",
			ParentSpanIDColumn: "ParentSpanId",
		}
	}
	inner := mkJoin(scan(), scan())
	return mkJoin(inner, scan())
}
