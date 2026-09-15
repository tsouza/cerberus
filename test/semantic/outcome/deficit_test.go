package outcome

import "testing"

// TestClassifyDeficitDirectionsStayDistinct is the direct test for the
// acceptance criterion that upstream-accept/cerberus-reject and the
// reverse must remain distinct deficits, never collapsing into one
// "disagreement" bucket.
func TestClassifyDeficitDirectionsStayDistinct(t *testing.T) {
	value := Outcome{Class: ClassValue, Authority: "promql/http-status"}
	rejected := Outcome{Class: ClassSemanticError, Authority: "promql/rejection-parity"}

	tooStrict, ok := ClassifyDeficit(value, rejected)
	if !ok {
		t.Fatal("reference accepted, system rejected: want ok=true")
	}
	if tooStrict != DeficitCerberusTooStrict {
		t.Fatalf("direction = %q, want %q", tooStrict, DeficitCerberusTooStrict)
	}

	tooLenient, ok := ClassifyDeficit(rejected, value)
	if !ok {
		t.Fatal("reference rejected, system accepted: want ok=true")
	}
	if tooLenient != DeficitCerberusTooLenient {
		t.Fatalf("direction = %q, want %q", tooLenient, DeficitCerberusTooLenient)
	}

	if tooStrict == tooLenient {
		t.Fatalf("the two deficit directions must never be equal: both were %q", tooStrict)
	}
}

func TestClassifyDeficitReturnsNotOkWhenBothSidesAgree(t *testing.T) {
	value := Outcome{Class: ClassValue}
	rejected := Outcome{Class: ClassSemanticError}

	tests := []struct {
		name      string
		reference Outcome
		system    Outcome
	}{
		{name: "both value", reference: value, system: value},
		{name: "both rejected", reference: rejected, system: rejected},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := ClassifyDeficit(tc.reference, tc.system); ok {
				t.Fatalf("ClassifyDeficit(%v, %v) reported a deficit direction for agreeing sides", tc.reference, tc.system)
			}
		})
	}
}
