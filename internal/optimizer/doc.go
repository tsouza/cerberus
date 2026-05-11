// Package optimizer rewrites a chplan tree to an equivalent, cheaper one by
// running registered rules to a fixpoint.
//
// Rule interface, fixpoint driver, and the seed rules (predicate pushdown,
// projection pushdown, constant folding) land in seed PR6.
package optimizer
