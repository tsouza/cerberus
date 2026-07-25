package logql

import syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"

// NormalizeDottedLabels is the logql-head entry point for the LogQL
// stream-selector dotted-label rewrite. Handlers OUTSIDE the logql
// package (internal/api/loki/index_stats.go's selectorMatchers,
// internal/api/loki/tail.go's stream parser) call this before handing
// the query string to ParseExprPermissive. The rewrite itself lives
// beside the parser in lsyntax so every consumer of that parser —
// including the migration parity gate's offline query classifier,
// which must not depend on this package — shares one implementation.
func NormalizeDottedLabels(q string) string {
	return syntax.NormalizeDottedLabels(q)
}

// normalizeLokiDottedLabels is the package-private form Lang.Parse
// routes through, for symmetry with the PromQL head's own
// dotted-name normaliser.
func normalizeLokiDottedLabels(q string) string {
	return syntax.NormalizeDottedLabels(q)
}
