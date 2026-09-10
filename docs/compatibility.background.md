# Compatibility harnesses — background

This document collects the design rationale, rejected alternatives, incident
history, and measurements behind [`compatibility.md`](compatibility.md). It
answers "why is it built this way" rather than "what does it do" — nothing
here is required to run a harness, read its report, or move a baseline
correctly; `compatibility.md` is self-sufficient for that.

## Per-head confidence

The three legs are *not* equally strong, and the docs should not imply
they are:

| Head    | Reference          | Corpus origin                                  | Numerical confidence                                                                        |
| ------- | ------------------ | ---------------------------------------------- | ------------------------------------------------------------------------------------------- |
| PromQL  | real Prometheus    | third-party `prometheus/compliance` (CNCF)     | **Highest** — industry-standard conformance suite, full parity, no allow-list               |
| LogQL   | real Loki          | Grafana's own `pkg/logql/bench` corpus         | **Solid** — real backend + real corpus, but a Grafana bench set, not a conformance standard |
| TraceQL | real Tempo         | cerberus-owned author-written TXTAR            | **Lowest** — real backend, but no third-party suite; corpus breadth is author-bounded       |

All three run against a real reference backend on identical seeded data,
so each catches genuine semantic divergence. The difference is *corpus
provenance and breadth*: PromQL inherits an externally-curated standard;
TraceQL's coverage is only as wide as the author wrote it. Raising
TraceQL's confidence is the top improvement item.

## Why the parity oracle compares with a relative tolerance

Cerberus accumulates in ClickHouse and the reference engine accumulates
in Go, and floating-point addition is not associative, so the same
samples folded in a different order land a few ULPs (units in the last
place) apart. Two independent libm implementations of a transcendental
function are permitted the same freedom: IEEE-754 requires each to be
*correctly rounded* for its own algorithm, not to agree bit-for-bit with
the other. Both are facts about IEEE-754 arithmetic; neither is a
lowering bug, and neither is chaseable in cerberus's SQL.

### The measured divergences behind the bound

The measured divergences `summationReorderRelativeTolerance` accepts — for
example `2 atan2 up`'s 1-ULP libm difference (reference
`1.3734007669450157` vs. cerberus `1.373400766945016`,
[#1985](https://github.com/tsouza/cerberus/issues/1985)), `^`'s 2-ULP
difference ([#2598](https://github.com/tsouza/cerberus/issues/2598)),
native exponential-histogram interpolation's 1-5 ULPs
([#2024](https://github.com/tsouza/cerberus/issues/2024)), and native
`increase()`'s reordered window sums
([#2909](https://github.com/tsouza/cerberus/issues/2909)) — all sit
three to four orders of magnitude *inside* the bound, while the
smallest genuine disagreement the round-trip lane has produced
(`3.03e-2` relative) sits ten orders *outside* it. That separation is what
`TestEqualValuesRejectsRealDivergence` pins.

### Why results are not buffered to chase bit-for-bit agreement

Buffering results client-side to chase bit-for-bit agreement on the 17th
significant digit would mean abandoning cerberus's
push-down/never-buffer-unboundedly architecture for a divergence with no
practical monitoring impact. That is why cerberus still evaluates
vector-involving `atan2`, `^`, and window sums in ClickHouse SQL rather
than in Go.

## Why a vanished or unrecorded case is fatal

`VANISHED` and `UNRECORDED` are loud rather than silent on purpose. A
divergence must never be retired by deleting or renaming its case, and
corpus coverage must never shrink unnoticed — so a disappearance is a
failure that names the missing IDs. Likewise, a newly-passing case that
nobody records is a case no future run is gated on, which means the
ratchet has not actually ratcheted. `ARRIVED-FAILING` is fatal for the
same reason the project has no allow-lists: "it wasn't passing before" is
exactly the reasoning an allow-list encodes, and accepting it would let a
corpus refresh import known-bad behaviour under a green check.

## Why the ratchet is the opposite of an allow-list

The baseline asserting `passed == total == cases.length` for every head is
what keeps it the opposite of the deleted `expected-failures.json`: an
allow-list names the cases you are permitted to fail, whereas this roster
names the cases that must pass, and every entry on it is an obligation.

## Why the per-head roster counts live only in the baseline

The selected head's deterministic buckets are synced from its
`compat-cases.json`, so a second copy of `passed` / `total` in
`compatibility.md` would be a hand-typed restatement that every
corpus-adding PR has to re-type — and that two such PRs conflict over.
That is the drift `doc-counts.mjs` exists to prevent, and why the
contract doc states the counts by reference instead of printing them.
