package promql

// This file is the home for offset / @ modifier semantics. The
// resolution rules live alongside the selector evaluation
// (selector.go::effectiveEvalTs) since modifiers attach to vector
// selectors specifically and the selector path is where they take
// effect.
//
// Keeping the file in the package boundary makes the design intent
// obvious to a reader scanning the file list: modifiers are first-
// class citizens of the evaluator, not buried inside the selector
// implementation.
//
// In a future expansion (subqueries, etc.) any cross-cutting modifier
// helpers (e.g., propagating @ts down through a Call argument) would
// live here.
