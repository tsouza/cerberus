// Package format consolidates per-handler formatting helpers shared
// across internal/api/{prom,loki,tempo}.
//
// Scope: helpers that operate on plain values (label maps, parameter
// strings, durations) — i.e. anything that doesn't touch a handler's
// wire-format structs. Vector / matrix pivots remain in each handler
// because the response shapes diverge (Prom uses named Sample{T,V}
// tuples, Loki uses [2]any heterogeneous tuples); folding those into
// a shared helper would require pushing the wire types here, which
// inverts the dependency direction.
package format
