package optimizer

import "github.com/tsouza/cerberus/internal/chplan"

// This file exports test-only AnalyzerRule implementations for the
// external `optimizer_test` package (analyzer_test.go). The sealed
// `isAnalyzerRule()` marker prevents external packages from claiming
// the AnalyzerRule contract; declaring the test fixtures inside
// `package optimizer` is the standard Go pattern for crossing that
// seal at test time without weakening it in production.
//
// File name follows the `*_export_test.go` convention: the `_test.go`
// suffix scopes the file to test builds only, and `export` flags its
// purpose to readers.

// IdempotentTestAnalyzerRule rewrites a Scan named "raw" → "canon"
// once, then leaves it alone. Models the AnalyzerRule contract:
// one-shot canonicalisation, then a no-op verification pass.
type IdempotentTestAnalyzerRule struct {
	Calls *int
}

// Name implements Rule.
func (r IdempotentTestAnalyzerRule) Name() string { return "idempotent-analyzer" }

// isAnalyzerRule satisfies the sealed marker.
func (r IdempotentTestAnalyzerRule) isAnalyzerRule() {}

// Apply implements Rule.
func (r IdempotentTestAnalyzerRule) Apply(n chplan.Node) (chplan.Node, bool) {
	*r.Calls++
	if s, ok := n.(*chplan.Scan); ok && s.Table == "raw" {
		cp := *s
		cp.Table = "canon"
		return &cp, true
	}
	return n, false
}

// NonIdempotentTestAnalyzerRule flips a Scan's name on every Apply —
// "a" ↔ "b". Models a contract violation; the Driver must panic during
// the analyzer batch's verification pass.
type NonIdempotentTestAnalyzerRule struct{}

// Name implements Rule.
func (NonIdempotentTestAnalyzerRule) Name() string { return "non-idempotent-analyzer" }

// isAnalyzerRule satisfies the sealed marker.
func (NonIdempotentTestAnalyzerRule) isAnalyzerRule() {}

// Apply implements Rule.
func (NonIdempotentTestAnalyzerRule) Apply(n chplan.Node) (chplan.Node, bool) {
	if s, ok := n.(*chplan.Scan); ok {
		cp := *s
		if cp.Table == "a" {
			cp.Table = "b"
		} else {
			cp.Table = "a"
		}
		return &cp, true
	}
	return n, false
}
