//go:build chdb

package spec

import "testing"

// TestNowAnchorLiteralMatchesDefault pins the invariant that the
// package-const [nowAnchorLiteral] (the fixed-anchor round-trip
// substitution literal, byte-frozen to the goldens) is exactly
// `chNow64Literal(defaultNowAnchor)`. This keeps the established
// fixed-anchor path and the per-eval [substituteNow64At] path sharing one
// source of truth for the default instant: if [chNow64Literal]'s
// formatting ever drifts, this fails instead of silently desyncing the
// two anchoring routes.
func TestNowAnchorLiteralMatchesDefault(t *testing.T) {
	if got := chNow64Literal(defaultNowAnchor); got != nowAnchorLiteral {
		t.Fatalf("chNow64Literal(defaultNowAnchor) = %q, want nowAnchorLiteral %q", got, nowAnchorLiteral)
	}
}
