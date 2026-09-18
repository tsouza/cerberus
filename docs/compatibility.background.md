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

## Why LogQL `/patterns` text is graded only on a constant line

Upstream's pattern ingester mines online, in push order, with per-ingester
lifetime state cerberus does not share — the first line to arrive seeds a
template that later lines join or split against as it stood at that
moment, out-of-order entries are dropped, and clusters are LRU-evicted,
pruned on chunk age and throttled by an eviction-ratio limiter — so a
template over a line with variable positions is not a function of the
data alone and cannot be diffed as one; a constant line's template is the
line itself under every miner, which is why the seeder's fixture line is
constant and the tester grades it verbatim. The same reasoning is why
`/index/stats`'s `chunks` and `bytes` and `/index/volume`'s byte values
are not compared: they are chunk-storage quantities (per-chunk counts and
KB-rounded uncompressed chunk sizes) a row store has no analogue of.

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

## Why the spec oracle runs delayed name removal off

"Matches the reference" has exactly one meaning only if both surfaces run
the reference engine's semantics-affecting options identically.
[Cerberus issue #3271](https://github.com/tsouza/cerberus/issues/3271)
found they did not: the spec oracle built its engine via
`promqltest.NewTestEngine`, which hardcodes `EnableDelayedNameRemoval:
true`, while `compatibility/prometheus/docker-compose.yml` enables only
`promql-experimental-functions` on the real server, leaving delayed name
removal at Prometheus's own documented default of **off**
(`docs/feature_flags.md` in the vendored Prometheus source).

On most shapes the two settings agree. They diverge on exactly one: a
name-dropping fold (`rate`, `increase`, `sum_over_time`, …, plus their
`sum()`/`avg()` wrappers) over a colliding histogram/float `or`. With the
flag off, reference raises `vector cannot contain metrics with the same
labelset`. With it on, reference silently answers **one
histogram-valued series**, discarding the float sample. The mechanism:
reference's `mergeSeriesWithSameLabelset` merges the two name-collided
series and checks duplicate timestamps SEPARATELY for its Floats and
Histograms slices, so a float point and a histogram point at the same
timestamp slip past the check; materialising the instant vector then
prefers whichever slice is non-empty by TYPE. Reversing the `or`'s arms
still answers the histogram (ruling out left bias), and the same
engine's RANGE answer for the identical query is one series carrying
BOTH a float and a histogram sample at that timestamp — an
instant-query-only artefact no emitter here can reproduce. See
`combineMixedFoldBranches`'s doc in
`internal/promql/histogram_native_mixed_or_aggregate.go` for the full
mechanism and its bearing on cerberus's own plan shape.

The real server won, and the spec oracle was aligned down to it.
`promql-delayed-name-removal` is Prometheus's own opt-in, EXPERIMENTAL
feature — introduced under a feature flag in 3.6.0 (upstream #14477) and
still opt-in through the v3.11.3 tag the compat lane pins, including two
rounds of its OWN bugfixes in that span (upstream #17161, #17678 — both
released well before 3.11.3, neither covering this shape). Nothing in
that history signals it is close to becoming Prometheus's default, so
aligning the spec oracle DOWN to the real server's off default is
aligning to the stable, currently-shipping behaviour, not chasing a
setting about to change under it. The spec oracle now builds its
`promql.Engine` explicitly instead of via `promqltest.NewTestEngine`, and
its `EnableDelayedNameRemoval` constant documents the reasoning above at
the point where a future reader would otherwise silently flip it back.

The parser-options difference is not held to the same rule because it
changes which fixtures the oracle can ATTEMPT, never which answers count
as passing: `Evaluate`'s own doc explains that an upstream parse
rejection on a cerberus-only extension is a fact about the fixture, not
a parity failure, so a broader oracle grammar only means fewer fixtures
go unattempted.
