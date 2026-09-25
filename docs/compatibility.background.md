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

"Matches the reference" has exactly one meaning only if both PromQL
reference surfaces — the spec lane's in-process oracle and the compat
lane's real `prom/prometheus` server — run the engine's
semantics-affecting options identically. Cerberus issue #3271 found they
did not: the spec oracle built its engine via `promqltest.NewTestEngine`,
which hardcodes `EnableDelayedNameRemoval: true`, while
`compatibility/prometheus/docker-compose.yml` enables only
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
`promql-delayed-name-removal` is Prometheus's own opt-in,
EXPERIMENTAL feature — introduced under a feature flag in 3.6.0
(upstream #14477) and still opt-in through the v3.11.3 tag the compat
lane pins, including two rounds of its OWN bugfixes in that span
(upstream #17161, #17678 — both released well before 3.11.3, neither
covering this shape). Nothing in that history signals it is close to
becoming Prometheus's default, so aligning the spec oracle DOWN to the
real server's off default is aligning to the stable, shipping behaviour,
not chasing a setting about to change under it. The oracle's
`EnableDelayedNameRemoval` constant documents this reasoning at the point
where a reader would otherwise silently flip it back, and
`test/regression/promql_oracle_engine_parity_test.go` turns any future
disagreement into a CI failure naming both sites instead of a silent
per-fixture disagreement.

## Why invalid UTF-8 is in scope, and how the emitted shapes read it

OTLP defines its string fields as UTF-8, but a writer that does not enforce
it — a direct `INSERT`, another exporter — can store any bytes, and the
reference engines answer on those bytes. Compatibility with them is the source
of truth, so a value that is not valid UTF-8 is held to the same standard as
any other rather than declared outside the input contract.

### What diverged

Go's `regexp` decodes its input with `utf8.DecodeRuneInString`: a byte that
does not begin a valid sequence is one U+FFFD rune of width one. ClickHouse
evaluates patterns with RE2 in UTF-8 mode, which never matches a lone invalid
byte, and which reads some invalid sequences as one character where Go reads
one U+FFFD per byte — measured on 26.7.13.12, 26.8.10.6 and 24.8.14.39:
`\xed\xa0\x80` (an encoded surrogate) matches `^.$` and `^[\x{D800}-\x{DFFF}]$`,
`\xe0\x80\x80` (overlong) and `\xf4\x90\x80\x80` (above U+10FFFF) match `^.$`
but not `^[^\x{FFFD}]*$`, so RE2 reads them as a single U+FFFD. ClickHouse
26.7's regex compiler matches bytes and agrees with Go on the shapes it
compiles, but only for those shapes, and only once a pattern has been used
`min_count_to_compile_regular_expression` times — so the same query could
answer differently from one run to the next.

### Why the value is rewritten per byte, not with `toValidUTF8`

`toValidUTF8` collapses a run of invalid bytes into one U+FFFD, so `^..$` on
`\xff\xfe` — two runes to Go — would read as one. The emitted rewrite splits
the value into chunks, each a byte that is not a continuation byte followed by
the continuation bytes after it: Go's decoder starts a rune at every such byte,
so a chunk keeps the sequence its lead byte announces when `isValidUTF8`
accepts it, and every other byte is one U+FFFD. A differential of 3,009 values
(random strings over lead, continuation and invalid bytes, up to eight bytes)
against `utf8.DecodeRuneInString` agreed on all three builds.

### Why text results go through a substitute block

Replacing each invalid byte by U+FFFD makes a match exact but loses the byte
a capture or a replaced string must carry, and U+FFFD cannot be mapped back:
the value may hold U+FFFD itself. Evaluating a second time over a spelling in
which each invalid byte is its own code point keeps the byte recoverable. The
block U+10FF80..U+10FFFF was chosen because a pattern treats all of it the way
it treats U+FFFD unless it singles out private-use characters or U+FFFD, and
because each code point's UTF-8 encoding carries the byte in its last two
bytes, so it is recovered with integer arithmetic. The two evaluations parse
identically, so their results align rune for rune and differ only where an
invalid byte was copied. A pattern that does tell the block from U+FFFD is
rewritten so that it does not; the rewrite cannot also keep a genuine
block character apart from U+FFFD, which is why such a value falls back to the
U+FFFD form.

### Why the guard is `isValidUTF8`, and what it costs

Rejected alternatives, each measured over 10M rows on 26.7.13.12:

- `match(v, p) OR (NOT isValidUTF8(v) AND match(<U+FFFD form>, p))` relies on
  RE2 never matching where Go does not, which the encoded-surrogate case above
  disproves, and ClickHouse's short-circuit evaluation copies the column for
  the second operand: +36% CPU on an anchored label matcher against +14% for
  the `if` form.
- `isASCII` is about twice as cheap as `isValidUTF8` but does not exist on the
  24.8 floor.
- Rewriting the pattern instead of the value (`\C` for bytes) cannot express
  "one invalid byte" without look-around, which RE2 lacks.

`isValidUTF8` is evaluated only for a pattern an invalid byte can take part
in; a pattern with no `.`, no class or literal holding U+FFFD or a surrogate
and no word boundary answers alike under every engine and is emitted
unguarded. The U+FFFD branch costs nothing on valid data: ClickHouse evaluates
it only for rows `isValidUTF8` rejects.

Measured with `just regex-jit-bench`'s warm scenario (26.7.13.12, a
container limited to 2 CPUs and 6 GiB, one hour of gauge samples for 20,000
pods and 10M log lines; server CPU milliseconds per request, mean of five),
on a host shared with other work, so differences under about 2% are noise —
the unguarded line filter, whose SQL did not change, moved by −1.4% and
+0.9%:

| Emitted shape                                  | Guarded | `jit=default`, before → after | `jit=off`, before → after |
| ---------------------------------------------- | ------- | ----------------------------- | ------------------------- |
| PromQL matcher `api-.*`                        | yes     | 7847 → 8067 (+2.8%)           | 8626 → 8905 (+3.2%)       |
| PromQL matcher `api-.*\|none-.*`               | yes     | 9224 → 9648 (+4.6%)           | 9176 → 9634 (+5.0%)       |
| `label_replace` with `(.*)-[0-9]+-.*`          | yes     | 9254 → 9319 (+0.7%)           | 9874 → 10233 (+3.6%)      |
| Line filter `timeout after [0-9]+ms`           | no      | 7401 → 7465 (+0.9%)           | 7648 → 7541 (−1.4%)       |
| Line filter `user=.*admin`                     | yes     | 12470 → 13238 (+6.2%)         | 12575 → 13341 (+6.1%)     |
| `unwrap duration()` after `logfmt`             | yes     | 81301 → 89287 (+9.8%)         | 82156 → 89559 (+9.0%)     |

An interleaved rerun of the two LogQL rows against one server over 2M lines
put them at +8.2% and +4.2%, and the same query with the guard left off the
regex functions the `logfmt` and `unwrap duration()` lowerings emit —
`[^0-9.]` in the duration error classification among them — measured the
same as before the change, which places the `unwrap` cost there: those
functions run on every row, and `isValidUTF8` costs about as much as the
regular expression it guards (0.6 ns per byte against the compiled
`match`'s 0.1).

### Why a literal U+FFFD follows Go's `regexp` rather than Prometheus's matcher

Prometheus's `FastRegexMatcher` answers a pattern's literal parts with string
comparison where it can — `\x{FFFD}` as an equality, `\x{FFFD}.*` as a prefix
test — and hands the rest to Go's `regexp`, so whether a literal U+FFFD
matches an invalid byte depends on how it decomposes the pattern: `\x{FFFD}`
does not match `\xff`, `\x{FFFD}+` does. Loki's line-filter simplification
does the same for literal filters. Reproducing that decomposition would tie
the emitter to one upstream optimiser's internals for a pattern that names
U+FFFD itself; the emitter reads it as the regular expression means.
