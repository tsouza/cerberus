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

How to add one: register a `chopt` feature with its `serverAtLeast` floor (a
real `minVersion`, or `chopt.AlwaysAvailable` for a client-side optimization
that depends on no server version — see Rule 3), resolve it into the boot
`EnabledSet`, select the concrete strategy at construction (or, when the
resolve must run after the client is built, install it once at boot before any
handler serves), and call through the interface per query.

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

## Rule 3: every performance toggle is a registered `chopt` feature, never a standalone `CERBERUS_*` env bool

Every performance / optimization toggle MUST be a named feature registered in
the `chopt` registry and reached through `CERBERUS_CH_OPTIMIZATIONS`, resolved
once at boot into the immutable `chopt.EnabledSet`. It MUST NOT be a standalone
`CERBERUS_*` environment bool with its own parse, default, and read site.

Why:

- **One surface.** Operators tune every optimization through a single env var
  (`CERBERUS_CH_OPTIMIZATIONS=auto|off|<list>`) plus its mode
  (`CERBERUS_CH_OPTIMIZATIONS_MODE=enforcing|permissive`). A per-knob
  `CERBERUS_X` bool fragments that surface into env sprawl, each with its own
  default and silent-typo failure mode.
- **Uniform semantics for free.** Registry features inherit `auto` (best
  available), `off` (absolute kill-switch), explicit-list opt-in, the
  enforcing-vs-permissive unsupported-feature policy, and the unknown-id typo
  guard. A standalone bool re-implements none of these and gets none of them.
- **One boot-resolution path.** The decision is resolved exactly once, after
  the version probe, into the `EnabledSet` every consumer reads via
  `EnabledSet.Has(...)`. There is no second source of truth and no per-knob
  read scattered through config.

How:

- Register the feature in `internal/chopt/registry.go`: give it an `ID`, a
  `Stability` (`Stable` for auto-eligible result-equivalent features,
  `Experimental` / opt-in for tradeoffs that must stay off by default), and a
  floor — a real `minVersion`, or `chopt.AlwaysAvailable` when it depends on no
  server version (a purely client-side optimization).
- Consume it via `EnabledSet.Has(FeatureX)` at the boot wiring point (Rule 1);
  never re-read the environment downstream.

Example — **`columnar_result_decode`** is a non-version-gated opt-in feature:
it is a client-side decode (the ch-go columnar `query_range` matrix path), so
its floor is `chopt.AlwaysAvailable` (no server version requirement) and its
stability is opt-in (never enabled by `auto`, since the second ch-go dial is a
tradeoff). It replaced the standalone `CERBERUS_COLUMNAR_MATRIX_DECODE` bool,
which violated this rule. The chclient keeps a source-agnostic
`Config.ColumnarMatrixDecode` knob, but its production value flows from
`EnabledSet.Has(FeatureColumnarResultDecode)` at boot, not from any env var.
