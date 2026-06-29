# Optimization rules

Binding rules for how performance optimizations are built in cerberus. They
exist because the cheap-looking version of an optimization tends to either rot
the hot path or get adopted without evidence.

## Rule 1: every optimization is a registered `chopt` feature, resolved once at boot, wired as a pure-polymorphic strategy

Every optimization whose use depends on the ClickHouse server version, a
resolved feature, or an operator toggle follows ONE lifecycle end to end —
register, resolve at boot, dispatch through a strategy — with no per-query
branch. This is a single rule, not separate ones.

**1. Register it as a `chopt` feature.** Every performance / optimization toggle
MUST be a named feature in the `chopt` registry, reached through
`CERBERUS_CH_OPTIMIZATIONS` — never a standalone `CERBERUS_*` environment bool
with its own parse, default, and read site. Register it in
`internal/chopt/registry.go` with an `ID`, a `Stability` (`Stable` for
auto-eligible result-equivalent features; `Experimental` / opt-in for tradeoffs
that must stay off by default), and a floor — a real `minVersion`, or
`chopt.AlwaysAvailable` when it depends on no server version (a purely
client-side optimization).

**2. Resolve it once at boot.** The feature is read exactly once, at startup,
when `chopt.EnabledSet` is resolved from the single server-version probe. There
is no second source of truth and no per-knob env read scattered through config.

**3. Wire it as a concrete polymorphic strategy.** The resolved
`EnabledSet.Has(FeatureX)` selects a concrete strategy implementation at
construction (or, when resolve must run after the client is built, installed
once at boot before any handler serves). The fallback is a CONCRETE default
implementation (the fan-out lowerer, the row decoder), not a nil field — always
wired.

**4. Keep the quickstart able to demo it.** When a new optimization's floor
exceeds the quickstart's ClickHouse version, bump the quickstart
(`docker-compose.yml` + the compatibility image tags) and every version
reference to a version that supports it, and update the central versions file
(`versions.yaml`). `versions.yaml` is the single source of truth for the
ClickHouse versions the deployment surface depends on (`min_clickhouse` =
preflight floor, `quickstart_clickhouse` = the demo image tag, `chdb_substrate`
= the test substrate); the per-feature floors stay in
`internal/chopt/registry.go` and are NOT duplicated there. The
version-consistency check
(`.github/scripts/clickhouse-version-sync.mjs`, wired into the `forbid-skip`
gate) derives the highest enabled floor from the registry and FAILS if the
quickstart is too old, so a floor-raising optimization that forgets this bump
cannot merge.

**The hard invariant:** the per-query hot path is a plain interface call with NO
branch of any kind — no `if cfg.X`, no `optSet.Has(...)`, no `serverAtLeast(...)`,
no `ProbeVersion`, and no nil / presence check of a strategy
(`if c.strategy != nil`). The decision is already made; at query time every
optimization has already landed. Query-shape eligibility ("is this rate over a
counter", "is this a matrix block") is allowed, but lives INSIDE the chosen
strategy, which delegates to its embedded fallback for shapes it cannot handle —
it never appears at the dispatch site.

Rationale: one env surface (operators tune everything through
`CERBERUS_CH_OPTIMIZATIONS=auto|off|<list>` plus
`CERBERUS_CH_OPTIMIZATIONS_MODE=enforcing|permissive`); uniform semantics for
free (auto / off / explicit-list opt-in / enforcing-vs-permissive policy /
unknown-id typo guard — a standalone bool re-implements and inherits none of
these); one boot-resolution path; and a hot path with no repeated flag reads and
no risk of version logic drifting back into it. The version decision happens
once; the codepath is fixed for the process lifetime.

In tree: the `promql.RangeLowerers` native-lowering strategies (rate, staleness
/ resample); the `chclient.cursorDecoder` (row vs columnar matrix decode); the
engine query-settings strategies (`aggregation_in_order`, `condition_cache`).
**`columnar_result_decode`** is the worked non-version-gated example: a
client-side ch-go columnar `query_range` decode, so its floor is
`chopt.AlwaysAvailable` (no server version requirement) and its stability is
opt-in (never enabled by `auto`, since the second ch-go dial is a tradeoff). It
replaced the standalone `CERBERUS_COLUMNAR_MATRIX_DECODE` bool that violated this
rule; the chclient keeps a source-agnostic `Config.ColumnarMatrixDecode` knob,
but its production value flows from `EnabledSet.Has(FeatureColumnarResultDecode)`
at boot, not from any env var.

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
`chplan.ReanchorRange` shares the immutable off-spine subtree copy-on-write
rather than deep-copying it K+1 times, collapsing the clone cost to
O(spine-depth). On the committed reproducible `BenchmarkSlice` (the canonical
`sum by (job)(rate(...))` shape) this measures ~2-2.5x faster with ~50-73% fewer
allocations at K = 2..16. A wide off-spine subtree can push the win far higher
(the 17-37x range), but that is an outlier, not the representative figure.

Process rule: any change motivated by cloning cost must be backed by a
benchmark against the current code, in a throwaway tree, before it is adopted.
