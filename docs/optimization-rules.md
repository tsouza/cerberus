# Optimization rules

Binding rules for how performance optimizations are built in cerberus. They
exist because the cheap-looking version of an optimization tends to either rot
the hot path or get adopted without evidence.

## Rule 1: version- and feature-gated optimizations are boot-wired strategies, never per-query branches

Every optimization whose use depends on the ClickHouse server version, a
resolved feature, or an operator toggle MUST be selected once at boot and
wired into a concrete polymorphic strategy. The per-query hot path is a plain
interface call with no branch of any kind.

- The version / feature / flag is read exactly once, at startup, when the
  `chopt.EnabledSet` is resolved from the single server-version probe, and is
  used to choose the concrete strategy implementation.
- The query path must contain no `if cfg.X`, no `optSet.Has(...)`, no
  `serverAtLeast(...)`, no `ProbeVersion`, and no nil / presence check of a
  strategy (`if c.strategy != nil`). The decision is already made.
- The fallback is a CONCRETE default implementation (the fan-out lowerer, the
  row decoder), not a nil field. It is always wired.
- Query-shape eligibility ("is this rate over a counter", "is this a matrix
  block") is allowed, but lives INSIDE the chosen strategy, which delegates to
  its embedded fallback for shapes it cannot handle. It never appears at the
  dispatch site.

Rationale: the version decision happens once; the codepath is fixed for the
process lifetime; the hot path carries no repeated flag reads and no risk of
version logic drifting back into it.

How to add one: register a `chopt` feature with its real `serverAtLeast`
floor, resolve it into the boot `EnabledSet`, select the concrete strategy at
construction, and call through the interface per query.

In tree: the `promql.RangeLowerers` native-lowering strategies (rate,
staleness / resample); the `chclient.cursorDecoder` (row vs columnar matrix
decode); the engine query-settings strategies (`aggregation_in_order`,
`condition_cache`).

## Rule 2: speed up cloning by cloning less, not by cloning faster

cerberus's deep-clone (`internal/chplan/clone.go`) is hand-written and already
optimal per node: exhaustive type switch, exact-length slice preallocation, no
reflection. Do NOT introduce a generic deep-copy library or a code generator
to make cloning faster. Measured against the current code, every such approach
lands at ~1.0x, because the hand-written clone already emits exactly what a
generator would produce. (A code generator may still be worth adopting purely
for maintainability / exhaustiveness-by-construction, but it is never a
speedup, and a reflection or library clone that cannot preserve unexported and
interface fields correctly is disqualified regardless of its speed.)

The real lever is structural: clone LESS. On the slicer hot path,
`chplan.ReanchorRange` deep-copies a byte-identical off-spine subtree K+1
times; copy-on-write sharing of the immutable off-spine collapses that to
O(spine-depth) and measures 9-13x faster with ~17x fewer allocations at
K = 2..16.

Process rule: any change motivated by cloning cost must be backed by a
benchmark against the current code, in a throwaway tree, before it is adopted.
