//go:build chdb

package spec

import "testing"

// TestNowAnchorLiteralMatchesDefault pins the invariant that the
// package-const [nowAnchorLiteral] (the fixed-anchor round-trip
// substitution literal, byte-frozen to the goldens) is exactly
// `CHNow64Literal(defaultNowAnchor)`. This keeps the established
// fixed-anchor path and the per-eval [SubstituteNow64At] path sharing one
// source of truth for the default instant: if [CHNow64Literal]'s
// formatting ever drifts, this fails instead of silently desyncing the
// two anchoring routes.
func TestNowAnchorLiteralMatchesDefault(t *testing.T) {
	if got := CHNow64Literal(defaultNowAnchor); got != nowAnchorLiteral {
		t.Fatalf("CHNow64Literal(defaultNowAnchor) = %q, want nowAnchorLiteral %q", got, nowAnchorLiteral)
	}
}
