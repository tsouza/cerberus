package chsql

// This file pins the equivalence status of a gremlins mutant in
// emit_size_bound.go (phase2 scope ./internal/chsql, cerberus issue #2741)
// that no test can distinguish from the original.

// NOT KILLABLE — documented, not defended by a test.
//
// emit_size_bound.go:251:23 (CONDITIONALS_BOUNDARY, `d > deepest` ->
// `d >= deepest` in gridCarrierNesting's per-child running-maximum update)
// is EQUIVALENT. The mutation only changes behaviour when `d == deepest`:
// the original skips the assignment (already equal, nothing to update) and
// the mutant runs `deepest = d`, which assigns the SAME int value back to
// itself — a same-value reassignment with no other side effects (deepest is
// a plain int, not a pointer or a type carrying identity). Every other
// input takes the identical branch under both spellings, so `deepest`'s
// final value — and therefore `gridCarrierNesting`'s returned nesting
// depth — is byte-for-byte identical whether the boundary is `>` or `>=`.
