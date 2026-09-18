package prom

import (
	"strconv"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestStripGuardExceptionEnvelope_RecognisesEveryEmittedGuardCode pins that
// the chDB text-envelope path decodes a guard message for EVERY error code
// chclient's typed path wraps into a ThrowIfError, and for no other: the
// prefixes are derived from chclient.EmittedGuardCodes rather than
// respelled here, so a code added on one side cannot be missing on the
// other.
func TestStripGuardExceptionEnvelope_RecognisesEveryEmittedGuardCode(t *testing.T) {
	t.Parallel()

	codes := chclient.EmittedGuardCodes()
	if len(codes) == 0 {
		t.Fatal("chclient reports no emitted guard codes; the loop below would be vacuous")
	}
	const guard = "cerberus guard: histogram merge budget exceeded"
	for _, code := range codes {
		raw := "Code: " + strconv.Itoa(int(code)) + ". DB::Exception: " + guard
		got, ok := stripGuardExceptionEnvelope(raw)
		if !ok || got != guard {
			t.Errorf("code %d: stripGuardExceptionEnvelope(%q) = %q, %v; want the bare guard message", code, raw, got, ok)
		}
	}
	// A code no emitted guard raises (BAD_ARGUMENTS) must not be decoded
	// as a guard, or an unrelated server fault would be answered as a
	// resource-bound rejection.
	const badArguments = 36
	if _, ok := stripGuardExceptionEnvelope("Code: " + strconv.Itoa(badArguments) + ". DB::Exception: " + guard); ok {
		t.Errorf("a BAD_ARGUMENTS envelope was decoded as an emitted guard")
	}
}
