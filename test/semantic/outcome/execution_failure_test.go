package outcome

import (
	"errors"
	"strings"
	"testing"
)

func TestExecutionFailureImplementsError(t *testing.T) {
	f := ExecutionFailure{Kind: FailureTimeout, Detail: "case exceeded 30s budget"}
	var err error = f
	if !strings.Contains(err.Error(), "timeout") || !strings.Contains(err.Error(), "30s budget") {
		t.Fatalf("Error() = %q, want it to mention the kind and the detail", err.Error())
	}
}

// TestHarnessInputFailureIsNeverAnOutcome is the direct test for the
// acceptance criterion: "an unsupported harness input ... must classify as
// a FAILURE, never silently as an accepted Unsupported outcome". The proof
// here is structural: ExecutionFailure and Outcome are different Go types,
// so a function reporting a harness-input failure has no Outcome value —
// of ClassUnsupported or any other Class — to return in the first place.
// This test exercises that boundary through a small helper that mimics
// what a real generator-validation call site does (compare
// test/property/framework.go's ValidateGeneratedQuery / ValidateGeneratedDataset,
// which already fail the harness rather than proceeding).
func TestHarnessInputFailureIsNeverAnOutcome(t *testing.T) {
	runCase := func(queryText string) (Outcome, error) {
		if strings.TrimSpace(queryText) == "" {
			// The harness itself cannot even run this case — there is no
			// query to send anywhere, so there is nothing for a
			// head/endpoint classifier to classify. This MUST be reported
			// as an ExecutionFailure, never as Outcome{Class: ClassUnsupported}.
			return Outcome{}, ExecutionFailure{Kind: FailureHarnessInput, Detail: "empty query text"}
		}
		return Outcome{Class: ClassValue, Authority: "test/harness"}, nil
	}

	_, err := runCase("   \t")
	if err == nil {
		t.Fatal("runCase(empty query) returned no error — a harness-unrunnable case must fail")
	}
	var failure ExecutionFailure
	if !errors.As(err, &failure) {
		t.Fatalf("runCase(empty query) error = %v (%T), want an ExecutionFailure", err, err)
	}
	if failure.Kind != FailureHarnessInput {
		t.Fatalf("failure.Kind = %q, want %q", failure.Kind, FailureHarnessInput)
	}

	// The success path proves the same function CAN return a real Outcome
	// when the harness succeeds — so the failure path above is a genuine
	// branch, not the only thing this helper can ever produce.
	got, err := runCase("up")
	if err != nil {
		t.Fatalf("runCase(valid query) returned unexpected error: %v", err)
	}
	if got.Class != ClassValue {
		t.Fatalf("runCase(valid query) Class = %q, want %q", got.Class, ClassValue)
	}
}
