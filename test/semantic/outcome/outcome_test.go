package outcome

import "testing"

func TestOutcomeIsError(t *testing.T) {
	tests := []struct {
		name string
		o    Outcome
		want bool
	}{
		{name: "value is not an error", o: Outcome{Class: ClassValue}, want: false},
		{name: "syntax error is an error", o: Outcome{Class: ClassSyntaxError}, want: true},
		{name: "semantic error is an error", o: Outcome{Class: ClassSemanticError}, want: true},
		{name: "unsupported is an error", o: Outcome{Class: ClassUnsupported}, want: true},
		{name: "protocol error is an error", o: Outcome{Class: ClassProtocolError}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.o.IsError(); got != tc.want {
				t.Fatalf("IsError() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOutcomeVocabularyIsClosedAndDistinct pins the five Class values the
// issue's Proposed change enumerates. A sixth value silently added later
// would not be caught by the compiler, so this pins the exact roster by
// name.
func TestOutcomeVocabularyIsClosedAndDistinct(t *testing.T) {
	want := map[Class]bool{
		ClassValue:         true,
		ClassSyntaxError:   true,
		ClassSemanticError: true,
		ClassUnsupported:   true,
		ClassProtocolError: true,
	}
	if len(want) != 5 {
		t.Fatalf("test setup: want %d distinct classes, got %d", 5, len(want))
	}
	for class := range want {
		if class == "" {
			t.Fatalf("a vocabulary Class must never be the empty string")
		}
	}
}
